// Package httpapi 实现基于 Gin 的 HTTP 路由、中间件与 handler。
package httpapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/platojobs/PJGoIoT/internal/store"
)

// 错误码。客户端应以 code 做分支判断，message 仅用于展示。
const (
	CodeBadRequest          = "BAD_REQUEST"
	CodeInvalidMAC          = "INVALID_MAC"
	CodeInvalidJSON         = "INVALID_JSON"
	CodeUnauthorized        = "UNAUTHORIZED"
	CodeForbidden           = "FORBIDDEN"
	CodeNotFound            = "NOT_FOUND"
	CodeMethodNotAllowed    = "METHOD_NOT_ALLOWED"
	CodeConflict            = "CONFLICT"
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	CodeRateLimited         = "RATE_LIMITED"
	CodePayloadTooLarge     = "PAYLOAD_TOO_LARGE"
	CodeNotImplemented      = "NOT_IMPLEMENTED"
	CodeUnavailable         = "UNAVAILABLE"
	CodeInternal            = "INTERNAL"
	CodeResyncRequired      = "RESYNC_REQUIRED"
	CodeTimeout             = "TIMEOUT"
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
			writeError(c.Writer, http.StatusUnauthorized, CodeUnauthorized, "token 无效、已过期或已吊销")
		case errors.Is(last, store.ErrForbidden):
			writeError(c.Writer, http.StatusForbidden, CodeForbidden, "没有操作权限")
		case errors.Is(last, store.ErrHouseholdOwnerMustTransfer):
			writeError(c.Writer, http.StatusConflict, CodeConflict, "家庭还有其他成员，请先移除成员或转让家庭后再退出")
		case errors.Is(last, store.ErrHouseholdOwnerCannotRemoveSelf):
			writeError(c.Writer, http.StatusBadRequest, CodeBadRequest, "owner 不能移除自己")
		case errors.Is(last, store.ErrPersonalHouseholdImmutable):
			writeError(c.Writer, http.StatusBadRequest, CodeBadRequest, "个人家庭不支持退出或该操作")
		case errors.Is(last, store.ErrRoomOrderMismatch):
			writeError(c.Writer, http.StatusConflict, CodeConflict, "房间排序列表与当前房间不一致，请刷新后整段重新提交")
		case errors.Is(last, store.ErrNotFound):
			writeError(c.Writer, http.StatusNotFound, CodeNotFound, "资源不存在或无权访问")
		case errors.Is(last, store.ErrIdempotencyConflict):
			writeError(c.Writer, http.StatusConflict, CodeIdempotencyConflict, "该 Idempotency-Key 已用于不同的请求体")
		case errors.Is(last, store.ErrDuplicateDevice):
			writeError(c.Writer, http.StatusBadRequest, CodeBadRequest, "同一批设备存在重复 id 或唯一键")
		case errors.Is(last, store.ErrCursorExpired):
			writeError(c.Writer, http.StatusGone, CodeResyncRequired, "游标已过期，请全量重新同步")
		case errors.Is(last, store.ErrTimeout), errors.Is(last, context.DeadlineExceeded), isStatementTimeout(last):
			// statement_timeout（SQLSTATE 57014）与客户端超时预算都按 503 TIMEOUT 返回，
			// 客户端可安全重试；同时计入 pjgoiot_db_errors_total 供告警。
			s.metrics.incDBErrors()
			writeError(c.Writer, http.StatusServiceUnavailable, CodeTimeout, "数据库查询超时，请稍后重试")
		case isDBUnavailable(last):
			// 数据库不可达（连接被拒/重置/关闭）：503 UNAVAILABLE，明确可重试，
			// 而不是 500 INTERNAL 让客户端误判为不可恢复错误。
			s.metrics.incDBErrors()
			s.log.Error("数据库不可用", "error", last, "request_id", c.GetString(ctxKeyRequestID), "path", c.Request.URL.Path)
			writeError(c.Writer, http.StatusServiceUnavailable, CodeUnavailable, "数据库暂不可用，请稍后重试")
		default:
			s.metrics.incDBErrors()
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

// isStatementTimeout 识别 PostgreSQL statement_timeout 导致的语句取消（SQLSTATE 57014）。
// 这类错误不是 context 超时，若无显式识别会被当成 500 INTERNAL。
func isStatementTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014"
}

// isDBUnavailable 识别数据库不可达类错误，HTTP 层统一返回 503 UNAVAILABLE。
// 连接拒绝/重置/关闭在数据库停机与重启窗口内都很常见，客户端应重试而不是放弃。
func isDBUnavailable(err error) bool {
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// 08 类：连接异常；57P01/57P02/57P03：服务端关闭/崩溃/暂不可连；53300：连接数耗尽。
		if strings.HasPrefix(pgErr.Code, "08") || pgErr.Code == "57P01" ||
			pgErr.Code == "57P02" || pgErr.Code == "57P03" || pgErr.Code == "53300" {
			return true
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection refused", "connection reset", "broken pipe", "no such host",
		"server closed the connection", "database is closed", "failed to connect",
		"network is unreachable", "connection closed", "unexpected eof",
		"failed to receive message", "terminating connection", "bad connection",
		"connection is already closed",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
