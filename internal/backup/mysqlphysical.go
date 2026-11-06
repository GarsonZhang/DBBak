package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"dbbak/internal/metadata"
)

// MySQLPhysicalClient 是 MySQL 的物理备份路线：Percona XtraBackup 全量 + mysqlbinlog 归档。
//
// 与逻辑路线（mysqldump）的根本差异有两点，代码必须体现而不是隐藏：
//  1. XtraBackup 直接读取 MySQL 数据目录，因此前提是执行器与实例同机；
//     跨主机必须依赖远程 Agent（阶段四），此处不做静默降级。
//  2. 产物是物理数据页而非 SQL 文本，恢复时必须先 --prepare 再 --copy-back，
//     因此 engine_metadata 记录 prepared=false 与恢复步骤，不能与逻辑备份混为一谈。
//
// 本实现按 Percona XtraBackup 8.x 的命令行接口编写，只提供 Linux 二进制；
// 在 Windows 上探测必然失败，能力矩阵据此标为 unsupported，而不是"暂时不可用"。
type MySQLPhysicalClient struct {
	XtraBackupPath string
	MySQLPath      string
	BinlogPath     string
}

// Engine 返回注册引擎名；物理与逻辑共用 mysql，靠 Method 区分路线。
func (c MySQLPhysicalClient) Engine() string { return "mysql" }

// Method 声明物理路线，与逻辑路线的 mysqldump 明确分离。
func (c MySQLPhysicalClient) Method() string { return "physical" }

// Version 返回 xtrabackup 的真实版本首行，供能力矩阵记录工具版本。
func (c MySQLPhysicalClient) Version(ctx context.Context) (string, error) {
	output, err := c.runTool(ctx, c.XtraBackupPath, []string{"--version"}, "")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(output, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed, nil
		}
	}
	return "", &ToolError{Code: "TOOL_VERSION_UNAVAILABLE", Message: "xtrabackup 未输出版本信息"}
}

// Probe 校验物理路线的真实前提：连接、同机数据目录、版本对齐与 binlog 状态。
//
// 只检查能在备份前可靠判断的条件；账号权限由真实备份失败给出准确错误，
// 不在这里用 SHOW GRANTS 的字符串匹配伪造一个看似严谨的结论。
func (c MySQLPhysicalClient) Probe(ctx context.Context, source metadata.Source, password string) (string, error) {
	detail, err := c.serverDetail(ctx, source, password)
	if err != nil {
		return "", err
	}
	if err = checkLocalDatadir(detail.Datadir); err != nil {
		return "", err
	}
	toolVersion, err := c.Version(ctx)
	if err != nil {
		return "", err
	}
	toolMajor := majorVersion(xtrabackupVersion(toolVersion))
	serverMajor := majorVersion(detail.Version)
	if toolMajor == "" || serverMajor == "" {
		return "", &ToolError{Code: "VERSION_UNPARSEABLE", Message: "无法解析服务端或 xtrabackup 版本：" + detail.Version + " / " + toolVersion}
	}
	if toolMajor != serverMajor {
		return "", &ToolError{
			Code: "VERSION_MISMATCH",
			Message: fmt.Sprintf("xtrabackup %s 与 MySQL %s 主版本不一致；物理备份要求主版本严格对齐（8.0 用 XtraBackup 8.0，8.4 用 8.4）",
				toolMajor, serverMajor),
		}
	}
	binlogState := "log_bin=OFF（只能做全量，无法归档 binlog）"
	if detail.LogBin {
		binlogState = "log_bin=ON; binlog_format=" + detail.BinlogFormat
	}
	return fmt.Sprintf("MySQL %s; xtrabackup %s; datadir=%s; %s", detail.Version, toolMajor, detail.Datadir, binlogState), nil
}

