// Package push 提供服务端推送适配层：FCM（Android）与 APNs（iOS）各一份实现。
//
// 没有凭据时适配器进入 dry-run 模式：不发起网络请求，但把「本应发送」的完整载荷
// 以 JSON 形式写入日志，并由 worker 把投递记录标记为 dry_run=true / sent。
// 这样既不会静默吞掉告警，也不会因为缺少凭据让队列卡死。
package push

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
)

// Message 是进入适配层的一条推送（字段来自投递队列快照）。
type Message struct {
	UserID   string
	AlertID  string
	DeviceID string
	Platform string
	Token    string
	Title    string
	Body     string
	Data     map[string]string
}

// Result 是适配层的发送结果。
// 约定：
//   - DryRun=true 表示没有凭据、未实际发送，但载荷已完整记录；
//   - Err==nil 且 DryRun=false 表示真实通道返回成功；
//   - Retryable=true 表示 5xx/超时/限流等可重试错误；
//   - DeactivateToken=true 表示 provider 明确判定令牌无效/unregistered，应立即停用令牌；
//   - Terminal=true 且 DeactivateToken=false 表示该条投递不应再重试，但令牌本身未必失效。
type Result struct {
	Provider        string
	DryRun          bool
	Retryable       bool
	Terminal        bool
	DeactivateToken bool
	StatusCode      int
	Err             error
}

// Adapter 是单一平台的推送实现。
type Adapter interface {
	Provider() string
	Send(ctx context.Context, msg Message) Result
}

// Service 按平台分发到具体适配器。
type Service struct {
	fcm  Adapter
	apns Adapter
	log  *slog.Logger
}

// NewService 从环境变量构建适配层。
// FCM：FCM_SERVER_KEY；APNs：APNS_KEY_FILE（或 APNS_KEY_PEM）、APNS_KEY_ID、APNS_TEAM_ID、APNS_TOPIC。
// 任何一组不完整时对应平台进入 dry-run。
func NewService(logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		fcm: NewFCMAdapter(os.Getenv("FCM_SERVER_KEY"), os.Getenv("FCM_ENDPOINT"), logger),
		apns: NewAPNsAdapter(APNsConfig{
			KeyFile:  os.Getenv("APNS_KEY_FILE"),
			KeyPEM:   os.Getenv("APNS_KEY_PEM"),
			KeyID:    os.Getenv("APNS_KEY_ID"),
			TeamID:   os.Getenv("APNS_TEAM_ID"),
			Topic:    os.Getenv("APNS_TOPIC"),
			Endpoint: os.Getenv("APNS_ENDPOINT"),
		}, logger),
		log: logger,
	}
}

// Send 实现按平台分发；未知平台按终态失败处理（数据库中 platform 已受 CHECK 约束）。
func (s *Service) Send(ctx context.Context, msg Message) Result {
	switch msg.Platform {
	case "android":
		return s.fcm.Send(ctx, msg)
	case "ios":
		return s.apns.Send(ctx, msg)
	default:
		return Result{Provider: "none", Terminal: true, Err: errUnknownPlatform(msg.Platform)}
	}
}

type unknownPlatformError string

func (e unknownPlatformError) Error() string { return "未知推送平台: " + string(e) }

func errUnknownPlatform(p string) error { return unknownPlatformError(p) }

// dryRunResult 记录完整载荷并返回 dry-run 结果。
// token 与 data 一并记录：这是「本应发送」的事实证据，不静默吞掉。
func dryRunResult(logger *slog.Logger, provider string, msg Message, payload any) Result {
	raw, err := json.Marshal(payload)
	if err != nil {
		logger.Error("dry-run 载荷序列化失败", "provider", provider, "error", err)
		return Result{Provider: provider, DryRun: true}
	}
	logger.Info("push dry-run：未配置凭据，本应发送",
		"provider", provider,
		"platform", msg.Platform,
		"userId", msg.UserID,
		"alertId", msg.AlertID,
		"pushDeviceId", msg.DeviceID,
		"token", msg.Token,
		"payload", json.RawMessage(raw),
	)
	return Result{Provider: provider, DryRun: true}
}
