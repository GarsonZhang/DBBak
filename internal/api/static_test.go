package api

import (
	"io/fs"
	"strings"
	"testing"

	"vault"
)

// 前端目录位于模块根的 web/；此测试锁定嵌入声明与子目录名一致。
// 若有人把 web 移出模块目录或改错 fs.Sub 路径，这里会直接失败。
func TestEmbeddedUIAssets(t *testing.T) {
	static, err := fs.Sub(vault.StaticFS, "web")
	if err != nil {
		t.Fatalf("前端子目录不存在，检查 embed.go 与目录结构: %v", err)
	}
	required := map[string]string{
		"index.html":    `id="sidebar"`,
		"app.js":        "/api/v1/",
		"style.css":     ".sidebar",
		"app-extra.css": ".login-panel",
	}
	for name, marker := range required {
		content, err := fs.ReadFile(static, name)
		if err != nil {
			t.Fatalf("嵌入资源缺失 %s: %v", name, err)
		}
		if len(content) == 0 {
			t.Fatalf("嵌入资源为空 %s", name)
		}
		if !strings.Contains(string(content), marker) {
			t.Fatalf("嵌入资源 %s 未包含预期标记 %q", name, marker)
		}
	}
}
