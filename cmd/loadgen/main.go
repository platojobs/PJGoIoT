// Command loadgen 对运行中的 PJGoIoT API 做三场景压测并输出 p50/p95/p99、RPS 与错误率。
//
// 场景：
//  1. devices：GET /api/v1/my/devices，默认 100 并发、30s；
//  2. push：POST /api/v1/sync/push，默认 50 并发，每请求 100 设备 + 200 读数；
//  3. series：GET /api/v1/my/devices/{platform}/{externalId}/readings/series?bucket=1d 90 天窗口，默认 50 并发。
//
// 运行前请先启动本地栈（Docker Compose 或 go run ./cmd/api），并关闭或调高限流：
//
//	RATE_LIMIT_PER_MINUTE=0 docker compose up -d api
//	go run ./cmd/loadgen -base-url https://localhost -duration 30s
//
// 若使用 Docker Compose 的 Caddy 自签证书，保持默认 -insecure-skip-verify=true。
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type config struct {
	baseURL        string
	token          string
	duration       time.Duration
	warmup         time.Duration
	scenario       string
	deviceCount    int
	pushDevices    int
	pushReadings   int
	concurrency    int
	seriesConc     int
	insecure       bool
	seedReadings   int
	reportPath     string
	requestTimeout time.Duration
}

type stats struct {
	mu        sync.Mutex
	latencies []time.Duration
	errors    int
	statuses  map[int]int
}

func (s *stats) record(d time.Duration, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latencies = append(s.latencies, d)
	if status < 200 || status >= 400 {
		s.errors++
	}
	s.statuses[status]++
}

func (s *stats) percentile(p float64) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return percentileLocked(s.latencies, p)
}

func (s *stats) snapshot() (total, errors int, p50, p95, p99 time.Duration, statuses map[int]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total = len(s.latencies)
	errors = s.errors
	statuses = make(map[int]int, len(s.statuses))
	for k, v := range s.statuses {
		statuses[k] = v
	}
	p50 = percentileLocked(s.latencies, 0.50)
	p95 = percentileLocked(s.latencies, 0.95)
	p99 = percentileLocked(s.latencies, 0.99)
	return
}

