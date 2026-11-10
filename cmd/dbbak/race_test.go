package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"dbbak/internal/config"
	"dbbak/internal/metadata"
	"dbbak/internal/security"
)

// TestBootstrapAdminConcurrent 覆盖多个实例共用同一数据卷同时启动的场景。
//
// 两个进程可能同时看到 users 为空并各自建号。预期结果：双方都返回成功
// （其中一方把唯一约束冲突识别为「另一实例已建好」），最终只有一个同名账号。
// 不允许出现重复账号，也不允许任何一方因为竞争而启动失败——那会让容器反复重启。
func TestBootstrapAdminConcurrent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Config{
		DataDir:                dir,
		DatabasePath:           filepath.Join(dir, "dbbak.db"),
		BackupRoot:             filepath.Join(dir, "backups"),
		MasterKeyPath:          filepath.Join(dir, "master.key"),
		BootstrapAdminUser:     "admin",
		BootstrapAdminPassword: "concurrent-pass-1",
	}
	if err := cfg.EnsureDirectories(); err != nil {
		t.Fatal(err)
	}
	store, err := metadata.Open(ctx, cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- bootstrapAdmin(ctx, store, cfg)
		}()
	}
	wg.Wait()
	close(errs)
	var errCount int
	for e := range errs {
		if e != nil {
			errCount++
			t.Logf("并发建号返回错误: %v", e)
		}
	}
	// 竞争不应导致启动失败：双方都要成功。
	if errCount != 0 {
		t.Fatalf("并发建号不应返回错误，实际 %d 次", errCount)
	}

	// 用公开接口核对：账号存在且密码可用，说明没有出现「都没建成」。
	if has, err := store.HasUsers(ctx); err != nil || !has {
		t.Fatalf("并发建号后应有管理员: has=%t err=%v", has, err)
	}
	if _, hash, err := store.Authenticate(ctx, "admin"); err != nil {
		t.Fatalf("并发建号后应能查到用户: %v", err)
	} else if !security.VerifyPassword(hash, cfg.BootstrapAdminPassword) {
		t.Fatal("并发建号后密码校验失败")
	}
}
