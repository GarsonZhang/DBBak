package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"dbbak/internal/api"
	"dbbak/internal/backup"
	"dbbak/internal/config"
	"dbbak/internal/metadata"
	"dbbak/internal/security"
)

// serveOptions 是 serve 的路径与监听覆盖项。
//
// 存在的原因是 Windows 服务没有 shell，只能把固定路径写进服务的 ImagePath 命令行；
// 优先级为“命令行参数 > DBBAK_* 环境变量 > 内置默认值”，三者都落到同一份 config.Load()。
type serveOptions struct {
	addr          string
	dataDir       string
	backupRoot    string
	masterKeyFile string
}

func parseServeFlags(args []string) (serveOptions, error) {
	var opts serveOptions
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.addr, "addr", "", "监听地址，覆盖 DBBAK_ADDR")
	fs.StringVar(&opts.dataDir, "data-dir", "", "数据目录，覆盖 DBBAK_DATA_DIR")
	fs.StringVar(&opts.backupRoot, "backup-root", "", "备份根目录，覆盖 DBBAK_BACKUP_ROOT")
	fs.StringVar(&opts.masterKeyFile, "master-key-file", "", "主密钥文件，覆盖 DBBAK_MASTER_KEY_FILE")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("serve 不接受多余参数: %s", strings.Join(fs.Args(), " "))
	}
	return opts, nil
}

// apply 把非空参数写入进程环境，使 config.Load() 无需感知命令行来源。
func (o serveOptions) apply() {
	for _, pair := range [][2]string{
		{"DBBAK_ADDR", o.addr},
		{"DBBAK_DATA_DIR", o.dataDir},
		{"DBBAK_BACKUP_ROOT", o.backupRoot},
		{"DBBAK_MASTER_KEY_FILE", o.masterKeyFile},
	} {
		if pair[1] != "" {
			_ = os.Setenv(pair[0], pair[1])
		}
	}
}

// serveFromArgs 是前台运行入口：Ctrl+C、关闭控制台或 SIGTERM 都会触发优雅关闭。
func serveFromArgs(args []string) error {
	opts, err := parseServeFlags(args)
	if err != nil {
		return err
	}
	opts.apply()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveWithConfig(ctx, cfg)
}

// bootstrapAdmin 在元数据库还没有任何用户时，用 DBBAK_ADMIN_PASSWORD 建首个管理员。
//
// 存在的理由是 Docker/Compose 场景：镜像的 CMD 只有 serve，用户没有方便的方式再执行
// 一次 init-admin，导致「容器起来了但登录不了」。行为边界：
//   - 未设置 DBBAK_ADMIN_PASSWORD：跳过，不写 users 表，与旧版本一致；
//   - 已有用户：跳过，不比较也不覆盖密码，因此环境变量可以长期保留；
//   - 密码不满足规则：返回错误并终止启动，而不是建一个弱口令账号。
//
// 日志只输出用户名和「已创建/已存在」，绝不输出密码。
func bootstrapAdmin(ctx context.Context, store *metadata.Store, cfg config.Config) error {
	if cfg.BootstrapAdminPassword == "" {
		return nil
	}
	created, err := ensureAdminIn(ctx, store, cfg.BootstrapAdminUser, cfg.BootstrapAdminPassword)
	if err != nil {
		return fmt.Errorf("按 DBBAK_ADMIN_PASSWORD 初始化管理员失败: %w", err)
	}
	if created {
		log.Printf("已创建初始管理员 %s（来源 DBBAK_ADMIN_PASSWORD）；建议登录后修改密码并从环境变量中移除", cfg.BootstrapAdminUser)
		return nil
	}
	// 已有账号时不做任何处理：这正是「重启不会重置密码」的保证。
	log.Printf("管理员已存在，忽略 DBBAK_ADMIN_PASSWORD")
	return nil
}

// serveWithConfig 在给定 ctx 下运行 HTTP 服务、调度器与本地执行器；ctx 取消即优雅关闭。
//
// 前台服务与 Windows 服务共用这一实现，避免两套生命周期在停止语义上分叉。
func serveWithConfig(ctx context.Context, cfg config.Config) error {
	// 自带工具目录必须先进入 PATH：客户端工具与其同目录 DLL、sqlcmd 依赖的 OLE DB 组件
	// 都按 PATH 解析，服务账号默认 PATH 里通常没有这些目录。
	cfg.ApplySearchPath()
	if err := cfg.EnsureDirectories(); err != nil {
		return err
	}
	store, err := metadata.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	// 容器化部署的首次启动：只用环境变量建一次号，之后完全忽略。
	// 失败必须让进程退出而不是继续启动：没有账号的控制台登录不了，静默成功更糟。
	if err := bootstrapAdmin(ctx, store, cfg); err != nil {
		return err
	}
	keys, err := security.OpenKeyStore(cfg.MasterKeyPath)
	if err != nil {
		return err
	}
	adapters := backup.NewRegistry(
		// 注册顺序决定 MySQL 的默认路线：逻辑全量在前，物理路线必须由任务显式选择。
		backup.MySQLClient{MySQLPath: cfg.MySQLPath, MySQLDumpPath: cfg.MySQLDumpPath},
		backup.MySQLPhysicalClient{XtraBackupPath: cfg.MySQLXtraBackupPath, MySQLPath: cfg.MySQLPath, BinlogPath: cfg.MySQLBinlogPath},
		backup.PostgreSQLClient{PSQLPath: cfg.PostgreSQLPath, BaseBackupPath: cfg.PGBaseBackupPath},
		backup.SQLServerClient{SQLCMDPath: cfg.SQLCMDPath},
	)
	runner := backup.NewRunner(store, adapters, keys, cfg.BackupTimeout)
	if err = runner.Start(ctx); err != nil {
		return err
	}
	cleanup := backup.NewCleanupService(store, keys)
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
		log.Printf("DBBak %s 正在监听 http://%s", version, cfg.Addr)
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
		return serveErr
	}
}
