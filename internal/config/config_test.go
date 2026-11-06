package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 未自带工具（开发、便携运行）时必须保持原有行为：回落到命令名，由 PATH 解析。
func TestLoadFallsBackToCommandName(t *testing.T) {
	t.Setenv("DBBAK_MYSQL", "")
	t.Setenv("DBBAK_MYSQLDUMP", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MySQLPath != "mysql" || cfg.MySQLDumpPath != "mysqldump" {
		t.Fatalf("无自带工具时应回落到命令名，实际 %q / %q", cfg.MySQLPath, cfg.MySQLDumpPath)
	}
}

// 显式配置优先于自带工具：运维在目标机指定了客户端版本时不能被安装包覆盖。
func TestLoadExplicitEnvWins(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom-mysqldump.exe")
	t.Setenv("DBBAK_MYSQLDUMP", custom)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MySQLDumpPath != custom {
		t.Fatalf("显式配置应优先，实际 %q", cfg.MySQLDumpPath)
	}
}

// ApplySearchPath 只影响本进程 PATH，且必须在原有 PATH 前追加真实存在的工具目录。
func TestApplySearchPathPrependsBundledDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	original := "C:" + string(os.PathSeparator) + `Windows\system32`
	t.Setenv("PATH", original)
	Config{}.ApplySearchPath()
	path := os.Getenv("PATH")
	expected := filepath.Join(dir, "tools")
	if len(path) < len(expected) || path[:len(expected)] != expected {
		t.Fatalf("tools 目录应位于 PATH 首位，实际 %q", path)
	}
	if !strings.HasSuffix(path, original) {
		t.Fatalf("原有 PATH 必须保留，实际 %q", path)
	}
}
