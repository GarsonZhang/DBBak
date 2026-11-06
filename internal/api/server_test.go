package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbbak/internal/backup"
	"dbbak/internal/config"
	"dbbak/internal/metadata"
	"dbbak/internal/security"
)

func TestAuthenticationCSRFAndPersistentAPI(t *testing.T) {
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
	adapters := backup.NewRegistry(tool)
	runner := backup.NewRunner(store, adapters, keys, time.Second)
	cfg := config.Config{DataDir: dataDir, BackupRoot: dataDir, SessionTTL: time.Hour, BackupTimeout: time.Minute}
	server, err := NewServer(store, keys, adapters, runner, backup.NewCleanupService(store, nil), cfg)
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
	rejected := postJSON(t, client, httpServer.URL+"/api/v1/sources", `{"name":"测试源","engine":"mysql","host":"127.0.0.1","port":3306,"database":"dbbak_source","username":"dbbak","password":"secret"}`, false)
	if rejected.StatusCode != http.StatusForbidden {
		t.Fatalf("Cookie 变更请求缺少确认头应返回 403: %s", rejected.Status)
	}
	created := postJSON(t, client, httpServer.URL+"/api/v1/sources", `{"name":"测试源","engine":"mysql","host":"127.0.0.1","port":3306,"database":"dbbak_source","username":"dbbak","password":"secret"}`, true)
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
	var createdJob metadata.Job
	if err = json.Unmarshal(readBytes(t, job), &createdJob); err != nil {
		t.Fatal(err)
	}
	plan := getJSON(t, client, httpServer.URL+"/api/v1/jobs/"+createdJob.ID+"/removal")
	if plan.StatusCode != http.StatusOK {
		t.Fatalf("删除预览失败: %s %s", plan.Status, readBody(t, plan))
	}
	if strings.Contains(readBody(t, plan), "data_dir") {
		t.Fatalf("删除预览不应返回服务端路径配置")
	}
	// 启用中的任务必须被拒绝，并在错误里给出解除方式。
	blocked := deleteJSON(t, client, httpServer.URL+"/api/v1/jobs/"+createdJob.ID+"?force=1", "")
	if blocked.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, blocked), "任务仍处于启用状态") {
		t.Fatalf("启用中的任务删除应返回 409 并说明原因: %s", blocked.Status)
	}
	// 暂停后按中文任务名强制删除：确认名称必须能承载非 ASCII 字符。
	// 浏览器 fetch 的请求头只接受 ISO-8859-1，因此确认名走查询参数 confirm。
	pauseBody := fmt.Sprintf("%s,\"revision\":%d}", strings.TrimSuffix(strings.Replace(jobBody, `"enabled":true`, `"enabled":false`, 1), "}"), createdJob.Revision)
	paused := patchJSON(t, client, httpServer.URL+"/api/v1/jobs/"+createdJob.ID, pauseBody)
	if paused.StatusCode != http.StatusOK {
		t.Fatalf("暂停任务失败: %s %s", paused.Status, readBody(t, paused))
	}
	// 缺少确认名时必须仍然拒绝，确认逻辑不能因为换了传参位置而失效。
	missing := deleteJSON(t, client, httpServer.URL+"/api/v1/jobs/"+createdJob.ID+"?force=1", "")
	if missing.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("缺少确认名称的强制删除应返回 422: %s %s", missing.Status, readBody(t, missing))
	}
	deleteURL := httpServer.URL + "/api/v1/jobs/" + createdJob.ID + "?force=1&confirm=" + url.QueryEscape(createdJob.Name)
	deleted := deleteJSON(t, client, deleteURL, "")
	if deleted.StatusCode != http.StatusOK {
		t.Fatalf("中文任务名确认删除失败: %s %s", deleted.Status, readBody(t, deleted))
	}

	// 存储位置：仍被任务引用时必须拒绝；任务删除后（含中文名）才能解除登记。
	used := getJSON(t, client, httpServer.URL+"/api/v1/storage-targets/"+target.ID+"/removal")
	if used.StatusCode != http.StatusOK || strings.Contains(readBody(t, used), "data_dir") {
		t.Fatalf("存储位置删除预览应成功且不泄露服务端路径: %s", used.Status)
	}
	// 这里的任务已被删除，因此重新建一个任务来验证“被引用即拒绝”。
	secondJob := postJSON(t, client, httpServer.URL+"/api/v1/jobs", strings.Replace(jobBody, `"enabled":true`, `"enabled":false`, 1), true)
	if secondJob.StatusCode != http.StatusCreated {
		t.Fatalf("二次创建任务失败: %s %s", secondJob.Status, readBody(t, secondJob))
	}
	var blockingJob metadata.Job
	if err = json.Unmarshal(readBytes(t, secondJob), &blockingJob); err != nil {
		t.Fatal(err)
	}
	inUse := deleteJSON(t, client, httpServer.URL+"/api/v1/storage-targets/"+target.ID+"?confirm="+url.QueryEscape(target.Name), "")
	if inUse.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, inUse), "备份任务") {
		t.Fatalf("被任务引用的存储位置删除应返回 409 并说明原因: %s %s", inUse.Status, readBody(t, inUse))
	}
	if cleared := deleteJSON(t, client, httpServer.URL+"/api/v1/jobs/"+blockingJob.ID+"?force=1&confirm="+url.QueryEscape(blockingJob.Name), ""); cleared.StatusCode != http.StatusOK {
		t.Fatalf("清理二次任务失败: %s %s", cleared.Status, readBody(t, cleared))
	}
	// 确认名不一致必须拒绝，且不能把位置删掉。
	wrongName := deleteJSON(t, client, httpServer.URL+"/api/v1/storage-targets/"+target.ID+"?confirm=%E9%94%99%E8%AF%AF%E5%90%8D%E7%A7%B0", "")
	if wrongName.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("确认名称不一致应返回 422: %s %s", wrongName.Status, readBody(t, wrongName))
	}
	// 中文位置名走查询参数，确认后解除登记。
	storageDelete := deleteJSON(t, client, httpServer.URL+"/api/v1/storage-targets/"+target.ID+"?confirm="+url.QueryEscape(target.Name), "")
	if storageDelete.StatusCode != http.StatusOK {
		t.Fatalf("存储位置删除失败: %s %s", storageDelete.Status, readBody(t, storageDelete))
	}
	if again := deleteJSON(t, client, httpServer.URL+"/api/v1/storage-targets/"+target.ID+"?confirm="+url.QueryEscape(target.Name), ""); again.StatusCode != http.StatusNotFound {
		t.Fatalf("重复删除应返回 404: %s", again.Status)
	}

	// 数据源：被任务引用时拒绝；确认名错误拒绝；中文确认后删除并清除凭据。
	secondSource := postJSON(t, client, httpServer.URL+"/api/v1/sources", `{"name":"测试源乙","engine":"mysql","host":"127.0.0.1","port":3306,"database":"dbbak_second","username":"dbbak","password":"secret2"}`, true)
	if secondSource.StatusCode != http.StatusCreated {
		t.Fatalf("第二数据源创建失败: %s %s", secondSource.Status, readBody(t, secondSource))
	}
	var sourceB metadata.Source
	if err = json.Unmarshal(readBytes(t, secondSource), &sourceB); err != nil {
		t.Fatal(err)
	}
	secondTarget := postJSON(t, client, httpServer.URL+"/api/v1/storage-targets", `{"name":"隔离目录乙","kind":"local","root":`+jsonString(t.TempDir())+`}`, true)
	if secondTarget.StatusCode != http.StatusCreated {
		t.Fatalf("第二存储创建失败: %s %s", secondTarget.Status, readBody(t, secondTarget))
	}
	var targetB metadata.StorageTarget
	if err = json.Unmarshal(readBytes(t, secondTarget), &targetB); err != nil {
		t.Fatal(err)
	}
	pinnedJob := postJSON(t, client, httpServer.URL+"/api/v1/jobs", `{"name":"钉住数据源","source_id":"`+sourceB.ID+`","target_id":"`+targetB.ID+`","timezone":"Asia/Shanghai","schedule_type":"daily","time_of_day":"02:00","weekday":0,"interval_minutes":15,"enabled":false,"retention_days":30,"min_full":1,"auto_cleanup":false}`, true)
	if pinnedJob.StatusCode != http.StatusCreated {
		t.Fatalf("引用任务创建失败: %s %s", pinnedJob.Status, readBody(t, pinnedJob))
	}
	var pinned metadata.Job
	if err = json.Unmarshal(readBytes(t, pinnedJob), &pinned); err != nil {
		t.Fatal(err)
	}
	sourcePlan := getJSON(t, client, httpServer.URL+"/api/v1/sources/"+sourceB.ID+"/removal")
	if sourcePlan.StatusCode != http.StatusOK || !strings.Contains(readBody(t, sourcePlan), "SOURCE_IN_USE_BY_JOB") {
		t.Fatalf("数据源删除预览应列出阻塞原因: %s %s", sourcePlan.Status, readBody(t, sourcePlan))
	}
	sourceBlocked := deleteJSON(t, client, httpServer.URL+"/api/v1/sources/"+sourceB.ID+"?confirm="+url.QueryEscape(sourceB.Name), "")
	if sourceBlocked.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, sourceBlocked), "备份任务") {
		t.Fatalf("被任务引用的数据源删除应返回 409: %s %s", sourceBlocked.Status, readBody(t, sourceBlocked))
	}
	if cleared := deleteJSON(t, client, httpServer.URL+"/api/v1/jobs/"+pinned.ID+"?force=1&confirm="+url.QueryEscape(pinned.Name), ""); cleared.StatusCode != http.StatusOK {
		t.Fatalf("清理引用任务失败: %s %s", cleared.Status, readBody(t, cleared))
	}
	if wrong := deleteJSON(t, client, httpServer.URL+"/api/v1/sources/"+sourceB.ID+"?confirm=%E9%94%99%E8%AF%AF", ""); wrong.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("确认名称不一致应返回 422: %s %s", wrong.Status, readBody(t, wrong))
	}
	// 中文数据源名走查询参数；删除后必须连凭据一起清除，否则留下不可解密的孤儿密文。
	sourceDeleted := deleteJSON(t, client, httpServer.URL+"/api/v1/sources/"+sourceB.ID+"?confirm="+url.QueryEscape(sourceB.Name), "")
	if sourceDeleted.StatusCode != http.StatusOK {
		t.Fatalf("数据源删除失败: %s %s", sourceDeleted.Status, readBody(t, sourceDeleted))
	}
	if sourceAgain := deleteJSON(t, client, httpServer.URL+"/api/v1/sources/"+sourceB.ID+"?confirm="+url.QueryEscape(sourceB.Name), ""); sourceAgain.StatusCode != http.StatusNotFound {
		t.Fatalf("重复删除数据源应返回 404: %s", sourceAgain.Status)
	}
	// 凭据必须随数据源一并清除：删除后取密码应返回 ErrNotFound，而不是留下孤儿密文。
	if _, err = store.SourcePassword(context.Background(), sourceB.ID); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("数据源删除后加密凭据应一并清除: %v", err)
	}
}

