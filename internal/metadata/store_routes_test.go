package metadata

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// 恢复链必须按备份路线隔离：逻辑与物理产物不能互为基线，
// 否则日志备份会挂到语义不同的基线上，恢复必然失败。
func TestArtifactsAreScopedByBackupRoute(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "dbbak.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	source := Source{ID: "src_route", Name: "路线源", Engine: "mysql", Host: "127.0.0.1", Port: 3306, Database: "shop", Username: "u", SecretRef: "sec_route", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateSource(ctx, source, "cipher"); err != nil {
		t.Fatal(err)
	}
	target := StorageTarget{ID: "repo_route", Name: "目录", Kind: "local", Root: t.TempDir(), Revision: 1, CreatedAt: now}
	if err = store.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	job := Job{ID: "job_route", Name: "物理任务", SourceID: source.ID, TargetID: target.ID, LocationKind: "storage", Timezone: "Asia/Shanghai", ScheduleType: "daily", TimeOfDay: "02:00", Enabled: false, BackupType: "full", BackupMethod: "physical", RetentionDays: 30, MinFull: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	// 路线属于任务配置，必须能原样读回。
	storedJob, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedJob.BackupMethod != "physical" {
		t.Fatalf("任务备份路线未持久化: %q", storedJob.BackupMethod)
	}

	createArtifact := func(runID, artifactID, method, backupMethod string) {
		run := Run{ID: runID, JobID: job.ID, TriggerType: "manual", IdempotencyKey: runID, State: "succeeded", Attempt: 1, CommandStatus: "succeeded", FileStatus: "verified", RestoreStatus: "not_run", CreatedAt: now, UpdatedAt: now}
		if _, _, err := store.CreateRun(ctx, run); err != nil {
			t.Fatalf("创建运行 %s 失败: %v", runID, err)
		}
		artifact := Artifact{
			ID: artifactID, RunID: runID, JobID: job.ID, SourceID: source.ID, TargetID: target.ID,
			ChainID: artifactID, Type: "full", Method: method, BackupMethod: backupMethod,
			RelativePath: "shop/2026/11/" + artifactID, Size: 10, SHA256: "hash",
			State: "verified", CreatedAt: now,
		}
		if err := store.CreateArtifact(ctx, artifact); err != nil {
			t.Fatalf("创建对象 %s 失败: %v", artifactID, err)
		}
	}
	createArtifact("run_physical", "art_physical", "xtrabackup", "physical")
	createArtifact("run_logical", "art_logical", "mysqldump", "logical")

	physicalOnly, err := store.LatestVerifiedArtifacts(ctx, source.ID, target.ID, "physical", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(physicalOnly) != 1 || physicalOnly[0].Method != "xtrabackup" {
		t.Fatalf("物理路线查询应只返回物理对象: %+v", physicalOnly)
	}
	if physicalOnly[0].BackupMethod != "physical" {
		t.Fatalf("对象备份路线未持久化: %q", physicalOnly[0].BackupMethod)
	}
	all, err := store.LatestVerifiedArtifacts(ctx, source.ID, target.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("不限定路线时应返回两条对象，实际 %d", len(all))
	}
}
