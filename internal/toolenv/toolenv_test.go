package toolenv

import (
	"os"
	"path/filepath"
	"testing"
)

// 发行形态的核心约束：自带工具目录必须优先于命令名，否则客户机仍会报 TOOL_MISSING。
func TestResolvePrefersBundledDir(t *testing.T) {
	dir := t.TempDir()
	bundled := filepath.Join(dir, "tools")
	if err := os.MkdirAll(bundled, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(bundled, "mysqldump.exe")
	if err := os.WriteFile(target, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Resolve("mysqldump.exe", "mysqldump", bundled); got != target {
		t.Fatalf("应返回自带工具路径 %s，实际 %q", target, got)
	}
	if got := Resolve("mysqldump.exe", "mysqldump", filepath.Join(dir, "missing")); got != "mysqldump" {
		t.Fatalf("未命中时应回落到命令名，实际 %q", got)
	}
}

// 目录存在但文件缺失（例如只解压了一半）不能算命中，否则会给出一个不存在的路径。
func TestResolveIgnoresDirectory(t *testing.T) {
	dir := t.TempDir()
	bundled := filepath.Join(dir, "tools")
	if err := os.MkdirAll(filepath.Join(bundled, "mysql.exe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Resolve("mysql.exe", "mysql", bundled); got != "mysql" {
		t.Fatalf("同名目录不应视为工具，实际 %q", got)
	}
}
