package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dbbak/internal/metadata"
)

func TestCleanupPreviewExecutionAndStaleProtection(t *testing.T) {
	ctx := context.Background()
	store, err := metadata.Open(ctx, filepath.Join(t.TempDir(), "dbbak.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	source := metadata.Source{ID: "src_cleanup", Name: "源", Engine: "mysql", Host: "localhost", Port: 3306, Database: "db", Username: "u", SecretRef: "sec_cleanup", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateSource(ctx, source, "cipher"); err != nil {
		t.Fatal(err)
	}
	target := metadata.StorageTarget{ID: "repo_cleanup", Name: "仓库", Kind: "local", Root: t.TempDir(), Revision: 1, CreatedAt: now}
	if err = store.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	job := metadata.Job{ID: "job_cleanup", Name: "任务", SourceID: source.ID, TargetID: target.ID, Timezone: "UTC", ScheduleType: "daily", TimeOfDay: "02:00", Enabled: true, RetentionDays: 30, MinFull: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	oldArtifact := createTestArtifact(t, store, job, target, "old", now.Add(-60*24*time.Hour))
	newArtifact := createTestArtifact(t, store, job, target, "new", now.Add(-24*time.Hour))
	oldPath, err := SecurePath(target.Root, oldArtifact.RelativePath, true, false)
	if err != nil {
		t.Fatal(err)
	}
	service := NewCleanupService(store, nil)
	result, preview, err := service.Preview(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.CandidateSize != oldArtifact.Size {
		t.Fatalf("候选大小错误: %+v", result)
	}
	var candidates int
	for _, item := range result.Items {
		if item.Candidate {
			candidates++
			if item.Artifact.ID != oldArtifact.ID {
				t.Fatalf("错误候选: %+v", item)
			}
		}
	}
	if candidates != 1 {
		t.Fatalf("应有 1 个候选，实际 %d", candidates)
	}
	executions, err := service.Execute(ctx, preview.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(executions) != 1 || !executions[0].Deleted {
		t.Fatalf("清理执行失败: %+v", executions)
	}
	if _, err = os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("过期文件仍存在: %v", err)
	}
	if _, err = SecurePath(target.Root, newArtifact.RelativePath, true, false); err != nil {
		t.Fatalf("受保护文件被删除: %v", err)
	}

	_, stale, err := service.Preview(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetArtifactLocked(ctx, newArtifact.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Execute(ctx, stale.ID); !errors.Is(err, metadata.ErrConflict) {
		t.Fatalf("状态变化后旧预览应被拒绝: %v", err)
	}
}

func createTestArtifact(t *testing.T, store *metadata.Store, job metadata.Job, target metadata.StorageTarget, name string, created time.Time) metadata.Artifact {
	t.Helper()
	ctx := context.Background()
	run := metadata.Run{ID: "run_" + name, JobID: job.ID, TriggerType: "manual", IdempotencyKey: name, State: "succeeded", Attempt: 1, CommandStatus: "succeeded", FileStatus: "verified", RestoreStatus: "not_run", CreatedAt: created, UpdatedAt: created, FinishedAt: &created}
	if _, _, err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	relative := "db/" + name + ".sql"
	path, err := SecurePath(target.Root, relative, false, false)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("-- test backup " + name + "\n")
	if err = os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	size, digest, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	artifact := metadata.Artifact{ID: "art_" + name, RunID: run.ID, JobID: job.ID, SourceID: job.SourceID, TargetID: target.ID, ChainID: "chain_" + name, Type: "full", Method: "mysqldump", RelativePath: relative, Size: size, SHA256: digest, State: "verified", ToolVersion: "test", CreatedAt: created, VerifiedAt: &created}
	if err = store.CreateArtifact(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	return artifact
}
