// reading.go 提供测量读数的校验与规范化，HTTP 层与存储层共用。
package validate

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// 读数限制。
const (
	// MaxReadingsPerPush 限制单次 push 的读数条数。
	MaxReadingsPerPush = 500
	// MaxMetricLength 限制指标名长度。
	MaxMetricLength = 32
	// MaxUnitLength 限制单位长度（允许空串）。
	MaxUnitLength = 16
	// MaxRecordedAtSkew 允许客户端测量时间超前服务端的最大偏差。
	MaxRecordedAtSkew = 5 * time.Minute
	// MaxSeriesSpan 限制分桶序列的时间跨度（90 天，与保留期一致）。
	MaxSeriesSpan = 90 * 24 * time.Hour
	// MaxSeriesBuckets 限制单次序列返回的桶数量。
	MaxSeriesBuckets = 500
)

// seriesBucketDurations 是 series 接口支持的桶枚举。
var seriesBucketDurations = map[string]time.Duration{
	"1m": time.Minute,
	"1h": time.Hour,
	"1d": 24 * time.Hour,
}

// seriesOrigin 是所有时间桶的 UTC 对齐原点（2000-01-01T00:00:00Z）。
// 1m/1h/1d 都是该原点的整数倍，因此 1h 对齐整点、1d 对齐 UTC 零点；
// 使用绝对时间计算，不受夏令时（DST）影响。
var seriesOrigin = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// metricPattern 约束小写 snake_case：小写字母开头，只含小写字母、数字与下划线。
var metricPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var (
	// ErrInvalidMetric 表示 metric 不符合小写 snake_case 约定。
	ErrInvalidMetric = errors.New("非法的 metric")
	// ErrInvalidValue 表示 value 不是有限浮点数。
	ErrInvalidValue = errors.New("非法的 value")
	// ErrInvalidUnit 表示 unit 超长。
	ErrInvalidUnit = errors.New("非法的 unit")
	// ErrRecordedAtTooFar 表示 recordedAt 超前服务端过多。
	ErrRecordedAtTooFar = errors.New("recordedAt 超前服务端时间")
	// ErrInvalidBucket 表示 bucket 不在 1m/1h/1d 之中。
	ErrInvalidBucket = errors.New("非法的 bucket")
	// ErrInvalidSeriesRange 表示 from/to 缺失、to<=from 或跨度超过 90 天。
	ErrInvalidSeriesRange = errors.New("非法的序列时间范围")
	// ErrTooManyBuckets 表示桶数量超过 500。
	ErrTooManyBuckets = errors.New("分桶数量超过上限")
)

// NormalizeMetric 校验并返回指标名；不允许空值。
func NormalizeMetric(raw string) (string, error) {
	metric := strings.TrimSpace(raw)
	if metric == "" {
		return "", fmt.Errorf("%w: 不能为空", ErrInvalidMetric)
	}
	if len([]rune(metric)) > MaxMetricLength {
		return "", fmt.Errorf("%w: 长度不能超过 %d", ErrInvalidMetric, MaxMetricLength)
	}
	if !metricPattern.MatchString(metric) {
		return "", fmt.Errorf("%w: 必须是小写 snake_case（小写字母、数字、下划线，字母开头）", ErrInvalidMetric)
	}
	return metric, nil
}

// NormalizeUnit 校验并返回单位；空串表示无量纲。
func NormalizeUnit(raw string) (string, error) {
	unit := strings.TrimSpace(raw)
	if len([]rune(unit)) > MaxUnitLength {
		return "", fmt.Errorf("%w: 长度不能超过 %d", ErrInvalidUnit, MaxUnitLength)
	}
	return unit, nil
}

// CheckFiniteValue 拒绝 NaN 与正负无穷。
func CheckFiniteValue(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("%w: 必须是有限浮点数", ErrInvalidValue)
	}
	return nil
}

// CheckRecordedAt 校验测量时间：必须非零，且不得超前服务端 now 超过 5 分钟。
func CheckRecordedAt(recordedAt time.Time, now time.Time) error {
	if recordedAt.IsZero() {
		return fmt.Errorf("%w: 不能为空", ErrRecordedAtTooFar)
	}
	if recordedAt.After(now.Add(MaxRecordedAtSkew)) {
		return fmt.Errorf("%w: 最多允许超前 %s", ErrRecordedAtTooFar, MaxRecordedAtSkew)
	}
	return nil
}

// NormalizeBucket 校验分桶粒度并返回对应的时长。
func NormalizeBucket(raw string) (time.Duration, error) {
	name := strings.TrimSpace(raw)
	d, ok := seriesBucketDurations[name]
	if !ok {
		return 0, fmt.Errorf("%w: 只能是 1m、1h 或 1d", ErrInvalidBucket)
	}
	return d, nil
}

// BucketStart 返回 at 在 UTC 对齐网格上的桶起点。
// 1h 对齐整点、1d 对齐 UTC 零点，且不受本地时区/DST 影响。
func BucketStart(at time.Time, size time.Duration) time.Time {
	offset := at.UTC().Sub(seriesOrigin)
	idx := offset / size
	if offset < 0 && offset%size != 0 {
		idx--
	}
	return seriesOrigin.Add(idx * size).UTC()
}

// ValidateSeriesRange 校验 from/to 与分桶数量：
// to 必须晚于 from、跨度不超过 90 天、覆盖到的桶数量不超过 500。
func ValidateSeriesRange(from, to time.Time, size time.Duration) (int, error) {
	if from.IsZero() || to.IsZero() {
		return 0, fmt.Errorf("%w: from 与 to 必填", ErrInvalidSeriesRange)
	}
	if !to.After(from) {
		return 0, fmt.Errorf("%w: to 必须晚于 from", ErrInvalidSeriesRange)
	}
	if to.Sub(from) > MaxSeriesSpan {
		return 0, fmt.Errorf("%w: 跨度不能超过 90 天", ErrInvalidSeriesRange)
	}
	first := BucketStart(from, size)
	last := BucketStart(to, size)
	count := int(last.Sub(first)/size) + 1
	if count > MaxSeriesBuckets {
		return count, fmt.Errorf("%w: 当前参数会产生 %d 个桶，上限 %d；请使用更粗的 bucket 或缩短时间范围",
			ErrTooManyBuckets, count, MaxSeriesBuckets)
	}
	return count, nil
}
