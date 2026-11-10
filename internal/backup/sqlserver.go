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

// instanceProfileQuery 一次取回判定压缩能力的全部事实。
//
// ProductMajorVersion 在 SQL Server 2016 之前不存在，返回 NULL，因此必须同时取
// ProductVersion 作为回退解析来源；Edition 决定备份压缩许可（Express/Web 不带），
// backup compression default 只用于向用户显示实例当前默认行为。
const instanceProfileQuery = `SET NOCOUNT ON; SELECT
 CAST(SERVERPROPERTY('Edition') AS nvarchar(128)),
 CAST(SERVERPROPERTY('ProductMajorVersion') AS nvarchar(16)),
 CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128)),
 CAST((SELECT CAST(value_in_use AS nvarchar(16)) FROM sys.configurations WHERE name = 'backup compression default') AS nvarchar(16));`

// instanceProfile 是压缩能力判定的输入事实。
type instanceProfile struct {
	Edition      string
	MajorVersion int
	ProductVer   string
	// CompressionDefault 为实例的 backup compression default；读不到时为空。
	CompressionDefault string
}

// CompressionCapabilities 按实例版本与许可判定各压缩选项是否可用。
//
// 判定必须是实时的：压缩能力取决于 Edition（Express/Web 无备份压缩许可）与
// 主版本号（ZSTD 从 SQL Server 2025 / 17.x 起提供），任何按引擎名硬编码的结论
// 都会在别的实例上变成假承诺。探测失败不做乐观假设——返回 unverified，
// 界面据此禁用选项，而不是让它写进任务后才在备份时失败。
func (c SQLServerClient) CompressionCapabilities(ctx context.Context, source metadata.Source, password string) (map[string]CompressionCapability, error) {
	args := c.connectionArgs(source)
	args = append(args, "-d", "master", "-b", "-r", "1", "-W", "-s", "|", "-h", "-1", "-Q", instanceProfileQuery)
	output, err := c.run(ctx, args, password, "")
	if err != nil {
		return nil, err
	}
	return compressionCapabilitiesFromProfile(parseInstanceProfile(output))
}

