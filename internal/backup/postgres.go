package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"vault/internal/metadata"
)

// PostgreSQLClient 使用 psql 与 pg_basebackup 实现整实例物理全量。
type PostgreSQLClient struct {
	PSQLPath       string
	BaseBackupPath string
}

// Engine 返回注册引擎名。
func (c PostgreSQLClient) Engine() string { return "postgresql" }

// Version 返回 pg_basebackup 的真实版本。
func (c PostgreSQLClient) Version(ctx context.Context) (string, error) {
	output, err := c.run(ctx, c.BaseBackupPath, []string{"--version"}, "")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// Probe 检查 SQL 连接、REPLICATION 权限和 walsender 前提。
func (c PostgreSQLClient) Probe(ctx context.Context, source metadata.Source, password string) (string, error) {
	query := `SELECT version(), current_setting('max_wal_senders'), (rolreplication OR rolsuper) FROM pg_roles WHERE rolname=current_user`
	output, err := c.run(ctx, c.PSQLPath, c.sqlArgs(source, query), password)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.TrimSpace(output), "|")
	if len(parts) != 3 {
		return "", &ToolError{Code: "PROBE_INVALID_RESPONSE", Message: "PostgreSQL 能力探测返回格式无效"}
	}
	if strings.TrimSpace(parts[1]) == "0" || strings.TrimSpace(parts[2]) != "t" {
		return "", &ToolError{Code: "PREREQUISITE_MISSING", Message: "备份账号需要 REPLICATION 或超级用户权限，且 max_wal_senders 必须大于 0"}
	}
	return strings.TrimSpace(parts[0]) + "; max_wal_senders=" + strings.TrimSpace(parts[1]) + "; replication=true", nil
}

// Execute 生成包含必要 WAL 的 base.tar，恢复时可解包到独立实例目录。
func (c PostgreSQLClient) Execute(ctx context.Context, source metadata.Source, _ metadata.StorageTarget, password, workPath string) (ExecuteResult, error) {
	identityQuery := `SELECT (SELECT system_identifier FROM pg_control_system()), (SELECT timeline_id FROM pg_control_checkpoint())`
	identity, err := c.run(ctx, c.PSQLPath, c.sqlArgs(source, identityQuery), password)
	if err != nil {
		return ExecuteResult{}, err
	}
	parts := strings.Split(strings.TrimSpace(identity), "|")
	if len(parts) != 2 {
		return ExecuteResult{}, &ToolError{Code: "CLUSTER_IDENTITY_UNAVAILABLE", Message: "无法读取 PostgreSQL system identifier 与 timeline"}
	}
	outputDir := filepath.Join(workPath, "output")
	args := []string{
		"--host=" + source.Host,
		fmt.Sprintf("--port=%d", source.Port),
		"--username=" + source.Username,
		"--dbname=dbname=" + source.Database,
		"--pgdata=" + outputDir,
		"--format=tar",
		"--wal-method=fetch",
		"--checkpoint=fast",
		"--progress",
		"--no-password",
	}
	if _, err = c.run(ctx, c.BaseBackupPath, args, password); err != nil {
		return ExecuteResult{}, err
	}
	produced := filepath.Join(outputDir, "base.tar")
	if info, statErr := os.Stat(produced); statErr != nil || !info.Mode().IsRegular() {
		return ExecuteResult{}, &ToolError{Code: "BACKUP_MANIFEST_MISSING", Message: "pg_basebackup 未生成预期 base.tar"}
	}
	engineMetadata, _ := json.Marshal(map[string]any{
		"logical": false, "scope": "cluster", "format": "tar", "wal_method": "fetch",
		"system_identifier": strings.TrimSpace(parts[0]), "timeline": strings.TrimSpace(parts[1]),
	})
	return ExecuteResult{ProducedPath: produced, Extension: ".tar", Method: "pg_basebackup", EngineMetadata: engineMetadata}, nil
}

func (c PostgreSQLClient) sqlArgs(source metadata.Source, query string) []string {
	return []string{
		"--host=" + source.Host,
		fmt.Sprintf("--port=%d", source.Port),
		"--username=" + source.Username,
		"--dbname=" + source.Database,
		"--tuples-only", "--no-psqlrc", "--set=ON_ERROR_STOP=1", "--command=" + query,
	}
}

func (c PostgreSQLClient) run(ctx context.Context, path string, args []string, password string) (string, error) {
	command := exec.CommandContext(ctx, path, args...)
	command.Env = append(os.Environ(), "PGPASSWORD="+password)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), nil
	}
	code := "POSTGRESQL_COMMAND_FAILED"
	message := strings.TrimSpace(stderr.String())
	lower := strings.ToLower(message)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		code, message = "BACKUP_TIMEOUT", "PostgreSQL 工具执行超时"
	case errors.Is(err, exec.ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "executable file not found"):
		code, message = "TOOL_MISSING", "找不到 PostgreSQL 工具，请检查 VAULT_PSQL 与 VAULT_PG_BASEBACKUP"
	case strings.Contains(lower, "password authentication failed"):
		code = "AUTH_FAILED"
	case strings.Contains(lower, "could not connect") || strings.Contains(lower, "connection refused") || strings.Contains(lower, "timeout expired"):
		code = "NETWORK_ERROR"
	case strings.Contains(lower, "replication") || strings.Contains(lower, "permission denied") || strings.Contains(lower, "pg_hba.conf"):
		code = "PREREQUISITE_MISSING"
	}
	if message == "" {
		message = err.Error()
	}
	return stdout.String(), &ToolError{Code: code, Message: sanitizeMessage(message, password)}
}