// Execute 按备份类型分派；未实现的类型明确失败，不用其他类型冒充。
func (c MySQLPhysicalClient) Execute(ctx context.Context, source metadata.Source, _ metadata.StorageTarget, password, workPath string, spec BackupSpec) (ExecuteResult, error) {
	switch spec.Type {
	case "", "full":
		return c.backupFull(ctx, source, password, workPath)
	case "log":
		return c.archiveBinlog(ctx, source, password, workPath, spec.Base)
	case "differential":
		// XtraBackup 的增量必须逐级 prepare 并保留每级 checkpoints，
		// 当前元数据模型还没有承载这条链，宁可明确拒绝，也不重复全量伪装成增量。
		return ExecuteResult{}, &ToolError{
			Code:    "BACKUP_TYPE_UNSUPPORTED",
			Message: "MySQL 物理路线当前只实现全量与 binlog 归档；XtraBackup 增量链（--incremental-basedir 与逐级 prepare）未实现",
		}
	default:
		return ExecuteResult{}, &ToolError{Code: "BACKUP_TYPE_UNSUPPORTED", Message: "未知的备份类型 " + spec.Type}
	}
}

// backupFull 以 --stream=xbstream 输出单个物理备份流，避免目录型产物打散现有单文件管道。
//
// 选 --stream 而不是 --target-dir 目录，是为了让产物仍是"一个文件"，
// 从而复用现有的哈希校验、FTP/OSS 上传与清理逻辑；--parallel 在 stream 模式下不生效，
// 因此不传，避免让使用者误以为能加速。
func (c MySQLPhysicalClient) backupFull(ctx context.Context, source metadata.Source, password, workPath string) (ExecuteResult, error) {
	detail, err := c.serverDetail(ctx, source, password)
	if err != nil {
		return ExecuteResult{}, err
	}
	if err = checkLocalDatadir(detail.Datadir); err != nil {
		return ExecuteResult{}, err
	}
	if err = os.MkdirAll(workPath, 0o700); err != nil {
		return ExecuteResult{}, &ToolError{Code: "STORAGE_ERROR", Message: "无法创建备份工作目录: " + err.Error()}
	}
	credPath, cleanupCred, err := writeClientCredentials(workPath, source.Username, password)
	if err != nil {
		return ExecuteResult{}, err
	}
	defer cleanupCred()

	targetDir := filepath.Join(workPath, "xtrabackup-target")
	lsnDir := filepath.Join(workPath, "xtrabackup-lsn")
	for _, dir := range []string{targetDir, lsnDir} {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return ExecuteResult{}, &ToolError{Code: "STORAGE_ERROR", Message: "无法创建 XtraBackup 工作目录: " + err.Error()}
		}
	}
	produced := filepath.Join(workPath, "backup.xbstream")
	args := []string{
		"--defaults-extra-file=" + credPath,
		"--backup",
		"--stream=xbstream",
		"--target-dir=" + targetDir,
		// 流式模式下 checkpoints 与 binlog 位点不会落到磁盘目录，
		// 必须用 --extra-lsndir 单独留存，否则恢复链无法确定起点。
		"--extra-lsndir=" + lsnDir,
		"--datadir=" + detail.Datadir,
		"--host=" + source.Host,
		fmt.Sprintf("--port=%d", source.Port),
	}
	if err = c.runStreaming(ctx, c.XtraBackupPath, args, produced, password); err != nil {
		return ExecuteResult{}, err
	}
	info, statErr := os.Stat(produced)
	if statErr != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return ExecuteResult{}, &ToolError{Code: "BACKUP_OUTPUT_EMPTY", Message: "xtrabackup 返回成功但备份流为空，已拒绝登记为可用备份"}
	}

	checkpoints := readCheckpointFile(lsnDir)
	binlogFile, binlogPos, gtid, binlogOK := readBinlogInfo(lsnDir)
	if !binlogOK {
		// 没有 binlog 位点不阻止全量备份成立，但必须记录，否则后续 PITR 会误以为可用。
		binlogFile, binlogPos, gtid = "", 0, ""
	}
	engineMetadata, err := json.Marshal(map[string]any{
		"logical":        false,
		"scope":          "instance",
		"format":         "xbstream",
		"tool":           "xtrabackup",
		"prepared":       false,
		"datadir":        detail.Datadir,
		"checkpoints":    checkpoints,
		"to_lsn":         checkpoints["to_lsn"],
		"from_lsn":       checkpoints["from_lsn"],
		"binlog_file":    binlogFile,
		"binlog_pos":     binlogPos,
		"gtid_executed":  gtid,
		"binlog_capture": binlogOK,
		"restore_steps":  []string{"xbstream -x -C <dir>", "xtrabackup --prepare --target-dir=<dir>", "xtrabackup --copy-back --target-dir=<dir>"},
	})
	if err != nil {
		return ExecuteResult{}, &ToolError{Code: "METADATA_ENCODE_FAILED", Message: "无法编码物理备份元数据: " + err.Error()}
	}
	return ExecuteResult{ProducedPath: produced, Extension: ".xbstream", Method: "xtrabackup", EngineMetadata: engineMetadata}, nil
}

