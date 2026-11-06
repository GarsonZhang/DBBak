package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dbbak/internal/backup"
	"dbbak/internal/config"
	"dbbak/internal/metadata"
	"dbbak/internal/security"
)

// 备份路线校验：路线与引擎、备份类型、启用开关必须一致，
// 否则任务会产出与预期恢复语义不符的产物。
func TestJobBackupRouteValidation(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := metadata.Open(ctx, filepath.Join(dataDir, "dbbak.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hash, err := security.HashPassword("api-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreateUser(ctx, "admin", hash); err != nil {
		t.Fatal(err)
	}
	keys, err := security.OpenKeyStore(filepath.Join(dataDir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	tool := fakeAPITool{}
	runner := backup.NewRunner(store, backup.NewRegistry(tool), keys, time.Second)
	cfg := config.Config{DataDir: dataDir, BackupRoot: dataDir, SessionTTL: time.Hour, BackupTimeout: time.Minute}
	server, err := NewServer(store, keys, backup.NewRegistry(tool), runner, backup.NewCleanupService(store, nil), cfg)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	if login := postJSON(t, client, httpServer.URL+"/api/v1/auth/login", `{"username":"admin","password":"api-test-password"}`, true); login.StatusCode != http.StatusOK {
		t.Fatalf("登录失败: %s", login.Status)
	}
	created := postJSON(t, client, httpServer.URL+"/api/v1/sources", `{"name":"MySQL 源","engine":"mysql","host":"127.0.0.1","port":3306,"database":"dbbak_a","username":"u","password":"p"}`, true)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("MySQL 源创建失败: %s %s", created.Status, readBody(t, created))
	}
	var source metadata.Source
	if err = json.Unmarshal(readBytes(t, created), &source); err != nil {
		t.Fatal(err)
	}
	storageRoot := t.TempDir()
	buildBody := func(name, method, backupType string) string {
		return `{"name":"` + name + `","source_id":"` + source.ID + `","storage_root":` + jsonString(storageRoot) +
			`,"timezone":"Asia/Shanghai","schedule_type":"daily","time_of_day":"02:00","weekday":0,"interval_minutes":15,` +
			`"enabled":false,"backup_type":"` + backupType + `","backup_method":"` + method + `",` +
			`"retention_days":30,"min_full":1,"auto_cleanup":false}`
	}

	// 默认路线仍是逻辑全量，空值即可创建。
	ok := postJSON(t, client, httpServer.URL+"/api/v1/jobs", buildBody("默认路线", "", "full"), true)
	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("默认路线任务应创建成功: %s %s", ok.Status, readBody(t, ok))
	}
	var defaultJob metadata.Job
	if err = json.Unmarshal(readBytes(t, ok), &defaultJob); err != nil {
		t.Fatal(err)
	}
	if defaultJob.BackupMethod != "" {
		t.Fatalf("默认路线的 backup_method 应保持空串，实际 %q", defaultJob.BackupMethod)
	}

	// 逻辑路线不记录 binlog 位点，不能提供日志备份。
	resp := postJSON(t, client, httpServer.URL+"/api/v1/jobs", buildBody("逻辑日志", "logical", "log"), true)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(readBody(t, resp), "只在物理备份路线") {
		t.Fatalf("逻辑路线的日志备份应被拒绝: %s %s", resp.Status, readBody(t, resp))
	}
	// 物理路线未验收时必须拒绝。
	resp = postJSON(t, client, httpServer.URL+"/api/v1/jobs", buildBody("物理全量", "physical", "full"), true)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(readBody(t, resp), "未在目标环境验收") {
		t.Fatalf("未启用时物理路线应被拒绝: %s %s", resp.Status, readBody(t, resp))
	}

	// 显式启用后，平台前提成为唯一门槛：Windows 上 XtraBackup 无二进制，必须仍然拒绝。
	enabledCfg := cfg
	enabledCfg.MySQLPhysicalEnabled = true
	enabledServer, err := NewServer(store, keys, backup.NewRegistry(tool), runner, backup.NewCleanupService(store, nil), enabledCfg)
	if err != nil {
		t.Fatal(err)
	}
	enabledHTTP := httptest.NewServer(enabledServer)
	defer enabledHTTP.Close()
	if login := postJSON(t, client, enabledHTTP.URL+"/api/v1/auth/login", `{"username":"admin","password":"api-test-password"}`, true); login.StatusCode != http.StatusOK {
		t.Fatalf("启用后登录失败: %s", login.Status)
	}
	resp = postJSON(t, client, enabledHTTP.URL+"/api/v1/jobs", buildBody("物理全量", "physical", "full"), true)
	if runtime.GOOS == "windows" {
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(readBody(t, resp), "Windows") {
			t.Fatalf("Windows 上物理路线应被拒绝并说明平台限制: %s %s", resp.Status, readBody(t, resp))
		}
	} else if resp.StatusCode != http.StatusCreated {
		t.Fatalf("Linux 启用后物理全量任务应可创建: %s %s", resp.Status, readBody(t, resp))
	}
}
