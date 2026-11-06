package backup

import (
	"os"
	"path/filepath"
	"strings"
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
	matches, err := filepath.Glob(filepath.Join(root, ".dbbak-write-check-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("写检查临时文件未清理: %v %v", matches, err)
	}
}

// TestNormalizeSubdir 覆盖任务级输出子目录的规范化与越界拦截。
func TestNormalizeSubdir(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"   ":                    "",
		"mysql-prod":             "mysql-prod",
		"  mysql/prod  ":         "mysql/prod",
		"mysql//prod/":           "mysql/prod",
		`mysql\prod`:             "mysql/prod",
		"生产/华东":                  "生产/华东",
		"./mysql-prod":           "mysql-prod",
		"mysql-prod/2026":        "mysql-prod/2026",
		"a b/c-d_e":              "a b/c-d_e",
		"mysql/./prod":           "mysql/prod",
		"mysql-prod/01月":         "mysql-prod/01月",
		"mysql-prod/.hidden-dir": "mysql-prod/.hidden-dir",
	}
	for input, expected := range cases {
		actual, err := NormalizeSubdir(input)
		if err != nil {
			t.Fatalf("子目录 %q 不应被拒绝: %v", input, err)
		}
		if actual != expected {
			t.Errorf("子目录 %q 规范化结果 %q，期望 %q", input, actual, expected)
		}
	}
	// 单段 . 表示“根目录本身”，与空值等价；多段中的 . 是冗余写法，Clean 后仍指向根目录内。
	for _, dotted := range []string{".", "./", "mysql/."} {
		if _, err := NormalizeSubdir(dotted); err != nil {
			t.Errorf("子目录 %q 属于根目录内路径，不应被拒绝: %v", dotted, err)
		}
	}
	invalid := []string{
		"../outside", "mysql/../../outside", "/absolute", `\absolute`, `C:\outside`,
		`\\server\share`, "mysql\x00prod", "mysql\nprod", "a:b", "a*b", "a?b", "a|b", "a<b", "a>b", `a"b`,
		"..", "../", "mysql/../../",
	}
	for _, input := range invalid {
		if _, err := NormalizeSubdir(input); err == nil {
			t.Errorf("子目录 %q 应被拒绝", input)
		}
	}
	long := strings.Repeat("a", 61)
	if _, err := NormalizeSubdir(long); err == nil {
		t.Errorf("单层超过 60 字符应被拒绝")
	}
}

// TestArtifactRelativePathGrouping 验证不同实例任务在根目录下的对象位置不同且仍在授权范围内。
func TestArtifactRelativePathGrouping(t *testing.T) {
	root := t.TempDir()
	first := ArtifactRelativePath("mysql-prod", "dbbak_source", "2026/11/run_a.sql")
	second := ArtifactRelativePath("pg-prod", "dbbak_source", "2026/11/run_b.sql")
	if first == second {
		t.Fatalf("不同子目录的同名数据库不应落到同一路径: %s", first)
	}
	if first != "mysql-prod/dbbak_source/2026/11/run_a.sql" {
		t.Fatalf("子目录拼接结果异常: %s", first)
	}
	if legacy := ArtifactRelativePath("", "dbbak_source", "2026/11/run_c.sql"); legacy != "dbbak_source/2026/11/run_c.sql" {
		t.Fatalf("空子目录应保持旧的根目录布局: %s", legacy)
	}
	for _, relative := range []string{first, second} {
		path, err := SecurePath(root, relative, false)
		if err != nil {
			t.Fatalf("子目录对象路径应通过授权校验: %s %v", relative, err)
		}
		if rel, relErr := filepath.Rel(root, path); relErr != nil || strings.HasPrefix(rel, "..") {
			t.Fatalf("对象路径越出授权根目录: %s", path)
		}
	}
}