// archiveBinlog 把基线位点之后、当前活动文件之前的 binlog 归档为单个 tar。
//
// 一次运行只做一次"点到点"归档，不启动 --stop-never 常驻进程：
// 本产品的运行模型是一次性 run，常驻归档需要独立的服务生命周期，属于后续设计。
// 起点取上一份对象的终点（日志接日志）或全量记录的位点（日志接全量），
// 因此连续运行不会重复归档同一段 binlog。
func (c MySQLPhysicalClient) archiveBinlog(ctx context.Context, source metadata.Source, password, workPath string, base *metadata.Artifact) (ExecuteResult, error) {
	if base == nil {
		return ExecuteResult{}, &ToolError{Code: "NO_BASE_BACKUP", Message: "binlog 归档需要一份已验证的物理全量基线，请先执行物理全量备份"}
	}
	fromFile, fromPos, err := binlogStart(base)
	if err != nil {
		return ExecuteResult{}, err
	}
	if err = os.MkdirAll(workPath, 0o700); err != nil {
		return ExecuteResult{}, &ToolError{Code: "STORAGE_ERROR", Message: "无法创建备份工作目录: " + err.Error()}
	}
	credPath, cleanupCred, err := writeClientCredentials(workPath, source.Username, password)
	if err != nil {
		return ExecuteResult{}, err
	}
	defer cleanupCred()

	files, err := c.binaryLogs(ctx, source, credPath)
	if err != nil {
		return ExecuteResult{}, err
	}
	// 先取当前位点再复制文件：记录的位置一定是已复制内容的下界，
	// 活动文件在复制期间的增量不会被错误地计入恢复承诺。
	activeFile, activePos, gtid, err := c.masterStatus(ctx, source, credPath)
	if err != nil {
		return ExecuteResult{}, err
	}
	start := -1
	for index, name := range files {
		if name == fromFile {
			start = index
			break
		}
	}
	if start < 0 {
		return ExecuteResult{}, &ToolError{
			Code:    "BINLOG_PURGED",
			Message: "基线位点所在的 binlog " + fromFile + " 已在服务器上清除，恢复链出现断点；请重新执行物理全量备份",
		}
	}
	selected := make([]string, 0, len(files)-start)
	for _, name := range files[start:] {
		selected = append(selected, name)
		if name == activeFile {
			break
		}
	}

	rawDir := filepath.Join(workPath, "binlog-raw")
	if err = os.MkdirAll(rawDir, 0o700); err != nil {
		return ExecuteResult{}, &ToolError{Code: "STORAGE_ERROR", Message: "无法创建 binlog 工作目录: " + err.Error()}
	}
	for _, name := range selected {
		// --raw 配合末尾带分隔符的 --result-file，按原始文件名落盘，不做 SQL 解码，
		// 保证归档文件与源文件字节一致，恢复时可直接作为 binlog 文件使用。
		args := []string{
			"--defaults-extra-file=" + credPath,
			"--read-from-remote-server",
			"--raw",
			"--host=" + source.Host,
			fmt.Sprintf("--port=%d", source.Port),
			"--result-file=" + rawDir + string(os.PathSeparator),
			name,
		}
		if _, err = c.runTool(ctx, c.BinlogPath, args, password); err != nil {
			return ExecuteResult{}, err
		}
	}
	produced := filepath.Join(workPath, "binlog.tar")
	if err = packDirectory(rawDir, produced); err != nil {
		return ExecuteResult{}, &ToolError{Code: "STORAGE_ERROR", Message: "无法打包 binlog: " + err.Error()}
	}
	_ = os.RemoveAll(rawDir)
	info, statErr := os.Stat(produced)
	if statErr != nil || info.Size() == 0 {
		return ExecuteResult{}, &ToolError{Code: "BACKUP_OUTPUT_EMPTY", Message: "binlog 归档为空，请检查 log_bin 是否开启以及基线位点是否有效"}
	}

	engineMetadata, err := json.Marshal(map[string]any{
		"logical":        false,
		"scope":          "binlog",
		"format":         "tar",
		"tool":           "mysqlbinlog",
		"from_file":      fromFile,
		"from_pos":       fromPos,
		"to_file":        activeFile,
		"to_pos":         activePos,
		"gtid_executed":  gtid,
		"files":          selected,
		"active_partial": true,
		"restore_steps":  []string{"解包后放入 datadir 可读目录", "mysqlbinlog --start-position=<from_pos> <from_file> | mysql", "按文件名顺序依次重放至 to_file:to_pos"},
	})
	if err != nil {
		return ExecuteResult{}, &ToolError{Code: "METADATA_ENCODE_FAILED", Message: "无法编码 binlog 归档元数据: " + err.Error()}
	}
	return ExecuteResult{ProducedPath: produced, Extension: ".tar", Method: "mysqlbinlog", EngineMetadata: engineMetadata}, nil
}

