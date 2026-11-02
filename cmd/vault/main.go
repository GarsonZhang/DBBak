// vault 是 Vault 单实例控制服务与本地执行器入口。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"vault/internal/api"
	"vault/internal/backup"
	"vault/internal/config"
	"vault/internal/metadata"
	"vault/internal/security"
)

const version = "0.2.0-phase2"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve()
	case "init-admin":
		err = initAdmin()
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Printf("错误: %v", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println("用法: vault init-admin | vault serve | vault version")
	fmt.Println("init-admin 需要 VAULT_ADMIN_PASSWORD；服务配置通过 VAULT_* 环境变量提供")
}

func initAdmin() error {
	password := os.Getenv("VAULT_ADMIN_PASSWORD")
	if password == "" {
		return fmt.Errorf("请通过 VAULT_ADMIN_PASSWORD 提供至少 12 字节的初始密码；密码不会写入命令行或日志")
	}
	username := strings.TrimSpace(os.Getenv("VAULT_ADMIN_USERNAME"))
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
	if err = cfg.EnsureDirectories(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := metadata.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	exists, err := store.HasUsers(ctx)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("管理员已初始化，拒绝覆盖现有账号")
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return err
	}
	user, err := store.CreateUser(ctx, username, hash)
	if err != nil {
		return err
	}
	fmt.Printf("管理员 %s 已创建，用户 ID %s\n", user.Username, user.ID)
	return nil
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err = cfg.EnsureDirectories(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := metadata.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	keys, err := security.OpenKeyStore(cfg.MasterKeyPath)
	if err != nil {
		return err
	}
	adapters := backup.NewRegistry(
		backup.MySQLClient{MySQLPath: cfg.MySQLPath, MySQLDumpPath: cfg.MySQLDumpPath},
		backup.PostgreSQLClient{PSQLPath: cfg.PostgreSQLPath, BaseBackupPath: cfg.PGBaseBackupPath},
		backup.SQLServerClient{SQLCMDPath: cfg.SQLCMDPath},
	)
	runner := backup.NewRunner(store, adapters, keys, cfg.BackupTimeout)
	if err = runner.Start(ctx); err != nil {
		return err
	}
	cleanup := backup.NewCleanupService(store)
	handler, err := api.NewServer(store, keys, adapters, runner, cleanup, cfg)
	if err != nil {
		return err
	}
	scheduler := backup.NewScheduler(store, runner, cleanup, cfg.SchedulerInterval)
	go scheduler.Start(ctx)
	server := &http.Server{
		Addr: cfg.Addr, Handler: handler,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	errChannel := make(chan error, 1)
	go func() {
		log.Printf("Vault %s 正在监听 http://%s", version, cfg.Addr)
		if serveErr := server.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errChannel <- serveErr
			return
		}
		errChannel <- nil
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case serveErr := <-errChannel:
		stop()
		return serveErr
	}
}
