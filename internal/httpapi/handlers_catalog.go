package httpapi

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/validate"
)

const (
	maxCatalogServices      = 20
	maxCatalogMetrics       = 20
	maxCatalogLimitations   = 20
	maxCatalogDisplayName   = 64
	maxCatalogDescription   = 2000
	maxCatalogServiceName   = 128
	maxCatalogLimitationLen = 500
)

// catalogTypePattern 与迁移中的 CHECK 约束一致。
var catalogTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// handleDeviceTypeCatalog 返回服务端设备类型目录。
//
// 缓存语义：
//   - ETag 由目录版本号派生（"catalog-v<version>"），管理端每次写入都会自增版本，
//     因此即使内容相同，ETag 也会变化，客户端一定能感知一次目录管理动作；
//   - If-None-Match 命中（支持弱比较与逗号列表）返回 304；
//   - 未带 If-None-Match 时按 If-Modified-Since 与目录 updated_at（秒级）比较。
//
// 鉴权：公开只读资源，匿名可读；带有效 token 时按 (userID, 版本) 记一次访问，失败不影响响应。
func (s *Server) handleDeviceTypeCatalog(c *gin.Context) {
	catalog, err := s.store.GetDeviceTypeCatalog(c.Request.Context())
	if err != nil {
		s.storeFailure(c, "查询设备类型目录失败", err)
		return
	}

	etag := catalogETag(catalog.Version)
	c.Header("ETag", etag)
	c.Header("Last-Modified", catalog.GeneratedAt.UTC().Format(http.TimeFormat))
	c.Header("Cache-Control", "public, max-age=60")

	// 访问记录用于后续灰度；即便命中缓存（304）也说明发生了一次读取，先记录再返回。
	if uid := userID(c); uid != "" {
		if err := s.store.RecordCatalogAccess(c.Request.Context(), uid, catalog.Version); err != nil {
			s.log.Warn("记录目录访问失败", "error", err, "user_id", uid)
		}
	}

	if inm := c.GetHeader("If-None-Match"); inm != "" {
		if etagMatches(inm, etag) {
			c.Status(http.StatusNotModified)
			return
		}
	} else if ims := c.GetHeader("If-Modified-Since"); ims != "" {
		if t, parseErr := http.ParseTime(ims); parseErr == nil && !catalog.GeneratedAt.Truncate(time.Second).After(t) {
			c.Status(http.StatusNotModified)
			return
		}
	}

	c.JSON(http.StatusOK, catalog)
}

// handleAdminUpdateCatalogDeviceType 整项更新已存在的目录项（运营侧）。
func (s *Server) handleAdminUpdateCatalogDeviceType(c *gin.Context) {
	deviceType, ok := catalogTypeParam(c)
	if !ok {
		return
	}
	var raw model.CatalogDeviceTypeInput
	if err := c.ShouldBindJSON(&raw); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	input, msg := normalizeCatalogInput(raw)
	if msg != "" {
		fail(c, http.StatusBadRequest, CodeBadRequest, msg)
		return
	}

	item, version, err := s.store.UpdateCatalogDeviceType(c.Request.Context(), deviceType, input)
	if err != nil {
		s.storeFailure(c, "更新设备类型目录失败", err)
		return
	}
	c.Header("ETag", catalogETag(version))
	c.JSON(http.StatusOK, item)
}

// handleAdminDeleteCatalogDeviceType 软删除目录项（运营侧）。
func (s *Server) handleAdminDeleteCatalogDeviceType(c *gin.Context) {
	deviceType, ok := catalogTypeParam(c)
	if !ok {
		return
	}
	version, err := s.store.DeleteCatalogDeviceType(c.Request.Context(), deviceType)
	if err != nil {
		s.storeFailure(c, "删除设备类型目录失败", err)
		return
	}
	c.Header("ETag", catalogETag(version))
	c.Status(http.StatusNoContent)
}

// catalogTypeParam 读取并规范化路径参数；非法 slug 统一返回 404，避免暴露路由细节。
func catalogTypeParam(c *gin.Context) (string, bool) {
	deviceType := strings.ToLower(strings.TrimSpace(c.Param("type")))
	if !catalogTypePattern.MatchString(deviceType) {
		fail(c, http.StatusNotFound, CodeNotFound, "资源不存在或无权访问")
		return "", false
	}
	return deviceType, true
}