// 数据库服务器本地备份：任务模型与校验规则的端到端覆盖。
func TestServerLocalBackupJobValidation(t *testing.T) {
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
	mysqlSource := postJSON(t, client, httpServer.URL+"/api/v1/sources", `{"name":"MySQL 源","engine":"mysql","host":"127.0.0.1","port":3306,"database":"dbbak_a","username":"u","password":"p"}`, true)
	if mysqlSource.StatusCode != http.StatusCreated {
		t.Fatalf("MySQL 源创建失败: %s %s", mysqlSource.Status, readBody(t, mysqlSource))
	}
	var mysql metadata.Source
	if err = json.Unmarshal(readBytes(t, mysqlSource), &mysql); err != nil {
		t.Fatal(err)
	}
	sqlSource := postJSON(t, client, httpServer.URL+"/api/v1/sources", `{"name":"SQL 源","engine":"sqlserver","host":"192.168.10.198","port":1433,"database":"TCE-Test","username":"u","password":"p"}`, true)
	if sqlSource.StatusCode != http.StatusCreated {
		t.Fatalf("SQL Server 源创建失败: %s %s", sqlSource.Status, readBody(t, sqlSource))
	}
	var mssql metadata.Source
	if err = json.Unmarshal(readBytes(t, sqlSource), &mssql); err != nil {
		t.Fatal(err)
	}

	// 非法组合必须逐一拒绝：MySQL 引擎、缺少服务器目录、同时给出存储位置。
	rejected := []struct {
		name   string
		source string
		body   string
		expect string
	}{
		{"MySQL 不支持服务器本地模式", mysql.ID, `{"name":"错1","source_id":"` + mysql.ID + `","location_kind":"database_server","server_dir":"D:\\bak","timezone":"Asia/Shanghai","schedule_type":"daily","time_of_day":"02:00","weekday":0,"interval_minutes":15,"enabled":false,"retention_days":30,"min_full":1,"auto_cleanup":false}`, "只支持 SQL Server"},
		{"缺少服务器目录", mssql.ID, `{"name":"错2","source_id":"` + mssql.ID + `","location_kind":"database_server","timezone":"Asia/Shanghai","schedule_type":"daily","time_of_day":"02:00","weekday":0,"interval_minutes":15,"enabled":false,"retention_days":30,"min_full":1,"auto_cleanup":false}`, "绝对路径"},
		{"相对路径服务器目录", mssql.ID, `{"name":"错3","source_id":"` + mssql.ID + `","location_kind":"database_server","server_dir":"bak","timezone":"Asia/Shanghai","schedule_type":"daily","time_of_day":"02:00","weekday":0,"interval_minutes":15,"enabled":false,"retention_days":30,"min_full":1,"auto_cleanup":false}`, "绝对路径"},
		{"与存储位置互斥", mssql.ID, `{"name":"错4","source_id":"` + mssql.ID + `","location_kind":"database_server","server_dir":"D:\\bak","storage_root":` + jsonString(t.TempDir()) + `,"timezone":"Asia/Shanghai","schedule_type":"daily","time_of_day":"02:00","weekday":0,"interval_minutes":15,"enabled":false,"retention_days":30,"min_full":1,"auto_cleanup":false}`, "不使用存储位置"},
	}
	for _, item := range rejected {
		resp := postJSON(t, client, httpServer.URL+"/api/v1/jobs", item.body, true)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(readBody(t, resp), item.expect) {
			t.Fatalf("%s 应返回 422 并说明原因: %s %s", item.name, resp.Status, readBody(t, resp))
		}
	}

	// 合法创建：不挂存储位置，target_id 为空。
	created := postJSON(t, client, httpServer.URL+"/api/v1/jobs", `{"name":"TCE 服务器本地备份","source_id":"`+mssql.ID+`","location_kind":"database_server","server_dir":"D:\\bak","timezone":"Asia/Shanghai","schedule_type":"daily","time_of_day":"02:00","weekday":0,"interval_minutes":15,"enabled":false,"retention_days":30,"min_full":1,"auto_cleanup":false}`, true)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("服务器本地任务创建失败: %s %s", created.Status, readBody(t, created))
	}
	var job metadata.Job
	if err = json.Unmarshal(readBytes(t, created), &job); err != nil {
		t.Fatal(err)
	}
	if job.LocationKind != "database_server" || job.TargetID != "" || job.ServerDir != `D:\bak` {
		t.Fatalf("任务字段不符: %+v", job)
	}
	// 任务列表接口返回相同字段，前端据此渲染。
	jobs := getJSON(t, client, httpServer.URL+"/api/v1/jobs")
	var listed []metadata.Job
	if err = json.Unmarshal(readBytes(t, jobs), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].LocationKind != "database_server" || listed[0].TargetID != "" {
		t.Fatalf("任务列表字段不符: %+v", listed)
	}
}

