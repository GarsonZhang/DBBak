package metadata

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistenceAndRunIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dbbak.db")
	ctx := context.Background()
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	source := Source{ID: "src_test", Name: "测试", Engine: "mysql", Host: "127.0.0.1", Port: 3306, Database: "dbbak_source", Username: "dbbak", SecretRef: "sec_test", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateSource(ctx, source, "encrypted-test-value"); err != nil {
		t.Fatal(err)
	}
	target := StorageTarget{ID: "repo_test", Name: "隔离目录", Kind: "local", Root: t.TempDir(), Revision: 1, CreatedAt: now}
	if err = store.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	job := Job{ID: "job_test", Name: "测试任务", SourceID: source.ID, TargetID: target.ID, Timezone: "Asia/Shanghai", ScheduleType: "interval", IntervalMinutes: 15, Enabled: true, RetentionDays: 30, MinFull: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	run := Run{ID: "run_test", JobID: job.ID, TriggerType: "manual", IdempotencyKey: "same-request", State: "queued", Attempt: 1, CommandStatus: "not_run", FileStatus: "pending", RestoreStatus: "not_run", CreatedAt: now, UpdatedAt: now}
	if _, duplicate, err := store.CreateRun(ctx, run); err != nil || duplicate {
		t.Fatalf("首次运行创建失败: %v", err)
	}
	_, duplicate, err := store.CreateRun(ctx, run)
	if err != nil || !duplicate {
		t.Fatalf("相同幂等键应返回原运行: duplicate=%t err=%v", duplicate, err)
	}
	other := run
	other.ID = "run_other"
	other.IdempotencyKey = "other-request"
	if _, _, err = store.CreateRun(ctx, other); !errors.Is(err, ErrConflict) {
		t.Fatalf("活动运行应冲突: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.GetSource(ctx, source.ID); err != nil {
		t.Fatal("重启后数据源丢失", err)
	}
	if _, err = store.GetJob(ctx, job.ID); err != nil {
		t.Fatal("重启后任务丢失", err)
	}
	if _, err = store.GetRun(ctx, run.ID); err != nil {
		t.Fatal("重启后运行记录丢失", err)
	}
	if err = store.ReconcileRuns(ctx); err != nil {
		t.Fatal(err)
	}
	reconciled, err := store.GetRun(ctx, run.ID)
	if err != nil || reconciled.State != "interrupted" || reconciled.ErrorCode != "SERVICE_RESTARTED" {
		t.Fatalf("启动对账错误: %+v %v", reconciled, err)
	}
}
