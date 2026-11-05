package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

const (
	maxFeedbackBodyBytes        = 256 << 10 // 256 KiB
	maxFeedbackBodyRunes        = 4000
	maxFeedbackContactRunes     = 200
	maxFeedbackDiagnosticsRunes = 8000
	maxFeedbackAppVersionRunes  = 64
	maxFeedbackBuildTypeRunes   = 32
	maxFeedbackOSVersionRunes   = 64
	maxFeedbackDeviceModelRunes = 128

	defaultFeedbackPageSize = 20
	maxFeedbackPageSize     = 100
)

// feedbackKinds 与 openapi 的枚举保持一致；未知类型直接 400，避免脏数据入库。
var feedbackKinds = map[string]struct{}{
	"bug":        {},
	"feature":    {},
	"experience": {},
	"other":      {},
}

type feedbackClientContext struct {
	Platform    string `json:"platform"`
	AppVersion  string `json:"appVersion"`
	BuildType   string `json:"buildType"`
	OSVersion   string `json:"osVersion"`
	DeviceModel string `json:"deviceModel"`
}

type feedbackCreateRequest struct {
	Kind        string                `json:"kind"`
	Body        string                `json:"body"`
	Contact     *string               `json:"contact"`
	Context     feedbackClientContext `json:"context"`
	Diagnostics *string               `json:"diagnostics"`
}

// feedbackCreateResponse 只返回最小必要字段；完整内容通过 GET /feedback/mine 查询。
type feedbackCreateResponse struct {
	FeedbackID string    `json:"feedbackId"`
	CreatedAt  time.Time `json:"createdAt"`
}

// feedbackRateLimit 对 POST /feedback 同时检查账号与 IP 两个维度。
// 两个额度独立配置，任一超限即返回 429 FEEDBACK_RATE_LIMITED，不静默丢弃请求。
func (s *Server) feedbackRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.feedbackUserLimiter != nil && !s.feedbackUserLimiter.allow(c.Request.Context(), "feedback:user:"+userID(c)) {
			feedbackLimited(c)
			return
		}
		if s.feedbackIPLimiter != nil && !s.feedbackIPLimiter.allow(c.Request.Context(), "feedback:ip:"+c.ClientIP()) {
			feedbackLimited(c)
			return
		}
		c.Next()
	}
}

func feedbackLimited(c *gin.Context) {
	c.Header("Retry-After", "60")
	c.AbortWithStatusJSON(http.StatusTooManyRequests, errorBody{
		Error: errorDetail{Code: CodeFeedbackRateLimited, Message: "反馈提交过于频繁，请稍后再试"},
	})
}