// 存储位置类型扩展：共享目录（UNC/SMB 路径语义同本地）与 FTP 的校验规则。
func TestStorageKindValidation(t *testing.T) {
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
	server, err := NewServer(store, keys, backup.NewRegistry(tool), runner, backup.NewCleanupService(store, keys), cfg)
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

	// 共享目录：路径语义与本地一致（映射盘/UNC 都走文件系统），用本地目录验证登记链路。
	uncRoot := filepath.Join(dataDir, "unc-root")
	unc := postJSON(t, client, httpServer.URL+"/api/v1/storage-targets", `{"name":"共享备份","kind":"unc","root":`+jsonString(uncRoot)+`}`, true)
	if unc.StatusCode != http.StatusCreated {
		t.Fatalf("共享目录创建失败: %s %s", unc.Status, readBody(t, unc))
	}
	var uncTarget metadata.StorageTarget
	if err = json.Unmarshal(readBytes(t, unc), &uncTarget); err != nil {
		t.Fatal(err)
	}
	if uncTarget.Kind != "unc" {
		t.Fatalf("kind 应为 unc: %+v", uncTarget)
	}

	// FTP：配置缺失与连接不可达必须明确失败，不得生成虚假登记。
	ftpHost := postJSON(t, client, httpServer.URL+"/api/v1/storage-targets", `{"name":"无主机","kind":"ftp","root":"/backups","port":21,"username":"u","password":"p"}`, true)
	if ftpHost.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(readBody(t, ftpHost), "主机必填") {
		t.Fatalf("缺主机应返回 422: %s %s", ftpHost.Status, readBody(t, ftpHost))
	}
	ftpRoot := postJSON(t, client, httpServer.URL+"/api/v1/storage-targets", `{"name":"根目录错","kind":"ftp","root":"D:\\bak","host":"127.0.0.1","port":21,"username":"u","password":"p"}`, true)
	if ftpRoot.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(readBody(t, ftpRoot), "/ 开头") {
		t.Fatalf("远程根目录格式错误应返回 422: %s %s", ftpRoot.Status, readBody(t, ftpRoot))
	}
	// 本地未监听端口：拨号立即失败，验证「连接失败必须如实拒绝」。
	ftpUnreachable := postJSON(t, client, httpServer.URL+"/api/v1/storage-targets", `{"name":"不可达","kind":"ftp","root":"/backups","host":"127.0.0.1","port":1,"username":"u","password":"p"}`, true)
	if ftpUnreachable.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("FTP 不可达应返回 422: %s %s", ftpUnreachable.Status, readBody(t, ftpUnreachable))
	}
	// 不应有任何 FTP 登记被写入。
	targets := getJSON(t, client, httpServer.URL+"/api/v1/storage-targets")
	if strings.Contains(readBody(t, targets), `"kind":"ftp"`) {
		t.Fatalf("写检查失败的 FTP 不应入库: %s", readBody(t, targets))
	}

	// 对象存储（S3 兼容 / 阿里云 OSS）：配置缺失、端点形态错误、类型混淆都必须在保存前拒绝。
	ossNoBucket := postJSON(t, client, httpServer.URL+"/api/v1/storage-targets",
		`{"name":"缺桶","kind":"oss","endpoint":"oss-cn-hangzhou.aliyuncs.com","username":"AK","password":"SK"}`, true)
	if ossNoBucket.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(readBody(t, ossNoBucket), "Bucket") {
		t.Fatalf("缺 Bucket 应返回 422: %s %s", ossNoBucket.Status, readBody(t, ossNoBucket))
	}
	ossNoEndpoint := postJSON(t, client, httpServer.URL+"/api/v1/storage-targets",
		`{"name":"缺端点","kind":"oss","bucket":"b1","username":"AK","password":"SK"}`, true)
	if ossNoEndpoint.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(readBody(t, ossNoEndpoint), "端点必填") {
		t.Fatalf("缺端点应返回 422: %s %s", ossNoEndpoint.Status, readBody(t, ossNoEndpoint))
	}
	// 把完整对象 URL 粘进端点是最常见的误操作，必须给出可读的拒绝理由。
	ossBadEndpoint := postJSON(t, client, httpServer.URL+"/api/v1/storage-targets",
		`{"name":"端点带路径","kind":"oss","endpoint":"https://oss-cn-hangzhou.aliyuncs.com/bucket1","bucket":"b1","username":"AK","password":"SK"}`, true)
	if ossBadEndpoint.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(readBody(t, ossBadEndpoint), "只填主机名") {
		t.Fatalf("端点带路径应返回 422: %s %s", ossBadEndpoint.Status, readBody(t, ossBadEndpoint))
	}
	// 对象存储不写执行器磁盘，混入本地目录参数属于矛盾配置。
	ossMixed := postJSON(t, client, httpServer.URL+"/api/v1/storage-targets",
		`{"name":"混淆","kind":"local","root":`+jsonString(uncRoot)+`,"bucket":"b1","endpoint":"oss-cn-hangzhou.aliyuncs.com"}`, true)
	if ossMixed.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(readBody(t, ossMixed), "对象存储参数") {
		t.Fatalf("本地目录混入对象存储参数应返回 422: %s %s", ossMixed.Status, readBody(t, ossMixed))
	}
	// 真实写检查会发起网络请求；这里用本机未监听端口验证「连不上必须如实拒绝且不入库」。
	ossUnreachable := postJSON(t, client, httpServer.URL+"/api/v1/storage-targets",
		`{"name":"不可达对象存储","kind":"oss","endpoint":"http://127.0.0.1:1","bucket":"b1","username":"AK","password":"SK","use_ssl":false,"path_style":true}`, true)
	if ossUnreachable.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("对象存储不可达应返回 422: %s %s", ossUnreachable.Status, readBody(t, ossUnreachable))
	}
	targets = getJSON(t, client, httpServer.URL+"/api/v1/storage-targets")
	if strings.Contains(readBody(t, targets), `"kind":"oss"`) {
		t.Fatalf("写检查失败的 OSS 不应入库: %s", readBody(t, targets))
	}
}

