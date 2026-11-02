package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vault/internal/metadata"
	"vault/internal/security"
)

type fakeMySQLTool struct {
	dump func(context.Context, metadata.Source, string, string) error
}

func (f fakeMySQLTool) Engine() string { return "mysql" }
func (f fakeMySQLTool) Execute(ctx context.Context, source metadata.Source, _ metadata.StorageTarget, password, workPath string) (ExecuteResult, error) {
	path := filepath.Join(workPath, "dump.sql")
	if err := f.dump(ctx, source, password, path); err != nil {
		return ExecuteResult{}, err
	}
	return ExecuteResult{ProducedPath: path, Extension: ".sql", Method: "mysqldump", EngineMetadata: []byte("{}")}, nil
}
func (f fakeMySQLTool) Version(context.Context) (string, error) { return "mysqldump test-double", nil }
func (f fakeMySQLTool) Probe(context.Context, metadata.Source, string) (string, error) {
	return "8.4.13", nil
}
func (f fakeMySQLTool) Dump(ctx context.Context, source metadata.Source, password, path string) error {
	return f.dump(ctx, source, password, path)
}

func TestRunnerConcurrencyAndVerifiedArtifact(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := metadata.Open(ctx, filepath.Join(t.TempDir(), "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	source := metadata.Source{ID: "src_runner", Name: "源库", Engine: "mysql", Host: "127.0.0.1", Port: 3306, Database: "vault_source", Username: "vault", SecretRef: "sec_runner", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateSource(ctx, source, "ciphertext"); err != nil {
		t.Fatal(err)
	}
	target := metadata.StorageTarget{ID: "repo_runner", Name: "隔离仓库", Kind: "local", Root: t.TempDir(), Revision: 1, CreatedAt: now}
	if err = store.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	job := metadata.Job{ID: "job_runner", Name: "备份", SourceID: source.ID, TargetID: target.ID, Timezone: "UTC", ScheduleType: "interval", IntervalMinutes: 15, Enabled: true, RetentionDays: 30, MinFull: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	keys, err := security.OpenKeyStore(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := keys.Encrypt("test-password")
	if err != nil {
		t.Fatal(err)
	}
	// 用真实密文替换测试占位，验证执行器只在内存中解密。
	if err = store.UpdateSource(ctx, source, ciphertext); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	tool := fakeMySQLTool{dump: func(ctx context.Context, _ metadata.Source, password, path string) error {
		if password != "test-password" {
			t.Errorf("执行器未正确解密凭据")
		}
		close(started)
		<-release
		return os.WriteFile(path, []byte("CREATE DATABASE vault_source; CREATE TABLE sample(id INT);\n"), 0o600)
	}}
	runner := NewRunner(store, NewRegistry(tool), keys, 5*time.Second)
	if err = runner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	run, duplicate, err := runner.Trigger(ctx, job.ID, "manual", "same-key", nil)
	if err != nil || duplicate {
		t.Fatalf("首次触发失败: %v", err)
	}
	<-started
	_, duplicate, err = runner.Trigger(ctx, job.ID, "manual", "same-key", nil)
	if err != nil || !duplicate {
		t.Fatalf("重复幂等键应返回原运行: %v", err)
	}
	if _, _, err = runner.Trigger(ctx, job.ID, "manual", "different-key", nil); !errors.Is(err, metadata.ErrConflict) {
		t.Fatalf("不同请求并发执行应冲突: %v", err)
	}
	close(release)
	waitRunState(t, store, run.ID, "succeeded")
	completed, _ := store.GetRun(ctx, run.ID)
	if completed.CommandStatus != "succeeded" || completed.FileStatus != "verified" || completed.RestoreStatus != "not_run" {
		t.Fatalf("验证等级混淆: %+v", completed)
	}
	artifacts, err := store.ListArtifacts(ctx, job.ID)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("备份对象数量错误: %v %v", artifacts, err)
	}
	artifact := artifacts[0]
	path, err := SecurePath(target.Root, artifact.RelativePath, true)
	if err != nil {
		t.Fatal(err)
	}
	size, digest, err := HashFile(path)
	if err != nil || size != artifact.Size || digest != artifact.SHA256 {
		t.Fatalf("文件与元数据不一致: %v", err)
	}
}

func waitRunState(t *testing.T, store *metadata.Store, id, expected string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		run, err := store.GetRun(context.Background(), id)
		if err == nil && run.State == expected {
			return
		}
		if err == nil && run.State == "failed" {
			t.Fatalf("运行失败: %s %s", run.ErrorCode, run.ErrorMessage)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待运行 %s 进入 %s 超时", id, expected)
}
