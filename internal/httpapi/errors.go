// Package httpapi 实现基于 Gin 的 HTTP 路由、中间件与 handler。
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/platojobs/PJGoIoT/internal/store"
)

// 错误码。客户端应以 code 做分支判断，message 仅用于展示。
const (
	CodeBadRequest          = "BAD_REQUEST"
	CodeInvalidMAC          = "INVALID_MAC"
	CodeInvalidJSON         = "INVALID_JSON"
	CodeUnauthorized        = "UNAUTHORIZED"
	CodeNotFound            = "NOT_FOUND"
	CodeMethodNotAllowed    = "METHOD_NOT_ALLOWED"
	CodeConflict            = "CONFLICT"
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	CodeRateLimited         = "RATE_LIMITED"
	CodePayloadTooLarge     = "PAYLOAD_TOO_LARGE"
	CodeNotImplemented      = "NOT_IMPLEMENTED"
	CodeUnavailable         = "UNAVAILABLE"
	CodeInternal            = "INTERNAL"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// apiError 是 handler/中间件主动返回的业务错误。
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// validationErr 包装 Gin/validator 的参数校验失败，由统一错误中间件映射为 400。
type validationErr struct {
	Err error
}

func (e *validationErr) Error() string {
	return "参数校验失败: " + e.Err.Error()
}

func (e *validationErr) Unwrap() error { return e.Err }

// fail 记录错误并终止后续 handler，由 errorHandler 统一写出响应。
func fail(c *gin.Context, status int, code, message string) {
	c.Error(&apiError{Status: status, Code: code, Message: message})
	c.Abort()
}

func failValidation(c *gin.Context, err error) {
	c.Error(&validationErr{Err: err})
	c.Abort()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Error("写入 JSON 响应失败", "error", err)
	}
}

// writeError 输出固定错误体 {"error":{"code","message"}}。
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

// errorHandler 是统一错误出口：
//   - 参数校验失败 -> 400
//   - 未认证 -> 401
//   - 资源不存在/越权 -> 404
//   - 幂等键冲突 -> 409
//   - 其它 -> 500
func (s *Server) errorHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if len(c.Errors) == 0 || c.Writer.Written() {
			return
		}

		last := c.Errors.Last().Err
		var apiErr *apiError
		var valErr *validationErr

		switch {
		case errors.As(last, &apiErr):
			writeError(c.Writer, apiErr.Status, apiErr.Code, apiErr.Message)
		case errors.As(last, &valErr):
			writeError(c.Writer, http.StatusBadRequest, CodeBadRequest, "请求参数校验失败")
		case errors.Is(last, store.ErrUnauthorized):
			writeError(c.Writer, http.StatusUnauthorized, CodeUnauthorized, "token 无效或已吊销")
		case errors.Is(last, store.ErrNotFound):
			writeError(c.Writer, http.StatusNotFound, CodeNotFound, "资源不存在或无权访问")
		case errors.Is(last, store.ErrIdempotencyConflict):
			writeError(c.Writer, http.StatusConflict, CodeIdempotencyConflict, "该 Idempotency-Key 已用于不同的请求体")
		default:
			s.log.Error("请求处理失败",
				"error", last,
				"request_id", c.GetString(ctxKeyRequestID),
				"path", c.Request.URL.Path,
			)
			writeError(c.Writer, http.StatusInternalServerError, CodeInternal, "服务内部错误")
		}
		c.Abort()
	}
}
