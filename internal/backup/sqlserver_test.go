package backup

import (
	"context"
	"path/filepath"
	"regexp"
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