// serverDetail 是同机判断与版本校验共用的服务端事实集合。
type serverDetail struct {
	Version      string
	LogBin       bool
	Datadir      string
	BinlogFormat string
}

func (c MySQLPhysicalClient) serverDetail(ctx context.Context, source metadata.Source, password string) (serverDetail, error) {
	query := "SELECT VERSION(), @@log_bin, @@datadir, @@binlog_format"
	output, err := c.query(ctx, source, password, query)
	if err != nil {
		return serverDetail{}, err
	}
	fields := strings.Split(strings.TrimSpace(output), "\t")
	if len(fields) < 4 {
		return serverDetail{}, &ToolError{Code: "PROBE_INVALID_RESPONSE", Message: "MySQL 能力探测返回格式无效"}
	}
	return serverDetail{
		Version:      strings.TrimSpace(fields[0]),
		LogBin:       strings.TrimSpace(fields[1]) == "1",
		Datadir:      strings.TrimSpace(fields[2]),
		BinlogFormat: strings.TrimSpace(fields[3]),
	}, nil
}

// query 通过官方 mysql 客户端执行只读查询，凭据走临时选项文件而不是命令行。
func (c MySQLPhysicalClient) query(ctx context.Context, source metadata.Source, password, statement string) (string, error) {
	workDir, err := os.MkdirTemp("", "dbbak-phys-query-")
	if err != nil {
		return "", &ToolError{Code: "STORAGE_ERROR", Message: "无法创建临时目录: " + err.Error()}
	}
	defer os.RemoveAll(workDir)
	credPath, cleanupCred, err := writeClientCredentials(workDir, source.Username, password)
	if err != nil {
		return "", err
	}
	defer cleanupCred()
	args := []string{
		"--defaults-extra-file=" + credPath,
		"--protocol=TCP",
		"--host=" + source.Host,
		fmt.Sprintf("--port=%d", source.Port),
		"--connect-timeout=8",
		"--batch",
		"--skip-column-names",
		"--execute=" + statement,
	}
	return c.runTool(ctx, c.MySQLPath, args, password)
}

