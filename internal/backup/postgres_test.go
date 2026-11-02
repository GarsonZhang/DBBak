package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"vault/internal/metadata"
)

func TestPostgreSQLToolAndRegistryFailures(t *testing.T) {
	client := PostgreSQLClient{BaseBackupPath: filepath.Join(t.TempDir(), "missing-pg_basebackup")}
	if _, err := client.Version(context.Background()); err == nil || ErrorCode(err) != "TOOL_MISSING" {
		t.Fatalf("缺失 PostgreSQL 工具应返回 TOOL_MISSING: %v", err)
	}
	registry := NewRegistry(client)
	if _, err := registry.ForEngine("sqlserver"); err == nil {
		t.Fatal("未实现引擎不得默认放行")
	}
}

func TestPostgreSQLArgumentsUseMaintenanceDatabase(t *testing.T) {
	client := PostgreSQLClient{}
	source := metadata.Source{Engine: "postgresql", Host: "127.0.0.1", Port: 35432, Database: "postgres", Username: "vault_pg"}
	args := client.sqlArgs(source, "SELECT version()")
	joined := strings.Join(args, " ")
	for _, expected := range []string{"--host=127.0.0.1", "--port=35432", "--username=vault_pg", "--dbname=postgres", "ON_ERROR_STOP=1"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("psql 参数缺少 %s: %s", expected, joined)
		}
	}
}
