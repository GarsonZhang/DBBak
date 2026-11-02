package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"vault/internal/backup"
	"vault/internal/config"
	"vault/internal/metadata"
	"vault/internal/security"
)

// fakeEngine 让能力矩阵测试不依赖真实数据库工具。
type fakeEngine struct{ engine string }

func (f fakeEngine) Engine() string { return f.engine }
func (f fakeEngine) Version(context.Context) (string, error) {
	return "fake 1.0", nil
}
func (f fakeEngine) Probe(context.Context, metadata.Source, string) (string, error) {
	return "fake 1.0", nil
}
func (f fakeEngine) Execute(context.Context, metadata.Source, metadata.StorageTarget, string, string, backup.BackupSpec) (backup.ExecuteResult, error) {
	return backup.ExecuteResult{}, nil
}

// SQL Server 未验收时必须保持 unverified；只有显式开关才能变为 supported。
func TestSQLServerCapabilityRequiresExplicitSwitch(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		dataDir := t.TempDir()
		store, err := metadata.Open(context.Background(), filepath.Join(dataDir, "vault.db"))
		if err != nil {
			t.Fatal(err)
		}
		keys, err := security.OpenKeyStore(filepath.Join(dataDir, "master.key"))
		if err != nil {
			store.Close()
			t.Fatal(err)
		}
		adapters := backup.NewRegistry(fakeEngine{"mysql"}, fakeEngine{"sqlserver"})
		runner := backup.NewRunner(store, adapters, keys, time.Second)
		cfg := config.Config{DataDir: dataDir, BackupRoot: dataDir, SessionTTL: time.Hour, BackupTimeout: time.Minute, SQLServerEnabled: enabled}
		server, err := NewServer(store, keys, adapters, runner, backup.NewCleanupService(store), cfg)
		if err != nil {
			store.Close()
			t.Fatal(err)
		}

		recorder := httptest.NewRecorder()
		server.capabilities(recorder, httptest.NewRequest("GET", "/api/v1/capabilities", nil))
		if recorder.Code != 200 {
			store.Close()
			t.Fatalf("能力接口状态码 %d", recorder.Code)
		}
		var payload struct {
			Capabilities []struct {
				Engine string `json:"engine"`
				Status string `json:"status"`
			} `json:"capabilities"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			store.Close()
			t.Fatal(err)
		}
		status := ""
		for _, item := range payload.Capabilities {
			if item.Engine == "sqlserver" && item.Status != "" {
				status = item.Status
				break
			}
		}
		store.Close()

		want := "unverified"
		if enabled {
			want = "supported"
		}
		if status != want {
			t.Fatalf("SQLServerEnabled=%v 时 sqlserver 能力应为 %s，实际 %q", enabled, want, status)
		}
	}
}
