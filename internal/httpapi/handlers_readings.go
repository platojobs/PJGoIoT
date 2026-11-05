package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
	"github.com/platojobs/PJGoIoT/internal/validate"
)

// handleReadingsList 处理 GET /my/devices/{platform}/{externalId}/readings。
func (s *Server) handleReadingsList(c *gin.Context, rawPlatform, rawExternalID string) {
	platform, externalID, ok := s.resolveReadingsDeviceKey(c, rawPlatform, rawExternalID)
	if !ok {
		return
	}

	var query readingsQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		failValidation(c, err)
		return
	}

	params := model.ReadingsQuery{Limit: defaultReadingsLimit, Cursor: strings.TrimSpace(query.Cursor)}
	if query.Limit != nil {
		params.Limit = *query.Limit
	}
	if raw := strings.TrimSpace(query.Metric); raw != "" {
		metric, err := validate.NormalizeMetric(raw)
		if err != nil {
			fail(c, http.StatusBadRequest, CodeBadRequest, "metric 不合法: "+err.Error())
			return
		}
		params.Metric = metric
	}
	if raw := strings.TrimSpace(query.From); raw != "" {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			fail(c, http.StatusBadRequest, CodeBadRequest, "from 必须是 RFC3339 时间")
			return
		}
		utc := at.UTC()
		params.From = &utc
	}
	if raw := strings.TrimSpace(query.To); raw != "" {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			fail(c, http.StatusBadRequest, CodeBadRequest, "to 必须是 RFC3339 时间")
			return
		}
		utc := at.UTC()
		params.To = &utc
	}

	page, err := s.store.ListReadings(c.Request.Context(), userID(c), platform, externalID, params)
	if err != nil {
		if errors.Is(err, store.ErrBadCursor) {
			fail(c, http.StatusBadRequest, CodeBadRequest, "cursor 不合法")
			return
		}
		s.storeFailure(c, "查询读数失败", err)
		return
	}
	if page.Items == nil {
		page.Items = []model.Reading{}
	}
	c.JSON(http.StatusOK, page)
}

// handleReadingsSummary 处理 GET /my/devices/{platform}/{externalId}/readings/summary。
// metric 必填；hours 默认 24，范围 1-2160（90 天，与服务端保留期一致）。
func (s *Server) handleReadingsSummary(c *gin.Context, rawPlatform, rawExternalID string) {
	platform, externalID, ok := s.resolveReadingsDeviceKey(c, rawPlatform, rawExternalID)
	if !ok {
		return
	}

	var query readingsSummaryQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		failValidation(c, err)
		return
	}
	if strings.TrimSpace(query.Metric) == "" {
		fail(c, http.StatusBadRequest, CodeBadRequest, "metric 不能为空")
		return
	}
	metric, err := validate.NormalizeMetric(query.Metric)
	if err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "metric 不合法: "+err.Error())
		return
	}

	hours := defaultSummaryHours
	if query.Hours != nil {
		hours = *query.Hours
	}
	windowEnd := time.Now().UTC()
	windowStart := windowEnd.Add(-time.Duration(hours) * time.Hour)

	summary, err := s.store.ReadingsSummary(c.Request.Context(), userID(c), platform, externalID, metric, windowStart, windowEnd)
	if err != nil {
		s.storeFailure(c, "聚合读数失败", err)
		return
	}
	c.JSON(http.StatusOK, summary)
}

// handleReadingsSeries 处理 GET /my/devices/{platform}/{externalId}/readings/series。
// 时间桶按 UTC 网格对齐（1h 整点、1d UTC 零点）；桶数上限 500、跨度上限 90 天。
func (s *Server) handleReadingsSeries(c *gin.Context, rawPlatform, rawExternalID string) {
	platform, externalID, ok := s.resolveReadingsDeviceKey(c, rawPlatform, rawExternalID)
	if !ok {
		return
	}

	var query readingsSeriesQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		failValidation(c, err)
		return
	}
	if strings.TrimSpace(query.Metric) == "" {
		fail(c, http.StatusBadRequest, CodeBadRequest, "metric 不能为空")
		return
	}
	metric, err := validate.NormalizeMetric(query.Metric)
	if err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "metric 不合法: "+err.Error())
		return
	}
	size, err := validate.NormalizeBucket(query.Bucket)
	if err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "bucket 不合法: "+err.Error())
		return
	}
	if strings.TrimSpace(query.From) == "" || strings.TrimSpace(query.To) == "" {
		fail(c, http.StatusBadRequest, CodeBadRequest, "from 与 to 必填（RFC3339）")
		return
	}
	from, err := time.Parse(time.RFC3339, query.From)
	if err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "from 必须是 RFC3339 时间")
		return
	}
	to, err := time.Parse(time.RFC3339, query.To)
	if err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, "to 必须是 RFC3339 时间")
		return
	}
	if _, err := validate.ValidateSeriesRange(from, to, size); err != nil {
		fail(c, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}

	series, err := s.store.ReadingsSeries(c.Request.Context(), userID(c), platform, externalID, metric, from, to, query.Bucket)
	if err != nil {
		s.storeFailure(c, "查询读数序列失败", err)
		return
	}
	if series.Buckets == nil {
		series.Buckets = []model.ReadingsBucket{}
	}
	c.JSON(http.StatusOK, series)
}

// handleReadingsMetrics 处理 GET /my/devices/{platform}/{externalId}/readings/metrics。
func (s *Server) handleReadingsMetrics(c *gin.Context, rawPlatform, rawExternalID string) {
	platform, externalID, ok := s.resolveReadingsDeviceKey(c, rawPlatform, rawExternalID)
	if !ok {
		return
	}

	result, err := s.store.ReadingsMetrics(c.Request.Context(), userID(c), platform, externalID)
	if err != nil {
		s.storeFailure(c, "查询指标清单失败", err)
		return
	}
	if result.Items == nil {
		result.Items = []model.ReadingMetric{}
	}
	c.JSON(http.StatusOK, result)
}

// resolveReadingsDeviceKey 校验路径中的设备键；非法时写 400 并返回 false。
func (s *Server) resolveReadingsDeviceKey(c *gin.Context, rawPlatform, rawExternalID string) (string, string, bool) {
	platform, err := validate.NormalizePlatform(rawPlatform)
	if err != nil {
		// 与单台设备查询保持一致：非法 platform 返回 400。
		fail(c, http.StatusBadRequest, CodeBadRequest, "platform 只能是 android 或 ios")
		return "", "", false
	}
	_, externalID, err := validate.NormalizeExternalID(platform, rawExternalID)
	if err != nil {
		if platform == validate.PlatformAndroid {
			fail(c, http.StatusBadRequest, CodeInvalidMAC, "Android externalId 必须是 6 字节 MAC")
			return "", "", false
		}
		fail(c, http.StatusBadRequest, CodeBadRequest, "iOS externalId 必须是 UUID")
		return "", "", false
	}
	return platform, externalID, true
}
