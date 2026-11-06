package push

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/platojobs/PJGoIoT/internal/store"
)

// WorkerConfig 是投递 worker 的运行参数。
type WorkerConfig struct {
	Interval          time.Duration
	Batch             int
	MaxAttempts       int
	BackoffBase       time.Duration
	SendingStaleAfter time.Duration
}

// Worker 消费 push_deliveries：抢占到期记录、调用适配层、写回 sent/retry/failed。
type Worker struct {
	store   store.Store
	service *Service
	log     *slog.Logger
	cfg     WorkerConfig
	now     func() time.Time
}

// NewWorker 构造 worker；零值参数会被替换为安全默认值。
func NewWorker(st store.Store, service *Service, logger *slog.Logger, cfg WorkerConfig) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 50
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = 30 * time.Second
	}
	if cfg.SendingStaleAfter <= 0 {
		cfg.SendingStaleAfter = 5 * time.Minute
	}
	return &Worker{store: st, service: service, log: logger, cfg: cfg, now: time.Now}
}

// Run 启动循环直到 ctx 取消；启动时先处理一轮，避免退避等待首个 tick。
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		if n, err := w.ProcessOnce(ctx); err != nil {
			w.log.Warn("推送投递 worker 处理失败", "error", err)
		} else if n > 0 {
			w.log.Info("推送投递 worker 处理完成", "deliveries", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProcessOnce 处理一批到期投递，返回本次抢占的条数。可用于测试与手动触发。
func (w *Worker) ProcessOnce(ctx context.Context) (int, error) {
	now := w.now().UTC()
	items, err := w.store.ClaimPushDeliveries(ctx, now, w.cfg.Batch, now.Add(-w.cfg.SendingStaleAfter))
	if err != nil {
		return 0, err
	}
	for _, d := range items {
		msg := Message{
			UserID:   d.UserID,
			AlertID:  d.AlertID,
			DeviceID: d.PushDeviceID,
			Platform: d.Platform,
			Token:    d.Token,
			Title:    d.Title,
			Body:     d.Body,
			Data:     d.Data,
		}
		res := w.service.Send(ctx, msg)
		switch {
		case res.DryRun || (res.Err == nil && !res.Retryable && !res.Terminal):
			if err := w.store.CompletePushDelivery(ctx, d.ID, res.Provider, res.DryRun, now); err != nil {
				w.log.Error("写回推送成功状态失败", "deliveryId", d.ID, "error", err)
			}
			w.log.Info("推送投递完成", "deliveryId", d.ID, "provider", res.Provider,
				"dryRun", res.DryRun, "attempts", d.Attempts)
		case res.Retryable && d.Attempts < w.cfg.MaxAttempts:
			next := now.Add(w.backoff(d.Attempts))
			reason := errorString(res.Err)
			if err := w.store.RetryPushDelivery(ctx, d.ID, next, reason); err != nil {
				w.log.Error("写回推送重试状态失败", "deliveryId", d.ID, "error", err)
			}
			w.log.Warn("推送投递失败，已安排重试", "deliveryId", d.ID, "provider", res.Provider,
				"attempts", d.Attempts, "nextAttemptAt", next, "error", reason)
		default:
			reason := errorString(res.Err)
			if res.Retryable && d.Attempts >= w.cfg.MaxAttempts {
				reason = fmt.Sprintf("达到最大重试次数 %d: %s", w.cfg.MaxAttempts, reason)
			}
			if err := w.store.FailPushDelivery(ctx, d.ID, reason, res.DeactivateToken, now); err != nil {
				w.log.Error("写回推送失败状态失败", "deliveryId", d.ID, "error", err)
			}
			w.log.Warn("推送投递终止", "deliveryId", d.ID, "provider", res.Provider,
				"deactivateToken", res.DeactivateToken, "attempts", d.Attempts, "error", reason)
		}
	}
	return len(items), nil
}

// backoff 返回第 attempt 次失败后的退避时长：base * 2^(attempt-1)，上限 30 分钟。
func (w *Worker) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := w.cfg.BackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d > 30*time.Minute {
			return 30 * time.Minute
		}
	}
	if d > 30*time.Minute {
		return 30 * time.Minute
	}
	return d
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
