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
)

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
