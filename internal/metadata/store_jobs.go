package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreateJob 持久化调度与保留策略。
func (s *Store) CreateJob(ctx context.Context, job Job) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id,name,source_id,target_id,timezone,schedule_type,time_of_day,weekday,interval_minutes,enabled,backup_type,retention_days,min_full,auto_cleanup,next_run_at,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		job.ID, job.Name, job.SourceID, job.TargetID, job.Timezone, job.ScheduleType, job.TimeOfDay, job.Weekday, job.IntervalMinutes, job.Enabled, job.BackupType, job.RetentionDays, job.MinFull, job.AutoCleanup, nullableTime(job.NextRunAt), job.Revision, formatTime(job.CreatedAt), formatTime(job.UpdatedAt))
	return err
}

// UpdateJob 使用 revision 防止覆盖并发修改。
func (s *Store) UpdateJob(ctx context.Context, job Job) error {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET name=?,source_id=?,target_id=?,timezone=?,schedule_type=?,time_of_day=?,weekday=?,interval_minutes=?,enabled=?,backup_type=?,retention_days=?,min_full=?,auto_cleanup=?,next_run_at=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`,
		job.Name, job.SourceID, job.TargetID, job.Timezone, job.ScheduleType, job.TimeOfDay, job.Weekday, job.IntervalMinutes, job.Enabled, job.BackupType, job.RetentionDays, job.MinFull, job.AutoCleanup, nullableTime(job.NextRunAt), formatTime(job.UpdatedAt), job.ID, job.Revision)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrConflict
	}
	return nil
}

// SetJobNextRun 推进调度时间，不改变配置 revision。
func (s *Store) SetJobNextRun(ctx context.Context, id string, next *time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET next_run_at=?,updated_at=? WHERE id=?`, nullableTime(next), formatTime(time.Now()), id)
	return err
}

// ListJobs 返回全部任务。
func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, jobSelect+` ORDER BY j.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

// ListDueJobs 返回到期启用任务。
func (s *Store) ListDueJobs(ctx context.Context, now time.Time) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, jobSelect+` WHERE j.enabled=1 AND j.next_run_at IS NOT NULL AND j.next_run_at<=? ORDER BY j.next_run_at`, formatTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

// GetJob 按 ID 获取任务。
func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx, jobSelect+` WHERE j.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return job, err
}

const jobSelect = `SELECT j.id,j.name,j.source_id,j.target_id,j.timezone,j.schedule_type,j.time_of_day,j.weekday,j.interval_minutes,j.enabled,j.backup_type,j.retention_days,j.min_full,j.auto_cleanup,j.next_run_at,j.revision,j.created_at,j.updated_at FROM jobs j`

func scanJob(row rowScanner) (Job, error) {
	var job Job
	var next sql.NullString
	var created, updated string
	err := row.Scan(&job.ID, &job.Name, &job.SourceID, &job.TargetID, &job.Timezone, &job.ScheduleType, &job.TimeOfDay, &job.Weekday, &job.IntervalMinutes, &job.Enabled, &job.BackupType, &job.RetentionDays, &job.MinFull, &job.AutoCleanup, &next, &job.Revision, &created, &updated)
	if err != nil {
		return Job{}, err
	}
	if job.NextRunAt, err = scanNullableTime(next); err != nil {
		return Job{}, err
	}
	if job.CreatedAt, err = parseTime(created); err != nil {
		return Job{}, fmt.Errorf("解析任务创建时间: %w", err)
	}
	job.UpdatedAt, err = parseTime(updated)
	return job, err
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}