// binaryLogs 返回服务器上仍存在的 binlog 文件名，按服务器给出的顺序（即序号顺序）。
func (c MySQLPhysicalClient) binaryLogs(ctx context.Context, source metadata.Source, credPath string) ([]string, error) {
	output, err := c.queryWithCredentials(ctx, source, credPath, "SHOW BINARY LOGS")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, 16)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		names = append(names, strings.TrimSpace(fields[0]))
	}
	if len(names) == 0 {
		return nil, &ToolError{Code: "PREREQUISITE_MISSING", Message: "服务器没有可用 binlog；请确认 log_bin=ON 且尚未清除"}
	}
	return names, nil
}

// masterStatus 读取当前 binlog 位点与已执行 GTID。
// MySQL 8.2 起 SHOW MASTER STATUS 更名为 SHOW BINARY LOG STATUS，保留旧名回退以兼容 8.0。
func (c MySQLPhysicalClient) masterStatus(ctx context.Context, source metadata.Source, credPath string) (string, int64, string, error) {
	output, err := c.queryWithCredentials(ctx, source, credPath, "SHOW BINARY LOG STATUS")
	if err != nil {
		output, err = c.queryWithCredentials(ctx, source, credPath, "SHOW MASTER STATUS")
		if err != nil {
			return "", 0, "", err
		}
	}
	line := strings.TrimSpace(strings.Split(strings.TrimSpace(output), "\n")[0])
	fields := strings.Split(line, "\t")
	if len(fields) < 2 {
		return "", 0, "", &ToolError{Code: "PROBE_INVALID_RESPONSE", Message: "无法读取当前 binlog 位点"}
	}
	position, parseErr := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
	if parseErr != nil {
		return "", 0, "", &ToolError{Code: "PROBE_INVALID_RESPONSE", Message: "binlog 位点不是有效整数"}
	}
	gtid := ""
	if len(fields) >= 5 {
		gtid = strings.TrimSpace(fields[4])
	}
	return strings.TrimSpace(fields[0]), position, gtid, nil
}

// queryWithCredentials 复用已写好的临时凭据文件，避免每条语句都重建目录。
func (c MySQLPhysicalClient) queryWithCredentials(ctx context.Context, source metadata.Source, credPath, statement string) (string, error) {
	args := []string{
		"--defaults-extra-file=" + credPath,
		"--protocol=TCP",
		"--host=" + source.Host,
		fmt.Sprintf("--port=%d", source.Port),
		"--connect-timeout=8",
		"--batch",
		"--skip-column-names",
		"--execute=" + statement,
	}
	return c.runTool(ctx, c.MySQLPath, args, "")
}

// runTool 执行一次性工具调用并返回 stdout；错误按真实原因分类，不合并成"失败"。
func (c MySQLPhysicalClient) runTool(ctx context.Context, path string, args []string, password string) (string, error) {
	command := exec.CommandContext(ctx, path, args...)
	var stdout, stderr cappedBuffer
	stderr.max = 64 << 10
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), nil
	}
	return stdout.String(), classifyToolError(ctx, err, stderr.String(), password, path)
}

// runStreaming 把工具 stdout 直接写入文件，避免几十 GB 的备份流经过内存。
func (c MySQLPhysicalClient) runStreaming(ctx context.Context, path string, args []string, outputPath, password string) error {
	file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return &ToolError{Code: "STORAGE_ERROR", Message: "无法创建备份流文件: " + err.Error()}
	}
	command := exec.CommandContext(ctx, path, args...)
	var stderr cappedBuffer
	stderr.max = 64 << 10
	command.Stdout = file
	command.Stderr = &stderr
	runErr := command.Run()
	closeErr := file.Close()
	if runErr != nil {
		// 失败的中途流不保留：半截物理备份既不能恢复，也会在清理时被误认成对象。
		_ = os.Remove(outputPath)
		return classifyToolError(ctx, runErr, stderr.String(), password, path)
	}
	if closeErr != nil {
		return &ToolError{Code: "STORAGE_ERROR", Message: "备份流写入磁盘失败: " + closeErr.Error()}
	}
	return nil
}

