package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"dbbak/internal/config"
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
