package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/auth"
	"github.com/platojobs/PJGoIoT/internal/store"
)

// pairingCodeTTL 是一次性配对码的有效期。
const pairingCodeTTL = 10 * time.Minute

type pairingCodeResponse struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type pairingClaimRequest struct {
	Code string `json:"code"`
}

type pairingClaimResponse struct {
	UserID string `json:"userId"`
	Token  string `json:"token"`
}

// handleCreatePairingCode 为当前用户签发一次性配对码；同用户新码使旧码失效。
func (s *Server) handleCreatePairingCode(c *gin.Context) {
	code, err := auth.NewPairingCode()
	if err != nil {
		s.log.Error("生成配对码失败", "error", err, "request_id", c.GetString(ctxKeyRequestID))
		fail(c, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	expiresAt := time.Now().UTC().Add(pairingCodeTTL)
	if err := s.store.CreatePairingCode(c.Request.Context(), userID(c), auth.PairingCodeHash(code), expiresAt); err != nil {
		c.Error(err)
		c.Abort()
		return
	}
	c.JSON(http.StatusCreated, pairingCodeResponse{Code: code, ExpiresAt: expiresAt})
}

// handleClaimPairingCode 用配对码换取同一账号的新 token。
// 不存在、过期、已用过、格式错误统一返回 404，避免枚举；响应不泄露账号信息。
func (s *Server) handleClaimPairingCode(c *gin.Context) {
	var req pairingClaimRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, CodeInvalidJSON, "请求体不是合法 JSON")
		return
	}
	raw := strings.TrimSpace(req.Code)
	if raw == "" || len(raw) > 16 {
		fail(c, http.StatusBadRequest, CodeBadRequest, "配对码格式错误")
		return
	}

	// 校验失败也按码计数（码错 5 次作废）；格式不合法的输入同样落库计数。
	normalized := auth.NormalizePairingCode(raw)
	if normalized == "" {
		normalized = strings.ToUpper(raw)
	}

	token, hash, err := auth.NewToken()
	if err != nil {
		s.log.Error("生成 token 失败", "error", err, "request_id", c.GetString(ctxKeyRequestID))
		fail(c, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		return
	}
	userID, err := s.store.ClaimPairingCode(c.Request.Context(), auth.PairingCodeHash(normalized), hash,
		time.Now().UTC(), pairingCodeTTL)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fail(c, http.StatusNotFound, CodeNotFound, "配对码无效或已过期")
			return
		}
		c.Error(err)
		c.Abort()
		return
	}
	c.JSON(http.StatusOK, pairingClaimResponse{UserID: userID, Token: token})
}

// claimRateLimit 对配对码领取接口做独立的按 IP 限流，防止暴力枚举。
func (s *Server) claimRateLimit() gin.HandlerFunc {
	if s.claimLimiter == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		if !s.claimLimiter.allow(c.ClientIP()) {
			c.Header("Retry-After", "60")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, errorBody{
				Error: errorDetail{Code: CodeRateLimited, Message: "配对码尝试过于频繁，请稍后再试"},
			})
			return
		}
		c.Next()
	}
}
