package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
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

// 压缩选项的 API 边界：
//
//  1. 只有 SQL Server 能落库，其他引擎提交时必须被清空，而不是报错或静默保存；
//  2. 未知取值必须明确拒绝——静默退化成「不压缩」会让用户以为压缩已经生效；
//  3. 能力查询接口对非 SQL Server 明确返回 supported=false，探测失败不回 5xx。
func TestJobCompressionOptionValidation(t *testing.T) {
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
	cfg := config.Config{DataDir: dataDir, BackupRoot: dataDir, SessionTTL: time.Hour, BackupTimeout: time.Minute, SQLServerEnabled: true}
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

	createSource := func(name, engine string) metadata.Source {
		body := `{"name":"` + name + `","engine":"` + engine + `","host":"10.0.0.1","port":1433,"database":"db1","username":"sa","password":"p"}`
		resp := postJSON(t, client, httpServer.URL+"/api/v1/sources", body, true)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s 源创建失败: %s %s", engine, resp.Status, readBody(t, resp))
		}
		var source metadata.Source
		if err = json.Unmarshal(readBytes(t, resp), &source); err != nil {
			t.Fatal(err)
		}
		return source
	}
	sqlSource := createSource("SQL 源", "sqlserver")
	mySource := createSource("MySQL 源", "mysql")
	storageRoot := t.TempDir()

	jobBody := func(sourceID, compression string) string {
		return `{"name":"压缩任务","source_id":"` + sourceID + `","storage_root":` + jsonString(storageRoot) +
			`,"timezone":"Asia/Shanghai","schedule_type":"daily","time_of_day":"02:00","weekday":0,"interval_minutes":15,` +
			`"enabled":false,"backup_type":"full","retention_days":30,"min_full":1,"auto_cleanup":false,` +
			`"backup_compression":"` + compression + `"}`
	}

	// SQL Server + 合法取值：原样保存。
	for _, option := range []string{"", "off", "on", "zstd_low", "zstd_medium", "zstd_high"} {
		resp := postJSON(t, client, httpServer.URL+"/api/v1/jobs", jobBody(sqlSource.ID, option), true)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("SQL Server 压缩选项 %q 应可保存: %s %s", option, resp.Status, readBody(t, resp))
		}
		var job metadata.Job
		if err = json.Unmarshal(readBytes(t, resp), &job); err != nil {
			t.Fatal(err)
		}
		if job.BackupCompression != option {
			t.Fatalf("压缩选项 %q 保存后变成 %q", option, job.BackupCompression)
		}
	}

	// SQL Server + 未知取值：必须拒绝，不能退化成不压缩。
	resp := postJSON(t, client, httpServer.URL+"/api/v1/jobs", jobBody(sqlSource.ID, "zstd_ultra"), true)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("未知压缩选项应被拒绝: %s %s", resp.Status, readBody(t, resp))
	}
	if body := readBody(t, resp); !strings.Contains(body, "压缩") {
		t.Fatalf("拒绝原因应说明压缩选项，实际: %s", body)
	}

	// 非 SQL Server：字段被清空而不是报错——界面切换引擎时不应残留旧值。
	resp = postJSON(t, client, httpServer.URL+"/api/v1/jobs", jobBody(mySource.ID, "zstd_high"), true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("MySQL 任务不应因压缩字段被拒绝: %s %s", resp.Status, readBody(t, resp))
	}
	var myJob metadata.Job
	if err = json.Unmarshal(readBytes(t, resp), &myJob); err != nil {
		t.Fatal(err)
	}
	if myJob.BackupCompression != "" {
		t.Fatalf("MySQL 任务的压缩选项必须被清空，实际 %q", myJob.BackupCompression)
	}
}

// 能力查询接口：非 SQL Server 返回 supported=false 且空能力集，
// SQL Server 探测失败时退化为全部 unverified（200，不抛 5xx），
// 让界面能显示原因而不是整页报错。
func TestCompressionCapabilityEndpoint(t *testing.T) {
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
	// fakeAPITool 不实现 CompressionSupported：用来验证「适配器未实现探测」也走 unverified 分支。
	tool := fakeAPITool{}
	runner := backup.NewRunner(store, backup.NewRegistry(tool), keys, time.Second)
	cfg := config.Config{DataDir: dataDir, BackupRoot: dataDir, SessionTTL: time.Hour, BackupTimeout: time.Minute, SQLServerEnabled: true}
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
	createSource := func(name, engine string) metadata.Source {
		body := `{"name":"` + name + `","engine":"` + engine + `","host":"10.0.0.1","port":1433,"database":"db1","username":"sa","password":"p"}`
		resp := postJSON(t, client, httpServer.URL+"/api/v1/sources", body, true)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s 源创建失败: %s %s", engine, resp.Status, readBody(t, resp))
		}
		var source metadata.Source
		if err = json.Unmarshal(readBytes(t, resp), &source); err != nil {
			t.Fatal(err)
		}
		return source
	}

	mySource := createSource("MySQL 源", "mysql")
	resp, err := client.Get(httpServer.URL + "/api/v1/sources/" + mySource.ID + "/compression")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("MySQL 能力查询应返回 200: %s", resp.Status)
	}
	var myPayload struct {
		Supported    bool                                    `json:"supported"`
		Capabilities map[string]backup.CompressionCapability `json:"capabilities"`
	}
	if err = json.Unmarshal(readBytes(t, resp), &myPayload); err != nil {
		t.Fatal(err)
	}
	if myPayload.Supported || len(myPayload.Capabilities) != 0 {
		t.Fatalf("MySQL 不应声明支持备份压缩: %+v", myPayload)
	}

	sqlSource := createSource("SQL 源", "sqlserver")
	resp, err = client.Get(httpServer.URL + "/api/v1/sources/" + sqlSource.ID + "/compression")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SQL Server 能力查询必须返回 200（探测失败也不能是 5xx）: %s", resp.Status)
	}
	var sqlPayload struct {
		Supported    bool                                    `json:"supported"`
		Capabilities map[string]backup.CompressionCapability `json:"capabilities"`
	}
	if err = json.Unmarshal(readBytes(t, resp), &sqlPayload); err != nil {
		t.Fatal(err)
	}
	if !sqlPayload.Supported {
		t.Fatal("SQL Server 应声明具备压缩能力字段")
	}
	for _, option := range []string{"off", "on", "zstd_low", "zstd_medium", "zstd_high"} {
		capability, ok := sqlPayload.Capabilities[option]
		if !ok {
			t.Fatalf("能力集缺少 %s", option)
		}
		if capability.Status != "unverified" {
			t.Fatalf("适配器未实现探测时 %s 应为 unverified，实际 %q", option, capability.Status)
		}
		if capability.Reason == "" {
			t.Fatalf("%s 标记 unverified 时必须给出原因", option)
		}
	}
	if sqlPayload.Capabilities[backup.CompressionDefault].Status != "supported" {
		t.Fatal("不指定压缩应始终可用，它是回退路径")
	}
}
