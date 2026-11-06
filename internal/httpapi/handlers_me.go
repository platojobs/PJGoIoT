package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/auth"
	"github.com/platojobs/PJGoIoT/internal/model"
)

const (
	// deletionRequestTTL 是一次性删除令牌的有效期。
	// 10 分钟足够用户读完「将删除哪些数据」的二次确认，又不会让令牌长期可重放。
	deletionRequestTTL = 10 * time.Minute
	// exportTaskRetention 是导出产物的保留期；到期后由 cmd/cleanup 整行清理。
	exportTaskRetention = 24 * time.Hour
	// exportDownloadTTL 是下载链接（一次性令牌）的有效期。
	exportDownloadTTL = 10 * time.Minute
	// exportBuildTimeout 是后台聚合单个账号数据的超时预算。
	exportBuildTimeout = 60 * time.Second
)

// deletionTokenHeader 是 DELETE /me 的确认凭据头。
// 该接口不使用 Bearer：账号删除后旧 token 已失效，无法再通过鉴权中间件，
// 若强行要求 Bearer 会让「重放删除」永远得到 401，与「再次删除返回 404」的语义冲突。
const deletionTokenHeader = "X-Deletion-Token"

// handleCreateDeletionRequest 签发一次性删除令牌（只存哈希，10 分钟有效）。
// 同一账号的新申请会让旧令牌立即失效。
func (s *Server) handleCreateDeletionRequest(c *gin.Context) {
	token, hash, err := auth.NewToken()
	if err != nil {
		s.storeFailure(c, "生成删除令牌失败", err)
		return
	}
	expiresAt := time.Now().UTC().Add(deletionRequestTTL)
	if err := s.store.CreateDeletionToken(c.Request.Context(), userID(c), hash, expiresAt); err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.JSON(http.StatusCreated, model.DeletionRequest{Token: token, ExpiresAt: expiresAt})
}

// handleDeleteMe 用一次性删除令牌删除当前账号及全部个人数据。
//
// 二次确认：必须先在 POST /me/deletion-request 取得令牌，再在 10 分钟内带
// X-Deletion-Token 调用本接口，避免误触与跨站请求直接删号。
// 删除成功后令牌墓碑保留：同一请求重放得到 404（账号已不存在），不会造成部分删除。
func (s *Server) handleDeleteMe(c *gin.Context) {
	token := c.GetHeader(deletionTokenHeader)
	if token == "" {
		fail(c, http.StatusBadRequest, CodeDeletionTokenRequired, "缺少 X-Deletion-Token，请先调用 POST /me/deletion-request")
		return
	}
	if err := s.store.DeleteAccount(c.Request.Context(), auth.Hash(token), time.Now().UTC()); err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.Status(http.StatusNoContent)
}

// handleCreateExport 创建异步导出任务，返回 202 与任务 id；客户端轮询 GET /me/export/{id}。
//
// 选择纯异步而非「小体量同步返回、大体量异步」，是因为两套语义会让客户端难以处理：
// 同步分支要自己兜底分页/截断，异步分支要轮询；统一异步后，接口契约只有一种。
// 导出采用读数聚合，产物大小与账号读数条数无关，后台任务通常在秒级完成。
func (s *Server) handleCreateExport(c *gin.Context) {
	expiresAt := time.Now().UTC().Add(exportTaskRetention)
	task, err := s.store.CreateExportTask(c.Request.Context(), userID(c), expiresAt)
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}

	uid := userID(c)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), exportBuildTimeout)
		defer cancel()
		artifact, buildErr := s.store.BuildPersonalDataExport(ctx, uid)
		if buildErr != nil {
			s.log.Error("生成个人数据导出失败", "error", buildErr, "task_id", task.ID)
			if err := s.store.FailExportTask(ctx, task.ID, "导出失败，请稍后重试"); err != nil {
				s.log.Error("标记导出任务失败状态失败", "error", err, "task_id", task.ID)
			}
			return
		}
		if err := s.store.FinishExportTask(ctx, task.ID, artifact); err != nil {
			s.log.Error("写入导出产物失败", "error", err, "task_id", task.ID)
		}
	}()

	c.JSON(http.StatusAccepted, exportTaskView(task, ""))
}

// handleGetExport 查询导出任务状态；ready 时签发新的下载令牌并返回 downloadUrl。
// 每次查询都会替换旧的下载令牌，因此同一时刻只有一个有效下载链接。
func (s *Server) handleGetExport(c *gin.Context) {
	task, err := s.store.GetExportTask(c.Request.Context(), userID(c), c.Param("id"))
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	if task.Status != "ready" {
		c.JSON(http.StatusOK, exportTaskView(task, ""))
		return
	}

	token, hash, err := auth.NewToken()
	if err != nil {
		s.storeFailure(c, "生成下载令牌失败", err)
		return
	}
	downloadExpiresAt := time.Now().UTC().Add(exportDownloadTTL)
	task, err = s.store.PrepareExportDownload(c.Request.Context(), userID(c), c.Param("id"), hash, downloadExpiresAt)
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.JSON(http.StatusOK, exportTaskView(task, token))
}

// handleDownloadExport 消费一次性下载令牌并返回导出 JSON。
// 该接口不使用 Bearer：链接本身即凭据（短期 + 一次性），便于在浏览器直接打开。
// 令牌错误返回 403；已过期或已使用返回 410，客户端应重新查询任务状态获取新链接。
func (s *Server) handleDownloadExport(c *gin.Context) {
	token := c.Query("token")
	if token == "" {
		fail(c, http.StatusForbidden, CodeExportDownloadInvalid, "缺少下载令牌或链接无效")
		return
	}
	artifact, err := s.store.ConsumeExportDownload(c.Request.Context(), auth.Hash(token), time.Now().UTC())
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="pjgoiot-export-%s.json"`, c.Param("id")))
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.JSON(http.StatusOK, artifact)
}

// exportTaskView 把存储层状态映射为响应体；token 非空时拼出一次性下载地址。
func exportTaskView(task model.ExportTaskStatus, token string) model.ExportTask {
	view := model.ExportTask{
		ID:                task.ID,
		Status:            task.Status,
		CreatedAt:         task.CreatedAt,
		CompletedAt:       task.CompletedAt,
		FailureReason:     task.FailureReason,
		DownloadExpiresAt: task.DownloadExpiresAt,
		ExpiresAt:         task.ExpiresAt,
	}
	if token != "" {
		view.DownloadURL = fmt.Sprintf("/api/v1/me/export/%s/download?token=%s",
			url.PathEscape(task.ID), url.QueryEscape(token))
	}
	return view
}
