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

// StorageTarget 描述备份落点登记：local/unc 为文件系统目录，ftp 为远程文件服务器，
// oss 为 S3 兼容对象存储（阿里云 OSS、MinIO、AWS S3 共用同一实现）。
type StorageTarget struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Root       string    `json:"root"`
	Revision   int64     `json:"revision"`
	ServerRoot string    `json:"server_root,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	// Kind='ftp' 时的远程连接配置；local/unc 不使用。SecretRef 指向加密的 FTP 密码。
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Username string `json:"username,omitempty"`
	SecretRef string `json:"-"`
	Passive  bool   `json:"passive"`
	// Kind='oss' 时的对象存储配置：Bucket 为存储空间名，Region 为该服务的签名地域，
	// Endpoint 为服务地址（如 oss-cn-hangzhou.aliyuncs.com），UseSSL 决定 https 与否，
	// PathStyle 为 true 时使用「端点/bucket/key」形式（MinIO、部分私有化 OSS 需要）。
	// Username 复用为 AccessKey ID，SecretRef 指向加密的 AccessKey Secret。
	Bucket    string `json:"bucket,omitempty"`
	Region    string `json:"region,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	UseSSL    bool   `json:"use_ssl"`
	PathStyle bool   `json:"path_style"`
}

// Job 同时保存调度与保留策略；Enabled 与运行状态无关。
type Job struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	SourceID string `json:"source_id"`
	// TargetID 是存储位置登记 ID；LocationKind='database_server' 时为空，
	// 备份由数据库服务直接写到任务配置的服务器目录，不经过执行器磁盘。
	TargetID string `json:"target_id"`
	// LocationKind 区分备份落点：'storage'（执行器可达的存储位置）或
	// 'database_server'（数据库服务器本地目录，仅 SQL Server 原生备份支持）。
	LocationKind string `json:"location_kind"`
	// ServerDir 是 database_server 模式下数据库服务器上的输出目录，如 D:\bak。
	ServerDir      string     `json:"server_dir"`
	// Subdir 是任务在存储根目录下的相对子目录，为不同数据库实例区分子目录；
	// 空值表示直接写入根目录（历史任务行为）。每次运行冻结其值，已产出的文件位置不受后续修改影响。
	Subdir          string     `json:"subdir"`
	Timezone        string     `json:"timezone"`
	ScheduleType    string     `json:"schedule_type"`
	TimeOfDay       string     `json:"time_of_day"`
	Weekday         int        `json:"weekday"`
	IntervalMinutes int        `json:"interval_minutes"`
	Enabled         bool       `json:"enabled"`
	BackupType      string     `json:"backup_type"`
	// BackupMethod 是任务选择的备份路线：空表示沿用引擎默认路线（MySQL 为逻辑全量），
	// logical 为 mysqldump，physical 为 XtraBackup 物理备份。
	// 同一引擎的两条路线恢复语义完全不同，必须在任务上固定，不能按运行时环境临时切换。
	BackupMethod    string     `json:"backup_method"`
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
	// BackupMethod 记录产出该对象的备份路线，用于把恢复链限定在同一路线内：
	// 逻辑备份与物理备份的产物不能互为基线，混链会生成无法恢复的依赖关系。
	BackupMethod   string          `json:"backup_method"`
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
