package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const defaultFCMEndpoint = "https://fcm.googleapis.com/fcm/send"

// FCMAdapter 使用 FCM legacy HTTP 接口（Authorization: key=<server key>）。
// 未配置 FCM_SERVER_KEY 时进入 dry-run，不发起网络请求。
type FCMAdapter struct {
	serverKey string
	endpoint  string
	client    *http.Client
	log       *slog.Logger
}

// NewFCMAdapter 构造 FCM 适配器；endpoint 为空时使用官方地址（测试可用 FCM_ENDPOINT 覆盖）。
func NewFCMAdapter(serverKey, endpoint string, logger *slog.Logger) *FCMAdapter {
	if logger == nil {
		logger = slog.Default()
	}
	if endpoint == "" {
		endpoint = defaultFCMEndpoint
	}
	return &FCMAdapter{
		serverKey: strings.TrimSpace(serverKey),
		endpoint:  endpoint,
		client:    &http.Client{Timeout: 10 * time.Second},
		log:       logger,
	}
}

func (a *FCMAdapter) Provider() string { return "fcm" }

type fcmNotification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type fcmPayload struct {
	To           string            `json:"to"`
	Notification fcmNotification   `json:"notification"`
	Data         map[string]string `json:"data,omitempty"`
	Priority     string            `json:"priority"`
}

// Send 实现 Adapter。
func (a *FCMAdapter) Send(ctx context.Context, msg Message) Result {
	payload := fcmPayload{
		To:           msg.Token,
		Notification: fcmNotification{Title: msg.Title, Body: msg.Body},
		Data:         msg.Data,
		Priority:     "high",
	}
	if a.serverKey == "" {
		return dryRunResult(a.log, "fcm-dry-run", msg, payload)
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return Result{Provider: "fcm", Terminal: true, Err: fmt.Errorf("序列化 FCM 载荷失败: %w", err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(raw))
	if err != nil {
		return Result{Provider: "fcm", Terminal: true, Err: fmt.Errorf("构造 FCM 请求失败: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "key="+a.serverKey)

	resp, err := a.client.Do(req)
	if err != nil {
		// 超时/连接错误按可重试处理，由 worker 指数退避。
		return Result{Provider: "fcm", Retryable: true, Err: fmt.Errorf("FCM 请求失败: %w", err)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode == http.StatusOK {
		var parsed struct {
			Success int `json:"success"`
			Failure int `json:"failure"`
			Results []struct {
				Error string `json:"error"`
			} `json:"results"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil && parsed.Failure > 0 && len(parsed.Results) > 0 {
			return classifyFCM("fcm", resp.StatusCode, parsed.Results[0].Error, nil)
		}
		return Result{Provider: "fcm", StatusCode: resp.StatusCode}
	}
	providerErr := fcmErrorFromBody(body)
	return classifyFCM("fcm", resp.StatusCode, providerErr, nil)
}

// fcmErrorFromBody 从 FCM 错误响应中提取错误串；非 JSON 时按纯文本处理。
func fcmErrorFromBody(body []byte) string {
	var parsed struct {
		Error   string `json:"error"`
		Results []struct {
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		if parsed.Error != "" {
			return parsed.Error
		}
		if len(parsed.Results) > 0 {
			return parsed.Results[0].Error
		}
	}
	return strings.TrimSpace(string(body))
}

// classifyFCM 把 HTTP 状态码与 provider 错误串映射到统一的 Result 语义。
func classifyFCM(provider string, status int, providerErr string, err error) Result {
	res := Result{Provider: provider, StatusCode: status, Err: err}
	switch {
	case status >= 500 || status == http.StatusTooManyRequests:
		res.Retryable = true
		if res.Err == nil {
			res.Err = fmt.Errorf("FCM 返回 %d: %s", status, providerErr)
		}
		return res
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		res.Terminal = true
		res.Err = fmt.Errorf("FCM 凭据或权限错误（%d）: %s", status, providerErr)
		return res
	}

	switch providerErr {
	case "NotRegistered", "InvalidRegistration":
		// 令牌已失效：终态 + 立即停用。
		res.Terminal = true
		res.DeactivateToken = true
		res.Err = fmt.Errorf("FCM 令牌失效: %s", providerErr)
	case "Unavailable", "InternalServerError", "DeviceMessageRateExceeded", "TopicsMessageRateExceeded":
		res.Retryable = true
		res.Err = fmt.Errorf("FCM 暂时不可用: %s", providerErr)
	default:
		if providerErr != "" {
			res.Terminal = true
			res.Err = fmt.Errorf("FCM 终态错误（%d）: %s", status, providerErr)
		}
	}
	return res
}
