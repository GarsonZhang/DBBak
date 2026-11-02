package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vault/internal/backup"
	"vault/internal/config"
	"vault/internal/metadata"
	"vault/internal/security"
)

func TestAuthenticationCSRFAndPersistentAPI(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := metadata.Open(ctx, filepath.Join(dataDir, "vault.db"))
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
	adapters := backup.NewRegistry(tool)
	runner := backup.NewRunner(store, adapters, keys, time.Second)
	cfg := config.Config{DataDir: dataDir, BackupRoot: dataDir, SessionTTL: time.Hour, BackupTimeout: time.Minute}
	server, err := NewServer(store, keys, adapters, runner, backup.NewCleanupService(store), cfg)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/api/v1/sources")
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未认证请求应返回 401: %v %v", response, err)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login := postJSON(t, client, httpServer.URL+"/api/v1/auth/login", `{"username":"admin","password":"api-test-password"}`, true)
	if login.StatusCode != http.StatusOK {
		t.Fatalf("登录失败: %s", login.Status)
	}
	rejected := postJSON(t, client, httpServer.URL+"/api/v1/sources", `{"name":"测试源","engine":"mysql","host":"127.0.0.1","port":3306,"database":"vault_source","username":"vault","password":"secret"}`, false)
	if rejected.StatusCode != http.StatusForbidden {
		t.Fatalf("Cookie 变更请求缺少确认头应返回 403: %s", rejected.Status)
	}
	created := postJSON(t, client, httpServer.URL+"/api/v1/sources", `{"name":"测试源","engine":"mysql","host":"127.0.0.1","port":3306,"database":"vault_source","username":"vault","password":"secret"}`, true)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("数据源创建失败: %s", created.Status)
	}
	body := readBody(t, created)
	if strings.Contains(body, "secret") || strings.Contains(body, "ciphertext") || strings.Contains(body, "password") {
		t.Fatalf("数据源响应泄露凭据: %s", body)
	}
	var source metadata.Source
	if err = json.Unmarshal([]byte(body), &source); err != nil {
		t.Fatal(err)
	}
	storage := postJSON(t, client, httpServer.URL+"/api/v1/storage-targets", `{"name":"隔离目录","kind":"local","root":`+jsonString(t.TempDir())+`}`, true)
	if storage.StatusCode != http.StatusCreated {
		t.Fatalf("存储创建失败: %s %s", storage.Status, readBody(t, storage))
	}
	var target metadata.StorageTarget
	if err = json.Unmarshal(readBytes(t, storage), &target); err != nil {
		t.Fatal(err)
	}
	jobBody := `{"name":"每日全量","source_id":"` + source.ID + `","target_id":"` + target.ID + `","timezone":"Asia/Shanghai","schedule_type":"daily","time_of_day":"02:00","weekday":0,"interval_minutes":15,"enabled":true,"retention_days":30,"min_full":1,"auto_cleanup":false}`
	job := postJSON(t, client, httpServer.URL+"/api/v1/jobs", jobBody, true)
	if job.StatusCode != http.StatusCreated {
		t.Fatalf("任务创建失败: %s %s", job.Status, readBody(t, job))
	}
}

type fakeAPITool struct{}

func (fakeAPITool) Engine() string { return "mysql" }
func (fakeAPITool) Execute(context.Context, metadata.Source, metadata.StorageTarget, string, string) (backup.ExecuteResult, error) {
	return backup.ExecuteResult{Extension: ".sql", Method: "mysqldump", EngineMetadata: []byte("{}")}, nil
}

func (fakeAPITool) Version(context.Context) (string, error) { return "test", nil }
func (fakeAPITool) Probe(context.Context, metadata.Source, string) (string, error) {
	return "8.4.13", nil
}
func (fakeAPITool) Dump(context.Context, metadata.Source, string, string) error { return nil }

func postJSON(t *testing.T, client *http.Client, url, body string, confirmed bool) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if confirmed {
		request.Header.Set("X-Vault-Request", "1")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	return string(readBytes(t, response))
}

func readBytes(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(response.Body); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
