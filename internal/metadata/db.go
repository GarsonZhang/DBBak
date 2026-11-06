package metadata

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store 是单实例 SQLite 元数据仓库。
type Store struct {
	db *sql.DB
}

// Open 打开 SQLite、设置 WAL 与外键，并执行版本化迁移。
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// 单实例写入串行化可避免 SQLITE_BUSY，并简化运行锁语义。
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("设置 SQLite busy_timeout: %w", err)
	}
	if _, err = db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("设置 SQLite WAL: %w", err)
	}
	if _, err = db.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		db.Close()
		return nil, fmt.Errorf("启用 SQLite 外键: %w", err)
	}
	store := &Store{db: db}
	if err = store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// Close 关闭元数据库。
func (s *Store) Close() error { return s.db.Close() }

// Ping 用于 readiness 检查。
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate(ctx context.Context) error {
	// 迁移 6 需要重建带外键的表，外键开关只能在事务外切换；
	// 因此整个迁移流程改为：事务外读版本 → 关外键 → 单事务执行全部迁移 → 恢复外键。
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return err
	}
	if version >= currentMigrationVersion {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("迁移前关闭外键: %w", err)
	}
	defer func() { _, _ = s.db.ExecContext(ctx, `PRAGMA foreign_keys=ON`) }()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version < 1 {
		if _, err = tx.ExecContext(ctx, schemaV1); err != nil {
			return fmt.Errorf("执行迁移 1: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(1,?)`, formatTime(time.Now())); err != nil {
			return err
		}
	}
	if version < 2 {
		if _, err = tx.ExecContext(ctx, "ALTER TABLE artifacts ADD COLUMN engine_metadata TEXT NOT NULL DEFAULT '{}'"); err != nil {
			return fmt.Errorf("执行迁移 2: %w", err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES(2,?)", formatTime(time.Now())); err != nil {
			return err
		}
	}
	if version < 3 {
		if _, err = tx.ExecContext(ctx, "ALTER TABLE storage_targets ADD COLUMN server_root TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("执行迁移 3: %w", err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES(3,?)", formatTime(time.Now())); err != nil {
			return err
		}
	}
	if version < 4 {
		// 备份类型属于任务配置；历史任务默认全量，保持既有行为不变。
		if _, err = tx.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN backup_type TEXT NOT NULL DEFAULT 'full'"); err != nil {
			return fmt.Errorf("执行迁移 4: %w", err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES(4,?)", formatTime(time.Now())); err != nil {
			return err
		}
	}
	if version < 5 {
		// 子目录属于任务级路径配置；历史任务保持空值，输出位置不变。
		if _, err = tx.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN subdir TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("执行迁移 5: %w", err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES(5,?)", formatTime(time.Now())); err != nil {
			return err
		}
	}
	if version < 6 {
		// 备份位置模型：任务可直连数据库服务器本地目录（location_kind='database_server'），
		// 此时不选择存储位置，jobs.target_id 与 artifacts.target_id 允许为空。
		// SQLite 无法直接去掉 NOT NULL，需按标准流程重建两张表。
		rebuild := []string{
			`CREATE TABLE jobs_new (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, source_id TEXT NOT NULL REFERENCES sources(id),
 target_id TEXT REFERENCES storage_targets(id), location_kind TEXT NOT NULL DEFAULT 'storage',
 server_dir TEXT NOT NULL DEFAULT '', timezone TEXT NOT NULL,
 schedule_type TEXT NOT NULL, time_of_day TEXT NOT NULL, weekday INTEGER NOT NULL,
 interval_minutes INTEGER NOT NULL, enabled INTEGER NOT NULL,
 backup_type TEXT NOT NULL DEFAULT 'full', subdir TEXT NOT NULL DEFAULT '',
 retention_days INTEGER NOT NULL, min_full INTEGER NOT NULL, auto_cleanup INTEGER NOT NULL,
 next_run_at TEXT, revision INTEGER NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
)`,
			`INSERT INTO jobs_new SELECT id,name,source_id,target_id,'storage','',timezone,
 schedule_type,time_of_day,weekday,interval_minutes,enabled,backup_type,subdir,
 retention_days,min_full,auto_cleanup,next_run_at,revision,created_at,updated_at FROM jobs`,
			`DROP TABLE jobs`,
			`ALTER TABLE jobs_new RENAME TO jobs`,
			`CREATE INDEX jobs_due ON jobs(enabled,next_run_at)`,
			`CREATE TABLE artifacts_new (
 id TEXT PRIMARY KEY, run_id TEXT NOT NULL UNIQUE REFERENCES runs(id), job_id TEXT NOT NULL REFERENCES jobs(id),
 source_id TEXT NOT NULL REFERENCES sources(id), target_id TEXT REFERENCES storage_targets(id),
 chain_id TEXT NOT NULL, parent_id TEXT, type TEXT NOT NULL, method TEXT NOT NULL,
 relative_path TEXT NOT NULL UNIQUE, size INTEGER NOT NULL, sha256 TEXT NOT NULL,
 state TEXT NOT NULL, locked INTEGER NOT NULL, tool_version TEXT NOT NULL,
 engine_metadata TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL, verified_at TEXT
)`,
			`INSERT INTO artifacts_new SELECT id,run_id,job_id,source_id,target_id,chain_id,parent_id,type,method,
 relative_path,size,sha256,state,locked,tool_version,engine_metadata,created_at,verified_at FROM artifacts`,
			`DROP TABLE artifacts`,
			`ALTER TABLE artifacts_new RENAME TO artifacts`,
			`CREATE INDEX artifacts_job_created ON artifacts(job_id,created_at DESC)`,
		}
		for _, statement := range rebuild {
			if _, err = tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("执行迁移 6: %w", err)
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(6,?)`, formatTime(time.Now())); err != nil {
			return err
		}
	}
	if version < 7 {
		// 存储位置类型扩展：FTP 远程存储需要主机、端口、账号与加密凭据；
		// UNC 共享目录复用 root，无需新列。
		alters := []string{
			`ALTER TABLE storage_targets ADD COLUMN host TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE storage_targets ADD COLUMN port INTEGER NOT NULL DEFAULT 21`,
			`ALTER TABLE storage_targets ADD COLUMN username TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE storage_targets ADD COLUMN secret_ref TEXT REFERENCES secrets(id)`,
			`ALTER TABLE storage_targets ADD COLUMN passive INTEGER NOT NULL DEFAULT 1`,
		}
		for _, statement := range alters {
			if _, err = tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("执行迁移 7: %w", err)
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(7,?)`, formatTime(time.Now())); err != nil {
			return err
		}
	}
	if version < 8 {
		// 对象存储（S3 兼容，含阿里云 OSS）：复用 host/port/username/secret_ref 承载
		// 端点、AccessKey ID 与加密的 AccessKey Secret，新增 bucket/region/use_ssl/path_style。
		// root 列存放对象键前缀（如 /backups 或空表示桶根），与本地目录列复用但语义由 kind 决定。
		alters := []string{
			`ALTER TABLE storage_targets ADD COLUMN bucket TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE storage_targets ADD COLUMN region TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE storage_targets ADD COLUMN use_ssl INTEGER NOT NULL DEFAULT 1`,
			`ALTER TABLE storage_targets ADD COLUMN path_style INTEGER NOT NULL DEFAULT 0`,
		}
		for _, statement := range alters {
			if _, err = tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("执行迁移 8: %w", err)
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(8,?)`, formatTime(time.Now())); err != nil {
			return err
		}
	}
	if version < 9 {
		// 备份路线：同一引擎可能同时存在逻辑与物理路线（如 MySQL 的 mysqldump 与 XtraBackup）。
		// 任务侧记录选择，对象侧记录来源，避免两条路线的产物被误当作彼此的恢复基线。
		// 历史对象没有路线信息，按实际使用的工具回填，不能一律标成逻辑备份。
		alters := []string{
			`ALTER TABLE jobs ADD COLUMN backup_method TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE artifacts ADD COLUMN backup_method TEXT NOT NULL DEFAULT ''`,
			`UPDATE artifacts SET backup_method = CASE method
 WHEN 'pg_basebackup' THEN 'physical'
 WHEN 'sqlserver_native' THEN 'native'
 WHEN 'xtrabackup' THEN 'physical'
 WHEN 'mysqlbinlog' THEN 'physical'
 ELSE 'logical' END`,
		}
		for _, statement := range alters {
			if _, err = tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("执行迁移 9: %w", err)
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(9,?)`, formatTime(time.Now())); err != nil {
			return err
		}
	}
	if version > currentMigrationVersion {
		return fmt.Errorf("元数据库版本 %d 高于当前程序支持版本", version)
	}
	return tx.Commit()
}

// currentMigrationVersion 是程序支持的最新元数据版本；新增迁移时同步递增。
const currentMigrationVersion = 9

const schemaV1 = `
CREATE TABLE users (
 id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE sessions (
 token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 expires_at TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE secrets (
 id TEXT PRIMARY KEY, ciphertext TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE sources (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, engine TEXT NOT NULL, host TEXT NOT NULL,
 port INTEGER NOT NULL, database_name TEXT NOT NULL, username TEXT NOT NULL,
 secret_ref TEXT NOT NULL REFERENCES secrets(id), revision INTEGER NOT NULL,
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE storage_targets (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL, root TEXT NOT NULL,
 revision INTEGER NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE jobs (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, source_id TEXT NOT NULL REFERENCES sources(id),
 target_id TEXT NOT NULL REFERENCES storage_targets(id), timezone TEXT NOT NULL,
 schedule_type TEXT NOT NULL, time_of_day TEXT NOT NULL, weekday INTEGER NOT NULL,
 interval_minutes INTEGER NOT NULL, enabled INTEGER NOT NULL,
 retention_days INTEGER NOT NULL, min_full INTEGER NOT NULL, auto_cleanup INTEGER NOT NULL,
 next_run_at TEXT, revision INTEGER NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX jobs_due ON jobs(enabled,next_run_at);
CREATE TABLE runs (
 id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id), trigger_type TEXT NOT NULL,
 scheduled_at TEXT, idempotency_key TEXT NOT NULL, state TEXT NOT NULL, attempt INTEGER NOT NULL,
 command_status TEXT NOT NULL, file_status TEXT NOT NULL, restore_status TEXT NOT NULL,
 error_code TEXT NOT NULL, error_message TEXT NOT NULL,
 started_at TEXT, finished_at TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX runs_idempotency ON runs(job_id,idempotency_key) WHERE idempotency_key<>'';
CREATE UNIQUE INDEX runs_one_active ON runs(job_id) WHERE state IN ('queued','running','verifying','reconciling');
CREATE INDEX runs_job_created ON runs(job_id,created_at DESC);
CREATE TABLE run_logs (
 id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 ts TEXT NOT NULL, level TEXT NOT NULL, message TEXT NOT NULL
);
CREATE INDEX run_logs_run ON run_logs(run_id,id);
CREATE TABLE artifacts (
 id TEXT PRIMARY KEY, run_id TEXT NOT NULL UNIQUE REFERENCES runs(id), job_id TEXT NOT NULL REFERENCES jobs(id),
 source_id TEXT NOT NULL REFERENCES sources(id), target_id TEXT NOT NULL REFERENCES storage_targets(id),
 chain_id TEXT NOT NULL, parent_id TEXT, type TEXT NOT NULL, method TEXT NOT NULL,
 relative_path TEXT NOT NULL UNIQUE, size INTEGER NOT NULL, sha256 TEXT NOT NULL,
 state TEXT NOT NULL, locked INTEGER NOT NULL, tool_version TEXT NOT NULL,
 created_at TEXT NOT NULL, verified_at TEXT
);
CREATE INDEX artifacts_job_created ON artifacts(job_id,created_at DESC);
CREATE TABLE cleanup_previews (
 id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id), job_revision INTEGER NOT NULL,
 fingerprint TEXT NOT NULL, payload TEXT NOT NULL, expires_at TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE audit_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, actor TEXT NOT NULL, action TEXT NOT NULL,
 target TEXT NOT NULL, result TEXT NOT NULL, request_id TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE INDEX audit_created ON audit_events(created_at DESC);
`

// NewID 生成带领域前缀的随机标识。
func NewID(prefix string) string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		panic(fmt.Sprintf("系统随机源不可用: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(raw)
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

func scanNullableTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || value.String == "" {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
