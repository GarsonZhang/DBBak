package backup

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"dbbak/internal/metadata"
)

// Scheduler 在后台计算到期任务，与浏览器连接无关。
type Scheduler struct {
	store          *metadata.Store
	runner         *Runner
	cleanup        *CleanupService
	interval       time.Duration
	lastCleanupDay string
}

// NewScheduler 创建单实例调度器。
func NewScheduler(store *metadata.Store, runner *Runner, cleanup *CleanupService, interval time.Duration) *Scheduler {
	return &Scheduler{store: store, runner: runner, cleanup: cleanup, interval: interval}
}

// Start 启动持续调度循环，上下文取消时停止领取新任务。
func (s *Scheduler) Start(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.tick(ctx)
	s.runAutoCleanup(ctx, time.Now().UTC())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.tickWithTime(ctx, now.UTC())
			s.runAutoCleanup(ctx, now.UTC())
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) { s.tickWithTime(ctx, time.Now().UTC()) }

func (s *Scheduler) tickWithTime(ctx context.Context, now time.Time) {
	jobs, err := s.store.ListDueJobs(ctx, now)
	if err != nil {
		log.Printf("查询到期任务失败: %v", err)
		return
	}
	for _, job := range jobs {
		if job.NextRunAt == nil {
			continue
		}
		scheduled := *job.NextRunAt
		// 长时间停机只记录一次错过事件并推进到未来，不集中补跑历史计划。
		if scheduled.Before(now.Add(-2 * s.interval)) {
			if err = s.recordMissed(ctx, job, scheduled, now); err != nil {
				log.Printf("记录任务 %s 错过计划失败: %v", job.ID, err)
				continue
			}
		} else {
			key := "scheduled:" + scheduled.Format(time.RFC3339Nano)
			if _, _, err = s.runner.Trigger(ctx, job.ID, "scheduled", key, &scheduled); err != nil && !isConflict(err) {
				log.Printf("触发任务 %s 失败: %v", job.ID, err)
				continue
			}
		}
		next, nextErr := nextAfter(job, scheduled, now)
		if nextErr != nil {
			log.Printf("计算任务 %s 下次计划失败: %v", job.ID, nextErr)
			continue
		}
		if err = s.store.SetJobNextRun(ctx, job.ID, &next); err != nil {
			log.Printf("推进任务 %s 计划失败: %v", job.ID, err)
		}
	}
}

func (s *Scheduler) recordMissed(ctx context.Context, job metadata.Job, scheduled, now time.Time) error {
	run := metadata.Run{
		ID: metadata.NewID("run"), JobID: job.ID, TriggerType: "scheduled", ScheduledAt: &scheduled,
		IdempotencyKey: "scheduled:" + scheduled.Format(time.RFC3339Nano), State: "missed", Attempt: 1,
		CommandStatus: "not_run", FileStatus: "not_created", RestoreStatus: "not_run",
		ErrorCode: "SCHEDULE_MISSED", ErrorMessage: "服务未在计划窗口内执行，按策略不集中补跑",
		CreatedAt: now, UpdatedAt: now, FinishedAt: &now,
	}
	_, _, err := s.store.CreateRun(ctx, run)
	if err == nil {
		_ = s.store.AddRunLog(ctx, run.ID, "WARN", run.ErrorMessage)
	}
	if isConflict(err) {
		return nil
	}
	return err
}

func nextAfter(job metadata.Job, scheduled, now time.Time) (time.Time, error) {
	cursor := scheduled
	for count := 0; count < 10000; count++ {
		next, err := NextRun(job, cursor)
		if err != nil {
			return time.Time{}, err
		}
		if next.After(now) {
			return next, nil
		}
		cursor = next
	}
	return time.Time{}, fmt.Errorf("计划推进次数过多")
}

func isConflict(err error) bool {
	return errors.Is(err, metadata.ErrConflict) || err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// runAutoCleanup 只处理用户明确启用的独立全量任务，复用人工清理的完整保护链路。
func (s *Scheduler) runAutoCleanup(ctx context.Context, now time.Time) {
	if s.cleanup == nil {
		return
	}
	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		log.Printf("自动清理读取任务失败: %v", err)
		return
	}
	for _, job := range jobs {
		if !job.AutoCleanup {
			continue
		}
		location, locationErr := time.LoadLocation(job.Timezone)
		if locationErr != nil {
			// 时区坏掉时宁可不清理：按 UTC 猜测会在错误的本地日期删文件。
			log.Printf("自动清理跳过任务 %s：时区 %q 无法加载: %v", job.ID, job.Timezone, locationErr)
			_ = s.store.Audit(ctx, "scheduler", "cleanup.auto", job.ID, "timezone_invalid", locationErr.Error())
			continue
		}
		localNow := now.In(location)
		day := localNow.Format("2006-01-02")
		if localNow.Hour() < 5 || s.lastCleanupDay == day {
			continue
		}
		s.lastCleanupDay = day
		result, preview, previewErr := s.cleanup.Preview(ctx, job.ID)
		if previewErr != nil {
			log.Printf("自动清理预览任务 %s 失败: %v", job.ID, previewErr)
			_ = s.store.Audit(ctx, "scheduler", "cleanup.auto", job.ID, "preview_failed", "")
			continue
		}
		candidates := 0
		for _, item := range result.Items {
			if item.Candidate {
				candidates++
			}
		}
		if candidates == 0 {
			continue
		}
		executions, executeErr := s.cleanup.Execute(ctx, preview.ID)
		if executeErr != nil {
			log.Printf("自动清理任务 %s 被复查拒绝: %v", job.ID, executeErr)
			_ = s.store.Audit(ctx, "scheduler", "cleanup.auto", job.ID, "rejected", "")
			continue
		}
		failed := 0
		for _, execution := range executions {
			if !execution.Deleted {
				failed++
			}
		}
		resultStatus := "success"
		if failed > 0 {
			resultStatus = "partial_failure"
		}
		_ = s.store.Audit(ctx, "scheduler", "cleanup.auto", job.ID, resultStatus, "")
		log.Printf("自动清理任务 %s：候选 %d，失败 %d", job.ID, candidates, failed)
	}
}
