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

	"dbbak/internal/metadata"
)

// MySQLTool 抽象真实客户端，便于用局部假实现测试状态机；真实验收不得用假实现替代。
type MySQLTool interface {
	Version(context.Context) (string, error)
	Probe(context.Context, metadata.Source, string) (string, error)
	Dump(context.Context, metadata.Source, string, string) error
}

// MySQLClient 通过官方 mysql/mysqldump 参数数组执行，不经过 shell。
type MySQLClient struct {
	MySQLPath     string
	MySQLDumpPath string
}

// ToolError 是可安全返回给前端的分类错误。
//
// Detail 保存排查用的技术细节（工具路径、原始 stderr），只写服务日志，
// 不进 API 响应；Message 是给运维看的中文说明。
type ToolError struct {
	Code    string
	Message string
	Detail  string
}

func (e *ToolError) Error() string { return e.Message }

// Version 返回 mysqldump 的真实版本输出。
func (c MySQLClient) Version(ctx context.Context) (string, error) {
	output, err := c.run(ctx, c.MySQLDumpPath, []string{"--version"}, "")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// Probe 真实连接并查询版本，分别识别工具、网络、认证和数据库错误。
func (c MySQLClient) Probe(ctx context.Context, source metadata.Source, password string) (string, error) {
	args := connectionArgs(source)
	args = append(args, "--connect-timeout=8", "--batch", "--skip-column-names", "--execute=SELECT VERSION()")
	output, err := c.run(ctx, c.MySQLPath, args, password)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// Dump 将单库逻辑全量写入执行器指定文件；result-file 避免 shell 重定向编码问题。
func (c MySQLClient) Dump(ctx context.Context, source metadata.Source, password, resultPath string) error {
	args := connectionArgs(source)
	args = append(args,
		"--single-transaction",
		"--skip-lock-tables",
		"--quick",
		"--routines",
		"--events",
		"--triggers",
		"--hex-blob",
		"--set-gtid-purged=OFF",
		"--no-tablespaces",
		"--default-character-set=utf8mb4",
		"--result-file="+resultPath,
		source.Database,
	)
	// 单库转储不包含 CREATE DATABASE/USE，恢复端可指定隔离目标库，避免覆盖源库。
	_, err := c.run(ctx, c.MySQLDumpPath, args, password)
	return err
}

// Engine 返回注册引擎名。
func (c MySQLClient) Engine() string { return "mysql" }

// Method 声明这是 MySQL 的逻辑备份路线（mysqldump），与物理路线并存但恢复语义不同。
func (c MySQLClient) Method() string { return "logical" }

// Execute 在隔离工作目录生成单库 SQL 文件。
func (c MySQLClient) Execute(ctx context.Context, source metadata.Source, _ metadata.StorageTarget, password, workPath string, spec BackupSpec) (ExecuteResult, error) {
	if spec.Type != "" && spec.Type != "full" {
		return ExecuteResult{}, &ToolError{Code: "BACKUP_TYPE_UNSUPPORTED", Message: "MySQL 适配器当前只实现逻辑全量，差异与日志未实现"}
	}
	produced := filepath.Join(workPath, "dump.sql")
	if err := c.Dump(ctx, source, password, produced); err != nil {
		return ExecuteResult{}, err
	}
	engineMetadata, _ := json.Marshal(map[string]any{"logical": true, "scope": "database", "database": source.Database})
	return ExecuteResult{ProducedPath: produced, Extension: ".sql", Method: "mysqldump", EngineMetadata: engineMetadata}, nil
}

func connectionArgs(source metadata.Source) []string {
	return []string{
		"--protocol=TCP",
		fmt.Sprintf("--host=%s", source.Host),
		fmt.Sprintf("--port=%d", source.Port),
		fmt.Sprintf("--user=%s", source.Username),
	}
}

func (c MySQLClient) run(ctx context.Context, path string, args []string, password string) (string, error) {
	command := exec.CommandContext(ctx, path, args...)
	// MYSQL_PWD 只传给子进程，不进入命令行、元数据或日志。
	command.Env = append(os.Environ(), "MYSQL_PWD="+password)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), nil
	}
	code := "BACKUP_COMMAND_FAILED"
	message := strings.TrimSpace(stderr.String())
	lower := strings.ToLower(message)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		code, message = "BACKUP_TIMEOUT", "数据库工具执行超时"
	case errors.Is(err, exec.ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "executable file not found"):
		code = "TOOL_MISSING"
		// 只说明“找不到”运维无法判断是路径写错还是根本没装，必须把实际尝试的路径带出来。
		// 详细工具名与路径见服务日志，界面保持短句。
		message = "找不到命令行工具 " + filepath.Base(path) + "，请确认该工具已安装且服务能看到其路径"
	case strings.Contains(lower, "access denied"):
		code = "AUTH_FAILED"
	case strings.Contains(lower, "can't connect") || strings.Contains(lower, "connection refused") || strings.Contains(lower, "timed out"):
		code = "NETWORK_ERROR"
	case strings.Contains(lower, "unknown database"):
		code = "DATABASE_NOT_FOUND"
	}
	if message == "" {
		message = err.Error()
	}
	message = sanitizeMessage(message, password)
	return stdout.String(), &ToolError{Code: code, Message: message, Detail: "工具 " + path + " 执行失败: " + err.Error() + "；原始输出: " + stderr.String()}
}

func sanitizeMessage(message, password string) string {
	if password != "" {
		message = strings.ReplaceAll(message, password, "[已脱敏]")
	}
	// 防止工具输出中的控制字符进入日志，并限制错误体量。
	message = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r >= 32 {
			return r
		}
		return -1
	}, message)
	const max = 1200
	if len(message) > max {
		message = message[:max] + "…"
	}
	return message
}

// ErrorCode 提取工具错误码，其他内部错误不暴露给前端。
func ErrorCode(err error) string {
	var toolErr *ToolError
	if errors.As(err, &toolErr) {
		return toolErr.Code
	}
	return "BACKUP_INTERNAL_ERROR"
}

// SafeErrorMessage 返回可记录的错误文本；内部错误使用通用说明。
func SafeErrorMessage(err error) string {
	var toolErr *ToolError
	if errors.As(err, &toolErr) {
		return toolErr.Message
	}
	return "备份执行失败，请检查服务端诊断日志"
}

// DiagnosticError 返回只写日志的排查细节，调用方不得把它写进 API 响应。
//
// 工具探测失败时界面只会显示“找不到命令行工具 X”，没有路径和原始输出时
// 运维无法判断是服务账号看不到该路径，还是配置根本没生效。
func DiagnosticError(err error) string {
	var toolErr *ToolError
	if errors.As(err, &toolErr) && toolErr.Detail != "" {
		return toolErr.Detail
	}
	return err.Error()
}
