package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"dbbak/internal/config"
	"dbbak/internal/metadata"
	"dbbak/internal/security"
)

// TestServeFlagsOverrideEnvironment 校验“参数优先于环境变量”的优先级，服务安装依赖该语义。
func TestServeFlagsOverrideEnvironment(t *testing.T) {
	t.Setenv("DBBAK_ADDR", "127.0.0.1:1111")
	t.Setenv("DBBAK_DATA_DIR", `C:\from-env`)

	opts, err := parseServeFlags([]string{"--addr", "0.0.0.0:9000", "--master-key-file", `C:\key\master.key`})
	if err != nil {
		t.Fatalf("解析参数失败: %v", err)
	}
	opts.apply()

	if got := os.Getenv("DBBAK_ADDR"); got != "0.0.0.0:9000" {
		t.Fatalf("DBBAK_ADDR = %q，期望被参数覆盖", got)
	}
	if got := os.Getenv("DBBAK_DATA_DIR"); got != `C:\from-env` {
		t.Fatalf("DBBAK_DATA_DIR = %q，未传参数时应保留环境变量", got)
	}
	if got := os.Getenv("DBBAK_MASTER_KEY_FILE"); got != `C:\key\master.key` {
		t.Fatalf("DBBAK_MASTER_KEY_FILE = %q，期望写入参数值", got)
	}
}

func TestServeFlagsRejectExtraArguments(t *testing.T) {
	if _, err := parseServeFlags([]string{"extra"}); err == nil {
		t.Fatal("多余参数应报错，避免拼错的参数被静默忽略")
	}
}

// TestEnsureInitialAdminIsIdempotent 覆盖安装程序重复安装时“已有管理员”必须复用而非覆盖的行为。
func TestEnsureInitialAdminIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		DataDir:       dir,
		DatabasePath:  filepath.Join(dir, "dbbak.db"),
		BackupRoot:    filepath.Join(dir, "backups"),
		MasterKeyPath: filepath.Join(dir, "master.key"),
	}
	ctx := context.Background()
	created, err := ensureInitialAdmin(ctx, cfg, "admin", "first-password-1")
	if err != nil {
		t.Fatalf("首次创建管理员失败: %v", err)
	}
	if !created {
		t.Fatal("首次创建应返回 created=true")
	}
	created, err = ensureInitialAdmin(ctx, cfg, "admin", "second-password-2")
	if err != nil {
		t.Fatalf("重复调用不应报错: %v", err)
	}
	if created {
		t.Fatal("已有管理员时不得覆盖账号")
	}
}

// bootstrapAdmin 的三个边界：未配置密码时不建号；首次启动建号；已有账号时不覆盖。
// 第三条是「重启不会重置密码」的保证，必须能挡住把环境变量长期留在 compose 里的场景。
func TestBootstrapAdmin(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Config{
		DataDir:       dir,
		DatabasePath:  filepath.Join(dir, "dbbak.db"),
		BackupRoot:    filepath.Join(dir, "backups"),
		MasterKeyPath: filepath.Join(dir, "master.key"),
	}
	if err := cfg.EnsureDirectories(); err != nil {
		t.Fatal(err)
	}
	store, err := metadata.Open(ctx, cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 未配置密码：不应创建任何账号，也不能报错。
	if err := bootstrapAdmin(ctx, store, cfg); err != nil {
		t.Fatalf("未配置密码时不应报错: %v", err)
	}
	if has, err := store.HasUsers(ctx); err != nil || has {
		t.Fatalf("未配置密码时不应建号: has=%t err=%v", has, err)
	}

	// 首次配置密码：建号，且能用该密码登录。
	cfg.BootstrapAdminUser = "admin"
	cfg.BootstrapAdminPassword = "first-password-1"
	if err := bootstrapAdmin(ctx, store, cfg); err != nil {
		t.Fatalf("首次建号失败: %v", err)
	}
	if _, hash, err := store.Authenticate(ctx, "admin"); err != nil {
		t.Fatalf("建号后应能查到用户: %v", err)
	} else if !security.VerifyPassword(hash, "first-password-1") {
		t.Fatal("建号后的密码校验失败")
	}

	// 已有账号但环境变量改成新密码：必须忽略，旧密码仍然有效。
	cfg.BootstrapAdminPassword = "second-password-2"
	if err := bootstrapAdmin(ctx, store, cfg); err != nil {
		t.Fatalf("已有账号时不应报错: %v", err)
	}
	_, hash, err := store.Authenticate(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if security.VerifyPassword(hash, "second-password-2") {
		t.Fatal("已有账号时不得用环境变量覆盖密码")
	}
	if !security.VerifyPassword(hash, "first-password-1") {
		t.Fatal("原有密码必须保持有效")
	}
}

// 过短的密码必须让启动失败，而不是建一个弱口令账号。
func TestBootstrapAdminRejectsShortPassword(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Config{
		DataDir:       dir,
		DatabasePath:  filepath.Join(dir, "dbbak.db"),
		BackupRoot:    filepath.Join(dir, "backups"),
		MasterKeyPath: filepath.Join(dir, "master.key"),
		BootstrapAdminUser:     "admin",
		BootstrapAdminPassword: "123",
	}
	if err := cfg.EnsureDirectories(); err != nil {
		t.Fatal(err)
	}
	store, err := metadata.Open(ctx, cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := bootstrapAdmin(ctx, store, cfg); err == nil {
		t.Fatal("过短密码应导致启动失败")
	}
	if has, err := store.HasUsers(ctx); err != nil || has {
		t.Fatalf("密码不合规时不得建号: has=%t err=%v", has, err)
	}
}
