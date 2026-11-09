package api

import (
	"context"
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

// 信任证书开关只对 SQL Server 生效：其他引擎即使提交 true 也不能落库，
// 否则前端或脚本的误传会让「证书不校验」成为一个看不见的默认值。
func TestSourceTrustCertificateOnlyForSQLServer(t *testing.T) {
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

	sql := postJSON(t, client, httpServer.URL+"/api/v1/sources", `{"name":"SQL 源","engine":"sqlserver","host":"10.0.0.1","port":1433,"database":"db1","username":"sa","password":"p","trust_server_certificate":true}`, true)
	if sql.StatusCode != http.StatusCreated {
		t.Fatalf("SQL Server 源创建失败: %s", readBody(t, sql))
	}
	if !strings.Contains(readBody(t, sql), `"trust_server_certificate":true`) {
		t.Fatalf("SQL Server 源应保存 trust_server_certificate=true: %s", readBody(t, sql))
	}

	my := postJSON(t, client, httpServer.URL+"/api/v1/sources", `{"name":"MySQL 源","engine":"mysql","host":"10.0.0.2","port":3306,"database":"db2","username":"u","password":"p","trust_server_certificate":true}`, true)
	if my.StatusCode != http.StatusCreated {
		t.Fatalf("MySQL 源创建失败: %s", readBody(t, my))
	}
	if strings.Contains(readBody(t, my), `"trust_server_certificate":true`) {
		t.Fatalf("MySQL 源不接受信任证书开关，必须落成 false: %s", readBody(t, my))
	}
}