func getJSON(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-DBBak-Request", "1")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func deleteJSON(t *testing.T, client *http.Client, url, confirm string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-DBBak-Request", "1")
	if confirm != "" {
		request.Header.Set("X-DBBak-Confirm", confirm)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func patchJSON(t *testing.T, client *http.Client, url, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPatch, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-DBBak-Request", "1")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

type fakeAPITool struct{}

func (fakeAPITool) Engine() string { return "mysql" }
func (fakeAPITool) Execute(context.Context, metadata.Source, metadata.StorageTarget, string, string, backup.BackupSpec) (backup.ExecuteResult, error) {
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
		request.Header.Set("X-DBBak-Request", "1")
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

// TestJobStorageRootAutoTarget 验证任务直接填写本地存储目录时，
// 服务端自动登记存储目标，且相同目录的多个任务复用同一条登记。
func TestJobStorageRootAutoTarget(t *testing.T) {
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
	adapters := backup.NewRegistry(fakeAPITool{})
	runner := backup.NewRunner(store, adapters, keys, time.Second)
	cfg := config.Config{DataDir: dataDir, BackupRoot: dataDir, SessionTTL: time.Hour, BackupTimeout: time.Minute}
	server, err := NewServer(store, keys, adapters, runner, backup.NewCleanupService(store, nil), cfg)
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
	created := postJSON(t, client, httpServer.URL+"/api/v1/sources", `{"name":"测试源","engine":"mysql","host":"127.0.0.1","port":3306,"database":"dbbak_source","username":"dbbak","password":"secret"}`, true)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("数据源创建失败: %s", created.Status)
	}
	var source metadata.Source
	if err = json.Unmarshal(readBytes(t, created), &source); err != nil {
		t.Fatal(err)
	}

	backupDir := t.TempDir()
	jobBody := `{"name":"直接目录任务","source_id":"` + source.ID + `","storage_root":` + jsonString(backupDir) + `,"timezone":"Asia/Shanghai","schedule_type":"daily","time_of_day":"02:00","weekday":0,"interval_minutes":15,"enabled":false,"retention_days":30,"min_full":1,"auto_cleanup":false}`
	jobResponse := postJSON(t, client, httpServer.URL+"/api/v1/jobs", jobBody, true)
	if jobResponse.StatusCode != http.StatusCreated {
		t.Fatalf("按目录创建任务失败: %s %s", jobResponse.Status, readBody(t, jobResponse))
	}
	var job metadata.Job
	if err = json.Unmarshal(readBytes(t, jobResponse), &job); err != nil {
		t.Fatal(err)
	}
	if job.TargetID == "" {
		t.Fatal("任务应自动关联登记的存储目标")
	}
	target, err := store.GetStorageTarget(ctx, job.TargetID)
	if err != nil {
		t.Fatalf("存储目标未登记: %v", err)
	}
	if !strings.EqualFold(filepath.Clean(target.Root), filepath.Clean(backupDir)) {
		t.Fatalf("登记目录不一致: %s", target.Root)
	}

	// 相同目录的第二个任务应复用同一存储目标。
	second := postJSON(t, client, httpServer.URL+"/api/v1/jobs", strings.Replace(jobBody, "直接目录任务", "第二个任务", 1), true)
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("第二个任务创建失败: %s %s", second.Status, readBody(t, second))
	}
	var job2 metadata.Job
	if err = json.Unmarshal(readBytes(t, second), &job2); err != nil {
		t.Fatal(err)
	}
	if job2.TargetID != job.TargetID {
		t.Fatalf("相同目录应复用存储目标: %s != %s", job2.TargetID, job.TargetID)
	}
	targets, err := store.ListStorageTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	matched := 0
	for _, item := range targets {
		if strings.EqualFold(filepath.Clean(item.Root), filepath.Clean(backupDir)) {
			matched++
		}
	}
	if matched != 1 {
		t.Fatalf("相同目录重复登记存储目标: %d 条", matched)
	}

	// 不可写目录必须明确失败，不能生成任务：用同名文件挡住父目录，使目录创建失败。
	blockerFile := filepath.Join(t.TempDir(), "占用")
	if err = os.WriteFile(blockerFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := postJSON(t, client, httpServer.URL+"/api/v1/jobs", `{"name":"坏目录任务","source_id":"`+source.ID+`","storage_root":`+jsonString(filepath.Join(blockerFile, "子目录"))+`,"timezone":"Asia/Shanghai","schedule_type":"daily","time_of_day":"02:00","weekday":0,"interval_minutes":15,"enabled":false,"retention_days":30,"min_full":1,"auto_cleanup":false}`, true)
	if bad.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("不可写目录应拒绝: %s %s", bad.Status, readBody(t, bad))
	}
}