// classifyToolError 把工具失败原因映射成可展示的错误码。
//
// 大库场景下最常见的失败是超时与磁盘不足，两者必须能被运维一眼区分，
// 因此单独分类，而不是统一成 BACKUP_COMMAND_FAILED。
func classifyToolError(ctx context.Context, err error, stderr, password, toolPath string) error {
	message := strings.TrimSpace(stderr)
	lower := strings.ToLower(message)
	code := "BACKUP_COMMAND_FAILED"
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		code, message = "BACKUP_TIMEOUT", "备份工具执行超过任务超时；大库请提高 DBBAK_BACKUP_TIMEOUT 后重试"
	case errors.Is(err, exec.ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "executable file not found"):
		code, message = "TOOL_MISSING", "找不到备份工具 " + toolPath + "，请检查 DBBAK_XTRABACKUP 与 DBBAK_MYSQLBINLOG"
	case strings.Contains(lower, "access denied"):
		code = "AUTH_FAILED"
	case strings.Contains(lower, "can't connect") || strings.Contains(lower, "connection refused") || strings.Contains(lower, "timed out"):
		code = "NETWORK_ERROR"
	case strings.Contains(lower, "cannot open data file") || strings.Contains(lower, "failed to open") && strings.Contains(lower, "datadir"):
		code = "PREREQUISITE_MISSING"
	case strings.Contains(lower, "unknown variable") || strings.Contains(lower, "unknown option"):
		code = "TOOL_INCOMPATIBLE"
	case strings.Contains(lower, "no space left") || strings.Contains(lower, "disk full"):
		code = "STORAGE_FULL"
	case strings.Contains(lower, "binary logging not enabled") || strings.Contains(lower, "log_bin is disabled"):
		code = "PREREQUISITE_MISSING"
	}
	if message == "" {
		message = err.Error()
	}
	return &ToolError{Code: code, Message: sanitizeMessage(message, password)}
}

// cappedBuffer 只保留前 max 字节工具输出，防止长任务把 stderr 无界堆进内存。
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.max <= 0 {
		return len(p), nil
	}
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		_, _ = b.buf.Write(p)
	}
	// 始终报告全部写入，否则 os/exec 会因短写中断管道读取。
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

// writeClientCredentials 生成只读一次即删除的 MySQL 选项文件。
// 密码不进命令行、不进环境变量，避免出现在进程列表或诊断输出里。
func writeClientCredentials(workPath, username, password string) (string, func(), error) {
	if strings.ContainsAny(username, "\r\n\"") || strings.ContainsAny(password, "\r\n") {
		return "", nil, &ToolError{Code: "SECRET_UNUSABLE", Message: "账号或密码包含换行或引号，无法安全写入临时凭据文件"}
	}
	path := filepath.Join(workPath, "client-credentials.cnf")
	content := "[client]\nuser=" + cnfQuote(username) + "\npassword=" + cnfQuote(password) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", nil, &ToolError{Code: "STORAGE_ERROR", Message: "无法写入临时凭据文件: " + err.Error()}
	}
	return path, func() { _ = os.Remove(path) }, nil
}

