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
	if storedJob.BackupCompression != "" {
		t.Fatalf("未指定压缩时 backup_compression 必须为空串，实际 %q", storedJob.BackupCompression)
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

// 迁移 11 必须能在已有数据库上升级：备份压缩是任务级配置，
// 历史任务升级后必须为空串（不写压缩子句），否则既有 SQL Server
// 备份语句会被悄悄改写。
func TestJobCompressionMigrationKeepsHistory(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "dbbak.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err = store.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentMigrationVersion || version < 11 {
		t.Fatalf("迁移版本应为 %d 且包含迁移 11，实际 %d", currentMigrationVersion, version)
	}
	// 模拟升级前写入的历史任务：按旧列清单插入（不含 backup_compression），
	// 该列取迁移默认值，读回时必须仍是空串。
	now := formatTime(time.Now().UTC())
	source := Source{ID: "src_mig", Name: "升级源", Engine: "sqlserver", Host: "127.0.0.1", Port: 1433, Database: "shop", Username: "sa", SecretRef: "sec_mig", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err = store.CreateSource(ctx, source, "cipher"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.ExecContext(ctx, `INSERT INTO jobs(id,name,source_id,target_id,location_kind,server_dir,subdir,timezone,schedule_type,time_of_day,weekday,interval_minutes,enabled,backup_type,backup_method,retention_days,min_full,auto_cleanup,next_run_at,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"job_mig", "历史任务", source.ID, nil, "storage", "", "", "Asia/Shanghai", "daily", "02:00", 0, 15, false, "full", "", 30, 1, false, nil, 1, now, now); err != nil {
		t.Fatalf("写入历史任务失败: %v", err)
	}
	stored, err := store.GetJob(ctx, "job_mig")
	if err != nil {
		t.Fatalf("历史任务读取失败: %v", err)
	}
	if stored.BackupCompression != "" {
		t.Fatalf("历史任务的压缩选项必须为空串，实际 %q", stored.BackupCompression)
	}
	// 写入一个压缩任务并确认能读回。
	stored.BackupCompression = "zstd_medium"
	stored.UpdatedAt = time.Now().UTC()
	if err = store.UpdateJob(ctx, stored); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.GetJob(ctx, "job_mig")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.BackupCompression != "zstd_medium" {
		t.Fatalf("压缩选项未持久化: %q", reloaded.BackupCompression)
	}
}
