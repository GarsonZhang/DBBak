// Package config 负责 DBBak 服务的环境配置与目录初始化。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dbbak/internal/toolenv"
)

// Config 是单实例服务的运行配置；所有路径均可跨平台指定。
type Config struct {
	Addr              string
	DataDir           string
	DatabasePath      string
	BackupRoot        string
	MasterKeyPath     string
	MySQLPath         string
	MySQLDumpPath     string
	// MySQLXtraBackupPath 与 MySQLBinlogPath 属于 MySQL 物理备份路线；
	// XtraBackup 只有 Linux 二进制，Windows 上必然探测失败。
	MySQLXtraBackupPath string
	MySQLBinlogPath     string
	PostgreSQLPath    string
	PGBaseBackupPath  string
	SQLCMDPath        string
	SessionTTL        time.Duration
	SchedulerInterval time.Duration
	BackupTimeout     time.Duration
	// SQLServerEnabled 由运维在完成真实备份与异名恢复验收后显式开启；
	// 默认关闭，避免未验收的备用路径被静默启用。
	SQLServerEnabled bool
	// MySQLPhysicalEnabled 同理：XtraBackup 物理路线需要同机执行器与版本对齐，
	// 未在目标环境真实验收前不得默认开放，避免用户创建必然失败的任务。
	MySQLPhysicalEnabled bool
	// BootstrapAdminUser 与 BootstrapAdminPassword 用于容器化部署的首次启动自动建号。
	//
	// 只在元数据库里还没有任何用户时生效；已有账号时这两个值完全被忽略，
	// 因此把密码长期留在 compose 环境变量里不会导致「重启即重置密码」。
	// 密码来自 DBBAK_ADMIN_PASSWORD，与 init-admin 共用同一变量名。
	BootstrapAdminUser     string
	BootstrapAdminPassword string
}

// Load 从环境变量读取配置，不生成或返回任何数据库凭据。
//
// 客户端工具路径的优先级是：DBBAK_* 显式配置 > 安装目录 tools 子目录 > 命令名。
// 发行包自带 tools，因此客户机装完即可用，不需要改 PATH 或设环境变量；
// 开发与便携运行没有 tools，仍然回落到 PATH 中的命令名。
func Load() (Config, error) {
	dataDir := env("DBBAK_DATA_DIR", ".dbbak")
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return Config{}, fmt.Errorf("解析数据目录: %w", err)
	}
	// 可执行文件目录与当前工作目录都作为自带工具候选：
	// Windows 服务的当前目录是 system32，用 exe 目录；开发时则从仓库根的 tools 取。
	toolDirs := []string{toolenv.BundledToolsDir(toolenv.ExecutableDir())}
	if wd, wdErr := os.Getwd(); wdErr == nil {
		toolDirs = append(toolDirs, toolenv.BundledToolsDir(wd))
	}
	cfg := Config{
		Addr:                env("DBBAK_ADDR", "127.0.0.1:8080"),
		DataDir:             dataDir,
		DatabasePath:        env("DBBAK_DATABASE", filepath.Join(dataDir, "dbbak.db")),
		BackupRoot:          env("DBBAK_BACKUP_ROOT", filepath.Join(dataDir, "backups")),
		MasterKeyPath:       env("DBBAK_MASTER_KEY_FILE", filepath.Join(dataDir, "master.key")),
		MySQLPath:           toolenv.Resolve("mysql.exe", env("DBBAK_MYSQL", "mysql"), toolDirs...),
		MySQLDumpPath:       toolenv.Resolve("mysqldump.exe", env("DBBAK_MYSQLDUMP", "mysqldump"), toolDirs...),
		MySQLXtraBackupPath: env("DBBAK_XTRABACKUP", "xtrabackup"),
		MySQLBinlogPath:     toolenv.Resolve("mysqlbinlog.exe", env("DBBAK_MYSQLBINLOG", "mysqlbinlog"), toolDirs...),
		PostgreSQLPath:      env("DBBAK_PSQL", "psql"),
		PGBaseBackupPath:    env("DBBAK_PG_BASEBACKUP", "pg_basebackup"),
		SQLCMDPath:          env("DBBAK_SQLCMD", "sqlcmd"),
		SessionTTL:          12 * time.Hour,
		SchedulerInterval:   2 * time.Second,
		// 逻辑备份默认 30 分钟在几十 GB 的库上必然超时，而超时会直接杀死工具进程。
		// 默认改为 6 小时；真正的防挂死依靠运行日志与明确失败，而不是靠过短的硬超时。
		BackupTimeout: 6 * time.Hour,
	}
	if cfg.SessionTTL, err = durationEnv("DBBAK_SESSION_TTL", cfg.SessionTTL); err != nil {
		return Config{}, err
	}
	if cfg.SchedulerInterval, err = durationEnv("DBBAK_SCHEDULER_INTERVAL", cfg.SchedulerInterval); err != nil {
		return Config{}, err
	}
	if cfg.BackupTimeout, err = durationEnv("DBBAK_BACKUP_TIMEOUT", cfg.BackupTimeout); err != nil {
		return Config{}, err
	}
	cfg.SQLServerEnabled = env("DBBAK_SQLSERVER_ENABLED", "") == "1" || strings.EqualFold(env("DBBAK_SQLSERVER_ENABLED", ""), "true")
	cfg.MySQLPhysicalEnabled = env("DBBAK_MYSQL_PHYSICAL_ENABLED", "") == "1" || strings.EqualFold(env("DBBAK_MYSQL_PHYSICAL_ENABLED", ""), "true")
	// 只读取不校验：密码长度、bcrypt 上限等规则留在 security 包，避免两处各写一份。
	// 空值表示不启用自动建号，serve 启动时不会碰 users 表。
	cfg.BootstrapAdminUser = strings.TrimSpace(env("DBBAK_ADMIN_USERNAME", "admin"))
	cfg.BootstrapAdminPassword = os.Getenv("DBBAK_ADMIN_PASSWORD")
	return cfg, nil
}

