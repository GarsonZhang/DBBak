package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecurePathAndHash(t *testing.T) {
	root := t.TempDir()
	path, err := SecurePath(root, "db/2026/10/backup.sql", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("CREATE TABLE test(id INT);\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := SecurePath(root, "db/2026/10/backup.sql", true)
	if err != nil {
		t.Fatal(err)
	}
	size, digest, err := HashFile(resolved)
	if err != nil || size == 0 || len(digest) != 64 {
		t.Fatalf("校验失败 size=%d digest=%q err=%v", size, digest, err)
	}
	for _, invalid := range []string{"../outside.sql", "db/../../outside.sql", "/absolute.sql", `\root-relative.sql`, `C:\outside.sql`, `\\server\share\outside.sql`} {
		if _, err = SecurePath(root, invalid, false); err == nil {
			t.Errorf("路径 %q 应被拒绝", invalid)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.sql")
	if err = os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = SecurePath(root, outside, true); err == nil {
		t.Fatal("授权目录外的绝对路径应被拒绝")
	}
}

func TestCheckWritable(t *testing.T) {
	root := t.TempDir()
	if err := CheckWritable(root); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(root, ".vault-write-check-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("写检查临时文件未清理: %v %v", matches, err)
	}
}
