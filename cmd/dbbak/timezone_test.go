package main

import (
	"testing"
	"time"

	"dbbak/internal/backup"
	"dbbak/internal/metadata"
)

// TestTimezoneDataAvailable 守住时区内嵌：发行机的 Windows 没有 zoneinfo 目录，
// 只有 import _ "time/tzdata" 才能解析标准时区名。这个断言在装过 Go 的开发机上
// 会因为 GOROOT 回退而恒真，所以真正生效的是嵌进二进制的数据。
func TestTimezoneDataAvailable(t *testing.T) {
	for _, name := range []string{"Asia/Shanghai", "UTC", "America/New_York"} {
		if _, err := time.LoadLocation(name); err != nil {
			t.Fatalf("时区 %s 无法加载，检查 cmd/dbbak 是否仍导入 time/tzdata: %v", name, err)
		}
	}
}

// TestDiagnoseTimezoneFailure 覆盖“无效时区”的可读提示：用户看到的是具体时区名
// 和可执行的下一步，而不是一个无法自行解决的表单错误。
func TestDiagnoseTimezoneFailure(t *testing.T) {
	_, err := backup.NextRun(metadata.Job{Timezone: "Mars/Olympus", ScheduleType: "daily", TimeOfDay: "02:00"}, time.Now())
	if err == nil {
		t.Fatal("非法时区必须报错，不能静默降级")
	}
	if got := err.Error(); !contains(got, "Mars/Olympus") || !contains(got, "tzdata") {
		t.Fatalf("错误信息应包含时区名和解决方向，实际: %s", got)
	}
	if _, err = backup.NextRun(metadata.Job{Timezone: "", ScheduleType: "daily", TimeOfDay: "02:00"}, time.Now()); err == nil || !contains(err.Error(), "时区必填") {
		t.Fatalf("空时区应提示必填，实际: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