func percentileLocked(latencies []time.Duration, p float64) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.baseURL, "base-url", "http://127.0.0.1:8080", "API 基地址（Docker Compose 经 Caddy 时为 https://localhost）")
	flag.StringVar(&cfg.token, "token", os.Getenv("LOADGEN_TOKEN"), "已有用户 token；为空时自动创建匿名用户")
	flag.DurationVar(&cfg.duration, "duration", 30*time.Second, "每个场景的压测时长")
	flag.DurationVar(&cfg.warmup, "warmup", 3*time.Second, "预热时长（不计入结果）")
	flag.StringVar(&cfg.scenario, "scenario", "all", "devices|push|series|all")
	flag.IntVar(&cfg.deviceCount, "devices", 100, "devices 场景前预置的设备数量")
	flag.IntVar(&cfg.pushDevices, "push-devices", 100, "push 场景每请求设备数")
	flag.IntVar(&cfg.pushReadings, "push-readings", 200, "push 场景每请求读数数")
	flag.IntVar(&cfg.concurrency, "concurrency", 100, "devices 场景并发")
	flag.IntVar(&cfg.seriesConc, "series-concurrency", 50, "series 场景并发（push 场景固定 50，可用 -push-concurrency 覆盖）")
	flag.IntVar(&cfg.seedReadings, "seed-readings", 2000, "series 场景前预置的读数条数（覆盖 90 天）")
	flag.BoolVar(&cfg.insecure, "insecure-skip-verify", true, "跳过 TLS 校验（自签证书场景）")
	flag.StringVar(&cfg.reportPath, "report", "", "可选：把 Markdown 结果写入文件")
	flag.DurationVar(&cfg.requestTimeout, "request-timeout", 10*time.Second, "单请求超时")
	flag.Parse()

	client := &http.Client{
		Timeout: cfg.requestTimeout,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: cfg.insecure},
			MaxIdleConns:        cfg.concurrency + cfg.seriesConc + cfg.concurrency,
			MaxIdleConnsPerHost: cfg.concurrency + cfg.seriesConc + cfg.concurrency,
			MaxConnsPerHost:     0,
		},
	}
	base := cfg.baseURL

	if cfg.token == "" {
		tok, err := createAnonymous(client, base)
		if err != nil {
			fmt.Fprintln(os.Stderr, "创建匿名用户失败:", err)
			os.Exit(1)
		}
		cfg.token = tok
	}

	macs := make([]string, cfg.deviceCount)
	for i := range macs {
		macs[i] = fmt.Sprintf("AA:BB:CC:DD:%02X:%02X", (i/256)%256, i%256)
	}
	if err := seedDevices(client, base, cfg.token, macs); err != nil {
		fmt.Fprintln(os.Stderr, "预置设备失败:", err)
		os.Exit(1)
	}
	if err := seedReadings(client, base, cfg.token, macs[0], cfg.seedReadings); err != nil {
		fmt.Fprintln(os.Stderr, "预置读数失败:", err)
		os.Exit(1)
	}

	type scenario struct {
		name        string
		concurrency int
		run         func(s *stats, worker int)
	}
	list := []scenario{
		{"GET /my/devices", cfg.concurrency, func(s *stats, _ int) { runDevices(client, base, cfg.token, s) }},
		{"POST /sync/push", 50, func(s *stats, worker int) { runPush(client, base, cfg.token, cfg, worker, s) }},
		{"GET /readings/series", cfg.seriesConc, func(s *stats, _ int) { runSeries(client, base, cfg.token, macs[0], s) }},
	}
	if cfg.scenario != "all" {
		filtered := list[:0]
		for _, sc := range list {
			if cfg.scenario == "devices" && sc.name == "GET /my/devices" ||
				cfg.scenario == "push" && sc.name == "POST /sync/push" ||
				cfg.scenario == "series" && sc.name == "GET /readings/series" {
				filtered = append(filtered, sc)
			}
		}
		list = filtered
	}
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "-scenario 只能是 devices|push|series|all")
		os.Exit(1)
	}

	var lines []string
	lines = append(lines, "| 场景 | 并发 | 时长 | 请求数 | RPS | p50 | p95 | p99 | 错误率 |")
	lines = append(lines, "| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
	for _, sc := range list {
		runWarmup(client, base, cfg, sc.name, sc.concurrency)
		st := &stats{statuses: map[int]int{}}
		start := time.Now()
		var wg sync.WaitGroup
		deadline := start.Add(cfg.duration)
		for i := 0; i < sc.concurrency; i++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for time.Now().Before(deadline) {
					sc.run(st, worker)
				}
			}(i)
		}
		wg.Wait()
		elapsed := time.Since(start)
		total, errs, p50, p95, p99, statuses := st.snapshot()
		rps := float64(total) / elapsed.Seconds()
		rate := 0.0
		if total > 0 {
			rate = float64(errs) / float64(total) * 100
		}
		line := fmt.Sprintf("| %s | %d | %s | %d | %.1f | %s | %s | %s | %.2f%% |",
			sc.name, sc.concurrency, cfg.duration, total, rps, p50.Round(time.Millisecond), p95.Round(time.Millisecond), p99.Round(time.Millisecond), rate)
		lines = append(lines, line)
		fmt.Printf("%s rps=%.1f errors=%.2f%% statuses=%v\n", sc.name, rps, rate, statuses)
	}

	report := join(lines)
	fmt.Println(report)
	if cfg.reportPath != "" {
		if err := os.WriteFile(cfg.reportPath, []byte(report+"\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "写入报告失败:", err)
			os.Exit(1)
		}
	}
}

func join(lines []string) string {
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

func runWarmup(client *http.Client, base string, cfg config, name string, concurrency int) {
	if cfg.warmup <= 0 {
		return
	}
	deadline := time.Now().Add(cfg.warmup)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				switch name {
				case "GET /my/devices":
					_, _ = get(client, base+"/api/v1/my/devices", cfg.token)
				case "POST /sync/push":
					_, _ = pushOnce(client, base, cfg.token, nil, cfg, 1)
				case "GET /readings/series":
					_, _ = seriesOnce(client, base, cfg.token, "")
				}
			}
		}()
	}
	wg.Wait()
}

func createAnonymous(client *http.Client, base string) (string, error) {
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/auth/anonymous", nil)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("status=%d body=%s", resp.StatusCode, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Token, nil
}

func seedDevices(client *http.Client, base, token string, macs []string) error {
	devices := make([]map[string]any, 0, len(macs))
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, mac := range macs {
		devices = append(devices, map[string]any{
			"id": uuid.NewString(), "platform": "android", "externalId": mac,
			"rules": map[string]any{}, "scanRecords": []any{}, "updatedAt": now,
		})
	}
	return postJSON(client, base+"/api/v1/sync/push", token, map[string]any{"devices": devices, "readings": []any{}})
}

