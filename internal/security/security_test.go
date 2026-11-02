package security

import (
	"path/filepath"
	"testing"
)

func TestPasswordAndSecretBoundaries(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("短密码应被拒绝")
	}
	hash, err := HashPassword("correct-horse-2026")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "correct-horse-2026") || VerifyPassword(hash, "wrong") {
		t.Fatal("bcrypt 校验结果错误")
	}
	store, err := OpenKeyStore(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := store.Encrypt("database-secret")
	if err != nil {
		t.Fatal(err)
	}
	if ciphertext == "database-secret" {
		t.Fatal("凭据被明文保存")
	}
	plain, err := store.Decrypt(ciphertext)
	if err != nil || plain != "database-secret" {
		t.Fatalf("凭据解密失败: %v", err)
	}
	token, err := NewToken()
	if err != nil || len(token) < 32 || TokenHash(token) == token {
		t.Fatalf("会话令牌或摘要错误: %v", err)
	}
}
