package httpapi

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/platojobs/PJGoIoT/internal/model"
)

// latencyBuckets 是延迟直方图的 le 边界（秒），最后自动补 +Inf。
var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type requestKey struct {
	method string
	status int
}

type latencyHistogram struct {
	counts []uint64
	sum    float64
	total  uint64
}

// metricsRegistry 是进程内 Prometheus 指标注册表，不依赖第三方客户端。
type metricsRegistry struct {
	mu        sync.Mutex
	requests  map[requestKey]uint64
	durations map[string]*latencyHistogram
	dbErrors  uint64
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{
		requests:  map[requestKey]uint64{},
		durations: map[string]*latencyHistogram{},
	}
}

func (m *metricsRegistry) observeRequest(method string, status int, d time.Duration) {
	seconds := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()

	m.requests[requestKey{method: method, status: status}]++

	h, ok := m.durations[method]
	if !ok {
		h = &latencyHistogram{counts: make([]uint64, len(latencyBuckets))}
		m.durations[method] = h
	}
	for i, le := range latencyBuckets {
		if seconds <= le {
			h.counts[i]++
		}
	}
	h.sum += seconds
	h.total++
}

func (m *metricsRegistry) incDBErrors() {
	m.mu.Lock()
	m.dbErrors++
	m.mu.Unlock()
}

func (m *metricsRegistry) render(counts model.StoreCounts) string {
	m.mu.Lock()
	requests := make(map[requestKey]uint64, len(m.requests))
	for k, v := range m.requests {
		requests[k] = v
	}
	durations := make(map[string]latencyHistogram, len(m.durations))
	for method, h := range m.durations {
		durations[method] = latencyHistogram{
			counts: append([]uint64(nil), h.counts...),
			sum:    h.sum,
			total:  h.total,
		}
	}
	dbErrors := m.dbErrors
	m.mu.Unlock()

	var b strings.Builder
	b.WriteString("# HELP pjgoiot_http_requests_total HTTP 请求总数，按 method 与 status 分组。\n")
	b.WriteString("# TYPE pjgoiot_http_requests_total counter\n")
	keys := make([]requestKey, 0, len(requests))
	for k := range requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].method != keys[j].method {
			return keys[i].method < keys[j].method
		}
		return keys[i].status < keys[j].status
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "pjgoiot_http_requests_total{method=%q,status=%q} %d\n",
			escapeLabel(k.method), strconv.Itoa(k.status), requests[k])
	}

	b.WriteString("# HELP pjgoiot_http_request_duration_seconds HTTP 请求耗时直方图。\n")
	b.WriteString("# TYPE pjgoiot_http_request_duration_seconds histogram\n")
	methods := make([]string, 0, len(durations))
	for method := range durations {
		methods = append(methods, method)
	}
	sort.Strings(methods)
	for _, method := range methods {
		h := durations[method]
		for i, le := range latencyBuckets {
			fmt.Fprintf(&b, "pjgoiot_http_request_duration_seconds_bucket{method=%q,le=%q} %d\n",
				escapeLabel(method), strconv.FormatFloat(le, 'f', -1, 64), h.counts[i])
		}
		fmt.Fprintf(&b, "pjgoiot_http_request_duration_seconds_bucket{method=%q,le=\"+Inf\"} %d\n",
			escapeLabel(method), h.total)
		fmt.Fprintf(&b, "pjgoiot_http_request_duration_seconds_sum{method=%q} %s\n",
			escapeLabel(method), strconv.FormatFloat(h.sum, 'f', 6, 64))
		fmt.Fprintf(&b, "pjgoiot_http_request_duration_seconds_count{method=%q} %d\n",
			escapeLabel(method), h.total)
	}

	b.WriteString("# HELP pjgoiot_db_errors_total 数据库/存储层错误总数。\n")
	b.WriteString("# TYPE pjgoiot_db_errors_total counter\n")
	fmt.Fprintf(&b, "pjgoiot_db_errors_total %d\n", dbErrors)

	b.WriteString("# HELP pjgoiot_users_approx 用户行数近似值（pg_class.reltuples）。\n")
	b.WriteString("# TYPE pjgoiot_users_approx gauge\n")
	fmt.Fprintf(&b, "pjgoiot_users_approx %d\n", counts.Users)
	b.WriteString("# HELP pjgoiot_devices_approx 设备行数近似值（pg_class.reltuples）。\n")
	b.WriteString("# TYPE pjgoiot_devices_approx gauge\n")
	fmt.Fprintf(&b, "pjgoiot_devices_approx %d\n", counts.Devices)
	b.WriteString("# HELP pjgoiot_readings_approx 读数行数近似值（pg_class.reltuples）。\n")
	b.WriteString("# TYPE pjgoiot_readings_approx gauge\n")
	fmt.Fprintf(&b, "pjgoiot_readings_approx %d\n", counts.Readings)
	return b.String()
}

func escapeLabel(v string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n").Replace(v)
}
