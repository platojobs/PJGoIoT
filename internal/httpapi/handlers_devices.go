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

func (s *Server) handleDeviceType(c *gin.Context) {
	var param typeURIParam
	if err := c.ShouldBindUri(&param); err != nil {
		failValidation(c, err)
		return
	}
	deviceType := strings.ToLower(strings.TrimSpace(param.Type))
	item, err := s.store.GetDeviceType(c.Request.Context(), deviceType)
	if err != nil {
		s.storeFailure(c, "查询设备类型失败", err)
		return
	}
	c.JSON(http.StatusOK, item)
}

// handleDeviceCatalog 汇总某设备类型的元数据、固件与配件，供无法获取 MAC 的客户端使用。
func (s *Server) handleDeviceCatalog(c *gin.Context) {
	var param typeURIParam
	if err := c.ShouldBindUri(&param); err != nil {
		failValidation(c, err)
		return
	}
	deviceType := strings.ToLower(strings.TrimSpace(param.Type))

	detail, err := s.store.GetDeviceType(c.Request.Context(), deviceType)
	if err != nil {
		s.storeFailure(c, "查询设备类型失败", err)
		return
	}
	firmwares, err := s.store.ListFirmwares(c.Request.Context(), deviceType)
	if err != nil {
		s.storeFailure(c, "查询固件列表失败", err)
		return
	}
	accessories, err := s.store.ListAccessories(c.Request.Context(), deviceType)
	if err != nil {
		s.storeFailure(c, "查询配件列表失败", err)
		return
	}
	if firmwares == nil {
		firmwares = []model.Firmware{}
	}
	if accessories == nil {
		accessories = []model.Accessory{}
	}
	c.JSON(http.StatusOK, model.DeviceCatalog{
		DeviceType:  detail,
		Firmwares:   firmwares,
		Accessories: accessories,
	})
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

// handleMyDeviceRoute 统一分发 /my/devices/{mac} 与 /my/devices/{platform}/{externalId}。
// Gin 的树结构不支持同一段上同时存在 :mac 与 :platform/:externalId，故使用 catch-all。
func (s *Server) handleMyDeviceRoute(c *gin.Context) {
	rest := strings.Trim(c.Param("key"), "/")
	if rest == "" {
		fail(c, http.StatusNotFound, CodeNotFound, "资源不存在或无权访问")
		return
	}
	parts := strings.Split(rest, "/")
	// /my/devices/{mac}、/my/devices/{platform}/{externalId} 以及读数子路径共用同一 catch-all。
	switch {
	case len(parts) == 1:
		s.respondMyDeviceByMAC(c, parts[0])
	case len(parts) == 2:
		s.respondMyDeviceByKey(c, parts[0], parts[1])
	case len(parts) == 3 && parts[2] == "readings":
		s.handleReadingsList(c, parts[0], parts[1])
	case len(parts) == 4 && parts[2] == "readings" && parts[3] == "summary":
		s.handleReadingsSummary(c, parts[0], parts[1])
	case len(parts) == 4 && parts[2] == "readings" && parts[3] == "series":
		s.handleReadingsSeries(c, parts[0], parts[1])
	case len(parts) == 4 && parts[2] == "readings" && parts[3] == "metrics":
		s.handleReadingsMetrics(c, parts[0], parts[1])
	default:
		fail(c, http.StatusNotFound, CodeNotFound, "资源不存在或无权访问")
	}
}

// respondMyDeviceByMAC 处理 Android 常见的按 MAC 查询。
func (s *Server) respondMyDeviceByMAC(c *gin.Context, rawMAC string) {
	mac, err := validate.NormalizeMAC(rawMAC)
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

// respondMyDeviceByKey 处理 iOS 等按 (platform, externalId) 查询。
// 任何不属于当前用户的键都返回 404，避免泄露记录是否存在。
func (s *Server) respondMyDeviceByKey(c *gin.Context, rawPlatform string, rawExternalID string) {
	platform, err := validate.NormalizePlatform(rawPlatform)
	if err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "platform 只能是 android 或 ios")
		return
	}
	_, externalID, err := validate.NormalizeExternalID(platform, rawExternalID)
	if err != nil {
		if platform == validate.PlatformAndroid {
			fail(c, http.StatusBadRequest, CodeInvalidMAC, "Android externalId 必须是 6 字节 MAC")
			return
		}
		fail(c, http.StatusBadRequest, CodeBadRequest, "iOS externalId 必须是 UUID")
		return
	}
	device, err := s.store.GetUserDeviceByKey(c.Request.Context(), userID(c), platform, externalID)
	if err != nil {
		s.storeFailure(c, "查询用户设备失败", err)
		return
	}
	c.JSON(http.StatusOK, device)
}

// storeFailure 把存储错误交给统一错误中间件：
// ErrNotFound/ErrUnauthorized 分别映射 404/401，其余记录日志后返回 500。
func (s *Server) storeFailure(c *gin.Context, operation string, err error) {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrUnauthorized) ||
		errors.Is(err, store.ErrForbidden) ||
		errors.Is(err, store.ErrTimeout) || errors.Is(err, store.ErrCursorExpired) ||
		errors.Is(err, store.ErrDuplicateDevice) || isStatementTimeout(err) || isDBUnavailable(err) {
		// 交给统一错误出口映射；数据库不可达/语句超时必须返回 503 可重试语义，
		// 不能在这里直接压成 500 INTERNAL。
		c.Error(err)
		c.Abort()
		return
	}
	s.log.Error(operation, "error", err, "request_id", c.GetString(ctxKeyRequestID))
	s.metrics.incDBErrors()
	fail(c, http.StatusInternalServerError, CodeInternal, "服务内部错误")
}
