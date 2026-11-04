// Package validate 提供请求参数校验与规范化工具。
package validate

import (
	"errors"
	"fmt"
	"net"
)

// ErrInvalidMAC 表示 MAC 地址格式不合法。
var ErrInvalidMAC = errors.New("非法的 MAC 地址")

// NormalizeMAC 校验并规范化 MAC 地址为小写冒号分隔形式。
// 蓝牙设备地址固定为 6 字节，因此拒绝 8 字节 EUI-64 等形式。
func NormalizeMAC(raw string) (string, error) {
	if raw == "" {
		return "", ErrInvalidMAC
	}
	hw, err := net.ParseMAC(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrInvalidMAC, raw)
	}
	if len(hw) != 6 {
		return "", fmt.Errorf("%w: 需要 6 字节蓝牙地址", ErrInvalidMAC)
	}
	return hw.String(), nil
}
