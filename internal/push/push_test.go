package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/platojobs/PJGoIoT/internal/model"
	"github.com/platojobs/PJGoIoT/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestFCMDryRunLogsFullPayload(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	adapter := NewFCMAdapter("", "", logger)
	msg := Message{
		UserID: "user-1", AlertID: "alert-1", DeviceID: "dev-1",
		Platform: "android", Token: "tok-abc", Title: "设备离线", Body: "客厅传感器离线",
		Data: map[string]string{"kind": "device_offline", "alertId": "alert-1"},
	}
	res := adapter.Send(context.Background(), msg)
	if !res.DryRun || res.Provider != "fcm-dry-run" {
		t.Fatalf("无凭据应为 dry-run，得到 %+v", res)
	}
	log := buf.String()
	for _, want := range []string{"dry-run", "tok-abc", "设备离线", "device_offline", "fcm-dry-run"} {
		if !strings.Contains(log, want) {
			t.Fatalf("dry-run 日志缺少 %q: %s", want, log)
		}
	}
}

func TestFCMClassification(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		retryable  bool
		terminal   bool
		deactivate bool
	}{
		{"成功", http.StatusOK, `{"success":1,"failure":0}`, false, false, false},
		{"令牌失效", http.StatusOK, `{"success":0,"failure":1,"results":[{"error":"NotRegistered"}]}`, false, true, true},
		{"服务端错误", http.StatusServiceUnavailable, `{"error":"Unavailable"}`, true, false, false},
		{"限流", http.StatusTooManyRequests, `{"error":"DeviceMessageRateExceeded"}`, true, false, false},
		{"凭据错误", http.StatusUnauthorized, `{"error":"Unauthorized"}`, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "key=server-key" {
					t.Errorf("Authorization 头不正确: %q", got)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			adapter := NewFCMAdapter("server-key", srv.URL, discardLogger())
			res := adapter.Send(context.Background(), Message{Platform: "android", Token: "tok"})
			if res.Retryable != tc.retryable || res.Terminal != tc.terminal || res.DeactivateToken != tc.deactivate {
				t.Fatalf("%s 分类错误: %+v", tc.name, res)
			}
		})
	}
}

func TestFCMNetworkErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("不支持 Hijack")
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()
	adapter := NewFCMAdapter("server-key", srv.URL, discardLogger())
	res := adapter.Send(context.Background(), Message{Platform: "android", Token: "tok"})
	if !res.Retryable || res.Terminal {
		t.Fatalf("连接中断应可重试，得到 %+v", res)
	}
}

func TestAPNsDryRunLogsFullPayload(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	adapter := NewAPNsAdapter(APNsConfig{}, logger)
	res := adapter.Send(context.Background(), Message{
		Platform: "ios", Token: "apns-token", Title: "告警", Body: "正文",
		Data: map[string]string{"kind": "device_offline"},
	})
	if !res.DryRun || res.Provider != "apns-dry-run" {
		t.Fatalf("无凭据应为 dry-run，得到 %+v", res)
	}
	if log := buf.String(); !strings.Contains(log, "apns-token") || !strings.Contains(log, "device_offline") {
		t.Fatalf("dry-run 日志缺少完整载荷: %s", log)
	}
}

func TestAPNsClassification(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("编码密钥失败: %v", err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	cases := []struct {
		name       string
		status     int
		body       string
		retryable  bool
		terminal   bool
		deactivate bool
	}{
		{"成功", http.StatusOK, ``, false, false, false},
		{"令牌失效", http.StatusGone, `{"reason":"Unregistered"}`, false, true, true},
		{"服务端错误", http.StatusInternalServerError, `{"reason":"InternalServerError"}`, true, false, false},
		{"凭据错误", http.StatusForbidden, `{"reason":"InvalidProviderToken"}`, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/3/device/") {
					t.Errorf("路径不正确: %s", r.URL.Path)
				}
				if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "bearer ") {
					t.Errorf("缺少 bearer JWT: %q", auth)
				}
				if r.Header.Get("apns-topic") != "com.example.app" {
					t.Errorf("apns-topic 不正确: %q", r.Header.Get("apns-topic"))
				}
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
				}
			}))
			defer srv.Close()
			adapter := NewAPNsAdapter(APNsConfig{
				KeyPEM: keyPEM, KeyID: "KEYID", TeamID: "TEAMID",
				Topic: "com.example.app", Endpoint: srv.URL,
			}, discardLogger())
			res := adapter.Send(context.Background(), Message{Platform: "ios", Token: "tok"})
			if res.Retryable != tc.retryable || res.Terminal != tc.terminal || res.DeactivateToken != tc.deactivate {
				t.Fatalf("%s 分类错误: %+v", tc.name, res)
			}
		})
	}
}

