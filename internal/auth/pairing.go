package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// pairingAlphabet 去掉易混字符 0/O/1/I，仅保留 32 个字符，8 位约 40 bit 熵。
const pairingAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// PairingCodeLength 是一次性配对码的长度。
const PairingCodeLength = 8

// NewPairingCode 生成 8 位大写字母数字配对码（不含 0/O/1/I）。
func NewPairingCode() (string, error) {
	buf := make([]byte, PairingCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, PairingCodeLength)
	for i, b := range buf {
		out[i] = pairingAlphabet[int(b)%len(pairingAlphabet)]
	}
	return string(out), nil
}

// NormalizePairingCode 去掉空白并转大写；格式非法时返回空串。
func NormalizePairingCode(raw string) string {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if len(code) != PairingCodeLength {
		return ""
	}
	for _, r := range code {
		if !strings.ContainsRune(pairingAlphabet, r) {
			return ""
		}
	}
	return code
}

// PairingCodeHash 返回配对码的 SHA-256 十六进制哈希，作为数据库主键。
func PairingCodeHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// EqualPairingCodeHash 以常量时间比较两个哈希，避免时序侧信道。
func EqualPairingCodeHash(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// MaskPairingCode 仅保留前 2 位用于排障日志，绝不输出完整码。
func MaskPairingCode(code string) string {
	if len(code) <= 2 {
		return "**"
	}
	return code[:2] + "******"
}
