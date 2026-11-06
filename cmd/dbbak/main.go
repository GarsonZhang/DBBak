// dbbak 是 DBBak 单实例控制服务与本地执行器入口。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	// 内嵌 IANA 时区库。发行机的 Windows 没有 zoneinfo 目录，Go 只在
	// GOROOT/lib/time/zoneinfo.zip 存在时才能回退；装过 Go 的开发机因此
	// 看起来正常，而真实部署机上 time.LoadLocation 会直接失败并报
	// “无效时区”。这个空白导入是任务时区能在发行环境生效的前提。
	_ "time/tzdata"

	"dbbak/internal/config"
	"dbbak/internal/metadata"
	"dbbak/internal/security"
)

const version = "0.2.0-phase5"

func main() {
	os.Exit(run(os.Args[1:]))
}

// run 是命令分派入口，返回进程退出码。
//
// 服务上下文的判断必须先于子命令解析：SCM 启动服务时只会执行 ImagePath 里的命令行，
// 不会附带任何交互式参数，误判会直接让服务启动失败。
func run(args []string) int {
	// 安装日志需要在写完成败结论之后才关闭：图形安装器读的就是这份文件。
	defer closeSetupLog()
	if isWindowsService() {
		if err := runWindowsService(args); err != nil {
			log.Printf("错误: %v", err)
			return 1
		}
		return 0
	}
	// 无参数运行要区分上下文：双击安装包时没有任何命令行参数，
	// 直接打印用法会让窗口一闪而过、用户以为没装上。
	if len(args) == 0 {
		return runWithoutArgs()
	}
	var err error
	switch args[0] {
	case "serve":
		err = serveFromArgs(args[1:])
	case "init-admin":
		err = initAdmin()
	case "version":
		fmt.Println(version)
		return 0
	case "setup":
		err = setupFromArgs(args[1:])
	case "uninstall":
		err = uninstallFromArgs(args[1:])
	case "service":
		err = serviceFromArgs(args[1:])
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		usage()
		return 2
	}
	if err != nil {
		log.Printf("错误: %v", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Println("用法:")
	fmt.Println("  dbbak setup [选项]        安装为 Windows 服务并启动（需管理员权限）")
	fmt.Println("  dbbak uninstall [选项]    停止并移除服务与程序，默认保留数据")
	fmt.Println("  dbbak service status|start|stop|restart")
	fmt.Println("  dbbak serve [--addr ADDR] [--data-dir DIR] [--backup-root DIR] [--master-key-file FILE]")
	fmt.Println("  dbbak init-admin          初始化管理员（密码来自 DBBAK_ADMIN_PASSWORD）")
	fmt.Println("  dbbak version")
	fmt.Println("运行配置通过 DBBAK_* 环境变量提供；命令行参数优先于环境变量。")
	fmt.Println("Windows 上双击安装包等同于 dbbak setup，会显示安装计划、请求管理员权限后安装并注册服务。")
}

func initAdmin() error {
	password := os.Getenv("DBBAK_ADMIN_PASSWORD")
	if password == "" {
		return fmt.Errorf("请通过 DBBAK_ADMIN_PASSWORD 提供至少 %d 个字符的初始密码；密码不会写入命令行或日志", security.MinPasswordLength)
	}
	username := strings.TrimSpace(os.Getenv("DBBAK_ADMIN_USERNAME"))
	if username == "" {
		username = "admin"
	}
	if len(username) > 64 || strings.ContainsAny(username, "\r\n\x00") {
		return fmt.Errorf("管理员用户名无效")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	created, err := ensureInitialAdmin(ctx, cfg, username, password)
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("管理员已初始化，拒绝覆盖现有账号")
	}
	fmt.Printf("管理员 %s 已创建\n", username)
	return nil
}

// ensureInitialAdmin 在元数据库为空时创建首个管理员，返回是否实际创建。
//
// 与 initAdmin 的差别是它允许“已存在”，供安装程序在重复安装（数据保留）时安全复用；
// 主密钥与目录由调用方负责准备。
func ensureInitialAdmin(ctx context.Context, cfg config.Config, username, password string) (bool, error) {
	if err := cfg.EnsureDirectories(); err != nil {
		return false, err
	}
	store, err := metadata.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return false, err
	}
	defer store.Close()
	exists, err := store.HasUsers(ctx)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return false, err
	}
	if _, err = store.CreateUser(ctx, username, hash); err != nil {
		return false, err
	}
	return true, nil
}