// handleCreateFeedback 提交反馈；同一请求体重复提交由幂等键去重并返回同一 feedbackId。
func (s *Server) handleCreateFeedback(c *gin.Context) {
	idemKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(idemKey) > maxIdempotencyKey {
		fail(c, http.StatusBadRequest, CodeBadRequest, "Idempotency-Key 长度不能超过 128")
		return
	}

	raw, err := c.GetRawData()
	if err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "读取请求体失败")
		return
	}
	if len(raw) > maxFeedbackBodyBytes {
		fail(c, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "请求体过大")
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	var req feedbackCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	sub, msg := normalizeFeedback(req)
	if msg != "" {
		fail(c, http.StatusBadRequest, CodeBadRequest, msg)
		return
	}

	// 请求哈希基于规范化后的内容，空白差异不会导致幂等失效。
	hashBytes, err := json.Marshal(sub)
	if err != nil {
		fail(c, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	sum := sha256.Sum256(hashBytes)
	requestHash := hex.EncodeToString(sum[:])

	// 未提供 header 时按请求体哈希派生幂等键：完全相同的重复提交无需客户端配合即可去重。
	if idemKey == "" {
		idemKey = "body:" + requestHash
	} else {
		idemKey = "key:" + idemKey
	}

	item, replayed, err := s.store.CreateFeedback(c.Request.Context(), userID(c), sub, store.FeedbackOptions{
		IdempotencyKey: idemKey,
		RequestHash:    requestHash,
	})
	if err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	if replayed {
		c.Header("Idempotent-Replayed", "true")
		c.JSON(http.StatusOK, feedbackCreateResponse{FeedbackID: item.FeedbackID, CreatedAt: item.CreatedAt})
		return
	}
	c.JSON(http.StatusCreated, feedbackCreateResponse{FeedbackID: item.FeedbackID, CreatedAt: item.CreatedAt})
}

// handleListMyFeedback 返回当前账号提交过的反馈与状态，只包含本人数据。
func (s *Server) handleListMyFeedback(c *gin.Context) {
	limit := defaultFeedbackPageSize
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxFeedbackPageSize {
			fail(c, http.StatusBadRequest, CodeBadRequest, "limit 取值范围 1-100")
			return
		}
		limit = n
	}

	page, err := s.store.ListUserFeedback(c.Request.Context(), userID(c), c.Query("cursor"), limit)
	if err != nil {
		if errors.Is(err, store.ErrBadCursor) {
			fail(c, http.StatusBadRequest, CodeBadRequest, "cursor 无效")
			return
		}
		s.storeFailure(c, "查询反馈列表失败", err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// normalizeFeedback 校验并规范化请求；返回非空字符串表示校验失败原因。
// contact / diagnostics 为空串时视为未提供，避免把空串当作个人信息存储。
func normalizeFeedback(req feedbackCreateRequest) (model.FeedbackSubmission, string) {
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if _, ok := feedbackKinds[kind]; !ok {
		return model.FeedbackSubmission{}, "kind 只能是 bug、feature、experience 或 other"
	}

	body := strings.TrimSpace(req.Body)
	n := utf8.RuneCountInString(body)
	if n == 0 {
		return model.FeedbackSubmission{}, "body 不能为空白"
	}
	if n > maxFeedbackBodyRunes {
		return model.FeedbackSubmission{}, "body 长度不能超过 4000 个字符"
	}

	ctx := req.Context
	platform := strings.ToLower(strings.TrimSpace(ctx.Platform))
	if platform != "android" && platform != "ios" {
		return model.FeedbackSubmission{}, "context.platform 只能是 android 或 ios"
	}
	appVersion := strings.TrimSpace(ctx.AppVersion)
	if msg := requireLimited("context.appVersion", appVersion, maxFeedbackAppVersionRunes); msg != "" {
		return model.FeedbackSubmission{}, msg
	}
	buildType := strings.TrimSpace(ctx.BuildType)
	if msg := requireLimited("context.buildType", buildType, maxFeedbackBuildTypeRunes); msg != "" {
		return model.FeedbackSubmission{}, msg
	}
	osVersion := strings.TrimSpace(ctx.OSVersion)
	if msg := requireLimited("context.osVersion", osVersion, maxFeedbackOSVersionRunes); msg != "" {
		return model.FeedbackSubmission{}, msg
	}
	deviceModel := strings.TrimSpace(ctx.DeviceModel)
	if msg := requireLimited("context.deviceModel", deviceModel, maxFeedbackDeviceModelRunes); msg != "" {
		return model.FeedbackSubmission{}, msg
	}

	var contact *string
	if req.Contact != nil {
		v := strings.TrimSpace(*req.Contact)
		if utf8.RuneCountInString(v) > maxFeedbackContactRunes {
			return model.FeedbackSubmission{}, "contact 长度不能超过 200 个字符"
		}
		if v != "" {
			contact = &v
		}
	}
	var diagnostics *string
	if req.Diagnostics != nil {
		v := strings.TrimSpace(*req.Diagnostics)
		if utf8.RuneCountInString(v) > maxFeedbackDiagnosticsRunes {
			return model.FeedbackSubmission{}, "diagnostics 长度不能超过 8000 个字符"
		}
		if v != "" {
			diagnostics = &v
		}
	}

	return model.FeedbackSubmission{
		Kind:        kind,
		Body:        body,
		Contact:     contact,
		Platform:    platform,
		AppVersion:  appVersion,
		BuildType:   buildType,
		OSVersion:   osVersion,
		DeviceModel: deviceModel,
		Diagnostics: diagnostics,
	}, ""
}

// requireLimited 校验必填字段：不能为空且不超过 max 个字符。
func requireLimited(field, value string, max int) string {
	if value == "" {
		return field + " 不能为空"
	}
	if utf8.RuneCountInString(value) > max {
		return field + " 长度不能超过 " + strconv.Itoa(max) + " 个字符"
	}
	return ""
}
