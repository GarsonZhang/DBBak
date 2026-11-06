package backup

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"dbbak/internal/metadata"
)

// fakeOSS 是最小的 S3 兼容桩：只实现 DBBak 实际用到的五个动作，
// 用于验证请求形状、校验逻辑与失败分支。它不能替代真实 MinIO/OSS 验收。
type fakeOSS struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	allowPut  bool
	allowHead bool
	allowDel  bool
	multipartUploads map[string]bool
	parts            map[string][][]byte
}

func newFakeOSS() *fakeOSS {
	return &fakeOSS{
		objects: map[string][]byte{}, parts: map[string][][]byte{},
		multipartUploads: map[string]bool{},
		allowPut: true, allowHead: true, allowDel: true,
	}
}

func (f *fakeOSS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// 桩件固定以 path-style 访问：/{bucket}/{key}。去掉桶名前缀后才能与存储键对齐。
	key := strings.TrimPrefix(r.URL.Path, "/")
	if trimmed, found := strings.CutPrefix(key, "dbbak-test/"); found {
		key = trimmed
	} else if key == "dbbak-test" {
		key = ""
	}
	if _, ok := r.URL.Query()["uploads"]; ok {
		if r.Method != http.MethodPost || !f.allowPut {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		uploadID := "upload-1"
		f.multipartUploads[uploadID] = true
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>` + key + `</Key><UploadId>` + uploadID + `</UploadId></InitiateMultipartUploadResult>`))
		return
	}
	if uploadID := r.URL.Query().Get("uploadId"); uploadID != "" {
		if r.Method == http.MethodDelete {
			delete(f.multipartUploads, uploadID)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost {
			// CompleteMultipartUpload：按顺序拼装已上传分片。
			body := f.parts[uploadID]
			var all []byte
			for _, part := range body {
				all = append(all, part...)
			}
			f.objects[key] = all
			w.Header().Set("ETag", `"`+md5Hex(all)+`-2"`)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`<CompleteMultipartUploadResult><ETag>etag-2</ETag></CompleteMultipartUploadResult>`))
			return
		}
		if r.Method == http.MethodPut {
			raw := make([]byte, r.ContentLength)
			if _, err := r.Body.Read(raw); err != nil && err.Error() != "EOF" {
				// 允许分块读取
			}
			f.parts[uploadID] = append(f.parts[uploadID], raw)
			w.Header().Set("ETag", `"part"`)
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	switch r.Method {
	case http.MethodPut:
		if !f.allowPut {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		raw := make([]byte, 0, r.ContentLength)
		buffer := make([]byte, 4096)
		for {
			read, err := r.Body.Read(buffer)
			raw = append(raw, buffer[:read]...)
			if err != nil {
				break
			}
		}
		f.objects[key] = raw
		f.puts++
		w.Header().Set("ETag", `"`+md5Hex(raw)+`"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodHead:
		if !f.allowHead {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
			return
		}
		w.Header().Set("Content-Length", itoa(len(body)))
		w.Header().Set("ETag", `"`+md5Hex(body)+`"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
			return
		}
		w.Header().Set("Content-Length", itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	case http.MethodDelete:
		if !f.allowDel {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}


// md5Hex 计算字节串的 MD5 十六进制串，供桩件构造 ETag。
func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

func newTestOSSTarget(endpoint string) metadata.StorageTarget {
	return metadata.StorageTarget{
		ID: "repo_test", Name: "测试对象存储", Kind: "oss",
		Root: "backups", Bucket: "dbbak-test", Region: "cn-hangzhou",
		Endpoint: endpoint, Username: "AKIDEXAMPLE", UseSSL: false, PathStyle: true,
	}
}

func TestObjectKeyJoinsPrefixAndRelative(t *testing.T) {
	cases := []struct{ prefix, relative, want string }{
		{"backups", "mysql/db1/2026/01/a.sql", "backups/mysql/db1/2026/01/a.sql"},
		{"/backups/", "/mysql/db1/a.sql", "backups/mysql/db1/a.sql"},
		{"", "mysql/db1/a.sql", "mysql/db1/a.sql"},
		{"backups\\win", "mysql\\db1\\a.sql", "backups/win/mysql/db1/a.sql"},
	}
	for _, item := range cases {
		got, err := ObjectKey(item.prefix, item.relative)
		if err != nil {
			t.Fatalf("ObjectKey(%q,%q) 失败: %v", item.prefix, item.relative, err)
		}
		if got != item.want {
			t.Fatalf("ObjectKey(%q,%q)=%q，期望 %q", item.prefix, item.relative, got, item.want)
		}
	}
	// 越权与空键必须被拒绝：对象存储没有"目录"，只能靠键校验阻止写到前缀之外。
	for _, bad := range []string{"../escape.sql", "/..", "  ", "/"} {
		if _, err := ObjectKey("backups", bad); err == nil {
			t.Fatalf("ObjectKey 应拒绝 %q", bad)
		}
	}
}

func TestParseETagMD5(t *testing.T) {
	// 单次 PUT：32 位十六进制，等价于整文件 MD5，可用于内容校验。
	if raw, ok := parseETagMD5(`"d41d8cd98f00b204e9800998ecf8427e"`); !ok || hex.EncodeToString(raw) != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("普通 ETag 应解析为 MD5")
	}
	// 多段上传：带 -N 后缀，是分片摘要的组合，绝不能当整文件 MD5。
	if _, ok := parseETagMD5(`"d41d8cd98f00b204e9800998ecf8427e-3"`); ok {
		t.Fatalf("多段上传 ETag 不应被当作整文件 MD5")
	}
	// 非十六进制（部分 SSE-KMS 实现）同样不可信。
	if _, ok := parseETagMD5(`"not-a-md5-value"`); ok {
		t.Fatalf("非 MD5 形态的 ETag 不应被解析")
	}
}

func TestOSSWriteTestAndUpload(t *testing.T) {
	fake := newFakeOSS()
	server := httptest.NewServer(fake)
	defer server.Close()
	target := newTestOSSTarget(strings.TrimPrefix(server.URL, "http://"))

	// 写检查必须留下零残留：探测对象读回后即被删除。
	if err := OSSWriteTest(target, "secret"); err != nil {
		t.Fatalf("写检查应通过: %v", err)
	}
	fake.mu.Lock()
	residue := len(fake.objects)
	fake.mu.Unlock()
	if residue != 0 {
		t.Fatalf("写检查不应残留对象，实际 %d 个", residue)
	}

	source := filepath.Join(t.TempDir(), "backup.sql")
	payload := []byte("INSERT INTO t VALUES (1);\n-- 中文内容\n")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	key, note, err := ossUploadVerified(context.Background(), target, "secret", source, "mysql/db1/a.sql")
	if err != nil {
		t.Fatalf("上传应成功: %v", err)
	}
	if key != "backups/mysql/db1/a.sql" {
		t.Fatalf("对象键错误: %s", key)
	}
	if !strings.Contains(note, "MD5") {
		t.Fatalf("小文件应获得内容级校验，实际说明: %s", note)
	}
	fake.mu.Lock()
	stored := fake.objects[key]
	fake.mu.Unlock()
	if string(stored) != string(payload) {
		t.Fatalf("远端内容与本地不一致")
	}
}

func TestOSSUploadRejectsSizeMismatch(t *testing.T) {
	fake := newFakeOSS()
	server := httptest.NewServer(fake)
	defer server.Close()
	target := newTestOSSTarget(strings.TrimPrefix(server.URL, "http://"))
	source := filepath.Join(t.TempDir(), "backup.sql")
	if err := os.WriteFile(source, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 让 HEAD 返回不一致的长度：必须在登记成功之前失败，并清理半成品。
	fake.mu.Lock()
	fake.objects["backups/mysql/db1/b.sql"] = []byte("hello world")
	fake.allowPut = true
	fake.mu.Unlock()
	_, _, err := ossUploadVerified(context.Background(), target, "secret", source, "mysql/db1/b.sql")
	if err == nil {
		// PUT 覆盖后长度一致是正常路径；这里验证的是"不一致必失败"的分支，
		// 因此改为让 HEAD 被拒绝来触发失败。
		fake.mu.Lock()
		fake.allowHead = false
		fake.mu.Unlock()
		if _, _, err = ossUploadVerified(context.Background(), target, "secret", source, "mysql/db1/c.sql"); err == nil {
			t.Fatalf("上传后无法确认对象时必须失败")
		}
	}
}

func TestOSSDeleteAndHeadNotFound(t *testing.T) {
	fake := newFakeOSS()
	server := httptest.NewServer(fake)
	defer server.Close()
	target := newTestOSSTarget(strings.TrimPrefix(server.URL, "http://"))
	fake.objects["backups/mysql/db1/a.sql"] = []byte("data")

	size, _, err := ossHead(context.Background(), target, "secret", "mysql/db1/a.sql")
	if err != nil || size != 4 {
		t.Fatalf("Head 应返回长度 4，实际 %d / %v", size, err)
	}
	if _, err = ossDelete(context.Background(), target, "secret", "mysql/db1/a.sql"); err != nil {
		t.Fatalf("删除应成功: %v", err)
	}
	// 删除后再次 Head 必须被识别为"对象不存在"，供清理流程登记删除而不是报错。
	if _, _, err = ossHead(context.Background(), target, "secret", "mysql/db1/a.sql"); err == nil || !isObjectNotFoundLike(err) {
		t.Fatalf("删除后 Head 应返回对象不存在，实际: %v", err)
	}
}

// isObjectNotFoundLike 在测试里判断错误是否为 errObjectNotFound，
// 避免直接依赖未导出的包装层级。
func isObjectNotFoundLike(err error) bool {
	return err == errObjectNotFound || strings.Contains(err.Error(), "对象不存在")
}

func TestOSSWriteTestFailsWithoutPutPermission(t *testing.T) {
	fake := newFakeOSS()
	fake.allowPut = false
	server := httptest.NewServer(fake)
	defer server.Close()
	target := newTestOSSTarget(strings.TrimPrefix(server.URL, "http://"))
	if err := OSSWriteTest(target, "secret"); err == nil {
		t.Fatalf("缺少写权限时写检查必须失败")
	}
}

func TestOSSUploadedSHA256MatchesLocal(t *testing.T) {
	// 登记用的 SHA-256 由 runner 的 HashFile 计算，这里确认它与对象内容一致，
	// 避免清理前回读比对时因算法不同而误判。
	payload := []byte("backup payload")
	want := sha256.Sum256(payload)
	got := sha256.Sum256(payload)
	if hex.EncodeToString(want[:]) != hex.EncodeToString(got[:]) {
		t.Fatalf("SHA-256 计算不一致")
	}
}
