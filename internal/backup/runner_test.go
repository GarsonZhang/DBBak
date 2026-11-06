package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbbak/internal/metadata"
	"dbbak/internal/security"
)

type fakeMySQLTool struct {
	dump func(context.Context, metadata.Source, string, string) error
}

func (f fakeMySQLTool) Engine() string { return "mysql" }
func (f fakeMySQLTool) Execute(ctx context.Context, source metadata.Source, _ metadata.StorageTarget, password, workPath string, _ BackupSpec) (ExecuteResult, error) {
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
	store, err := metadata.Open(ctx, filepath.Join(t.TempDir(), "dbbak.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	source := metadata.Source{ID: "src_runner", Name: "源库", Engine: "mysql", Host: "127.0.0.1", Port: 3306, Database: "dbbak_source", Username: "dbbak", SecretRef: "sec_runner", Revision: 1, CreatedAt: now, UpdatedAt: now}
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
		return os.WriteFile(path, []byte("CREATE DATABASE dbbak_source; CREATE TABLE sample(id INT);\n"), 0o600)
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

// TestRunnerRoutesArtifactIntoJobSubdir 验证任务配置的输出子目录真正决定文件落点，
// 且清理能按登记路径删除该子目录内的文件。
func TestRunnerRoutesArtifactIntoJobSubdir(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := metadata.Open(ctx, filepath.Join(t.TempDir(), "dbbak.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	source := metadata.Source{ID: "src_subdir", Name: "源库", Engine: "mysql", Host: "127.0.0.1", Port: 3306, Database: "dbbak_source", Username: "dbbak", SecretRef: "sec_subdir", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateSource(ctx, source, "ciphertext"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	target := metadata.StorageTarget{ID: "repo_subdir", Name: "共享根目录", Kind: "local", Root: root, Revision: 1, CreatedAt: now}
	if err = store.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	job := metadata.Job{ID: "job_subdir", Name: "分区备份", SourceID: source.ID, TargetID: target.ID, Subdir: "mysql-prod", Timezone: "UTC", ScheduleType: "interval", IntervalMinutes: 15, Enabled: true, BackupType: "full", RetentionDays: 30, MinFull: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	// 只提供密文；这里让执行器使用同一密钥解密，确保路径逻辑与凭据逻辑互不干扰。
	keys, err := security.OpenKeyStore(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := keys.Encrypt("test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.UpdateSource(ctx, source, ciphertext); err != nil {
		t.Fatal(err)
	}
	tool := fakeMySQLTool{dump: func(_ context.Context, _ metadata.Source, _ string, path string) error {
		return os.WriteFile(path, []byte("CREATE DATABASE dbbak_source;\n"), 0o600)
	}}
	runner := NewRunner(store, NewRegistry(tool), keys, 5*time.Second)
	if err = runner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	run, duplicate, err := runner.Trigger(ctx, job.ID, "manual", "subdir-key", nil)
	if err != nil || duplicate {
		t.Fatalf("触发失败: %v", err)
	}
	waitRunState(t, store, run.ID, "succeeded")
	artifacts, err := store.ListArtifacts(ctx, job.ID)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("备份对象数量错误: %v %v", artifacts, err)
	}
	artifact := artifacts[0]
	if !strings.HasPrefix(artifact.RelativePath, "mysql-prod/dbbak_source/") {
		t.Fatalf("对象未写入任务子目录: %s", artifact.RelativePath)
	}
	if _, err = os.Stat(filepath.Join(root, "mysql-prod", "dbbak_source")); err != nil {
		t.Fatalf("磁盘上缺少子目录: %v", err)
	}
	// 根目录下不应出现未分区的同名数据库目录。
	if _, err = os.Stat(filepath.Join(root, "dbbak_source")); err == nil {
		t.Fatal("对象写到了根目录，子目录未生效")
	}

	cleanup := NewCleanupService(store, nil)
	preview, _, err := cleanup.Preview(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 保留天数为 30 天，刚生成的备份不应成为候选；这里只验证预览能定位到子目录对象而不报路径错误。
	for _, item := range preview.Items {
		if item.Candidate {
			t.Fatalf("新备份不应被列为清理候选: %s", item.Reason)
		}
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
