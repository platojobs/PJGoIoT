// Package auth 负责不透明 token 的生成与哈希。
// 数据库只保存 SHA-256 哈希，即使库泄露也无法还原 token。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

const tokenPrefix = "pjiot_"

// NewToken 生成一个 32 字节随机 token（base64url 编码），并返回可入库的哈希。
func NewToken() (token string, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("生成随机 token 失败: %w", err)
	}
	token = tokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, Hash(token), nil
}

// Hash 计算 token 的 SHA-256 十六进制哈希。
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
