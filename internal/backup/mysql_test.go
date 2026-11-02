package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingMySQLToolFailsClearly(t *testing.T) {
	client := MySQLClient{MySQLDumpPath: filepath.Join(t.TempDir(), "missing-mysqldump")}
	_, err := client.Version(context.Background())
	if err == nil || ErrorCode(err) != "TOOL_MISSING" {
		t.Fatalf("缺失工具必须返回 TOOL_MISSING: %v", err)
	}
}

func TestErrorMessageRedactsPassword(t *testing.T) {
	message := sanitizeMessage("authentication failed for password=super-secret", "super-secret")
	if strings.Contains(message, "super-secret") || !strings.Contains(message, "[已脱敏]") {
		t.Fatalf("错误消息未脱敏: %s", message)
	}
}
