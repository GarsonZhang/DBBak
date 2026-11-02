// Package security 提供凭据加密、密码哈希和会话令牌能力。
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// KeyStore 从独立密钥文件加载 AES-256 主密钥。
type KeyStore struct {
	aead cipher.AEAD
	path string
}

// OpenKeyStore 加载或首次生成主密钥；密钥不会写入 SQLite。
func OpenKeyStore(path string) (*KeyStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建密钥目录: %w", err)
	}
	key, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		key = make([]byte, 32)
		if _, err = io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("生成主密钥: %w", err)
		}
		if err = os.WriteFile(path, key, 0o600); err != nil {
			return nil, fmt.Errorf("保存主密钥: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("读取主密钥: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("主密钥长度必须为 32 字节")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &KeyStore{aead: aead, path: path}, nil
}

// Encrypt 使用随机 nonce 加密单个秘密。
func (k *KeyStore) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := k.aead.Seal(nil, nonce, []byte(plaintext), nil)
	return base64.RawStdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

// Decrypt 解密秘密；调用方不得将返回值写入日志或响应。
func (k *KeyStore) Decrypt(encoded string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(raw) < k.aead.NonceSize() {
		return "", fmt.Errorf("凭据密文格式无效")
	}
	nonce := raw[:k.aead.NonceSize()]
	plain, err := k.aead.Open(nil, nonce, raw[k.aead.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("凭据解密失败: %w", err)
	}
	return string(plain), nil
}

// KeyPath 用于部署文档和启动诊断，不返回密钥内容。
func (k *KeyStore) KeyPath() string { return k.path }
