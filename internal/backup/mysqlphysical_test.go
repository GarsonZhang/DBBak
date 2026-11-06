package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dbbak/internal/metadata"
)

// 同一引擎的两条路线必须能被分别选中，且默认路线仍是逻辑全量。
func TestRegistryResolvesMySQLRoutes(t *testing.T) {
	logical := MySQLClient{MySQLDumpPath: "mysqldump"}
	physical := MySQLPhysicalClient{XtraBackupPath: "xtrabackup"}
	registry := NewRegistry(logical, physical)

	adapter, err := registry.ForEngine("mysql")
	if err != nil {
		t.Fatalf("默认路线必须存在: %v", err)
	}
	if RouteMethod(adapter) != "logical" {
		t.Fatalf("MySQL 默认路线应为 logical，实际 %q", RouteMethod(adapter))
	}

	adapter, err = registry.ForRoute("mysql", "physical")
	if err != nil {
		t.Fatalf("物理路线应可选中: %v", err)
	}
	if RouteMethod(adapter) != "physical" {
		t.Fatalf("显式路线解析错误: %q", RouteMethod(adapter))
	}

	// 显式指定未实现的路线必须报错，不能悄悄回退到逻辑全量。
	if _, err = registry.ForRoute("mysql", "increment"); err == nil {
		t.Fatal("未实现的路线必须返回错误，而不是回退默认路线")
	}
	if len(registry.Routes()) != 2 {
		t.Fatalf("应输出两条 MySQL 路线，实际 %d", len(registry.Routes()))
	}
}

func TestMissingXtraBackupFailsClearly(t *testing.T) {
	client := MySQLPhysicalClient{XtraBackupPath: filepath.Join(t.TempDir(), "missing-xtrabackup")}
	_, err := client.Version(context.Background())
	if err == nil || ErrorCode(err) != "TOOL_MISSING" {
		t.Fatalf("缺失工具必须返回 TOOL_MISSING: %v", err)
	}
}

// 增量链未实现时必须明确拒绝，不能用重复全量伪装成差异/增量。
func TestPhysicalRejectsUnimplementedBackupTypes(t *testing.T) {
	client := MySQLPhysicalClient{XtraBackupPath: "xtrabackup"}
	_, err := client.Execute(context.Background(), metadata.Source{Database: "shop"}, metadata.StorageTarget{}, "", t.TempDir(), BackupSpec{Type: "differential"})
	if err == nil || ErrorCode(err) != "BACKUP_TYPE_UNSUPPORTED" {
		t.Fatalf("差异备份应明确不支持: %v", err)
	}
	if !strings.Contains(err.Error(), "增量链") {
		t.Fatalf("错误信息应说明增量链未实现: %v", err)
	}
}

func TestBinlogStartFromLogAndFullArtifacts(t *testing.T) {
	logArtifact := metadata.Artifact{
		Type:           "log",
		EngineMetadata: json.RawMessage(`{"from_file":"binlog.000003","from_pos":120,"to_file":"binlog.000005","to_pos":4567}`),
	}
	file, position, err := binlogStart(&logArtifact)
	if err != nil || file != "binlog.000005" || position != 4567 {
		t.Fatalf("日志接日志应取上一段终点，实际 %s:%d %v", file, position, err)
	}

	fullArtifact := metadata.Artifact{
		Type:           "full",
		EngineMetadata: json.RawMessage(`{"binlog_file":"binlog.000002","binlog_pos":987}`),
	}
	file, position, err = binlogStart(&fullArtifact)
	if err != nil || file != "binlog.000002" || position != 987 {
		t.Fatalf("日志接全量应取全量记录的一致位点，实际 %s:%d %v", file, position, err)
	}

	// 没有位点的基线不能凭空推算起点。
	empty := metadata.Artifact{Type: "full", EngineMetadata: json.RawMessage(`{}`)}
	if _, _, err = binlogStart(&empty); err == nil || ErrorCode(err) != "BASE_BINLOG_UNKNOWN" {
		t.Fatalf("缺位点的基线必须报 BASE_BINLOG_UNKNOWN: %v", err)
	}
}

func TestReadBinlogInfoVariants(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantF    string
		wantP    int64
		wantGTID string
		wantOK   bool
	}{
		{"带 GTID", "3e11fa47-71ca-11e1-9e33-c80aa9429562:1-5\tbinlog.000007\t1234\n", "binlog.000007", 1234, "3e11fa47-71ca-11e1-9e33-c80aa9429562:1-5", true},
		{"无 GTID", "binlog.000009\t88\n", "binlog.000009", 88, "", true},
		{"格式异常", "只写了一行说明\n", "", 0, "", false},
	}
	for _, testCase := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "xtrabackup_binlog_info"), []byte(testCase.content), 0o600); err != nil {
			t.Fatal(err)
		}
		file, position, gtid, ok := readBinlogInfo(dir)
		if ok != testCase.wantOK || file != testCase.wantF || position != testCase.wantP || gtid != testCase.wantGTID {
			t.Fatalf("%s: 解析结果 %s/%d/%q/%v", testCase.name, file, position, gtid, ok)
		}
	}
}

func TestVersionAlignmentParsing(t *testing.T) {
	line := "xtrabackup version 8.4.0-1 based on MySQL server 8.4.0 Linux (x86_64) (revision id: abcdef)"
	if got := majorVersion(xtrabackupVersion(line)); got != "8.4" {
		t.Fatalf("xtrabackup 版本主号应为 8.4，实际 %q", got)
	}
	if got := majorVersion("8.0.35-0ubuntu0.22.04.1"); got != "8.0" {
		t.Fatalf("MySQL 版本主号应为 8.0，实际 %q", got)
	}
	if got := majorVersion("8.4.13"); got != "8.4" {
		t.Fatalf("MySQL 8.4.13 主号应为 8.4，实际 %q", got)
	}
}

// 凭据必须写进临时选项文件并加引号，而不是拼进命令行。
func TestClientCredentialsFileQuotesValues(t *testing.T) {
	dir := t.TempDir()
	path, cleanup, err := writeClientCredentials(dir, "backup", `p@ss"word`)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		cleanup()
		t.Fatal(readErr)
	}
	text := string(content)
	if !strings.Contains(text, `password="p@ss\"word"`) {
		cleanup()
		t.Fatalf("凭据文件未正确转义: %s", text)
	}
	cleanup()
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("清理函数应删除临时凭据文件")
	}
	if _, _, err = writeClientCredentials(dir, "back\nup", "x"); err == nil {
		t.Fatal("含换行的账号必须被拒绝")
	}
}

// 执行器看不到数据目录时必须报前提缺失，而不是让 xtrabackup 抛 I/O 错误。
func TestCheckLocalDatadirRejectsRemotePath(t *testing.T) {
	if err := checkLocalDatadir(filepath.Join(t.TempDir(), "no-such-datadir")); err == nil || ErrorCode(err) != "PREREQUISITE_MISSING" {
		t.Fatalf("不存在的数据目录必须报前提缺失: %v", err)
	}
	if err := checkLocalDatadir(""); err == nil {
		t.Fatal("空数据目录必须报错")
	}
}
