// Package config 负责 Vault 服务的环境配置与目录初始化。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	PostgreSQLPath    string
	PGBaseBackupPath  string
	SQLCMDPath        string
	SessionTTL        time.Duration
	SchedulerInterval time.Duration
	BackupTimeout     time.Duration
	// SQLServerEnabled 由运维在完成真实备份与异名恢复验收后显式开启；
	// 默认关闭，避免未验收的备用路径被静默启用。
	SQLServerEnabled bool
}

// Load 从环境变量读取配置，不生成或返回任何数据库凭据。
func Load() (Config, error) {
	dataDir := env("VAULT_DATA_DIR", ".vault")
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return Config{}, fmt.Errorf("解析数据目录: %w", err)
	}
	cfg := Config{
		Addr:              env("VAULT_ADDR", "127.0.0.1:8080"),
		DataDir:           dataDir,
		DatabasePath:      env("VAULT_DATABASE", filepath.Join(dataDir, "vault.db")),
		BackupRoot:        env("VAULT_BACKUP_ROOT", filepath.Join(dataDir, "backups")),
		MasterKeyPath:     env("VAULT_MASTER_KEY_FILE", filepath.Join(dataDir, "master.key")),
		MySQLPath:         env("VAULT_MYSQL", "mysql"),
		MySQLDumpPath:     env("VAULT_MYSQLDUMP", "mysqldump"),
		PostgreSQLPath:    env("VAULT_PSQL", "psql"),
		PGBaseBackupPath:  env("VAULT_PG_BASEBACKUP", "pg_basebackup"),
		SQLCMDPath:        env("VAULT_SQLCMD", "sqlcmd"),
		SessionTTL:        12 * time.Hour,
		SchedulerInterval: 2 * time.Second,
		BackupTimeout:     30 * time.Minute,
	}
	if cfg.SessionTTL, err = durationEnv("VAULT_SESSION_TTL", cfg.SessionTTL); err != nil {
		return Config{}, err
	}
	if cfg.SchedulerInterval, err = durationEnv("VAULT_SCHEDULER_INTERVAL", cfg.SchedulerInterval); err != nil {
		return Config{}, err
	}
	if cfg.BackupTimeout, err = durationEnv("VAULT_BACKUP_TIMEOUT", cfg.BackupTimeout); err != nil {
		return Config{}, err
	}
	cfg.SQLServerEnabled = env("VAULT_SQLSERVER_ENABLED", "") == "1" || strings.EqualFold(env("VAULT_SQLSERVER_ENABLED", ""), "true")
	return cfg, nil
}

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
