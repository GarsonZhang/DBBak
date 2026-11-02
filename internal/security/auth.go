package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword 使用 bcrypt 哈希管理员密码，拒绝空密码和超过 72 字节的输入。
func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", fmt.Errorf("密码至少 12 字节")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

// VerifyPassword 比较 bcrypt 哈希，不记录密码内容。
func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// NewToken 生成 256 位随机会话令牌。
func NewToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// TokenHash 使数据库只保存会话令牌摘要。
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}
