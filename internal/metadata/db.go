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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var version int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return err
	}
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
	if version > 3 {
		return fmt.Errorf("元数据库版本 %d 高于当前程序支持版本", version)
	}
	return tx.Commit()
}

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
