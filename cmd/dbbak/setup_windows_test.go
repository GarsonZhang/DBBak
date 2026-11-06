//go:build windows

package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewInstallPlanRejectsUnsafeLayout 校验安装路径的下限保护：卷根目录、程序与数据同目录都必须拒绝。
func TestNewInstallPlanRejectsUnsafeLayout(t *testing.T) {
	volume := filepath.VolumeName(os.Getenv("ProgramData")) + `\`
	if _, err := newInstallPlan(volume, `C:\ProgramData\DBBak`, "127.0.0.1:8080", virtualServiceAccount); err == nil {
		t.Fatal("安装目录为卷根目录时应拒绝")
	}
	if _, err := newInstallPlan(`C:\DBBak`, `C:\DBBak`, "127.0.0.1:8080", virtualServiceAccount); err == nil {
		t.Fatal("程序目录与数据目录相同时应拒绝")
	}
	if _, err := newInstallPlan(`C:\DBBak`, `C:\ProgramData\DBBak`, "127.0.0.1", virtualServiceAccount); err == nil {
		t.Fatal("监听地址缺少端口时应拒绝")
	}
	if _, err := newInstallPlan(`C:\DBBak`, `C:\ProgramData\DBBak`, "127.0.0.1:8080", `DOMAIN\someone`); err == nil {
		t.Fatal("不支持的账号应拒绝，避免静默使用无密码账号")
	}
}

func TestNewInstallPlanDefaults(t *testing.T) {
	plan, err := newInstallPlan("", "", "127.0.0.1:8080", virtualServiceAccount)
	if err != nil {
		t.Fatalf("默认计划应有效: %v", err)
	}
	if !strings.HasSuffix(strings.ToLower(plan.ExePath), `program files\dbbak\dbbak.exe`) {
		t.Fatalf("默认程序路径异常: %s", plan.ExePath)
	}
	if plan.DBPath != filepath.Join(plan.DataRoot, "dbbak.db") {
		t.Fatalf("元数据库路径异常: %s", plan.DBPath)
	}
	args := strings.Join(plan.serviceArgs(), " ")
	for _, want := range []string{"serve", "--data-dir", "--backup-root", "--master-key-file"} {
		if !strings.Contains(args, want) {
			t.Fatalf("服务命令行缺少 %q: %s", want, args)
		}
	}
}

// TestRandomPasswordShape 确认随机密码长度与可用字符，安装输出依赖它可直接登录。
func TestRandomPasswordShape(t *testing.T) {
	password, err := randomPassword(24)
	if err != nil {
		t.Fatalf("生成随机密码失败: %v", err)
	}
	if len(password) != 24 {
		t.Fatalf("密码长度 = %d，期望 24", len(password))
	}
	for _, char := range password {
		if !strings.ContainsRune("abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789", char) {
			t.Fatalf("密码包含非法字符 %q", char)
		}
	}
}

// TestExeFromImagePath 覆盖服务 ImagePath 的两种写法。
//
// 这里曾按第一个空格切分，路径含空格时会截出 "C:\Program"，
// 导致升级时把自家服务误判成"别的程序的服务"而拒绝接管。
func TestExeFromImagePath(t *testing.T) {
	cases := map[string]string{
		`"C:\Program Files\DBak\dbbak.exe" serve --addr 127.0.0.1:8080`: `C:\Program Files\DBak\dbbak.exe`,
		`"C:\Program Files\DBBak\dbbak.exe"`:                            `C:\Program Files\DBBak\dbbak.exe`,
		`C:\DBBak\dbbak.exe serve --addr 127.0.0.1:8080`:               `C:\DBBak\dbbak.exe`,
		`C:\Program Files\DBBak\dbbak.exe`:                             `C:\Program Files\DBBak\dbbak.exe`,
		``:                                                             ``,
	}
	for input, want := range cases {
		if got := exeFromImagePath(input); got != want {
			t.Fatalf("exeFromImagePath(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// TestEnsureListenAddrFreeRejectsBusyPort 确认端口被占时安装前就能报错，
// 而不是等服务启动后靠 90 秒 readiness 超时才发现。
func TestEnsureListenAddrFreeRejectsBusyPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("无法占用临时端口: %v", err)
	}
	defer listener.Close()
	addr := listener.Addr().String()
	if err := ensureListenAddrFree(addr); err == nil {
		t.Fatalf("端口 %s 已被占用，应返回错误", addr)
	} else if !strings.Contains(err.Error(), addr) {
		t.Fatalf("错误信息应包含监听地址，实际: %v", err)
	}
}

// TestEnsureListenAddrFreeAcceptsFreePort 确认空闲端口不会被误拦。
func TestEnsureListenAddrFreeAcceptsFreePort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("无法占用临时端口: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()
	if err := ensureListenAddrFree(addr); err != nil {
		t.Fatalf("端口 %s 已释放，不应报错: %v", addr, err)
	}
}

// TestRotatingWriterKeepsBoundedFiles 校验服务日志按大小轮转且份数有上限。
func TestRotatingWriterKeepsBoundedFiles(t *testing.T) {
	dir := t.TempDir()
	writer, err := newRotatingWriter(filepath.Join(dir, "dbbak.log"), 1024, 3)
	if err != nil {
		t.Fatalf("创建日志写入器失败: %v", err)
	}
	defer writer.Close()
	chunk := []byte(strings.Repeat("x", 512) + "\n")
	for i := 0; i < 12; i++ {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatalf("写入日志失败: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取日志目录失败: %v", err)
	}
	if len(entries) > 4 {
		t.Fatalf("日志文件数 = %d，超过 1+3 上限", len(entries))
	}
	info, err := os.Stat(filepath.Join(dir, "dbbak.log"))
	if err != nil {
		t.Fatalf("当前日志不存在: %v", err)
	}
	if info.Size() < 512 {
		t.Fatalf("当前日志大小 = %d，轮转后应重新写入", info.Size())
	}
}