func seedReadings(client *http.Client, base, token, mac string, count int) error {
	const batch = 500
	now := time.Now().UTC()
	window := 90 * 24 * time.Hour
	for offset := 0; offset < count; offset += batch {
		n := count - offset
		if n > batch {
			n = batch
		}
		readings := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			idx := offset + i
			recorded := now.Add(-window + time.Duration(idx)*window/time.Duration(count))
			readings = append(readings, map[string]any{
				"id": uuid.NewString(), "platform": "android", "externalId": mac,
				"metric": "heart_rate", "value": 60 + float64(idx%40), "unit": "bpm", "recordedAt": recorded,
			})
		}
		if err := postJSON(client, base+"/api/v1/sync/push", token, map[string]any{"devices": []any{}, "readings": readings}); err != nil {
			return err
		}
	}
	return nil
}

func postJSON(client *http.Client, url, token string, body any) error {
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status=%d", resp.StatusCode)
	}
	return nil
}

func get(client *http.Client, url, token string) (int, time.Duration) {
	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return 0, time.Since(start)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, time.Since(start)
}

func runDevices(client *http.Client, base, token string, st *stats) {
	status, d := get(client, base+"/api/v1/my/devices", token)
	st.record(d, status)
}

var pushReqSeq int64

func runPush(client *http.Client, base, token string, cfg config, worker int, st *stats) {
	start := time.Now()
	status, err := pushOnce(client, base, token, workerMACs(worker, cfg.pushDevices), cfg, int(atomic.AddInt64(&pushReqSeq, 1)))
	if err != nil {
		st.record(time.Since(start), 0)
		return
	}
	st.record(time.Since(start), status)
}

// workerMACs 为每个并发 worker 分配独立设备集合，模拟多个客户端各自推送，
// 避免 50 个并发事务反复争抢同一批行而把压测变成锁排队测试。
func workerMACs(worker, n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = fmt.Sprintf("AA:%02X:%02X:00:00:01", worker%256, i%256)
	}
	return out
}

// pushOnce 构造一次 push 请求：固定 100 台设备（updatedAt 递增）与 200 条读数。
func pushOnce(client *http.Client, base, token string, macs []string, cfg config, nonce int) (int, error) {
	if len(macs) == 0 {
		macs = []string{"AA:BB:CC:DD:00:00"}
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	devices := make([]map[string]any, 0, cfg.pushDevices)
	for i := 0; i < cfg.pushDevices; i++ {
		mac := macs[i%len(macs)]
		devices = append(devices, map[string]any{
			"id": pushDeviceID(mac), "platform": "android", "externalId": mac,
			"rules": map[string]any{}, "scanRecords": []any{}, "updatedAt": now,
		})
	}
	readings := make([]map[string]any, 0, cfg.pushReadings)
	for i := 0; i < cfg.pushReadings; i++ {
		mac := macs[i%len(macs)]
		readings = append(readings, map[string]any{
			"id": uuid.NewString(), "platform": "android", "externalId": mac,
			"metric": "heart_rate", "value": 60 + float64(nonce%40), "unit": "bpm", "recordedAt": now,
		})
	}
	buf, _ := json.Marshal(map[string]any{"devices": devices, "readings": readings})
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/sync/push", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// pushDeviceID 为每台 MAC 生成稳定 UUID（UUIDv5 风格，负载测试无需真实实现）。
var deviceIDs sync.Map

func pushDeviceID(mac string) string {
	if v, ok := deviceIDs.Load(mac); ok {
		return v.(string)
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(mac)).String()
	deviceIDs.Store(mac, id)
	return id
}

func runSeries(client *http.Client, base, token, mac string, st *stats) {
	status, d := get(client, seriesURL(base, mac), token)
	st.record(d, status)
}

func seriesOnce(client *http.Client, base, token, mac string) (int, error) {
	if mac == "" {
		mac = "AA:BB:CC:DD:00:00"
	}
	status, _ := get(client, seriesURL(base, mac), token)
	return status, nil
}

func seriesURL(base, mac string) string {
	now := time.Now().UTC()
	from := now.Add(-90 * 24 * time.Hour).Format(time.RFC3339)
	to := now.Format(time.RFC3339)
	return fmt.Sprintf("%s/api/v1/my/devices/android/%s/readings/series?metric=heart_rate&bucket=1d&from=%s&to=%s",
		base, mac, from, to)
}