// CompressionDefault 返回实例的 backup compression default（"1"/"0"）。
//
// 单独查询而不是复用能力探测结果：能力探测失败时仍要尽量显示默认值，
// 而默认值查询失败只需要让界面显示「未读取到」，不影响能力结论。
func (c SQLServerClient) CompressionDefault(ctx context.Context, source metadata.Source, password string) (string, error) {
	args := c.connectionArgs(source)
	query := "SET NOCOUNT ON; SELECT CAST(value_in_use AS nvarchar(16)) FROM sys.configurations WHERE name = 'backup compression default';"
	args = append(args, "-d", "master", "-b", "-r", "1", "-W", "-h", "-1", "-Q", query)
	output, err := c.run(ctx, args, password, "")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// compressionCapabilitiesFromProfile 把实例事实翻译成能力结论，便于脱离数据库单测。
func compressionCapabilitiesFromProfile(profile instanceProfile) (map[string]CompressionCapability, error) {
	result := make(map[string]CompressionCapability, 5)
	for _, option := range []string{CompressionOff, CompressionOn, CompressionZSTDLow, CompressionZSTDMed, CompressionZSTDHi} {
		result[option] = CompressionCapability{Status: "unverified"}
	}
	// 默认值 ""（不指定）永远可选：它保持历史语句不变，是回退路径而不是新能力。
	result[CompressionDefault] = CompressionCapability{Status: "supported"}

	if profile.Edition == "" || profile.MajorVersion <= 0 {
		reason := "无法读取实例版本或版本号，不能确认是否支持备份压缩"
		if profile.Edition == "" {
			reason = "无法读取实例 Edition，不能确认是否具备备份压缩许可"
		}
		for option := range result {
			if option == CompressionDefault {
				continue
			}
			result[option] = CompressionCapability{Status: "unverified", Reason: reason}
		}
		return result, nil
	}

	licensed := editionHasBackupCompression(profile.Edition)
	// NO_COMPRESSION 不需要压缩许可，但 Express/Web 上也没有任何理由提供它，
	// 统一按「该实例不支持备份压缩」处理，避免界面出现一个永远不会被选中的开关。
	if !licensed {
		reason := "实例 Edition 为 " + profile.Edition + "，不带备份压缩许可；SQL Server 备份压缩需要 Enterprise、Standard、Developer 或 Evaluation"
		for option := range result {
			if option == CompressionDefault {
				continue
			}
			result[option] = CompressionCapability{Status: "unsupported", Reason: reason}
		}
		return result, nil
	}

	result[CompressionOff] = CompressionCapability{Status: "supported"}
	result[CompressionOn] = CompressionCapability{Status: "supported"}
	if profile.MajorVersion >= 17 {
		for _, option := range []string{CompressionZSTDLow, CompressionZSTDMed, CompressionZSTDHi} {
			result[option] = CompressionCapability{Status: "supported"}
		}
		return result, nil
	}
	reason := fmt.Sprintf("ZSTD 压缩算法自 SQL Server 2025（17.x）起提供，当前实例主版本为 %d（%s）", profile.MajorVersion, strings.TrimSpace(profile.ProductVer))
	for _, option := range []string{CompressionZSTDLow, CompressionZSTDMed, CompressionZSTDHi} {
		result[option] = CompressionCapability{Status: "unsupported", Reason: reason}
	}
	return result, nil
}

// editionHasBackupCompression 判断 Edition 是否带备份压缩许可。
//
// 只做前置正向判断，遇到不认识的 Edition 返回 false 并落到 unsupported，
// 让用户看到明确原因后再决定，而不是默认允许后由 BACKUP 语句报错。
func editionHasBackupCompression(edition string) bool {
	upper := strings.ToUpper(edition)
	for _, keyword := range []string{"ENTERPRISE", "STANDARD", "DEVELOPER", "EVALUATION"} {
		if strings.Contains(upper, keyword) {
			return true
		}
	}
	return false
}

// parseInstanceProfile 解析 instanceProfileQuery 的竖线分隔输出。
//
// 字段可能为空（如低版本没有 ProductMajorVersion），因此按位置取而不是跳过空行，
// 否则会把后面的字段整体前移，得出错误的版本号。
func parseInstanceProfile(output string) instanceProfile {
	line := strings.TrimSpace(output)
	if line == "" {
		return instanceProfile{}
	}
	fields := strings.Split(line, "|")
	value := func(index int) string {
		if index >= len(fields) {
			return ""
		}
		text := strings.TrimSpace(fields[index])
		if strings.EqualFold(text, "NULL") {
			return ""
		}
		return text
	}
	profile := instanceProfile{
		Edition:            value(0),
		ProductVer:         value(2),
		CompressionDefault: value(3),
	}
	if major, err := strconv.Atoi(value(1)); err == nil && major > 0 {
		profile.MajorVersion = major
	} else if dot := strings.Index(profile.ProductVer, "."); dot > 0 {
		// 回退：ProductVersion 形如 15.0.4322.2，首段即主版本号。
		if major, err := strconv.Atoi(profile.ProductVer[:dot]); err == nil && major > 0 {
			profile.MajorVersion = major
		}
	}
	return profile
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
	// 服务器本地模式由数据库服务自己写盘，文件名必须用 runner 给的
	// <数据库名>-<YYYYMMDDHHmmss>；workPath 是执行器临时目录（dbbak-run-xxxx），
	// 拿它的 basename 当文件名会让服务器上出现与备份对象毫无关系的随机名。
	name := spec.FileName + extension
	if spec.FileName == "" {
		// 兜底：适配器被单独调用（测试或未来的远程 Agent）时退回原行为，避免空文件名。
		name = filepath.Base(workPath) + extension
	}
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
	statement, err := backupStatement(kind, source.Database, serverOutput, spec.Compress)
	if err != nil {
		return ExecuteResult{}, err
	}
	scriptPath := filepath.Join(workPath, "backup.sql")
	if err = os.WriteFile(scriptPath, []byte(statement), 0o600); err != nil {
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
	if len(parts) != 7 {
		return ExecuteResult{}, &ToolError{Code: "BACKUP_METADATA_MISSING", Message: "无法读取 SQL Server 备份 LSN 元数据"}
	}
	backupSize, _ := strconv.ParseInt(strings.TrimSpace(parts[5]), 10, 64)
	// compressed_backup_size 是备份集在介质上占用的大小。未启用压缩时它与
	// backup_size 相等；启用压缩后 backup_size 仍是未压缩的逻辑大小，
	// 直接拿它当文件大小会让界面的文件大小和保留清理的预计释放空间虚高。
	compressedSize, _ := strconv.ParseInt(strings.TrimSpace(parts[6]), 10, 64)
	if compressedSize <= 0 {
		compressedSize = backupSize
	}
	engineMetadata, _ := json.Marshal(map[string]any{
		"logical": false, "scope": "database", "format": strings.TrimPrefix(extension, "."), "backup_type": kind,
		"backup_set_id":          strings.TrimSpace(parts[0]),
		"first_lsn":              strings.TrimSpace(parts[1]),
		"last_lsn":               strings.TrimSpace(parts[2]),
		"database_backup_lsn":    strings.TrimSpace(parts[3]),
		"checkpoint_lsn":         strings.TrimSpace(parts[4]),
		"backup_size":            backupSize,
		"compressed_backup_size": compressedSize,
		// 算法必须随备份一起记录：ZSTD 备份无法在 2022 及更早实例上还原，
		// 排查「为什么这份文件恢复不了」时只能靠这里，别无其他线索。
		"compression":            spec.Compress,
		"compression_algorithm":  compressionAlgorithm(spec.Compress),
		"server_local":           serverLocal,
		"server_path":            serverOutput,
	})
	producedPath := localOutput
	if serverLocal {
		// 空 ProducedPath 告知执行器：文件在数据库服务器上，跳过本地哈希校验。
		producedPath = ""
	}
	return ExecuteResult{ProducedPath: producedPath, Extension: extension, Method: "sqlserver_native", EngineMetadata: engineMetadata}, nil
}

// backupStatement 生成对应类型的 BACKUP 语句；日志备份使用 BACKUP LOG。
//
// 压缩子句只在任务显式指定时出现：空串保持历史语句逐字节不变，
// 因为现有 SQL Server 全量、差异、日志链式恢复的验收结论都基于 WITH CHECKSUM。
func backupStatement(kind, database, destination, compress string) (string, error) {
	clause, err := compressionClauses(compress)
	if err != nil {
		return "", &ToolError{Code: "COMPRESSION_OPTION_INVALID", Message: err.Error()}
	}
	verb := "DATABASE"
	options := make([]string, 0, 3)
	if kind == "log" {
		verb = "LOG"
	} else if kind == "differential" {
		options = append(options, "DIFFERENTIAL")
	}
	if clause != "" {
		options = append(options, clause)
	}
	// CHECKSUM 始终保留：它是文件完整性校验的前提，不能为了压缩而牺牲。
	options = append(options, "CHECKSUM")
	return fmt.Sprintf("BACKUP %s [%s] TO DISK = N'%s' WITH %s;", verb, database, destination, strings.Join(options, ", ")), nil
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
// backup_size 是未压缩的逻辑大小；compressed_backup_size 才是介质上实际占用，
// 服务器本地备份必须用后者，否则启用压缩后登记的文件大小会虚高。
func backupMetadataQuery(database, kind string) string {
	// D=全量，I=差异，L=事务日志；类型必须与本次备份一致，否则会误取历史记录。
	setType := map[string]string{"full": "D", "differential": "I", "log": "L"}[kind]
	return fmt.Sprintf("SELECT TOP (1) backup_set_id, first_lsn, last_lsn, database_backup_lsn, checkpoint_lsn, backup_size, compressed_backup_size "+
		"FROM msdb.dbo.backupset WHERE database_name = '%s' AND type = '%s' ORDER BY backup_set_id DESC;", database, setType)
}

// compressionAlgorithm 把压缩选项映射为写入 EngineMetadata 的算法名。
//
// 空串表示未指定子句、算法由实例决定（MS_XPRESS），因此不能写成 xpress：
// 那样会把「实例默认」固化成错误的事实。
func compressionAlgorithm(compress string) string {
	switch compress {
	case CompressionOn:
		return "server_default"
	case CompressionZSTDLow, CompressionZSTDMed, CompressionZSTDHi:
		return "zstd"
	case CompressionOff:
		return "none"
	default:
		return ""
	}
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
