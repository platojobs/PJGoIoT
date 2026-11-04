// Package validate 提供请求参数校验与规范化工具。
package validate

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// 平台取值。
const (
	PlatformAndroid = "android"
	PlatformIOS     = "ios"
)

// MaxExternalIDLength 限制 externalId 长度，避免异常客户端写入超长键。
const MaxExternalIDLength = 128

var (
	// ErrInvalidPlatform 表示 platform 不是 android/ios。
	ErrInvalidPlatform = errors.New("非法的 platform")
	// ErrInvalidExternalID 表示 externalId 不合法。
	ErrInvalidExternalID = errors.New("非法的 externalId")
)

// NormalizePlatform 规范化平台值。空值按 Android 兼容旧客户端。
func NormalizePlatform(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", PlatformAndroid:
		return PlatformAndroid, nil
	case PlatformIOS:
		return PlatformIOS, nil
	default:
		return "", fmt.Errorf("%w: %s", ErrInvalidPlatform, raw)
	}
}

// ResolveDeviceKey 计算设备唯一键与 mac 元数据。
//
// Android：externalId 为规范化大写 MAC；缺省时由 mac 派生，mac 元数据自动填充为小写。
// iOS：externalId 为 CBPeripheral.identifier（UUID）；mac 可为空，仅作为可选元数据。
//
// 返回值依次为 platform、externalId、mac 元数据。
func ResolveDeviceKey(platform, externalID, mac string) (string, string, string, error) {
	p, err := NormalizePlatform(platform)
	if err != nil {
		return "", "", "", err
	}

	macMeta := ""
	if strings.TrimSpace(mac) != "" {
		m, err := NormalizeMAC(mac)
		if err != nil {
			return "", "", "", err
		}
		macMeta = m
	}

	if p == PlatformAndroid {
		ext := strings.TrimSpace(externalID)
		if ext == "" {
			ext = macMeta
		}
		if ext == "" {
			return "", "", "", fmt.Errorf("%w: Android 需要 externalId 或 mac", ErrInvalidExternalID)
		}
		m, err := NormalizeMAC(ext)
		if err != nil {
			return "", "", "", fmt.Errorf("%w: Android externalId 必须是 MAC: %s", ErrInvalidExternalID, externalID)
		}
		ext = strings.ToUpper(m)
		if macMeta != "" && macMeta != strings.ToLower(ext) {
			return "", "", "", fmt.Errorf("%w: mac 与 externalId 不一致", ErrInvalidExternalID)
		}
		return p, ext, strings.ToLower(ext), nil
	}

	ext := strings.TrimSpace(externalID)
	if ext == "" {
		return "", "", "", fmt.Errorf("%w: iOS 必须提供 externalId（CBPeripheral.identifier）", ErrInvalidExternalID)
	}
	if len(ext) > MaxExternalIDLength {
		return "", "", "", fmt.Errorf("%w: externalId 长度不能超过 %d", ErrInvalidExternalID, MaxExternalIDLength)
	}
	id, err := uuid.Parse(ext)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: iOS externalId 必须是 UUID", ErrInvalidExternalID)
	}
	return p, strings.ToUpper(id.String()), macMeta, nil
}

// NormalizeExternalID 仅规范化 externalId，保留 mac 元数据不变。
// 用于按复合键查询等不需要 mac 元数据的场景。
func NormalizeExternalID(platform, externalID string) (string, string, error) {
	p, ext, _, err := ResolveDeviceKey(platform, externalID, "")
	return p, ext, err
}