// ApplySearchPath 把安装目录下的 tools 与发行所需的系统目录追加到进程 PATH。
//
// 只影响本进程及它启动的子进程（客户端工具要加载同目录 DLL），不修改系统环境。
// 目的有两个：MySQL 客户端在 tools 目录内找不到同级 DLL 时仍能按 PATH 解析；
// sqlcmd 依赖的 OLE DB 提供程序目录在服务账号的 PATH 里往往缺失。
func (c Config) ApplySearchPath() {
	dirs := []string{}
	if exeDir := toolenv.ExecutableDir(); exeDir != "" {
		dirs = append(dirs, toolenv.BundledToolsDir(exeDir))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, toolenv.BundledToolsDir(wd))
	}
	existing := append([]string{}, dirs...)
	// 最后追加发行默认路径：它只在系统 PATH 缺失时才起作用，不覆盖已有配置。
	existing = append(existing, DefaultToolSearchPath...)
	current := os.Getenv("PATH")
	parts := []string{}
	for _, dir := range existing {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			parts = append(parts, dir)
		}
	}
	if len(parts) == 0 {
		return
	}
	_ = os.Setenv("PATH", strings.Join(append(parts, current), string(os.PathListSeparator)))
}

// DefaultToolSearchPath 是随发行形态追加的系统目录，仅当目录真实存在时才加入 PATH。
var DefaultToolSearchPath = []string{toolenv.DefaultSearchPath}

// EnsureDirectories 创建服务拥有的数据与备份目录，不触碰其他目录。
func (c Config) EnsureDirectories() error {
	for _, dir := range []string{c.DataDir, filepath.Dir(c.DatabasePath), filepath.Dir(c.MasterKeyPath), c.BackupRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建目录 %s: %w", dir, err)
		}
	}
	return nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	if value := os.Getenv(key); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("%s 必须是正 Go duration，例如 30m", key)
		}
		return parsed, nil
	}
	return fallback, nil
}
