package backup

import (
	"fmt"
	"time"

	"dbbak/internal/metadata"
)

// NextRun 计算下一次 UTC 触发时间。每日/每周按任务时区，分钟间隔按绝对时间推进。
func NextRun(job metadata.Job, after time.Time) (time.Time, error) {
	// time.LoadLocation("") 会静默返回 UTC，任务会按错误时区触发，必须先拦下。
	if job.Timezone == "" {
		return time.Time{}, fmt.Errorf("时区必填，例如 Asia/Shanghai")
	}
	location, err := time.LoadLocation(job.Timezone)
	if err != nil {
		// 时区数据缺失属于运行环境问题，和用户填错时区要分开提示，
		// 否则用户只会看到一个无法自行解决的表单错误。
		return time.Time{}, fmt.Errorf("无效时区 %s：服务端缺少时区数据，请确认程序为内嵌 time/tzdata 的正式构建（标准时区名如 Asia/Shanghai、UTC）", job.Timezone)
	}
	if job.ScheduleType == "interval" {
		if job.IntervalMinutes < 1 || job.IntervalMinutes > 1440 {
			return time.Time{}, fmt.Errorf("分钟间隔必须在 1 到 1440 之间")
		}
		return after.UTC().Add(time.Duration(job.IntervalMinutes) * time.Minute), nil
	}
	var hour, minute int
	if _, err = fmt.Sscanf(job.TimeOfDay, "%d:%d", &hour, &minute); err != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return time.Time{}, fmt.Errorf("执行时间必须为 HH:MM")
	}
	localAfter := after.In(location)
	for day := 0; day < 15; day++ {
		date := localAfter.AddDate(0, 0, day)
		candidate := time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, location)
		// 夏令时不存在的本地时刻会归一化到其他时间，跳过该日。
		if candidate.Hour() != hour || candidate.Minute() != minute {
			continue
		}
		if job.ScheduleType == "weekly" && int(candidate.Weekday()) != job.Weekday {
			continue
		}
		if candidate.After(after) {
			return candidate.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("无法计算下次执行时间")
}

// ValidateSchedule 校验阶段一支持的调度字段。
func ValidateSchedule(job metadata.Job) error {
	if job.ScheduleType != "daily" && job.ScheduleType != "weekly" && job.ScheduleType != "interval" {
		return fmt.Errorf("阶段一仅支持 daily、weekly、interval 调度")
	}
	if job.ScheduleType == "weekly" && (job.Weekday < 0 || job.Weekday > 6) {
		return fmt.Errorf("weekday 必须在 0 到 6 之间")
	}
	_, err := NextRun(job, time.Now())
	return err
}
