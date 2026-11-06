// Package auth 提供 token 哈希与运营侧凭据校验等鉴权原语。
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"time"
)

// AdminKeySet 表示运营入口（X-Admin-Token）的「当前密钥 + 上一把密钥」集合。
//
// 轮换语义：
//   - current 始终有效；
//   - previous 仅在 rotatedAt + window 之前有效，窗口结束后自动淘汰；
//   - 任一时刻只要 current 或 previous 命中即通过，因此轮换期间新旧调用方都不中断；
//   - 两者都不命中（未知密钥）一律拒绝。
//
// 比较前把两侧各自 SHA-256 成等长摘要，再对两把密钥都执行
// subtle.ConstantTimeCompare，避免按长度提前返回或按命中位置泄露差异。
type AdminKeySet struct {
	enabled         bool
	current         [sha256.Size]byte
	previous        [sha256.Size]byte
	hasPrevious     bool
	previousExpires time.Time
}

// NewAdminKeySet 构建密钥集合。previous 为空、rotatedAt 为零或 window<=0 时
// 只保留当前密钥；这些非法组合应在配置加载阶段被拒绝，这里的降级仅用于
// 测试或直接构造 Config 的场景。
func NewAdminKeySet(current, previous string, rotatedAt time.Time, window time.Duration) AdminKeySet {
	ks := AdminKeySet{enabled: current != "", current: sha256.Sum256([]byte(current))}
	if previous != "" && !rotatedAt.IsZero() && window > 0 {
		ks.hasPrevious = true
		ks.previous = sha256.Sum256([]byte(previous))
		ks.previousExpires = rotatedAt.Add(window)
	}
	return ks
}

// Enabled 表示是否配置了当前密钥；未配置时运营入口整体按 404 处理。
func (k AdminKeySet) Enabled() bool { return k.enabled }

// Accept 判断请求携带的凭据是否应被接受。now 由调用方注入便于测试过期窗口。
func (k AdminKeySet) Accept(provided string, now time.Time) bool {
	if !k.enabled {
		return false
	}
	sum := sha256.Sum256([]byte(provided))
	currentMatch := subtle.ConstantTimeCompare(sum[:], k.current[:]) == 1
	// 无论是否配置上一把密钥都执行比较，保持两条路径的耗时一致。
	previousMatch := subtle.ConstantTimeCompare(sum[:], k.previous[:]) == 1
	previousValid := k.hasPrevious && now.Before(k.previousExpires) && previousMatch
	return currentMatch || previousValid
}
