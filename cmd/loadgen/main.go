// Command loadgen 对运行中的 PJGoIoT API 做多场景压测并输出 p50/p95/p99、RPS 与错误率。
//
// 场景：
//  1. devices：GET /api/v1/my/devices，默认 100 并发、30s；
//  2. push：POST /api/v1/sync/push，默认 50 并发，每请求 100 设备 + 200 读数；
//  3. series：GET /api/v1/my/devices/{platform}/{externalId}/readings/series?bucket=1d 90 天窗口，默认 50 并发；
//  4. changes：GET /api/v1/sync/changes 增量读，游标按响应推进、到底后回到 0；
//  5. scenes：GET /api/v1/scenes 列表 + PUT /api/v1/scenes/{id} 字段级 LWW 写入；
//  6. feedback：POST /api/v1/feedback 提交 + GET /api/v1/feedback/mine 列表；
//  7. catalog：GET /api/v1/catalog/device-types，预热取 ETag 后带 If-None-Match 命中 304。
//
// 运行前请先启动本地栈（Docker Compose 或 go run ./cmd/api），并关闭或调高限流：
//
//	RATE_LIMIT_PER_MINUTE=0 FEEDBACK_USER_RATE_PER_MINUTE=0 FEEDBACK_IP_RATE_PER_MINUTE=0 go run ./cmd/api
//	go run ./cmd/loadgen -base-url http://127.0.0.1:8080 -scenario all -duration 30s
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
	"strings"
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
	pushConc       int
	seriesConc     int
	changesLimit   int
	scenesCount    int
	changesConc    int
	scenesConc     int
	feedbackConc   int
	catalogConc    int
	insecure       bool
	seedReadings   int
	reportPath     string
	requestTimeout time.Duration
	// window > 0 时按固定时间窗输出分窗统计，用于长稳压测观察随时间的漂移；
	// 0 表示只输出整段汇总（保持原有行为）。
	window time.Duration

	smoke         bool
	maxErrorRate  float64
	maxP95Devices time.Duration
	maxP95Series  time.Duration
	maxP95Push    time.Duration
}

