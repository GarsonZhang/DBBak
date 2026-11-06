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
)

func removalFixture(t *testing.T) (*metadata.Store, metadata.Job, metadata.StorageTarget, metadata.Artifact, metadata.Artifact) {
	t.Helper()
	ctx := context.Background()
	store, err := metadata.Open(ctx, filepath.Join(t.TempDir(), "dbbak.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Now().UTC()
	source := metadata.Source{ID: "src_removal", Name: "源", Engine: "mysql", Host: "localhost", Port: 3306, Database: "db", Username: "u", SecretRef: "sec_removal", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateSource(ctx, source, "cipher"); err != nil {
		t.Fatal(err)
	}
	target := metadata.StorageTarget{ID: "repo_removal", Name: "仓库", Kind: "local", Root: t.TempDir(), Revision: 1, CreatedAt: now}
	if err = store.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	job := metadata.Job{ID: "job_removal", Name: "删除测试任务", SourceID: source.ID, TargetID: target.ID, Timezone: "UTC", ScheduleType: "daily", TimeOfDay: "02:00", Enabled: false, BackupType: "full", RetentionDays: 30, MinFull: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	older := createTestArtifact(t, store, job, target, "removal-a", now.Add(-48*time.Hour))
	newer := createTestArtifact(t, store, job, target, "removal-b", now.Add(-24*time.Hour))
	return store, job, target, older, newer
}

// 已校验备份在没有强制确认时只能预览、不能删除；强制确认名称不一致也要拒绝。
func TestRemovalRequiresForceConfirmation(t *testing.T) {
	ctx := context.Background()
	store, job, target, older, _ := removalFixture(t)
	service := NewRemovalService(store, nil)

	plan, err := service.Plan(ctx, RemovalTargetArtifact, older.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != "CONFIRMATION_REQUIRED" {
		t.Fatalf("已校验备份应先返回确认阻塞: %+v", plan.Blockers)
	}
	if _, err = service.Execute(ctx, RemovalTargetArtifact, older.ID, "", false, "tester"); !errors.Is(err, ErrRemovalBlocked) {
		t.Fatalf("无确认时执行必须被拒绝: %v", err)
	}
	if _, err = service.Execute(ctx, RemovalTargetArtifact, older.ID, "错误的名称", true, "tester"); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("确认名称不一致必须被拒绝: %v", err)
	}
	path, _ := SecurePath(target.Root, older.RelativePath, true)
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("被拒绝时文件不应删除: %v", statErr)
	}
	if _, getErr := store.GetJob(ctx, job.ID); getErr != nil {
		t.Fatalf("任务不应受影响: %v", getErr)
	}
}

// 强制删除单个对象：文件真实删除、元数据标记 deleted，审计可追溯。
func TestRemovalArtifactForceDeletesFile(t *testing.T) {
	ctx := context.Background()
	store, _, target, older, newer := removalFixture(t)
	service := NewRemovalService(store, nil)
	result, err := service.Execute(ctx, RemovalTargetArtifact, older.ID, older.RelativePath, true, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedFiles != 1 || result.FailedFiles != 0 {
		t.Fatalf("删除结果不符: %+v", result)
	}
	oldPath, _ := SecurePath(target.Root, older.RelativePath, true)
	if _, statErr := os.Stat(oldPath); !os.IsNotExist(statErr) {
		t.Fatalf("目标文件仍存在: %v", statErr)
	}
	deleted, err := store.GetArtifact(ctx, older.ID)
	if err != nil || deleted.State != "deleted" {
		t.Fatalf("元数据状态未更新: %v %+v", err, deleted)
	}
	newPath, _ := SecurePath(target.Root, newer.RelativePath, true)
	if _, statErr := os.Stat(newPath); statErr != nil {
		t.Fatalf("其他备份被误删: %v", statErr)
	}
}

// 文件摘要与登记不一致时必须停止删除，避免删掉未知内容或被替换的文件。
func TestRemovalRefusesHashMismatch(t *testing.T) {
	ctx := context.Background()
	store, _, target, older, _ := removalFixture(t)
	path, _ := SecurePath(target.Root, older.RelativePath, true)
	if err := os.WriteFile(path, []byte("tampered content"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := NewRemovalService(store, nil).Execute(ctx, RemovalTargetArtifact, older.ID, older.RelativePath, true, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if result.FailedFiles != 1 || result.DeletedFiles != 0 {
		t.Fatalf("摘要不一致必须拒绝删除: %+v", result)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("文件不应被删除: %v", statErr)
	}
}

// 有差异/日志依赖的恢复点不能被删除。
func TestRemovalBlocksChainDependency(t *testing.T) {
	ctx := context.Background()
	store, job, target, older, _ := removalFixture(t)
	now := time.Now().UTC()
	parent := older.ID
	child := metadata.Artifact{ID: "art_child", RunID: "run_child", JobID: job.ID, SourceID: job.SourceID, TargetID: target.ID, ChainID: older.ChainID, ParentID: &parent, Type: "differential", Method: "BACKUP DATABASE", RelativePath: "db/child.bak", Size: 1, SHA256: "x", State: "verified", ToolVersion: "test", CreatedAt: now}
	run := metadata.Run{ID: "run_child", JobID: job.ID, TriggerType: "manual", IdempotencyKey: "child", State: "succeeded", Attempt: 1, CommandStatus: "succeeded", FileStatus: "verified", RestoreStatus: "not_run", CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateArtifact(ctx, child); err != nil {
		t.Fatal(err)
	}
	plan, err := NewRemovalService(store, nil).Plan(ctx, RemovalTargetArtifact, older.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	blocked := false
	for _, blocker := range plan.Blockers {
		if blocker.Code == "CHAIN_DEPENDENT" {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("恢复链依赖必须阻塞删除: %+v", plan.Blockers)
	}
}

// 启用中的任务必须拒绝删除；暂停后先删文件再删记录。
func TestRemovalJobRequiresPausedThenDeletesEverything(t *testing.T) {
	ctx := context.Background()
	store, job, target, older, newer := removalFixture(t)
	service := NewRemovalService(store, nil)
	plan, err := service.Plan(ctx, RemovalTargetJob, job.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) != 0 {
		t.Fatalf("暂停任务不应被阻塞: %+v", plan.Blockers)
	}
	job.Enabled = true
	job.Revision = 1
	job.UpdatedAt = time.Now().UTC()
	if err = store.UpdateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	plan, err = service.Plan(ctx, RemovalTargetJob, job.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) == 0 || plan.Blockers[0].Code != "JOB_ENABLED" {
		t.Fatalf("启用中任务必须阻塞删除: %+v", plan.Blockers)
	}
	job, _ = store.GetJob(ctx, job.ID)
	job.Enabled = false
	if err = store.UpdateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(ctx, RemovalTargetJob, job.ID, job.Name, true, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if !result.JobDeleted || result.DeletedFiles != 2 || result.FailedFiles != 0 {
		t.Fatalf("删除任务结果不符: %+v", result)
	}
	for _, artifact := range []metadata.Artifact{older, newer} {
		path, _ := SecurePath(target.Root, artifact.RelativePath, true)
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("备份文件未删除: %s %v", path, statErr)
		}
	}
	if _, getErr := store.GetJob(ctx, job.ID); !errors.Is(getErr, metadata.ErrNotFound) {
		t.Fatalf("任务记录应已删除: %v", getErr)
	}
	if runs, listErr := store.ListRuns(ctx, job.ID, 100); listErr != nil || len(runs) != 0 {
		t.Fatalf("运行记录应一并删除: %v %d", listErr, len(runs))
	}
}

// 只删文件、保留任务时，任务配置仍在，列表变为无备份记录。
func TestRemovalJobFilesKeepsJob(t *testing.T) {
	ctx := context.Background()
	store, job, target, older, newer := removalFixture(t)
	result, err := NewRemovalService(store, nil).Execute(ctx, RemovalTargetJobFiles, job.ID, job.Name, true, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if result.JobDeleted || result.DeletedFiles != 2 {
		t.Fatalf("只删文件的结果不符: %+v", result)
	}
	if _, err = store.GetJob(ctx, job.ID); err != nil {
		t.Fatalf("任务应保留: %v", err)
	}
	for _, artifact := range []metadata.Artifact{older, newer} {
		path, _ := SecurePath(target.Root, artifact.RelativePath, true)
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("备份文件未删除: %s %v", path, statErr)
		}
	}
}

// createResidueArtifact 构造一条“备份中断后遗留的对象记录”，每个对象使用独立运行记录。
func createResidueArtifact(t *testing.T, store *metadata.Store, job metadata.Job, target metadata.StorageTarget, name, relative, state string) metadata.Artifact {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	run := metadata.Run{
		ID: "run_" + name, JobID: job.ID, TriggerType: "scheduled", IdempotencyKey: name,
		State: "failed", Attempt: 1, CommandStatus: "failed", FileStatus: "quarantined", RestoreStatus: "not_run",
		CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	artifact := metadata.Artifact{
		ID: "art_" + name, RunID: run.ID, JobID: job.ID, SourceID: job.SourceID, TargetID: target.ID,
		Type: "full", Method: "pg_basebackup", RelativePath: relative, Size: 0, State: state,
		ToolVersion: "test", CreatedAt: now,
	}
	if err := store.CreateArtifact(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	return artifact
}

// 备份中断会留下空的临时目录（pg_basebackup 的 .run_<id>.attempt-1）。
// 这类残留必须能删除，否则任务会永久卡在“部分文件未删除”。
func TestRemovalDeletesEmptyDirectoryResidue(t *testing.T) {
	ctx := context.Background()
	store, job, target, _, _ := removalFixture(t)

	relative := "postgres/2026/10/.run_7749645711870f9c1b1d068c.attempt-1"
	dirPath := filepath.Join(target.Root, filepath.FromSlash(relative))
	if err := os.MkdirAll(dirPath, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := createResidueArtifact(t, store, job, target, "dir_residue", relative, "quarantined")

	execution, err := NewRemovalService(store, nil).Execute(ctx, RemovalTargetArtifact, artifact.ID, relative, true, "tester")
	if err != nil {
		t.Fatalf("删除空目录残留不应报错: %v", err)
	}
	if execution.FailedFiles != 0 || len(execution.Files) != 1 || !execution.Files[0].Deleted {
		t.Fatalf("空目录残留应被删除: %+v", execution.Files)
	}
	if _, statErr := os.Stat(dirPath); !os.IsNotExist(statErr) {
		t.Fatalf("空目录应已移除: %v", statErr)
	}
	stored, getErr := store.GetArtifact(ctx, artifact.ID)
	if getErr != nil {
		t.Fatalf("记录查询失败: %v", getErr)
	}
	if stored.State != "deleted" {
		t.Fatalf("空目录删除后记录应标记 deleted: %s", stored.State)
	}
}

// 非 runner 命名的非空目录绝不能递归删除，必须如实拒绝并保留内容。
func TestRemovalRefusesNonEmptyDirectory(t *testing.T) {
	ctx := context.Background()
	store, job, target, _, _ := removalFixture(t)

	relative := "postgres/2026/10/full-backup-20261001"
	dirPath := filepath.Join(target.Root, filepath.FromSlash(relative))
	if err := os.MkdirAll(dirPath, 0o700); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(dirPath, "base.tar")
	if err := os.WriteFile(inner, []byte("base backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := createResidueArtifact(t, store, job, target, "dir_nonempty", relative, "quarantined")

	execution, err := NewRemovalService(store, nil).Execute(ctx, RemovalTargetArtifact, artifact.ID, relative, true, "tester")
	if err != nil {
		t.Fatalf("非空目录应返回可重试的失败结果，而不是错误: %v", err)
	}
	if execution.FailedFiles != 1 {
		t.Fatalf("非空目录必须拒绝删除: %+v", execution.Files)
	}
	if !strings.Contains(execution.Message, "目录非空") {
		t.Fatalf("失败信息应说明目录非空: %s", execution.Message)
	}
	if _, statErr := os.Stat(inner); statErr != nil {
		t.Fatalf("非空目录内容不能被删除: %v", statErr)
	}
	stored, getErr := store.GetArtifact(ctx, artifact.ID)
	if getErr != nil || stored.State == "deleted" {
		t.Fatalf("拒绝删除时记录应保留原状态: %v %+v", getErr, stored)
	}
}

// SQL Server 备份中断会在 .run_<id>.attempt-1 里留下 backup.sql 等中间产物。
// 这类 runner 自建的工作目录必须整棵清理，否则任务永远删不掉。
func TestRemovalCleansNonEmptyAttemptResidue(t *testing.T) {
	ctx := context.Background()
	store, job, target, _, _ := removalFixture(t)

	relative := "postgres/2026/10/.run_7f496d57f8f70fc9dbd068c.attempt-1"
	dirPath := filepath.Join(target.Root, filepath.FromSlash(relative))
	nested := filepath.Join(dirPath, "logs")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"backup.sql", filepath.Join("logs", "dump.log")} {
		if err := os.WriteFile(filepath.Join(dirPath, name), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	artifact := createResidueArtifact(t, store, job, target, "residue_nonempty", relative, "quarantined")

	execution, err := NewRemovalService(store, nil).Execute(ctx, RemovalTargetArtifact, artifact.ID, relative, true, "tester")
	if err != nil {
		t.Fatalf("清理中断残留不应报错: %v", err)
	}
	if execution.FailedFiles != 0 || len(execution.Files) != 1 || !execution.Files[0].Deleted {
		t.Fatalf("中断残留目录应被整棵清理: %+v", execution.Files)
	}
	if !strings.Contains(execution.Files[0].Message, "已清理中断残留目录") {
		t.Fatalf("删除信息应说明是残留清理: %s", execution.Files[0].Message)
	}
	if _, statErr := os.Stat(dirPath); !os.IsNotExist(statErr) {
		t.Fatalf("残留目录应已移除: %v", statErr)
	}
	stored, getErr := store.GetArtifact(ctx, artifact.ID)
	if getErr != nil {
		t.Fatalf("记录查询失败: %v", getErr)
	}
	if stored.State != "deleted" {
		t.Fatalf("残留清理后记录应标记 deleted: %s", stored.State)
	}
}

// 残留目录里出现符号链接时必须整体放弃：递归删除不能顺着链接跑到授权根目录之外。
func TestRemovalRefusesSymlinkInAttemptResidue(t *testing.T) {
	ctx := context.Background()
	store, job, target, _, _ := removalFixture(t)

	relative := "postgres/2026/10/.run_aabbccddeeff0011223344ff.attempt-1"
	dirPath := filepath.Join(target.Root, filepath.FromSlash(relative))
	if err := os.MkdirAll(dirPath, 0o700); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(dirPath, "base.tar")
	if err := os.WriteFile(kept, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dirPath, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("当前环境无法创建符号链接，跳过: %v", err)
	}
	artifact := createResidueArtifact(t, store, job, target, "residue_symlink", relative, "quarantined")

	execution, err := NewRemovalService(store, nil).Execute(ctx, RemovalTargetArtifact, artifact.ID, relative, true, "tester")
	if err != nil {
		t.Fatalf("符号链接应返回可重试的失败结果: %v", err)
	}
	if execution.FailedFiles != 1 {
		t.Fatalf("含符号链接的残留目录必须拒绝删除: %+v", execution.Files)
	}
	if _, statErr := os.Stat(kept); statErr != nil {
		t.Fatalf("拒绝时残留内容不能被删除: %v", statErr)
	}
	if data, readErr := os.ReadFile(outside); readErr != nil || string(data) != "must survive" {
		t.Fatalf("链接指向的外部文件不能被删除: %v %q", readErr, data)
	}
	stored, getErr := store.GetArtifact(ctx, artifact.ID)
	if getErr != nil || stored.State == "deleted" {
		t.Fatalf("拒绝删除时记录应保留原状态: %v %+v", getErr, stored)
	}
}

// 递归删除的范围必须能被准确识别：只有 runner 自己命名的工作目录算残留。
func TestAttemptResidueDetection(t *testing.T) {
	cases := map[string]bool{
		".run_7749645711870f9c1b1d068c.attempt-1": true,
		"run_7749645711870f9c1b1d068c.attempt-2":  true,
		"postgres":                                false,
		"full-backup-20261001":                    false,
		".run_7749645711870f9c1b1d068c":           false,
		".run_notahexid.attempt-1":                false,
		".run_7749645711870f9c1b1d068c.attempt-1.bak": false,
	}
	for name, want := range cases {
		if got := isAttemptResidueDir(name); got != want {
			t.Fatalf("isAttemptResidueDir(%q)=%v，期望 %v", name, got, want)
		}
	}
}

// 条目类型判定要拒绝符号链接与特殊文件；Windows 下普通用户无法创建符号链接，
// 因此这里直接构造 FileMode 覆盖判定分支。
func TestResidueEntryProblem(t *testing.T) {
	allowed := []os.FileMode{0o644, os.ModeDir | 0o755}
	for _, mode := range allowed {
		if err := residueEntryProblem("ok", mode); err != nil {
			t.Fatalf("普通条目不应被拒绝: %v %v", mode, err)
		}
	}
	rejected := []os.FileMode{
		os.ModeSymlink | 0o777,
		os.ModeDevice | 0o600,
		os.ModeNamedPipe | 0o600,
		os.ModeSocket | 0o600,
	}
	for _, mode := range rejected {
		if err := residueEntryProblem("bad", mode); err == nil {
			t.Fatalf("特殊条目必须被拒绝: %v", mode)
		}
	}
}

// 文件被外部删掉后，记录不能永久悬挂：应登记为已删除而不是报错。
func TestRemovalRegistersExternallyDeletedFile(t *testing.T) {
	ctx := context.Background()
	store, _, target, older, _ := removalFixture(t)

	path, err := SecurePath(target.Root, older.RelativePath, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}

	execution, err := NewRemovalService(store, nil).Execute(ctx, RemovalTargetArtifact, older.ID, older.RelativePath, true, "tester")
	if err != nil {
		t.Fatalf("文件已不存在时应登记删除: %v", err)
	}
	if execution.FailedFiles != 0 || !execution.Files[0].Deleted {
		t.Fatalf("文件缺失应登记为已删除: %+v", execution.Files)
	}
	stored, getErr := store.GetArtifact(ctx, older.ID)
	if getErr != nil {
		t.Fatalf("记录查询失败: %v", getErr)
	}
	if stored.State != "deleted" {
		t.Fatalf("文件缺失时记录应标记 deleted: %s", stored.State)
	}
}