// normalizeCatalogInput 校验并规范化管理端提交的目录项。
func normalizeCatalogInput(raw model.CatalogDeviceTypeInput) (model.CatalogDeviceTypeInput, string) {
	var out model.CatalogDeviceTypeInput

	out.DisplayName = strings.TrimSpace(raw.DisplayName)
	if out.DisplayName == "" {
		return out, "displayName 不能为空"
	}
	if len([]rune(out.DisplayName)) > maxCatalogDisplayName {
		return out, fmt.Sprintf("displayName 长度不能超过 %d", maxCatalogDisplayName)
	}
	out.Description = strings.TrimSpace(raw.Description)
	if len([]rune(out.Description)) > maxCatalogDescription {
		return out, fmt.Sprintf("description 长度不能超过 %d", maxCatalogDescription)
	}

	if len(raw.GATTServices) > maxCatalogServices {
		return out, fmt.Sprintf("gattServices 最多 %d 项", maxCatalogServices)
	}
	out.GATTServices = make([]model.GATTService, 0, len(raw.GATTServices))
	for i, svc := range raw.GATTServices {
		id, err := uuid.Parse(strings.TrimSpace(svc.UUID))
		if err != nil {
			return out, fmt.Sprintf("gattServices[%d].uuid 必须是 128 位 UUID", i)
		}
		name := strings.TrimSpace(svc.Name)
		if name == "" {
			return out, fmt.Sprintf("gattServices[%d].name 不能为空", i)
		}
		if len([]rune(name)) > maxCatalogServiceName {
			return out, fmt.Sprintf("gattServices[%d].name 长度不能超过 %d", i, maxCatalogServiceName)
		}
		out.GATTServices = append(out.GATTServices, model.GATTService{UUID: id.String(), Name: name})
	}

	if len(raw.Metrics) > maxCatalogMetrics {
		return out, fmt.Sprintf("metrics 最多 %d 项", maxCatalogMetrics)
	}
	out.Metrics = make([]model.CatalogMetric, 0, len(raw.Metrics))
	seenMetric := map[string]struct{}{}
	for i, m := range raw.Metrics {
		key, err := validate.NormalizeMetric(m.Key)
		if err != nil {
			return out, fmt.Sprintf("metrics[%d].key: %s", i, err)
		}
		if _, dup := seenMetric[key]; dup {
			return out, fmt.Sprintf("metrics[%d].key 重复: %s", i, key)
		}
		seenMetric[key] = struct{}{}
		unit, err := validate.NormalizeUnit(m.Unit)
		if err != nil {
			return out, fmt.Sprintf("metrics[%d].unit: %s", i, err)
		}
		id, err := uuid.Parse(strings.TrimSpace(m.CharacteristicUUID))
		if err != nil {
			return out, fmt.Sprintf("metrics[%d].characteristicUuid 必须是 128 位 UUID", i)
		}
		out.Metrics = append(out.Metrics, model.CatalogMetric{
			Key: key, Unit: unit, CharacteristicUUID: id.String(),
		})
	}

	if len(raw.Limitations) > maxCatalogLimitations {
		return out, fmt.Sprintf("limitations 最多 %d 项", maxCatalogLimitations)
	}
	out.Limitations = make([]string, 0, len(raw.Limitations))
	for i, limitation := range raw.Limitations {
		text := strings.TrimSpace(limitation)
		if text == "" {
			return out, fmt.Sprintf("limitations[%d] 不能为空", i)
		}
		if len([]rune(text)) > maxCatalogLimitationLen {
			return out, fmt.Sprintf("limitations[%d] 长度不能超过 %d", i, maxCatalogLimitationLen)
		}
		out.Limitations = append(out.Limitations, text)
	}

	compat, err := normalizeObject(raw.Compatibility)
	if err != nil {
		return out, "compatibility 必须是 JSON 对象"
	}
	out.Compatibility = compat
	return out, ""
}

// catalogETag 由目录版本号派生；强 ETag，客户端可直接用于 If-None-Match。
func catalogETag(version int64) string {
	return fmt.Sprintf(`"catalog-v%d"`, version)
}

// etagMatches 支持 `*`、逗号分隔列表与弱比较（忽略 W/ 前缀）。
func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		value := strings.TrimSpace(part)
		if value == "*" {
			return true
		}
		if strings.HasPrefix(value, "W/") {
			value = strings.TrimSpace(value[len("W/"):])
		}
		if value == etag {
			return true
		}
	}
	return false
}
