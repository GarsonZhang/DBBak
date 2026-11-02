package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"vault/internal/metadata"
)

func TestSQLServerToolAndPathValidation(t *testing.T) {
	client := SQLServerClient{SQLCMDPath: filepath.Join(t.TempDir(), "missing-sqlcmd")}
	if _, err := client.Version(context.Background()); err == nil || ErrorCode(err) != "TOOL_MISSING" {
		t.Fatalf("缺失 sqlcmd 应返回 TOOL_MISSING: %v", err)
	}
	source := metadata.Source{Engine: "sqlserver", Host: `localhost\MSSQLSERVER`, Port: 1433, Database: "vault_test_source", Username: "integrated"}
	if _, err := client.Execute(context.Background(), source, metadata.StorageTarget{}, "", t.TempDir()); err == nil || ErrorCode(err) != "SERVER_PATH_REQUIRED" {
		t.Fatalf("缺少数据库服务路径应明确失败: %v", err)
	}
}

func TestSQLServerConnectionModes(t *testing.T) {
	client := SQLServerClient{}
	trusted := strings.Join(client.connectionArgs(metadata.Source{Host: "localhost", Port: 1433, Username: "integrated"}), " ")
	if !strings.Contains(trusted, "-E") || strings.Contains(trusted, "-U") {
		t.Fatalf("受信任连接参数错误: %s", trusted)
	}
	sqlLogin := strings.Join(client.connectionArgs(metadata.Source{Host: "db.example", Port: 1433, Username: "vault_test"}), " ")
	if !strings.Contains(sqlLogin, "tcp:db.example,1433") || !strings.Contains(sqlLogin, "-U vault_test") {
		t.Fatalf("SQL 登录参数错误: %s", sqlLogin)
	}
}
