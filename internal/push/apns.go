package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultAPNsEndpoint = "https://api.push.apple.com"

// APNsConfig 是 APNs 凭据（p8 私钥 + Key ID + Team ID + Topic）。
type APNsConfig struct {
	KeyFile  string
	KeyPEM   string
	KeyID    string
	TeamID   string
	Topic    string
	Endpoint string
}

// APNsAdapter 使用 APNs HTTP/2 接口，JWT 以 ES256 签名。
// 凭据不完整时进入 dry-run；凭据齐全但私钥无法解析时按终态错误处理（避免假装发送成功）。
type APNsAdapter struct {
	key      *ecdsa.PrivateKey
	keyID    string
	teamID   string
	topic    string
	endpoint string
	client   *http.Client
	log      *slog.Logger
	initErr  error
}

// NewAPNsAdapter 构造 APNs 适配器。
func NewAPNsAdapter(cfg APNsConfig, logger *slog.Logger) *APNsAdapter {
	if logger == nil {
		logger = slog.Default()
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = defaultAPNsEndpoint
	}
	a := &APNsAdapter{
		keyID:    strings.TrimSpace(cfg.KeyID),
		teamID:   strings.TrimSpace(cfg.TeamID),
		topic:    strings.TrimSpace(cfg.Topic),
		endpoint: strings.TrimRight(endpoint, "/"),
		log:      logger,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
				ForceAttemptHTTP2: true,
			},
		},
	}
	if a.keyID == "" || a.teamID == "" || a.topic == "" {
		return a // 凭据不完整 -> dry-run
	}
	keyData, err := a.loadKey(cfg)
	if err != nil {
		a.initErr = err
		return a
	}
	a.key = keyData
	return a
}

func (a *APNsAdapter) loadKey(cfg APNsConfig) (*ecdsa.PrivateKey, error) {
	raw := []byte(cfg.KeyPEM)
	if file := strings.TrimSpace(cfg.KeyFile); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("读取 APNs 私钥失败: %w", err)
		}
		raw = data
	}
	if len(raw) == 0 {
		return nil, errors.New("APNs 私钥为空")
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("APNs 私钥不是合法 PEM")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if key, ok := parsed.(*ecdsa.PrivateKey); ok {
			return key, nil
		}
		return nil, errors.New("APNs 私钥不是 EC 私钥")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func (a *APNsAdapter) Provider() string { return "apns" }

// configured 表示是否具备真实发送条件（dry-run 判断依据）。
func (a *APNsAdapter) configured() bool { return a.key != nil }

type apnsAlert struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type apnsPayload struct {
	APS  map[string]any    `json:"aps"`
	Data map[string]string `json:"data,omitempty"`
}

// Send 实现 Adapter。
func (a *APNsAdapter) Send(ctx context.Context, msg Message) Result {
	payload := apnsPayload{
		APS: map[string]any{
			"alert": apnsAlert{Title: msg.Title, Body: msg.Body},
			"sound": "default",
		},
		Data: msg.Data,
	}
	if !a.configured() {
		return dryRunResult(a.log, "apns-dry-run", msg, payload)
	}
	if a.initErr != nil {
		return Result{Provider: "apns", Terminal: true, Err: a.initErr}
	}
	token, err := a.jwt()
	if err != nil {
		return Result{Provider: "apns", Terminal: true, Err: fmt.Errorf("生成 APNs JWT 失败: %w", err)}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Result{Provider: "apns", Terminal: true, Err: fmt.Errorf("序列化 APNs 载荷失败: %w", err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.endpoint+"/3/device/"+msg.Token, bytes.NewReader(raw))
	if err != nil {
		return Result{Provider: "apns", Terminal: true, Err: fmt.Errorf("构造 APNs 请求失败: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("apns-topic", a.topic)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")

	resp, err := a.client.Do(req)
	if err != nil {
		return Result{Provider: "apns", Retryable: true, Err: fmt.Errorf("APNs 请求失败: %w", err)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusOK {
		return Result{Provider: "apns", StatusCode: resp.StatusCode}
	}
	var parsed struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(body, &parsed)
	return classifyAPNs(resp.StatusCode, parsed.Reason)
}

// jwt 生成 APNs 所需的 ES256 JWT。签名必须是固定 64 字节的 R||S，而不是 ASN.1 DER。
func (a *APNsAdapter) jwt() (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "ES256", "kid": a.keyID})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{"iss": a.teamID, "iat": time.Now().Unix()})
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, a.key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	copy(sig[32-len(r.Bytes()):32], r.Bytes())
	copy(sig[64-len(s.Bytes()):64], s.Bytes())
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// classifyAPNs 把 APNs 状态码与 reason 映射到统一 Result。
func classifyAPNs(status int, reason string) Result {
	res := Result{Provider: "apns", StatusCode: status}
	switch {
	case status >= 500 || status == http.StatusTooManyRequests:
		res.Retryable = true
		res.Err = fmt.Errorf("APNs 返回 %d: %s", status, reason)
		return res
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		res.Terminal = true
		res.Err = fmt.Errorf("APNs 凭据或权限错误（%d）: %s", status, reason)
		return res
	}
	switch reason {
	case "Unregistered", "BadDeviceToken", "DeviceTokenNotForTopic":
		res.Terminal = true
		res.DeactivateToken = true
		res.Err = fmt.Errorf("APNs 令牌失效: %s", reason)
	case "ExpiredProviderToken", "TooManyProviderTokenUpdates":
		// 凭据/令牌刷新问题，重试无益；保留令牌，等待运维修复。
		res.Terminal = true
		res.Err = fmt.Errorf("APNs 凭据问题: %s", reason)
	default:
		if reason != "" {
			res.Terminal = true
			res.Err = fmt.Errorf("APNs 终态错误（%d）: %s", status, reason)
		}
	}
	return res
}