func cnfQuote(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

// checkLocalDatadir 判断数据目录是否就在执行器本机。
// 这一条是物理路线的硬前提，失败时必须说清楚，而不是让 xtrabackup 抛出难以理解的 I/O 错误。
func checkLocalDatadir(datadir string) error {
	if datadir == "" {
		return &ToolError{Code: "PREREQUISITE_MISSING", Message: "无法读取服务端 datadir，物理备份前提不成立"}
	}
	info, err := os.Stat(datadir)
	if err != nil || !info.IsDir() {
		return &ToolError{
			Code: "PREREQUISITE_MISSING",
			Message: "执行器在本机看不到数据目录 " + datadir +
				"；Percona XtraBackup 直接读取数据文件，执行器必须与 MySQL 实例同机运行（跨主机需远程 Agent，当前未实现）",
		}
	}
	return nil
}

// readCheckpointFile 读取 --extra-lsndir 留存的 checkpoints，保留原始键值便于核对。
func readCheckpointFile(lsnDir string) map[string]string {
	result := map[string]string{}
	data, err := os.ReadFile(filepath.Join(lsnDir, "xtrabackup_checkpoints"))
	if err != nil {
		return result
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		result[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return result
}

// readBinlogInfo 读取 ${gtid}\t${file}\t${pos} 或 ${file}\t${pos}。
// 返回值 ok=false 表示本次备份没有采集到位点，恢复链不得声称支持时间点恢复。
func readBinlogInfo(lsnDir string) (string, int64, string, bool) {
	data, err := os.ReadFile(filepath.Join(lsnDir, "xtrabackup_binlog_info"))
	if err != nil {
		return "", 0, "", false
	}
	line := strings.TrimSpace(strings.Split(strings.TrimSpace(string(data)), "\n")[0])
	if line == "" {
		return "", 0, "", false
	}
	fields := strings.Split(line, "\t")
	var gtid, file, posText string
	switch len(fields) {
	case 3:
		gtid, file, posText = strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1]), strings.TrimSpace(fields[2])
	case 2:
		file, posText = strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1])
	default:
		return "", 0, "", false
	}
	if file == "" {
		return "", 0, "", false
	}
	position, parseErr := strconv.ParseInt(posText, 10, 64)
	if parseErr != nil {
		return "", 0, "", false
	}
	return file, position, gtid, true
}

// binlogStart 从基线对象解析 binlog 起点。
// 日志接日志时用上一段的终点，日志接全量时用全量记录的一致位点，
// 两种来源都不允许凭空推算。
func binlogStart(base *metadata.Artifact) (string, int64, error) {
	var meta map[string]any
	if err := json.Unmarshal(base.EngineMetadata, &meta); err != nil {
		return "", 0, &ToolError{Code: "BASE_BINLOG_UNKNOWN", Message: "基线对象的引擎元数据无法解析，不能确定 binlog 起点"}
	}
	readNumber := func(key string) int64 {
		value, ok := meta[key].(float64)
		if !ok {
			return 0
		}
		return int64(value)
	}
	if base.Type == "log" {
		if file, _ := meta["to_file"].(string); file != "" {
			return file, readNumber("to_pos"), nil
		}
	}
	file, _ := meta["binlog_file"].(string)
	if file == "" {
		return "", 0, &ToolError{
			Code:    "BASE_BINLOG_UNKNOWN",
			Message: "基线全量没有记录 binlog 位点（备份时 log_bin 可能未开启），无法从该基线归档 binlog",
		}
	}
	return file, readNumber("binlog_pos"), nil
}

// packDirectory 把目录内的普通文件按名字顺序打成单个 tar，保持单文件产物模型。
func packDirectory(dir, outputPath string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writer := tar.NewWriter(file)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		source := filepath.Join(dir, entry.Name())
		info, statErr := os.Stat(source)
		if statErr != nil || !info.Mode().IsRegular() {
			writer.Close()
			file.Close()
			return fmt.Errorf("binlog 归档项 %s 不是普通文件", entry.Name())
		}
		header, headerErr := tar.FileInfoHeader(info, "")
		if headerErr != nil {
			writer.Close()
			file.Close()
			return headerErr
		}
		header.Name = entry.Name()
		if err = writer.WriteHeader(header); err != nil {
			writer.Close()
			file.Close()
			return err
		}
		input, openErr := os.Open(source)
		if openErr != nil {
			writer.Close()
			file.Close()
			return openErr
		}
		_, copyErr := io.Copy(writer, input)
		input.Close()
		if copyErr != nil {
			writer.Close()
			file.Close()
			return copyErr
		}
	}
	if err = writer.Close(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// majorVersion 取 "8.4.13" 的 "8.4"，用于主版本对齐判断。
func majorVersion(version string) string {
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// xtrabackupVersion 从 "xtrabackup version 8.4.0-1 based on ..." 取版本号。
func xtrabackupVersion(line string) string {
	fields := strings.Fields(line)
	for index, field := range fields {
		if strings.EqualFold(field, "version") && index+1 < len(fields) {
			return fields[index+1]
		}
	}
	return ""
}
