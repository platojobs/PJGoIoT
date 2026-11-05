package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/platojobs/PJGoIoT/internal/auth"
	"github.com/platojobs/PJGoIoT/internal/store"
)

const (
	ctxKeyRequestID = "request_id"
	ctxKeyUserID    = "user_id"
	ctxKeyTokenHash = "token_hash"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// globalMiddleware 返回 Gin 全局中间件。
// 顺序：request id -> panic 恢复 -> 访问日志 -> CORS -> 限流 -> 统一错误出口。
func (s *Server) globalMiddleware() []gin.HandlerFunc {
	return []gin.HandlerFunc{
		s.requestID(),
		s.recoverPanic(),
		s.accessLog(),
		s.cors(),
		s.rateLimit(),
		s.errorHandler(),
	}
}

func (s *Server) requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-Id")
		if !requestIDPattern.MatchString(id) {
			id = newRequestID()
		}
		c.Header("X-Request-Id", id)
		c.Set(ctxKeyRequestID, id)
		c.Next()
	}
}

func (s *Server) recoverPanic() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("handler panic",
					"panic", rec,
					"stack", string(debug.Stack()),
					"request_id", c.GetString(ctxKeyRequestID),
				)
				if !c.Writer.Written() {
					writeError(c.Writer, http.StatusInternalServerError, CodeInternal, "服务内部错误")
				}
				c.Abort()
			}
		}()
		c.Next()
	}
}

func (s *Server) accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		s.metrics.observeRequest(c.Request.Method, routeLabel(c), status, time.Since(start))

		attrs := []any{
			"request_id", c.GetString(ctxKeyRequestID),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", c.ClientIP(),
		}
		if uid := c.GetString(ctxKeyUserID); uid != "" {
			attrs = append(attrs, "user_id", uid)
		}
		if status >= 500 {
			s.log.Error("http request", attrs...)
			return
		}
		if c.Request.URL.Path == "/api/v1/health" || c.Request.URL.Path == "/healthz" {
			s.log.Debug("http request", attrs...)
			return
		}
		s.log.Info("http request", attrs...)
	}
}

func (s *Server) cors() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			if allowed, value := s.allowedOrigin(origin); allowed {
				c.Header("Access-Control-Allow-Origin", value)
				c.Header("Vary", "Origin")
				c.Header("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
				c.Header("Access-Control-Expose-Headers", "X-Request-Id, Idempotent-Replayed, Content-Disposition")
				c.Header("Access-Control-Max-Age", "600")
			}
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func (s *Server) allowedOrigin(origin string) (bool, string) {
	for _, allowed := range s.cfg.CORSAllowedOrigins {
		if allowed == "*" {
			return true, "*"
		}
		if strings.EqualFold(allowed, origin) {
			return true, origin
		}
	}
	return false, ""
}

func (s *Server) rateLimit() gin.HandlerFunc {
	if s.limiter == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		// 健康探针与指标端点不应被业务限流误伤。
		if c.Request.URL.Path == "/api/v1/health" || c.Request.URL.Path == "/healthz" || c.Request.URL.Path == "/metrics" {
			c.Next()
			return
		}
		if !s.limiter.allow(c.ClientIP()) {
			c.Header("Retry-After", "60")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, errorBody{
				Error: errorDetail{Code: CodeRateLimited, Message: "请求过于频繁，请稍后再试"},
			})
			return
		}
		c.Next()
	}
}

// requireAuth 解析 Bearer token 并从数据库哈希中解析 userId。
func (s *Server) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		const prefix = "Bearer "
		if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
			fail(c, http.StatusUnauthorized, CodeUnauthorized, "缺少 Bearer token")
			return
		}
		token := strings.TrimSpace(header[len(prefix):])
		if token == "" {
			fail(c, http.StatusUnauthorized, CodeUnauthorized, "缺少 Bearer token")
			return
		}

		userID, err := s.store.UserIDByTokenHash(c.Request.Context(), auth.Hash(token))
		if err != nil {
			if errors.Is(err, store.ErrUnauthorized) {
				fail(c, http.StatusUnauthorized, CodeUnauthorized, "token 无效、已过期或已吊销")
				return
			}
			// 数据库不可达/超时不能被压成 500：鉴权查询失败时同样应返回
			// 503 UNAVAILABLE/TIMEOUT，客户端才知道可以重试。
			c.Error(err)
			c.Abort()
			return
		}

		c.Set(ctxKeyUserID, userID)
		c.Set(ctxKeyTokenHash, auth.Hash(token))
		c.Next()
	}
}

// routeLabel 返回低基数的路由模板，作为 Prometheus 的 route 标签。
// Gin 对 /my/devices/*key 只会给出通配模板，因此这里按真实路径还原读数子资源，
// 否则 devices 与 readings/series 会混在同一个 route 下，无法支撑分接口 SLO。
func routeLabel(c *gin.Context) string {
	full := c.FullPath()
	if full == "" {
		return "unmatched"
	}
	if !strings.Contains(full, "*") {
		return full
	}
	const prefix = "/api/v1/my/devices/"
	path := c.Request.URL.Path
	if !strings.HasPrefix(path, prefix) {
		return full
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	switch {
	case len(parts) == 1 && parts[0] != "":
		return "/api/v1/my/devices/:key"
	case len(parts) == 2 && parts[0] != "" && parts[1] != "":
		return "/api/v1/my/devices/:platform/:externalId"
	case len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] == "readings":
		return "/api/v1/my/devices/:platform/:externalId/readings"
	case len(parts) == 4 && parts[0] != "" && parts[1] != "" && parts[2] == "readings":
		return "/api/v1/my/devices/:platform/:externalId/readings/" + parts[3]
	default:
		return full
	}
}

func userID(c *gin.Context) string {
	return c.GetString(ctxKeyUserID)
}

func tokenHash(c *gin.Context) string {
	return c.GetString(ctxKeyTokenHash)
}

func newRequestID() string {
	return uuid.NewString()
}
