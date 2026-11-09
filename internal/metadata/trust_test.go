package metadata

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// trust_server_certificate 必须能持久化并在更新后回读；默认值来自迁移，不能靠代码兜底。
func TestSourceTrustServerCertificatePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dbbak.db")
	ctx := context.Background()
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	source := Source{ID: "src_tls", Name: "SQL", Engine: "sqlserver", Host: "10.0.0.5", Port: 1433, Database: "TCE-Test", Username: "sa", TrustServerCertificate: true, SecretRef: "sec_tls", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.CreateSource(ctx, source, "cipher"); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSource(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TrustServerCertificate {
		t.Fatalf("新建时勾选的信任开关没有持久化")
	}
	got.TrustServerCertificate = false
	got.UpdatedAt = time.Now().UTC()
	if err = store.UpdateSource(ctx, got, ""); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetSource(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TrustServerCertificate {
		t.Fatalf("取消勾选后仍读取到信任")
	}
}
