package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbbak/internal/backup"
	"dbbak/internal/config"
	"dbbak/internal/metadata"
	"dbbak/internal/security"
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
		store, err := metadata.Open(context.Background(), filepath.Join(dataDir, "dbbak.db"))
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
		server, err := NewServer(store, keys, adapters, runner, backup.NewCleanupService(store, nil), cfg)
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

// TestMissingToolMessageCarriesConfiguredPath 固定“工具缺失”时的可操作性：
// 界面提示必须说明是哪个工具找不到、服务实际用的哪个路径，而不是只让运维去猜环境变量。
func TestMissingToolMessageCarriesConfiguredPath(t *testing.T) {
	dataDir := t.TempDir()
	store, err := metadata.Open(context.Background(), filepath.Join(dataDir, "dbbak.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	keys, err := security.OpenKeyStore(filepath.Join(dataDir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	missingMySQL := filepath.Join(dataDir, "no-such-mysql.exe")
	missingDump := filepath.Join(dataDir, "no-such-mysqldump.exe")
	adapters := backup.NewRegistry(backup.MySQLClient{MySQLPath: missingMySQL, MySQLDumpPath: missingDump})
	runner := backup.NewRunner(store, adapters, keys, time.Second)
	cfg := config.Config{DataDir: dataDir, BackupRoot: dataDir, SessionTTL: time.Hour, BackupTimeout: time.Minute, MySQLPath: missingMySQL, MySQLDumpPath: missingDump}
	server, err := NewServer(store, keys, adapters, runner, backup.NewCleanupService(store, nil), cfg)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	server.capabilities(recorder, httptest.NewRequest("GET", "/api/v1/capabilities", nil))
	var payload struct {
		LocalExecutor struct {
			Status       string            `json:"status"`
			ToolVersions map[string]string `json:"tool_versions"`
			Message      string            `json:"message"`
		} `json:"local_executor"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.LocalExecutor.Status != "prerequisite_missing" {
		t.Fatalf("默认路线缺工具时执行器状态应为 prerequisite_missing，实际 %q", payload.LocalExecutor.Status)
	}
	message := payload.LocalExecutor.Message
	if !strings.Contains(message, "mysqldump") {
		t.Fatalf("提示必须点名缺失的工具，实际 %q", message)
	}
	if !strings.Contains(message, missingDump) {
		t.Fatalf("提示必须带出服务实际使用的路径 %s，实际 %q", missingDump, message)
	}
}
