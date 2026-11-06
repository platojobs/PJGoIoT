package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/scene"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// sceneListResponse 是 GET /scenes 的响应；items 恒为非 null 数组，客户端无需处理 null。
// 未传 limit 时只返回 items（与分页前完全一致）；传 limit 时额外返回分页字段。
type sceneListResponse struct {
	Items      []model.Scene `json:"items"`
	NextCursor *string       `json:"nextCursor,omitempty"`
	HasMore    *bool         `json:"hasMore,omitempty"`
}

// handleListScenes 返回本人未删除的场景，按 updatedAt 倒序。
// limit/cursor 为可选参数：不传 limit 时返回全量，保持两端现有实现不变。
func (s *Server) handleListScenes(c *gin.Context) {
	q := store.SceneQuery{Cursor: c.Query("cursor")}
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			fail(c, http.StatusBadRequest, CodeBadRequest, "limit 必须是正整数")
			return
		}
		q.Limit = n
	}
	page, err := s.store.ListScenes(c.Request.Context(), userID(c), q)
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	if page.Items == nil {
		page.Items = []model.Scene{}
	}
	resp := sceneListResponse{Items: page.Items}
	if q.Limit > 0 {
		hasMore := page.HasMore
		resp.HasMore = &hasMore
		if page.HasMore {
			next := page.NextCursor
			resp.NextCursor = &next
		}
	}
	c.JSON(http.StatusOK, resp)
}

// handleGetScene 返回本人单个场景；不存在、已删除或属于他人统一 404。
func (s *Server) handleGetScene(c *gin.Context) {
	item, err := s.store.GetScene(c.Request.Context(), userID(c), c.Param("sceneId"))
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.JSON(http.StatusOK, item)
}

// handleCreateScene 校验并创建场景。
func (s *Server) handleCreateScene(c *gin.Context) {
	var input model.SceneInput
	if err := c.ShouldBindJSON(&input); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	normalized, err := scene.NormalizeInput(input)
	if err != nil {
		s.sceneValidationFailure(c, err)
		return
	}
	item, err := s.store.CreateScene(c.Request.Context(), userID(c), normalized)
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.JSON(http.StatusCreated, item)
}

// handleUpdateScene 按字段级 LWW 应用补丁：
// 每个出现的字段独立比较 updatedAt，只改一个字段时不会覆盖其他字段。
func (s *Server) handleUpdateScene(c *gin.Context) {
	var patch model.ScenePatch
	if err := c.ShouldBindJSON(&patch); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	normalized, err := scene.NormalizePatch(patch)
	if err != nil {
		s.sceneValidationFailure(c, err)
		return
	}
	item, err := s.store.UpdateScene(c.Request.Context(), userID(c), c.Param("sceneId"), normalized, time.Now().UTC())
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.JSON(http.StatusOK, item)
}

// handleDeleteScene 软删除场景；tombstone 进入 change_log 供其他设备同步删除。
func (s *Server) handleDeleteScene(c *gin.Context) {
	if err := s.store.DeleteScene(c.Request.Context(), userID(c), c.Param("sceneId"), time.Now().UTC()); err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.Status(http.StatusNoContent)
}

// sceneValidationFailure 把场景校验错误交给统一错误出口；
// store.ErrScene* 有明确错误码，其余（如补丁缺少 updatedAt）按 400 参数错误返回。
func (s *Server) sceneValidationFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrSceneInvalidName),
		errors.Is(err, store.ErrSceneCooldownOutOfRange),
		errors.Is(err, store.ErrSceneInvalidTrigger),
		errors.Is(err, store.ErrSceneInvalidAction),
		errors.Is(err, store.ErrSceneDeviceNotFound),
		errors.Is(err, store.ErrSceneMetricNotSupported):
		c.Error(err)
		c.Abort()
	default:
		fail(c, http.StatusBadRequest, CodeBadRequest, "请求参数校验失败: "+err.Error())
	}
}
