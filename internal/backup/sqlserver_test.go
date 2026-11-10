package backup

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"dbbak/internal/metadata"
)

func TestSQLServerToolAndPathValidation(t *testing.T) {
	client := SQLServerClient{SQLCMDPath: filepath.Join(t.TempDir(), "missing-sqlcmd")}
	if _, err := client.Version(context.Background()); err == nil || ErrorCode(err) != "TOOL_MISSING" {
		t.Fatalf("缺失 sqlcmd 应返回 TOOL_MISSING: %v", err)
	}
	source := metadata.Source{Engine: "sqlserver", Host: `localhost\MSSQLSERVER`, Port: 1433, Database: "dbbak_test_source", Username: "integrated"}
	if _, err := client.Execute(context.Background(), source, metadata.StorageTarget{}, "", t.TempDir(), BackupSpec{Type: "full"}); err == nil || ErrorCode(err) != "SERVER_PATH_REQUIRED" {
		t.Fatalf("缺少数据库服务路径应明确失败: %v", err)
	}
}

// 服务器本地备份的文件名必须来自 BackupSpec.FileName（<数据库名>-<YYYYMMDDHHmmss>），
// 不能是执行器临时目录的 basename（dbbak-run-xxxx）——那个名字与备份对象毫无关系。
// Windows 上无法直接执行 shell 替身，因此用 Windows 侧只读校验：ServerRoot 造出来的目标路径
// 必须包含任务文件名。真正跑通仍以 Docker/Linux 环境的一次真实备份为准。
func TestSQLServerServerLocalUsesSpecFileName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 无法执行 shell 替身；该用例在 Linux/Docker 上执行")
	}
	workDir := t.TempDir()
	capture := filepath.Join(workDir, "captured.sql")
	// 替身只把 -i 指定的脚本原样存下来，不做真实连接。
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = \"-i\" ]; then cp \"$2\" " + capture + "; fi; shift; done\necho '1|2|3|4|5|1024'\n"
	toolPath := filepath.Join(workDir, "fake-sqlcmd.sh")
	if err := os.WriteFile(toolPath, []byte(script), 0o755); err != nil {
		t.Skipf("当前环境无法准备 sqlcmd 替身: %v", err)
	}
	client := SQLServerClient{SQLCMDPath: toolPath}
	source := metadata.Source{Engine: "sqlserver", Host: "127.0.0.1", Port: 1433, Database: "tce_prd", Username: "sa"}
	target := metadata.StorageTarget{Kind: "database_server", ServerRoot: `D:\bak`}
	if _, err := client.Execute(context.Background(), source, target, "p", workDir, BackupSpec{Type: "full", FileName: "tce_prd-20261110133700"}); err != nil {
		t.Fatalf("服务器本地备份不应失败: %v", err)
	}
	content, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("未截获 BACKUP 语句: %v", err)
	}
	if !strings.Contains(string(content), "tce_prd-20261110133700.bak") {
		t.Fatalf("备份目标未使用任务文件名: %s", content)
	}
	if strings.Contains(string(content), "dbbak-run-") {
		t.Fatalf("备份目标误用了执行器临时目录名: %s", content)
	}
}

func TestSQLServerConnectionModes(t *testing.T) {
	client := SQLServerClient{}
	trusted := strings.Join(client.connectionArgs(metadata.Source{Host: "localhost", Port: 1433, Username: "integrated"}), " ")
	if !strings.Contains(trusted, "-E") || strings.Contains(trusted, "-U") {
		t.Fatalf("受信任连接参数错误: %s", trusted)
	}
	sqlLogin := strings.Join(client.connectionArgs(metadata.Source{Host: "db.example", Port: 1433, Username: "dbbak_test"}), " ")
	if !strings.Contains(sqlLogin, "tcp:db.example,1433") || !strings.Contains(sqlLogin, "-U dbbak_test") {
		t.Fatalf("SQL 登录参数错误: %s", sqlLogin)
	}
}

// ODBC Driver 18 默认 Encrypt=yes，自签名证书必须靠 -C 才能连上；是否信任由数据源
// 决定，回归测试同时防止「该带不带」和「不该带给带上」两种错误。
func TestSQLServerConnectionCertificateTrust(t *testing.T) {
	client := SQLServerClient{}
	hasC := func(args []string) bool {
		for _, arg := range args {
			if arg == "-C" {
				return true
			}
		}
		return false
	}
	for _, source := range []metadata.Source{
		{Host: "localhost", Port: 1433, Username: "integrated", TrustServerCertificate: true},
		{Host: "db.example", Port: 1433, Username: "dbbak_test", TrustServerCertificate: true},
		{Host: `localhost\MSSQLSERVER`, TrustServerCertificate: true},
	} {
		if !hasC(client.connectionArgs(source)) {
			t.Fatalf("勾选信任时 %s 的连接参数缺少 -C", source.Host)
		}
	}
	// 未勾选必须严格校验证书：云上实例配了受信证书，不应被静默降级。
	untrusted := client.connectionArgs(metadata.Source{Host: "db.example", Port: 1433, Username: "dbbak_test"})
	if hasC(untrusted) {
		t.Fatalf("未勾选信任时不应带 -C: %v", untrusted)
	}
}

// msdb.dbo.backupset 没有 backup_lsn 列；回归测试防止再次写出无效查询。
func TestSQLServerBackupMetadataQueryUsesRealColumns(t *testing.T) {
	query := backupMetadataQuery("dbbak_test_source", "full")
	for _, column := range []string{"backup_set_id", "first_lsn", "last_lsn", "database_backup_lsn", "checkpoint_lsn"} {
		if !strings.Contains(query, column) {
			t.Fatalf("备份元数据查询缺少列 %s: %s", column, query)
		}
	}
	if regexp.MustCompile(`\bbackup_lsn\b`).MatchString(query) {
		t.Fatalf("备份元数据查询使用了不存在的 backup_lsn 列: %s", query)
	}
}

// 三种备份类型必须生成不同语句；日志不能写成 BACKUP DATABASE。
func TestSQLServerBackupStatements(t *testing.T) {
	destination := "C:/backup/x.bak"
	cases := map[string]string{
		"full":         "BACKUP DATABASE [db] TO DISK = N'C:/backup/x.bak' WITH CHECKSUM;",
		"differential": "BACKUP DATABASE [db] TO DISK = N'C:/backup/x.bak' WITH DIFFERENTIAL, CHECKSUM;",
		"log":          "BACKUP LOG [db] TO DISK = N'C:/backup/x.bak' WITH CHECKSUM;",
	}
	for kind, want := range cases {
		if got := backupStatement(kind, "db", destination); got != want {
			t.Fatalf("%s 语句错误: %s", kind, got)
		}
	}
	// 元数据查询必须按类型过滤，避免取到其他类型的 backupset 记录。
	if query := backupMetadataQuery("db", "log"); !strings.Contains(query, "type = 'L'") {
		t.Fatalf("日志元数据查询未按 L 过滤: %s", query)
	}
	if query := backupMetadataQuery("db", "differential"); !strings.Contains(query, "type = 'I'") {
		t.Fatalf("差异元数据查询未按 I 过滤: %s", query)
	}
}
