// Package metadata 定义持久化模型与 SQLite 访问逻辑。
package metadata

import (
	"encoding/json"
	"time"
)

// User 是不含密码哈希的管理员视图。
type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

// Source 是数据源的安全视图，凭据只通过 SecretRef 关联。
type Source struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Engine    string    `json:"engine"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Database  string    `json:"database"`
	Username  string    `json:"username"`
	SecretRef string    `json:"-"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StorageTarget 描述执行器本地授权目录；首版不支持 NAS/S3。
type StorageTarget struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Root       string    `json:"root"`
	Revision   int64     `json:"revision"`
	ServerRoot string    `json:"server_root,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Job 同时保存调度与保留策略；Enabled 与运行状态无关。
type Job struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	SourceID        string     `json:"source_id"`
	TargetID        string     `json:"target_id"`
	Timezone        string     `json:"timezone"`
	ScheduleType    string     `json:"schedule_type"`
	TimeOfDay       string     `json:"time_of_day"`
	Weekday         int        `json:"weekday"`
	IntervalMinutes int        `json:"interval_minutes"`
	Enabled         bool       `json:"enabled"`
	RetentionDays   int        `json:"retention_days"`
	MinFull         int        `json:"min_full"`
	AutoCleanup     bool       `json:"auto_cleanup"`
	NextRunAt       *time.Time `json:"next_run_at"`
	Revision        int64      `json:"revision"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// Run 区分命令、文件和恢复验证状态。
type Run struct {
	ID             string     `json:"id"`
	JobID          string     `json:"job_id"`
	TriggerType    string     `json:"trigger_type"`
	ScheduledAt    *time.Time `json:"scheduled_at,omitempty"`
	IdempotencyKey string     `json:"-"`
	State          string     `json:"state"`
	Attempt        int        `json:"attempt"`
	CommandStatus  string     `json:"command_status"`
	FileStatus     string     `json:"file_status"`
	RestoreStatus  string     `json:"restore_status"`
	ErrorCode      string     `json:"error_code,omitempty"`
	ErrorMessage   string     `json:"error_message,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// RunLog 是已脱敏的结构化运行日志。
type RunLog struct {
	ID        int64     `json:"id"`
	RunID     string    `json:"run_id"`
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
}

// Artifact 是独立全量恢复点；ParentID 为后续差异与日志链预留。
type Artifact struct {
	ID             string          `json:"id"`
	RunID          string          `json:"run_id"`
	JobID          string          `json:"job_id"`
	SourceID       string          `json:"source_id"`
	TargetID       string          `json:"target_id"`
	ChainID        string          `json:"chain_id"`
	ParentID       *string         `json:"parent_id"`
	Type           string          `json:"type"`
	Method         string          `json:"method"`
	RelativePath   string          `json:"relative_path"`
	Size           int64           `json:"size"`
	SHA256         string          `json:"sha256"`
	State          string          `json:"state"`
	Locked         bool            `json:"locked"`
	ToolVersion    string          `json:"tool_version"`
	EngineMetadata json.RawMessage `json:"engine_metadata"`
	CreatedAt      time.Time       `json:"created_at"`
	VerifiedAt     *time.Time      `json:"verified_at,omitempty"`
}

// AuditEvent 记录敏感操作，不保存请求载荷或凭据。
type AuditEvent struct {
	ID        int64     `json:"id"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	Result    string    `json:"result"`
	RequestID string    `json:"request_id"`
	CreatedAt time.Time `json:"created_at"`
}