type scenarioResult struct {
	name  string
	conc  int
	total int
	rps   float64
	rate  float64
	p50   time.Duration
	p95   time.Duration
	p99   time.Duration
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

func percentilesOf(latencies []time.Duration) (p50, p95, p99 time.Duration) {
	if len(latencies) == 0 {
		return 0, 0, 0
	}
	sorted := append([]time.Duration(nil), latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[int(float64(len(sorted)-1)*0.50)],
		sorted[int(float64(len(sorted)-1)*0.95)],
		sorted[int(float64(len(sorted)-1)*0.99)]
}

// takeWindow 原子地把当前统计桶清空并返回快照，供分窗压测使用。
func (s *stats) takeWindow() (total, errors int, p50, p95, p99 time.Duration, statuses map[int]int, latencies []time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	latencies = s.latencies
	s.latencies = nil
	s.errors = 0
	statuses = s.statuses
	s.statuses = map[int]int{}
	total = len(latencies)
	errors = countErrors(statuses)
	p50, p95, p99 = percentilesOf(latencies)
	return
}

// countErrors 与 record 的判定保持一致：网络错误计为 status 0，其余按 <200 或 >=400 计错。
func countErrors(statuses map[int]int) int {
	n := 0
	for status, cnt := range statuses {
		if status < 200 || status >= 400 {
			n += cnt
		}
	}
	return n
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
	flag.IntVar(&cfg.pushConc, "push-concurrency", 50, "push 场景并发")
	flag.IntVar(&cfg.seriesConc, "series-concurrency", 50, "series 场景并发")
	flag.IntVar(&cfg.changesLimit, "changes-limit", 100, "changes 场景每页条数（1-500）")
	flag.IntVar(&cfg.scenesCount, "scenes-count", 50, "scenes 场景预置的场景数")
	flag.IntVar(&cfg.changesConc, "changes-concurrency", 50, "changes 场景并发")
	flag.IntVar(&cfg.scenesConc, "scenes-concurrency", 50, "scenes 场景并发")
	flag.IntVar(&cfg.feedbackConc, "feedback-concurrency", 20, "feedback 场景并发")
	flag.IntVar(&cfg.catalogConc, "catalog-concurrency", 100, "catalog 场景并发")
	flag.IntVar(&cfg.seedReadings, "seed-readings", 2000, "series 场景前预置的读数条数（覆盖 90 天）")
	flag.BoolVar(&cfg.insecure, "insecure-skip-verify", true, "跳过 TLS 校验（自签证书场景）")
	flag.StringVar(&cfg.reportPath, "report", "", "可选：把 Markdown 结果写入文件")
	flag.DurationVar(&cfg.requestTimeout, "request-timeout", 10*time.Second, "单请求超时")
	flag.DurationVar(&cfg.window, "window", 0, "可选：长稳压测分窗统计的窗口长度（如 1m），0 表示不分窗")

	flag.BoolVar(&cfg.smoke, "smoke", false, "CI 短跑模式：10s、低并发（devices 20 / push 5 / series 10），并按阈值断言")
	flag.Float64Var(&cfg.maxErrorRate, "max-error-rate", 0, "smoke 模式允许的错误率上限（百分比，0 表示必须为 0）")
	flag.DurationVar(&cfg.maxP95Devices, "max-p95-devices", 400*time.Millisecond, "smoke 模式 devices 场景 p95 上限")
	flag.DurationVar(&cfg.maxP95Series, "max-p95-series", 400*time.Millisecond, "smoke 模式 series 场景 p95 上限")
	flag.DurationVar(&cfg.maxP95Push, "max-p95-push", 4*time.Second, "smoke 模式 push 场景 p95 上限")
	flag.Parse()

	if cfg.smoke {
		cfg.duration = 10 * time.Second
		cfg.warmup = 2 * time.Second
		cfg.deviceCount = 20
		cfg.concurrency = 20
		cfg.pushConc = 5
		cfg.seriesConc = 10
		cfg.seedReadings = 500
		cfg.scenario = "all"
	}
	if cfg.concurrency <= 0 || cfg.pushConc <= 0 || cfg.seriesConc <= 0 {
		fmt.Fprintln(os.Stderr, "并发数必须大于 0")
		os.Exit(1)
	}

	// 连接池按所有场景并发之和配置，避免高并发下反复建连污染延迟数字。
	pool := cfg.concurrency + cfg.pushConc + cfg.seriesConc + cfg.changesConc +
		cfg.scenesConc + cfg.feedbackConc + cfg.catalogConc
	client := &http.Client{
		Timeout: cfg.requestTimeout,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: cfg.insecure},
			MaxIdleConns:        pool,
			MaxIdleConnsPerHost: pool,
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

	// 场景名即报告里的路径标签，也用于 -scenario 过滤。
	const (
		scDevices       = "GET /my/devices"
		scPush          = "POST /sync/push"
		scSeries        = "GET /readings/series"
		scChanges       = "GET /sync/changes"
		scScenesRead    = "GET /scenes"
		scScenesWrite   = "PUT /scenes"
		scFeedbackWrite = "POST /feedback"
		scFeedbackRead  = "GET /feedback/mine"
		scCatalog       = "GET /catalog/device-types"
	)
	// extended 是 v0.21.0 新增的压测场景；CI smoke 只跑原有三个，避免短跑里创建
	// 反馈/场景数据并触发未配置阈值。
	extended := map[string]bool{
		scChanges: true, scScenesRead: true, scScenesWrite: true,
		scFeedbackWrite: true, scFeedbackRead: true, scCatalog: true,
	}
	enabled := func(name string) bool {
		if cfg.scenario == "all" {
			return !cfg.smoke || !extended[name]
		}
		switch cfg.scenario {
		case "devices":
			return name == scDevices
		case "push":
			return name == scPush
		case "series":
			return name == scSeries
		case "changes":
			return name == scChanges
		case "scenes":
			return name == scScenesRead || name == scScenesWrite
		case "feedback":
			return name == scFeedbackWrite || name == scFeedbackRead
		case "catalog":
			return name == scCatalog
		}
		return false
	}

	macs := make([]string, cfg.deviceCount)
	for i := range macs {
		macs[i] = fmt.Sprintf("AA:BB:CC:DD:%02X:%02X", (i/256)%256, i%256)
	}
	if enabled(scDevices) || enabled(scPush) || enabled(scSeries) || enabled(scChanges) {
		if err := seedDevices(client, base, cfg.token, macs); err != nil {
			fmt.Fprintln(os.Stderr, "预置设备失败:", err)
			os.Exit(1)
		}
	}
	if enabled(scSeries) {
		if err := seedReadings(client, base, cfg.token, macs[0], cfg.seedReadings); err != nil {
			fmt.Fprintln(os.Stderr, "预置读数失败:", err)
			os.Exit(1)
		}
	}
	var sceneIDs []string
	if enabled(scScenesRead) || enabled(scScenesWrite) {
		ids, err := seedScenes(client, base, cfg.token, cfg.scenesCount)
		if err != nil {
			fmt.Fprintln(os.Stderr, "预置场景失败:", err)
			os.Exit(1)
		}
		sceneIDs = ids
	}
	var catalogETag string
	if enabled(scCatalog) {
		status, etag, err := fetchETag(client, base+"/api/v1/catalog/device-types", cfg.token)
		if err != nil || status != http.StatusOK || etag == "" {
			fmt.Fprintf(os.Stderr, "获取 catalog ETag 失败: status=%d etag=%q err=%v\n", status, etag, err)
			os.Exit(1)
		}
		catalogETag = etag
	}

	type scenario struct {
		name        string
		concurrency int
		run         func(s *stats, worker int)
	}
	list := []scenario{
		{scDevices, cfg.concurrency, func(s *stats, _ int) { runDevices(client, base, cfg.token, s) }},
		{scPush, cfg.pushConc, func(s *stats, worker int) { runPush(client, base, cfg.token, cfg, worker, s) }},
		{scSeries, cfg.seriesConc, func(s *stats, _ int) { runSeries(client, base, cfg.token, macs[0], s) }},
		{scChanges, cfg.changesConc, runChanges(client, base, cfg.token, cfg.changesLimit)},
		{scScenesRead, cfg.scenesConc, func(s *stats, _ int) { runScenesRead(client, base, cfg.token, s) }},
		{scScenesWrite, cfg.scenesConc, runScenesWrite(client, base, cfg.token, sceneIDs)},
		{scFeedbackWrite, cfg.feedbackConc, func(s *stats, _ int) { runFeedbackWrite(client, base, cfg.token, s) }},
		{scFeedbackRead, cfg.feedbackConc, func(s *stats, _ int) { runFeedbackRead(client, base, cfg.token, s) }},
		{scCatalog, cfg.catalogConc, func(s *stats, _ int) { runCatalog(client, base, cfg.token, catalogETag, s) }},
	}
	filtered := list[:0]
	for _, sc := range list {
		if enabled(sc.name) {
			filtered = append(filtered, sc)
		}
	}
	list = filtered
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "-scenario 只能是 devices|push|series|changes|scenes|feedback|catalog|all")
		os.Exit(1)
	}

	var lines []string
	lines = append(lines, "| 场景 | 并发 | 时长 | 请求数 | RPS | p50 | p95 | p99 | 错误率 |")
	lines = append(lines, "| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
	var results []scenarioResult
	for _, sc := range list {
		res, windows := runScenario(cfg, sc.name, sc.concurrency, sc.run)
		results = append(results, res)
		lines = append(lines, fmt.Sprintf("| %s | %d | %s | %d | %.1f | %s | %s | %s | %.2f%% |",
			res.name, res.conc, cfg.duration, res.total, res.rps,
			res.p50.Round(time.Millisecond), res.p95.Round(time.Millisecond),
			res.p99.Round(time.Millisecond), res.rate))
		fmt.Printf("%s rps=%.1f errors=%.2f%%\n", res.name, res.rps, res.rate)
		if len(windows) > 0 {
			lines = append(lines, "")
			lines = append(lines,
				fmt.Sprintf("### %s 分窗稳定性（window=%s）", res.name, cfg.window),
				"",
				"| 窗口 | 起点 | 请求数 | RPS | p50 | p95 | p99 | 错误率 | 状态码 |",
				"| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
			for _, w := range windows {
				lines = append(lines, fmt.Sprintf("| %d | %s | %d | %.1f | %s | %s | %s | %.2f%% | %s |",
					w.index, w.at.Round(time.Second), w.total, w.rps,
					w.p50.Round(time.Millisecond), w.p95.Round(time.Millisecond),
					w.p99.Round(time.Millisecond), w.rate, formatStatuses(w.statuses)))
			}
		}
	}

	report := join(lines)
	fmt.Println(report)
	if cfg.reportPath != "" {
		if err := os.WriteFile(cfg.reportPath, []byte(report+"\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "写入报告失败:", err)
			os.Exit(1)
		}
	}
	if cfg.smoke {
		if !assertSmoke(cfg, results) {
			os.Exit(1)
		}
		fmt.Println("SMOKE PASS: 全部场景满足错误率与 p95 阈值")
	}
}

// windowRow 是一个分窗统计快照，供长稳压测报告展示随时间的漂移。
type windowRow struct {
	index    int
	at       time.Duration
	total    int
	rps      float64
	p50      time.Duration
	p95      time.Duration
	p99      time.Duration
	rate     float64
	statuses map[int]int
}

func formatStatuses(statuses map[int]int) string {
	if len(statuses) == 0 {
		return "-"
	}
	codes := make([]int, 0, len(statuses))
	for code := range statuses {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	parts := make([]string, 0, len(codes))
	for _, code := range codes {
		parts = append(parts, fmt.Sprintf("%d:%d", code, statuses[code]))
	}
	return strings.Join(parts, " ")
}

// runScenario 运行单个场景。window>0 时持续按窗口清空统计桶并保留分窗结果，
// 全部样本另存一份用于整段 p50/p95/p99；window<=0 保持原有的一次性汇总行为。
func runScenario(cfg config, name string, concurrency int, run func(*stats, int)) (scenarioResult, []windowRow) {
	runWarmup(cfg, concurrency, run)
	st := &stats{statuses: map[int]int{}}
	start := time.Now()
	deadline := start.Add(cfg.duration)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				run(st, worker)
			}
		}(i)
	}

	if cfg.window <= 0 {
		wg.Wait()
		elapsed := time.Since(start)
		total, errs, p50, p95, p99, _ := st.snapshot()
		rate := errorRate(total, errs)
		return scenarioResult{
			name: name, conc: concurrency, total: total,
			rps: float64(total) / elapsed.Seconds(), rate: rate,
			p50: p50, p95: p95, p99: p99,
		}, nil
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	ticker := time.NewTicker(cfg.window)
	defer ticker.Stop()

	var (
		rows       []windowRow
		all        []time.Duration
		totalAll   int
		errAll     int
		lastFlush  = start
		windowSeen int
	)
	flush := func(now time.Time) {
		total, errs, p50, p95, p99, statuses, latencies := st.takeWindow()
		if total == 0 {
			lastFlush = now
			return
		}
		all = append(all, latencies...)
		totalAll += total
		errAll += errs
		windowSeen++
		rows = append(rows, windowRow{
			index: windowSeen, at: now.Sub(start), total: total,
			rps: float64(total) / now.Sub(lastFlush).Seconds(),
			p50: p50, p95: p95, p99: p99,
			rate:     errorRate(total, errs),
			statuses: statuses,
		})
		lastFlush = now
	}

	for {
		select {
		case now := <-ticker.C:
			flush(now)
		case <-done:
			flush(time.Now())
			elapsed := time.Since(start)
			p50, p95, p99 := percentilesOf(all)
			return scenarioResult{
				name: name, conc: concurrency, total: totalAll,
				rps: float64(totalAll) / elapsed.Seconds(), rate: errorRate(totalAll, errAll),
				p50: p50, p95: p95, p99: p99,
			}, rows
		}
	}
}

func errorRate(total, errors int) float64 {
	if total == 0 {
		return 0
	}
	return float64(errors) / float64(total) * 100
}

// assertSmoke 校验 smoke 结果：请求数非 0、错误率不超阈值、各场景 p95 不超阈值。
// 任何一项不满足都会打印原因并让进程非零退出，CI 直接变红。
func assertSmoke(cfg config, results []scenarioResult) bool {
	limits := map[string]time.Duration{
		"GET /my/devices":      cfg.maxP95Devices,
		"POST /sync/push":      cfg.maxP95Push,
		"GET /readings/series": cfg.maxP95Series,
	}
	ok := true
	for _, r := range results {
		limit, known := limits[r.name]
		var reasons []string
		if !known {
			reasons = append(reasons, "未知场景，未配置阈值")
		}
		if r.total == 0 {
			reasons = append(reasons, "没有完成任何请求")
		}
		if r.rate > cfg.maxErrorRate {
			reasons = append(reasons, fmt.Sprintf("错误率 %.2f%% 超过 %.2f%%", r.rate, cfg.maxErrorRate))
		}
		if known && r.p95 > limit {
			reasons = append(reasons, fmt.Sprintf("p95 %s 超过 %s", r.p95.Round(time.Millisecond), limit))
		}
		status := "PASS"
		if len(reasons) > 0 {
			status = "FAIL"
			ok = false
		}
		fmt.Fprintf(os.Stderr, "SMOKE %s %s: conc=%d requests=%d p95=%s (limit %s) p99=%s error_rate=%.2f%% (limit %.2f%%)",
			r.name, status, r.conc, r.total, r.p95.Round(time.Millisecond), limit,
			r.p99.Round(time.Millisecond), r.rate, cfg.maxErrorRate)
		if len(reasons) > 0 {
			fmt.Fprintf(os.Stderr, " reasons=%s", strings.Join(reasons, "; "))
		}
		fmt.Fprintln(os.Stderr)
	}
	return ok
}

func join(lines []string) string {
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// runWarmup 用与正式跑完全相同的请求逻辑预热，结果写入独立的丢弃桶，不进入统计。
func runWarmup(cfg config, concurrency int, run func(*stats, int)) {
	if cfg.warmup <= 0 {
		return
	}
	discard := &stats{statuses: map[int]int{}}
	deadline := time.Now().Add(cfg.warmup)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				run(discard, worker)
			}
		}(i)
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

func seriesURL(base, mac string) string {
	now := time.Now().UTC()
	from := now.Add(-90 * 24 * time.Hour).Format(time.RFC3339)
	to := now.Format(time.RFC3339)
	return fmt.Sprintf("%s/api/v1/my/devices/android/%s/readings/series?metric=heart_rate&bucket=1d&from=%s&to=%s",
		base, mac, from, to)
}

// doRequest 发送一次请求并读出响应体；headers 可为 nil，返回状态码、响应体与耗时错误。
func doRequest(client *http.Client, method, url, token string, body []byte, headers map[string]string) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

// seedScenes 用定时条件（不依赖设备）预置场景，供列表与字段级写入场景使用。
func seedScenes(client *http.Client, base, token string, count int) ([]string, error) {
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		body, _ := json.Marshal(map[string]any{
			"name":    fmt.Sprintf("loadgen-%03d", i),
			"trigger": map[string]any{"kind": "schedule", "timeOfDay": "08:00", "timezone": "UTC"},
			"actions": []any{map[string]any{"kind": "log", "message": "tick", "level": "info"}},
		})
		status, data, err := doRequest(client, http.MethodPost, base+"/api/v1/scenes", token, body, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusCreated {
			return nil, fmt.Errorf("创建场景 status=%d body=%s", status, data)
		}
		var out struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(data, &out); err != nil || out.ID == "" {
			return nil, fmt.Errorf("解析场景 id 失败: body=%s err=%v", data, err)
		}
		ids = append(ids, out.ID)
	}
	return ids, nil
}

// fetchETag 预取目录版本 ETag：后续请求带 If-None-Match 才能命中 304。
func fetchETag(client *http.Client, url, token string) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("ETag"), nil
}

// runChanges 返回增量读场景：每个 worker 维护自己的游标，按响应 nextCursor 推进，
// 到底后回到 0 继续读，模拟客户端反复增量同步。
func runChanges(client *http.Client, base, token string, limit int) func(*stats, int) {
	var (
		mu      sync.Mutex
		cursors = map[int]*int64{}
	)
	cursorFor := func(worker int) *int64 {
		mu.Lock()
		defer mu.Unlock()
		p, ok := cursors[worker]
		if !ok {
			p = new(int64)
			cursors[worker] = p
		}
		return p
	}
	return func(s *stats, worker int) {
		c := cursorFor(worker)
		url := fmt.Sprintf("%s/api/v1/sync/changes?limit=%d&cursor=%d", base, limit, atomic.LoadInt64(c))
		start := time.Now()
		status, data, err := doRequest(client, http.MethodGet, url, token, nil, nil)
		elapsed := time.Since(start)
		if err != nil {
			s.record(elapsed, 0)
			return
		}
		if status == http.StatusOK {
			var page struct {
				NextCursor int64 `json:"nextCursor"`
				HasMore    bool  `json:"hasMore"`
			}
			if json.Unmarshal(data, &page) == nil {
				next := page.NextCursor
				if !page.HasMore {
					next = 0
				}
				atomic.StoreInt64(c, next)
			}
		}
		s.record(elapsed, status)
	}
}

func runScenesRead(client *http.Client, base, token string, st *stats) {
	start := time.Now()
	status, _, err := doRequest(client, http.MethodGet, base+"/api/v1/scenes", token, nil, nil)
	elapsed := time.Since(start)
	if err != nil {
		st.record(elapsed, 0)
		return
	}
	st.record(elapsed, status)
}

var sceneWriteSeq int64

// runScenesWrite 返回字段级写入场景：每个请求改一个不同场景的 name，触发一次真实 UPDATE
// 与 change_log 写入；value 每次不同，保证不会被 LWW 判成幂等 no-op。
func runScenesWrite(client *http.Client, base, token string, ids []string) func(*stats, int) {
	return func(s *stats, worker int) {
		if len(ids) == 0 {
			s.record(0, 0)
			return
		}
		seq := atomic.AddInt64(&sceneWriteSeq, 1)
		id := ids[worker%len(ids)]
		body, _ := json.Marshal(map[string]any{
			"name": map[string]any{
				"value":     fmt.Sprintf("loadgen-%d", seq),
				"updatedAt": time.Now().UTC(),
			},
		})
		start := time.Now()
		status, _, err := doRequest(client, http.MethodPut, base+"/api/v1/scenes/"+id, token, body, nil)
		elapsed := time.Since(start)
		if err != nil {
			s.record(elapsed, 0)
			return
		}
		s.record(elapsed, status)
	}
}

var feedbackSeq int64

func runFeedbackWrite(client *http.Client, base, token string, st *stats) {
	seq := atomic.AddInt64(&feedbackSeq, 1)
	body, _ := json.Marshal(map[string]any{
		"kind": "experience",
		"body": fmt.Sprintf("loadgen feedback %d at %s", seq, time.Now().UTC().Format(time.RFC3339Nano)),
		"context": map[string]any{
			"platform": "android", "appVersion": "0.24.0", "buildType": "release",
			"osVersion": "14", "deviceModel": "loadgen",
		},
	})
	start := time.Now()
	status, _, err := doRequest(client, http.MethodPost, base+"/api/v1/feedback", token, body, nil)
	elapsed := time.Since(start)
	if err != nil {
		st.record(elapsed, 0)
		return
	}
	st.record(elapsed, status)
}

func runFeedbackRead(client *http.Client, base, token string, st *stats) {
	start := time.Now()
	status, _, err := doRequest(client, http.MethodGet, base+"/api/v1/feedback/mine?limit=20", token, nil, nil)
	elapsed := time.Since(start)
	if err != nil {
		st.record(elapsed, 0)
		return
	}
	st.record(elapsed, status)
}

// runCatalog 返回目录读取场景：带 If-None-Match，命中时服务端返回 304。
func runCatalog(client *http.Client, base, token, etag string, st *stats) {
	start := time.Now()
	status, _, err := doRequest(client, http.MethodGet, base+"/api/v1/catalog/device-types", token, nil,
		map[string]string{"If-None-Match": etag})
	elapsed := time.Since(start)
	if err != nil {
		st.record(elapsed, 0)
		return
	}
	st.record(elapsed, status)
}
