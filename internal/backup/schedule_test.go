package backup

import (
	"testing"
	"time"

	"vault/internal/metadata"
)

func TestNextRunDailyTimezone(t *testing.T) {
	job := metadata.Job{Timezone: "Asia/Shanghai", ScheduleType: "daily", TimeOfDay: "02:30"}
	after := time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC) // 北京 09:00
	next, err := NextRun(job, after)
	if err != nil {
		t.Fatal(err)
	}
	if next.Format(time.RFC3339) != "2026-10-30T18:30:00Z" {
		t.Fatalf("下次执行时间错误: %s", next)
	}
}

func TestNextRunSkipsNonexistentDSTTime(t *testing.T) {
	job := metadata.Job{Timezone: "America/New_York", ScheduleType: "daily", TimeOfDay: "02:30"}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("本机缺少时区数据: %v", err)
	}
	after := time.Date(2026, 3, 8, 0, 0, 0, 0, location)
	next, err := NextRun(job, after)
	if err != nil {
		t.Fatal(err)
	}
	local := next.In(location)
	if local.YearDay() != after.YearDay()+1 || local.Hour() != 2 || local.Minute() != 30 {
		t.Fatalf("不存在的夏令时时刻未被跳过: %s", local)
	}
}

func TestNextRunWeeklyAndInterval(t *testing.T) {
	after := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) // 周五
	weekly := metadata.Job{Timezone: "UTC", ScheduleType: "weekly", TimeOfDay: "01:00", Weekday: 1}
	next, err := NextRun(weekly, after)
	if err != nil || next.Weekday() != time.Monday {
		t.Fatalf("周计划错误: %v %v", next, err)
	}
	interval := metadata.Job{Timezone: "UTC", ScheduleType: "interval", IntervalMinutes: 15}
	next, err = NextRun(interval, after)
	if err != nil || !next.Equal(after.Add(15*time.Minute)) {
		t.Fatalf("分钟计划错误: %v %v", next, err)
	}
}
