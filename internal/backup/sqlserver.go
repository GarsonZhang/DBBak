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

// SQLServerClient 通过 sqlcmd 执行原生全量；备份路径属于数据库服务而非管理端。
// 当前实现未进行真实恢复验收，API 必须将其标为 unverified。
type SQLServerClient struct {
	SQLCMDPath string
}

func (c SQLServerClient) Engine() string { return "sqlserver" }

// Version 返回 sqlcmd 工具版本，不代表数据库组合已通过恢复验收。
func (c SQLServerClient) Version(ctx context.Context) (string, error) {
	output, err := c.run(ctx, []string{"-?"}, "", "")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// Probe 使用真实 SQL 连接读取服务器版本。
func (c SQLServerClient) Probe(ctx context.Context, source metadata.Source, password string) (string, error) {
	args := c.connectionArgs(source)
	args = append(args, "-d", "master", "-b", "-r", "1", "-W", "-s", "|", "-h", "-1", "-Q", "SELECT @@VERSION;")
	output, err := c.run(ctx, args, password, "")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// Execute 在数据库服务路径生成 .bak，并在同主机的授权执行器目录校验文件。
func (c SQLServerClient) Execute(ctx context.Context, source metadata.Source, target metadata.StorageTarget, password, workPath string) (ExecuteResult, error) {
	if target.ServerRoot == "" {
		return ExecuteResult{}, &ToolError{Code: "SERVER_PATH_REQUIRED", Message: "SQL Server 原生备份必须配置数据库服务可访问的 server_root"}
	}
	name := filepath.Base(workPath) + ".bak"
	localOutput := filepath.Join(target.Root, name)
	serverOutput := filepath.Join(target.ServerRoot, name)
	if _, err := os.Stat(localOutput); err == nil {
		return ExecuteResult{}, &ToolError{Code: "BACKUP_ALREADY_EXISTS", Message: "目标 SQL Server 备份文件已存在，拒绝覆盖"}
	} else if !os.IsNotExist(err) {
		return ExecuteResult{}, err
	}
	if strings.ContainsRune(serverOutput, '\'') {
		return ExecuteResult{}, &ToolError{Code: "SERVER_PATH_REJECTED", Message: "SQL Server 备份路径包含不安全字符"}
	}
	scriptPath := filepath.Join(workPath, "backup.sql")
	statement := fmt.Sprintf("BACKUP DATABASE [%s] TO DISK = N'%s' WITH CHECKSUM;", source.Database, serverOutput)
	if err := os.WriteFile(scriptPath, []byte(statement), 0o600); err != nil {
		return ExecuteResult{}, err
	}
	args := c.connectionArgs(source)
	args = append(args, "-d", "master", "-b", "-r", "1", "-f", "65001", "-i", scriptPath)
	if _, err := c.run(ctx, args, password, ""); err != nil {
		return ExecuteResult{}, err
	}
	if info, err := os.Stat(localOutput); err != nil || !info.Mode().IsRegular() {
		return ExecuteResult{}, &ToolError{Code: "BACKUP_FILE_NOT_VISIBLE", Message: "BACKUP 返回成功，但执行器未在授权目录看到备份文件；请检查服务路径映射和权限"}
	}
	metadataQuery := fmt.Sprintf("SELECT TOP (1) backup_set_id, backup_lsn, database_backup_lsn FROM msdb.dbo.backupset WHERE database_name = '%s' AND type = 'D' ORDER BY backup_set_id DESC;", source.Database)
	args = c.connectionArgs(source)
	args = append(args, "-d", "msdb", "-b", "-r", "1", "-W", "-s", "|", "-h", "-1", "-Q", metadataQuery)
	output, err := c.run(ctx, args, password, "")
	if err != nil {
		return ExecuteResult{}, err
	}
	parts := strings.Split(strings.TrimSpace(output), "|")
	if len(parts) != 3 {
		return ExecuteResult{}, &ToolError{Code: "BACKUP_METADATA_MISSING", Message: "无法读取 SQL Server 全量备份 LSN 元数据"}
	}
	engineMetadata, _ := json.Marshal(map[string]any{
		"logical": false, "scope": "database", "format": "bak", "backup_type": "full",
		"backup_set_id": strings.TrimSpace(parts[0]), "backup_lsn": strings.TrimSpace(parts[1]), "database_backup_lsn": strings.TrimSpace(parts[2]),
		"server_path": serverOutput,
	})
	return ExecuteResult{ProducedPath: localOutput, Extension: ".bak", Method: "sqlserver_native", EngineMetadata: engineMetadata}, nil
}

func (c SQLServerClient) connectionArgs(source metadata.Source) []string {
	server := source.Host
	if source.Port > 0 {
		server = fmt.Sprintf("tcp:%s,%d", source.Host, source.Port)
	}
	args := []string{"-S", server}
	if source.Username == "" || source.Username == "integrated" {
		args = append(args, "-E")
	} else {
		args = append(args, "-U", source.Username)
	}
	return args
}

func (c SQLServerClient) run(ctx context.Context, args []string, password, _ string) (string, error) {
	command := exec.CommandContext(ctx, c.SQLCMDPath, args...)
	if password != "" {
		command.Env = append(os.Environ(), "SQLCMDPASSWORD="+password)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), nil
	}
	code := "SQLSERVER_COMMAND_FAILED"
	message := strings.TrimSpace(stderr.String())
	lower := strings.ToLower(message)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		code, message = "BACKUP_TIMEOUT", "SQL Server 工具执行超时"
	case errors.Is(err, exec.ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "executable file not found"):
		code, message = "TOOL_MISSING", "找不到 sqlcmd，请检查 VAULT_SQLCMD"
	case strings.Contains(lower, "login failed"):
		code = "AUTH_FAILED"
	case strings.Contains(lower, "could not open a connection") || strings.Contains(lower, "connection refused"):
		code = "NETWORK_ERROR"
	case strings.Contains(lower, "access is denied") || strings.Contains(lower, "operating system error 5"):
		code = "SERVER_PATH_NOT_WRITABLE"
	}
	if message == "" {
		message = err.Error()
	}
	return stdout.String(), &ToolError{Code: code, Message: sanitizeMessage(message, password)}
}
