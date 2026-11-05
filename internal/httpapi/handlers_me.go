package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

const (
	defaultExportLimit = 5000
	maxExportLimit     = 5000
)

// handleExport 导出当前用户全部数据：user、devices（含 tombstone）、readings（按 recordedAt 升序）。
// 读数超过 limit 时返回 nextCursor/hasMore，客户端带 cursor 继续拉取。
func (s *Server) handleExport(c *gin.Context) {
	limit := defaultExportLimit
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxExportLimit {
			fail(c, http.StatusBadRequest, CodeBadRequest, "limit 取值范围 1-5000")
			return
		}
		limit = n
	}

	user, err := s.store.GetUser(c.Request.Context(), userID(c))
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	devices, err := s.store.ListUserDevicesForExport(c.Request.Context(), userID(c))
	if err != nil {
		s.storeFailure(c, "导出用户设备失败", err)
		return
	}
	readings, nextCursor, hasMore, err := s.store.ExportReadings(c.Request.Context(), userID(c), c.Query("cursor"), limit)
	if err != nil {
		if errors.Is(err, store.ErrBadCursor) {
			fail(c, http.StatusBadRequest, CodeBadRequest, "cursor 无效")
			return
		}
		s.storeFailure(c, "导出读数失败", err)
		return
	}
	if devices == nil {
		devices = []model.UserDevice{}
	}
	if readings == nil {
		readings = []model.ExportReading{}
	}

	page := model.ExportPage{
		User:       user,
		Devices:    devices,
		Readings:   readings,
		ExportedAt: time.Now().UTC(),
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="pjkotlinapp-export-%s.json"`, user.ID))
	c.JSON(http.StatusOK, page)
}

// handleDeleteMe 硬删除当前用户及其全部关联数据（外键级联）。
// 成功返回 204；重复调用因 token 已失效返回 401，而不是 404。
func (s *Server) handleDeleteMe(c *gin.Context) {
	if err := s.store.DeleteUser(c.Request.Context(), userID(c)); err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.Status(http.StatusNoContent)
}
