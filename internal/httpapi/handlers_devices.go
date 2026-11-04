package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/validate"
)

type listResponse struct {
	Items any `json:"items"`
}

func (s *Server) handlePublicDevice(c *gin.Context) {
	var param macURIParam
	if err := c.ShouldBindUri(&param); err != nil {
		failValidation(c, err)
		return
	}
	mac, err := validate.NormalizeMAC(param.MAC)
	if err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidMAC, "MAC 地址格式不合法，需为 6 字节蓝牙地址")
		return
	}
	device, err := s.store.GetPublicDevice(c.Request.Context(), mac)
	if err != nil {
		s.storeFailure(c, "查询公共设备失败", err)
		return
	}
	c.JSON(http.StatusOK, device)
}

func (s *Server) handleDeviceTypes(c *gin.Context) {
	types, err := s.store.ListDeviceTypes(c.Request.Context())
	if err != nil {
		s.storeFailure(c, "查询设备类型失败", err)
		return
	}
	if types == nil {
		types = []model.DeviceType{}
	}
	c.JSON(http.StatusOK, listResponse{Items: types})
}

func (s *Server) handleFirmwares(c *gin.Context) {
	var param typeURIParam
	if err := c.ShouldBindUri(&param); err != nil {
		failValidation(c, err)
		return
	}
	deviceType := strings.ToLower(strings.TrimSpace(param.Type))

	ok, err := s.store.DeviceTypeExists(c.Request.Context(), deviceType)
	if err != nil {
		s.storeFailure(c, "查询设备类型失败", err)
		return
	}
	if !ok {
		s.storeFailure(c, "固件列表", store.ErrNotFound)
		return
	}
	items, err := s.store.ListFirmwares(c.Request.Context(), deviceType)
	if err != nil {
		s.storeFailure(c, "查询固件列表失败", err)
		return
	}
	if items == nil {
		items = []model.Firmware{}
	}
	c.JSON(http.StatusOK, listResponse{Items: items})
}

func (s *Server) handleAccessories(c *gin.Context) {
	var param typeURIParam
	if err := c.ShouldBindUri(&param); err != nil {
		failValidation(c, err)
		return
	}
	deviceType := strings.ToLower(strings.TrimSpace(param.Type))

	ok, err := s.store.DeviceTypeExists(c.Request.Context(), deviceType)
	if err != nil {
		s.storeFailure(c, "查询设备类型失败", err)
		return
	}
	if !ok {
		s.storeFailure(c, "配件列表", store.ErrNotFound)
		return
	}
	items, err := s.store.ListAccessories(c.Request.Context(), deviceType)
	if err != nil {
		s.storeFailure(c, "查询配件列表失败", err)
		return
	}
	if items == nil {
		items = []model.Accessory{}
	}
	c.JSON(http.StatusOK, listResponse{Items: items})
}

func (s *Server) handleMyDevices(c *gin.Context) {
	items, err := s.store.ListUserDevices(c.Request.Context(), userID(c))
	if err != nil {
		s.storeFailure(c, "查询用户设备失败", err)
		return
	}
	if items == nil {
		items = []model.UserDevice{}
	}
	c.JSON(http.StatusOK, listResponse{Items: items})
}

func (s *Server) handleMyDevice(c *gin.Context) {
	var param macURIParam
	if err := c.ShouldBindUri(&param); err != nil {
		failValidation(c, err)
		return
	}
	mac, err := validate.NormalizeMAC(param.MAC)
	if err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidMAC, "MAC 地址格式不合法，需为 6 字节蓝牙地址")
		return
	}
	device, err := s.store.GetUserDevice(c.Request.Context(), userID(c), mac)
	if err != nil {
		s.storeFailure(c, "查询用户设备失败", err)
		return
	}
	c.JSON(http.StatusOK, device)
}

// storeFailure 把存储错误交给统一错误中间件：
// ErrNotFound/ErrUnauthorized 分别映射 404/401，其余记录日志后返回 500。
func (s *Server) storeFailure(c *gin.Context, operation string, err error) {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrUnauthorized) {
		c.Error(err)
		c.Abort()
		return
	}
	s.log.Error(operation, "error", err, "request_id", c.GetString(ctxKeyRequestID))
	fail(c, http.StatusInternalServerError, CodeInternal, "服务内部错误")
}
