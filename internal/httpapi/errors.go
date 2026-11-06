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
	// CodeRoomSetMismatch 用于整段房间重排时集合不一致，客户端可直接据此代码触发「重新拉取再提交」。
	CodeRoomSetMismatch = "ROOM_SET_MISMATCH"
	// CodeFeedbackRateLimited 表示反馈提交超过账号或 IP 维度的额度；
	// 与全局 RATE_LIMITED 区分，便于客户端提示「反馈提交过于频繁」而不是泛化重试。
	CodeFeedbackRateLimited = "FEEDBACK_RATE_LIMITED"
	// CodeFeedbackInvalidTransition 表示反馈状态机不允许该跳转（含对已关闭反馈的任何推进）。
	// 与 CONFLICT 区分，客户端可直接提示「该反馈已关闭」而不是泛化冲突重试。
	CodeFeedbackInvalidTransition = "FEEDBACK_INVALID_TRANSITION"
	// CodeDeletionTokenRequired 表示 DELETE /me 缺少二次确认令牌。HTTP 400。
	CodeDeletionTokenRequired = "DELETION_TOKEN_REQUIRED"
	// CodeDeletionTokenInvalid 表示删除令牌不存在或与任何账号不匹配。HTTP 403。
	CodeDeletionTokenInvalid = "DELETION_TOKEN_INVALID"
	// CodeDeletionTokenExpired 表示删除令牌已超过 10 分钟 TTL。HTTP 410。
	CodeDeletionTokenExpired = "DELETION_TOKEN_EXPIRED"
	// CodeAccountDeletionBlocked 表示 owner 的家庭还有其他成员，必须先转让或移除成员。HTTP 409。
	CodeAccountDeletionBlocked = "ACCOUNT_DELETION_BLOCKED"
	// CodeExportNotReady 表示导出任务尚未完成，暂不能下载。HTTP 409。
	CodeExportNotReady = "EXPORT_NOT_READY"
	// CodeExportDownloadInvalid 表示下载令牌不匹配任何可下载产物。HTTP 403。
	CodeExportDownloadInvalid = "EXPORT_DOWNLOAD_INVALID"
	// CodeExportDownloadExpired 表示下载链接已过期或已被使用。HTTP 410。
	CodeExportDownloadExpired = "EXPORT_DOWNLOAD_EXPIRED"
	// CodeSceneNameInvalid 表示场景名称为空或超过 60 个字符。HTTP 400。
	CodeSceneNameInvalid = "SCENE_NAME_INVALID"
	// CodeSceneCooldownOutOfRange 表示冷却时间不在 30..86400 秒范围内。HTTP 400。
	CodeSceneCooldownOutOfRange = "SCENE_COOLDOWN_OUT_OF_RANGE"
	// CodeSceneTriggerInvalid 表示触发条件种类/字段组合/取值范围非法。HTTP 400。
	CodeSceneTriggerInvalid = "SCENE_TRIGGER_INVALID"
	// CodeSceneActionInvalid 表示动作种类/字段组合/数量/取值范围非法。HTTP 400。
	CodeSceneActionInvalid = "SCENE_ACTION_INVALID"
	// CodeSceneDeviceNotFound 表示触发条件引用的设备不存在、已删除或不属于本人。HTTP 400。
	CodeSceneDeviceNotFound = "SCENE_DEVICE_NOT_FOUND"
	// CodeSceneMetricNotSupported 表示指标越界绑定的设备类型不存在或该类型不支持该指标。HTTP 400。
	CodeSceneMetricNotSupported = "SCENE_METRIC_NOT_SUPPORTED"
	// CodePushDeviceInvalid 表示推送设备注册参数不合法（platform/token）。HTTP 400。
	CodePushDeviceInvalid = "PUSH_DEVICE_INVALID"
	// CodeAlertInvalid 表示运营侧告警内容不合法。HTTP 400。
	CodeAlertInvalid = "ALERT_INVALID"
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
			writeError(c.Writer, http.StatusConflict, CodeConflict, "家庭还有其他成员，请先转让所有权或移除其他成员后再退出")
		case errors.Is(last, store.ErrHouseholdOwnerCannotRemoveSelf):
			writeError(c.Writer, http.StatusBadRequest, CodeBadRequest, "owner 不能移除自己")
		case errors.Is(last, store.ErrPersonalHouseholdImmutable):
			writeError(c.Writer, http.StatusBadRequest, CodeBadRequest, "个人家庭不支持退出或该操作")
		case errors.Is(last, store.ErrHouseholdTargetNotMember):
			writeError(c.Writer, http.StatusNotFound, CodeNotFound, "目标用户不是该家庭成员")
		case errors.Is(last, store.ErrHouseholdTargetAlreadyOwner):
			writeError(c.Writer, http.StatusBadRequest, CodeBadRequest, "目标用户已经是该家庭 owner")
		case errors.Is(last, store.ErrRoomOrderMismatch):
			writeError(c.Writer, http.StatusConflict, CodeRoomSetMismatch, "房间排序列表与当前房间不一致，请刷新后整段重新提交")
		case errors.Is(last, store.ErrNotFound):
			writeError(c.Writer, http.StatusNotFound, CodeNotFound, "资源不存在或无权访问")
		case errors.Is(last, store.ErrFeedbackInvalidTransition):
			writeError(c.Writer, http.StatusConflict, CodeFeedbackInvalidTransition, "反馈状态不允许该变更（已关闭的反馈不能再次修改）")
		case errors.Is(last, store.ErrDeletionTokenInvalid):
			writeError(c.Writer, http.StatusForbidden, CodeDeletionTokenInvalid, "删除令牌无效，请重新申请")
		case errors.Is(last, store.ErrDeletionTokenExpired):
			writeError(c.Writer, http.StatusGone, CodeDeletionTokenExpired, "删除令牌已过期，请重新申请")
		case errors.Is(last, store.ErrAccountDeletionBlocked):
			writeError(c.Writer, http.StatusConflict, CodeAccountDeletionBlocked, store.ErrAccountDeletionBlocked.Error())
		case errors.Is(last, store.ErrExportTaskNotReady):
			writeError(c.Writer, http.StatusConflict, CodeExportNotReady, "导出任务尚未完成，请稍后重试")
		case errors.Is(last, store.ErrExportDownloadInvalid):
			writeError(c.Writer, http.StatusForbidden, CodeExportDownloadInvalid, "下载令牌无效或导出任务不可下载")
		case errors.Is(last, store.ErrExportDownloadExpired):
			writeError(c.Writer, http.StatusGone, CodeExportDownloadExpired, "下载链接已过期或已被使用，请重新查询导出任务")
		case errors.Is(last, store.ErrSceneInvalidName):
			writeError(c.Writer, http.StatusBadRequest, CodeSceneNameInvalid, last.Error())
		case errors.Is(last, store.ErrSceneCooldownOutOfRange):
			writeError(c.Writer, http.StatusBadRequest, CodeSceneCooldownOutOfRange, last.Error())
		case errors.Is(last, store.ErrSceneInvalidTrigger):
			writeError(c.Writer, http.StatusBadRequest, CodeSceneTriggerInvalid, last.Error())
		case errors.Is(last, store.ErrSceneInvalidAction):
			writeError(c.Writer, http.StatusBadRequest, CodeSceneActionInvalid, last.Error())
		case errors.Is(last, store.ErrSceneDeviceNotFound):
			writeError(c.Writer, http.StatusBadRequest, CodeSceneDeviceNotFound, last.Error())
		case errors.Is(last, store.ErrSceneMetricNotSupported):
			writeError(c.Writer, http.StatusBadRequest, CodeSceneMetricNotSupported, last.Error())
		case errors.Is(last, store.ErrPushDeviceInvalid):
			writeError(c.Writer, http.StatusBadRequest, CodePushDeviceInvalid, last.Error())
		case errors.Is(last, store.ErrBadCursor):
			writeError(c.Writer, http.StatusBadRequest, CodeBadRequest, "游标不合法")
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
