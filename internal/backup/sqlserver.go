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
	"strconv"
	"strings"

	"dbbak/internal/metadata"
)

// SQLServerClient 通过 sqlcmd 执行原生全量；备份路径属于数据库服务而非管理端。
// 当前实现未进行真实恢复验收，API 必须将其标为 unverified。
type SQLServerClient struct {
	SQLCMDPath string
}

func (c SQLServerClient) Engine() string { return "sqlserver" }

// Method 声明这是 SQL Server 的原生备份路线（BACKUP DATABASE / BACKUP LOG）。
func (c SQLServerClient) Method() string { return "native" }

// Version 返回 sqlcmd 工具版本，不代表数据库组合已通过恢复验收。
// sqlcmd -? 输出整页帮助，这里只取包含 Version 的行，避免把帮助文本写进能力矩阵。
func (c SQLServerClient) Version(ctx context.Context) (string, error) {
	output, err := c.run(ctx, []string{"-?"}, "", "")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "Version") {
			return trimmed, nil
		}
	}
	return "sqlcmd", nil
}

// Probe 使用真实 SQL 连接读取服务器版本。
func (c SQLServerClient) Probe(ctx context.Context, source metadata.Source, password string) (string, error) {
	args := c.connectionArgs(source)
	args = append(args, "-d", "master", "-b", "-r", "1", "-W", "-s", "|", "-h", "-1", "-Q", "SET NOCOUNT ON; SELECT @@VERSION;")
	output, err := c.run(ctx, args, password, "")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// Execute 按 full / differential / log 生成原生备份文件，并在同主机的授权执行器目录校验。
func (c SQLServerClient) Execute(ctx context.Context, source metadata.Source, target metadata.StorageTarget, password, workPath string, spec BackupSpec) (ExecuteResult, error) {
	kind := spec.Type
	if kind == "" {
		kind = "full"
	}
	extension := ".bak"
	switch kind {
	case "full":
	case "differential":
	case "log":
		extension = ".trn"
		// 事务日志备份要求 FULL 恢复模式；SIMPLE 下不提前拦截会得到晦涩的引擎错误。
		model, err := c.recoveryModel(ctx, source, password)
		if err != nil {
			return ExecuteResult{}, err
		}
		if !strings.EqualFold(model, "FULL") {
			return ExecuteResult{}, &ToolError{
				Code:    "RECOVERY_MODEL_NOT_FULL",
				Message: "事务日志备份要求数据库处于 FULL 恢复模式，当前为 " + model,
			}
		}
	default:
		return ExecuteResult{}, &ToolError{Code: "BACKUP_TYPE_UNSUPPORTED", Message: "SQL Server 只支持 full、differential、log 三种备份类型"}
	}
	if target.ServerRoot == "" {
		return ExecuteResult{}, &ToolError{Code: "SERVER_PATH_REQUIRED", Message: "SQL Server 原生备份必须配置数据库服务可访问的 server_root"}
	}
	name := filepath.Base(workPath) + extension
	serverLocal := target.Kind == "database_server"
	var localOutput string
	if !serverLocal {
		// 存储位置模式：执行器与数据库服务共享同一实际目录，备份完成后立即在本地可见。
		localOutput = filepath.Join(target.Root, name)
		if _, err := os.Stat(localOutput); err == nil {
			return ExecuteResult{}, &ToolError{Code: "BACKUP_ALREADY_EXISTS", Message: "目标 SQL Server 备份文件已存在，拒绝覆盖"}
		} else if !os.IsNotExist(err) {
			return ExecuteResult{}, err
		}
	}
	serverOutput := filepath.Join(target.ServerRoot, name)
	if strings.ContainsRune(serverOutput, '\'') {
		return ExecuteResult{}, &ToolError{Code: "SERVER_PATH_REJECTED", Message: "SQL Server 备份路径包含不安全字符"}
	}
	statement := backupStatement(kind, source.Database, serverOutput)
	scriptPath := filepath.Join(workPath, "backup.sql")
	if err := os.WriteFile(scriptPath, []byte(statement), 0o600); err != nil {
		return ExecuteResult{}, err
	}
	args := c.connectionArgs(source)
	args = append(args, "-d", "master", "-b", "-r", "1", "-f", "65001", "-i", scriptPath)
	if _, err := c.run(ctx, args, password, ""); err != nil {
		return ExecuteResult{}, err
	}
	if !serverLocal {
		// 存储位置模式必须能在本地看到文件；服务器本地模式由 msdb 元数据背书。
		if info, err := os.Stat(localOutput); err != nil || !info.Mode().IsRegular() {
			return ExecuteResult{}, &ToolError{Code: "BACKUP_FILE_NOT_VISIBLE", Message: "BACKUP 返回成功，但执行器未在授权目录看到备份文件；请检查服务路径映射和权限"}
		}
	}
	metadataQuery := backupMetadataQuery(source.Database, kind)
	args = c.connectionArgs(source)
	args = append(args, "-d", "msdb", "-b", "-r", "1", "-W", "-s", "|", "-h", "-1", "-Q", "SET NOCOUNT ON; "+metadataQuery)
	output, err := c.run(ctx, args, password, "")
	if err != nil {
		return ExecuteResult{}, err
	}
	parts := strings.Split(strings.TrimSpace(output), "|")
	if len(parts) != 6 {
		return ExecuteResult{}, &ToolError{Code: "BACKUP_METADATA_MISSING", Message: "无法读取 SQL Server 备份 LSN 元数据"}
	}
	backupSize, _ := strconv.ParseInt(strings.TrimSpace(parts[5]), 10, 64)
	engineMetadata, _ := json.Marshal(map[string]any{
		"logical": false, "scope": "database", "format": strings.TrimPrefix(extension, "."), "backup_type": kind,
		"backup_set_id":       strings.TrimSpace(parts[0]),
		"first_lsn":           strings.TrimSpace(parts[1]),
		"last_lsn":            strings.TrimSpace(parts[2]),
		"database_backup_lsn": strings.TrimSpace(parts[3]),
		"checkpoint_lsn":      strings.TrimSpace(parts[4]),
		"backup_size":         backupSize,
		"server_local":        serverLocal,
		"server_path":         serverOutput,
	})
	producedPath := localOutput
	if serverLocal {
		// 空 ProducedPath 告知执行器：文件在数据库服务器上，跳过本地哈希校验。
		producedPath = ""
	}
	return ExecuteResult{ProducedPath: producedPath, Extension: extension, Method: "sqlserver_native", EngineMetadata: engineMetadata}, nil
}

// backupStatement 生成对应类型的 BACKUP 语句；日志备份使用 BACKUP LOG。
func backupStatement(kind, database, destination string) string {
	switch kind {
	case "differential":
		return fmt.Sprintf("BACKUP DATABASE [%s] TO DISK = N'%s' WITH DIFFERENTIAL, CHECKSUM;", database, destination)
	case "log":
		return fmt.Sprintf("BACKUP LOG [%s] TO DISK = N'%s' WITH CHECKSUM;", database, destination)
	default:
		return fmt.Sprintf("BACKUP DATABASE [%s] TO DISK = N'%s' WITH CHECKSUM;", database, destination)
	}
}

// recoveryModel 读取数据库恢复模式，用于在日志备份前给出明确前提判断。
func (c SQLServerClient) recoveryModel(ctx context.Context, source metadata.Source, password string) (string, error) {
	query := fmt.Sprintf("SET NOCOUNT ON; SELECT recovery_model_desc FROM sys.databases WHERE name = N'%s';", source.Database)
	args := c.connectionArgs(source)
	args = append(args, "-d", "master", "-b", "-r", "1", "-W", "-h", "-1", "-Q", query)
	output, err := c.run(ctx, args, password, "")
	if err != nil {
		return "", err
	}
	model := strings.TrimSpace(output)
	if model == "" {
		return "", &ToolError{Code: "DATABASE_NOT_FOUND", Message: "无法读取数据库恢复模式，数据库可能不存在"}
	}
	return model, nil
}

// backupMetadataQuery 只查询 msdb.dbo.backupset 中真实存在的列。
// SQL Server 没有 backup_lsn 列；差异与日志链依赖 database_backup_lsn 与序列号。
// backup_size 由数据库服务登记，服务器本地备份用它作为文件大小的可信来源。
func backupMetadataQuery(database, kind string) string {
	// D=全量，I=差异，L=事务日志；类型必须与本次备份一致，否则会误取历史记录。
	setType := map[string]string{"full": "D", "differential": "I", "log": "L"}[kind]
	return fmt.Sprintf("SELECT TOP (1) backup_set_id, first_lsn, last_lsn, database_backup_lsn, checkpoint_lsn, backup_size "+
		"FROM msdb.dbo.backupset WHERE database_name = '%s' AND type = '%s' ORDER BY backup_set_id DESC;", database, setType)
}

// connectionArgs 组装 sqlcmd 连接参数。
//
// ODBC Driver 18 默认 Encrypt=yes 且强制校验证书，自建/测试实例几乎都是 SQL Server
// 自签名的实例证书，此时必须带 -C（TrustServerCertificate=yes），否则连接只会返回
//
//	SSL Provider: certificate verify failed:self-signed certificate
//
// 是否信任由数据源上的 trust_server_certificate 决定，不在代码里写死：同一部署里可能
// 同时有自建实例（需信任）和云上使用受信证书的实例（应保持严格校验）。-C 只放弃证书
// 链校验，加密通道和登录认证都不受影响。
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
	if source.TrustServerCertificate {
		args = append(args, "-C")
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
		code = "TOOL_MISSING"
		// sqlcmd 属于微软专有组件，不随安装包分发；提示必须说明是它缺失以及可用 DBBAK_SQLCMD 指定。
		message = "找不到 sqlcmd，请安装 Microsoft Command Line Utilities 或通过 DBBAK_SQLCMD 指定路径"
	case strings.Contains(lower, "login failed"):
		code = "AUTH_FAILED"
	// 证书错误单列：默认已带 -C，仍出现说明是服务端要求强制加密或证书被中间人替换，
	// 这是需要人工判断的安全问题，不能混进 NETWORK_ERROR 让人误以为是网络抖动。
	case strings.Contains(lower, "certificate verify failed") || strings.Contains(lower, "self-signed certificate"):
		code = "TLS_VERIFY_FAILED"
		message = "SQL Server 证书校验失败（" + message + "）；请确认实例未强制使用不匹配的证书，或在数据库侧配置受信证书"
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
