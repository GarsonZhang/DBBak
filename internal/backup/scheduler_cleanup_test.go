package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dbbak/internal/metadata"
)

func TestSchedulerAutoCleanupUsesProtectionPipeline(t *testing.T) {
	ctx := context.Background()
	store, err := metadata.Open(ctx, filepath.Join(t.TempDir(), "dbbak.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	source := metadata.Source{ID: "src_auto", Name: "源", Engine: "mysql", Host: "localhost", Port: 3306, Database: "db", Username: "u", SecretRef: "sec_auto", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateSource(ctx, source, "cipher"); err != nil {
		t.Fatal(err)
	}
	target := metadata.StorageTarget{ID: "repo_auto", Name: "仓库", Kind: "local", Root: t.TempDir(), Revision: 1, CreatedAt: now}
	if err = store.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	job := metadata.Job{ID: "job_auto", Name: "自动清理", SourceID: source.ID, TargetID: target.ID, Timezone: "Asia/Shanghai", ScheduleType: "daily", TimeOfDay: "02:00", Enabled: true, RetentionDays: 30, MinFull: 1, AutoCleanup: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	oldArtifact := createTestArtifact(t, store, job, target, "auto-old", now.Add(-60*24*time.Hour))
	newArtifact := createTestArtifact(t, store, job, target, "auto-new", now)
	scheduler := NewScheduler(store, nil, NewCleanupService(store, nil), time.Second)
	scheduler.runAutoCleanup(ctx, now)
	deleted, err := store.GetArtifact(ctx, oldArtifact.ID)
	if err != nil || deleted.State != "deleted" {
		t.Fatalf("到期独立全量未被自动清理: %+v %v", deleted, err)
	}
	protected, err := store.GetArtifact(ctx, newArtifact.ID)
	if err != nil || protected.State != "verified" {
		t.Fatalf("最低份数备份被错误清理: %+v %v", protected, err)
	}
	if _, err = os.Stat(filepath.Join(target.Root, "db", "auto-new.sql")); err != nil {
		t.Fatalf("受保护文件不存在: %v", err)
	}
}