// fakeDeliveryStore 只实现 worker 用到的方法；其余方法由嵌入的接口占位（不会被调用）。
type fakeDeliveryStore struct {
	store.Store
	claimed         []model.PushDelivery
	completed       []string
	completedDryRun []bool
	retried         []time.Time
	failed          []string
	deactivated     []bool
}

func (f *fakeDeliveryStore) ClaimPushDeliveries(ctx context.Context, now time.Time, limit int, staleBefore time.Time) ([]model.PushDelivery, error) {
	out := f.claimed
	f.claimed = nil
	return out, nil
}

func (f *fakeDeliveryStore) CompletePushDelivery(ctx context.Context, id string, provider string, dryRun bool, sentAt time.Time) error {
	f.completed = append(f.completed, id)
	f.completedDryRun = append(f.completedDryRun, dryRun)
	return nil
}

func (f *fakeDeliveryStore) RetryPushDelivery(ctx context.Context, id string, next time.Time, lastError string) error {
	f.retried = append(f.retried, next)
	return nil
}

func (f *fakeDeliveryStore) FailPushDelivery(ctx context.Context, id string, reason string, deactivate bool, now time.Time) error {
	f.failed = append(f.failed, id)
	f.deactivated = append(f.deactivated, deactivate)
	return nil
}

type stubAdapter struct{ res Result }

func (s stubAdapter) Provider() string                             { return "stub" }
func (s stubAdapter) Send(ctx context.Context, msg Message) Result { return s.res }

func TestWorkerDryRunCompletesDelivery(t *testing.T) {
	st := &fakeDeliveryStore{claimed: []model.PushDelivery{{
		ID: "d1", Platform: "android", Token: "tok", Attempts: 1,
	}}}
	w := NewWorker(st, NewService(discardLogger()), discardLogger(), WorkerConfig{})
	w.now = func() time.Time { return time.Unix(1000, 0).UTC() }
	if _, err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if len(st.completed) != 1 || !st.completedDryRun[0] {
		t.Fatalf("dry-run 应完成投递: %+v", st.completed)
	}
}

func TestWorkerRetriesWithBackoffAndHonorsMaxAttempts(t *testing.T) {
	svc := NewService(discardLogger())
	svc.fcm = stubAdapter{res: Result{Provider: "stub", Retryable: true, Err: errors.New("503")}}
	now := time.Unix(1000, 0).UTC()

	st := &fakeDeliveryStore{claimed: []model.PushDelivery{{
		ID: "d1", Platform: "android", Attempts: 1,
	}}}
	w := NewWorker(st, svc, discardLogger(), WorkerConfig{MaxAttempts: 5, BackoffBase: 30 * time.Second})
	w.now = func() time.Time { return now }
	if _, err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if len(st.retried) != 1 || !st.retried[0].Equal(now.Add(30*time.Second)) {
		t.Fatalf("首次失败应按 30s 退避: %+v", st.retried)
	}

	// 第二次失败：attempts=2 -> 60s。
	st.retried = nil
	st.claimed = []model.PushDelivery{{ID: "d1", Platform: "android", Attempts: 2}}
	if _, err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if len(st.retried) != 1 || !st.retried[0].Equal(now.Add(60*time.Second)) {
		t.Fatalf("第二次失败应按 60s 退避: %+v", st.retried)
	}

	// 达到上限后转 failed，不重复重试。
	st.retried = nil
	st.claimed = []model.PushDelivery{{ID: "d1", Platform: "android", Attempts: 5}}
	if _, err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if len(st.retried) != 0 || len(st.failed) != 1 {
		t.Fatalf("达到上限应终态失败: retried=%v failed=%v", st.retried, st.failed)
	}
}

func TestWorkerTerminalInvalidTokenDeactivates(t *testing.T) {
	svc := NewService(discardLogger())
	svc.apns = stubAdapter{res: Result{Provider: "stub", Terminal: true, DeactivateToken: true, Err: errors.New("Unregistered")}}
	st := &fakeDeliveryStore{claimed: []model.PushDelivery{{
		ID: "d1", Platform: "ios", Token: "tok", Attempts: 1,
	}}}
	w := NewWorker(st, svc, discardLogger(), WorkerConfig{})
	if _, err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if len(st.failed) != 1 || !st.deactivated[0] {
		t.Fatalf("失效令牌应停用: failed=%v deactivated=%v", st.failed, st.deactivated)
	}
}

func TestUnknownPlatformIsTerminal(t *testing.T) {
	res := NewService(discardLogger()).Send(context.Background(), Message{Platform: "web"})
	if !res.Terminal || res.DryRun {
		t.Fatalf("未知平台应按终态处理，得到 %+v", res)
	}
}
