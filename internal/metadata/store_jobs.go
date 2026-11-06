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
	if job.LocationKind == "" {
		// 迁移前模式缺省为存储位置；显式 NULL 会绕过列默认值，必须在写入前归一化。
		job.LocationKind = "storage"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id,name,source_id,target_id,location_kind,server_dir,subdir,timezone,schedule_type,time_of_day,weekday,interval_minutes,enabled,backup_type,backup_method,retention_days,min_full,auto_cleanup,next_run_at,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		job.ID, job.Name, job.SourceID, nullableString(job.TargetID), job.LocationKind, job.ServerDir, job.Subdir, job.Timezone, job.ScheduleType, job.TimeOfDay, job.Weekday, job.IntervalMinutes, job.Enabled, job.BackupType, job.BackupMethod, job.RetentionDays, job.MinFull, job.AutoCleanup, nullableTime(job.NextRunAt), job.Revision, formatTime(job.CreatedAt), formatTime(job.UpdatedAt))
	return err
}

// UpdateJob 使用 revision 防止覆盖并发修改。
func (s *Store) UpdateJob(ctx context.Context, job Job) error {
	if job.LocationKind == "" {
		job.LocationKind = "storage"
	}
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET name=?,source_id=?,target_id=?,location_kind=?,server_dir=?,subdir=?,timezone=?,schedule_type=?,time_of_day=?,weekday=?,interval_minutes=?,enabled=?,backup_type=?,backup_method=?,retention_days=?,min_full=?,auto_cleanup=?,next_run_at=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`,
		job.Name, job.SourceID, nullableString(job.TargetID), nullableString(job.LocationKind), job.ServerDir, job.Subdir, job.Timezone, job.ScheduleType, job.TimeOfDay, job.Weekday, job.IntervalMinutes, job.Enabled, job.BackupType, job.BackupMethod, job.RetentionDays, job.MinFull, job.AutoCleanup, nullableTime(job.NextRunAt), formatTime(job.UpdatedAt), job.ID, job.Revision)
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

// DeleteJob 在同一事务中删除任务的运行日志、运行记录、清理预览与任务本身，
// 并写入独立的审计事件。备份文件必须已由调用方删除并用 DeleteArtifactRecord 标记，
// 否则这里会因外键约束拒绝删除，避免出现「记录没了、文件还在」的悬挂对象。
func (s *Store) DeleteJob(ctx context.Context, id, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var remaining int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE job_id=? AND state<>'deleted'`, id).Scan(&remaining); err != nil {
		return err
	}
	if remaining > 0 {
		return fmt.Errorf("%w: 仍有 %d 个备份对象未删除", ErrConflict, remaining)
	}
	var name, backupType string
	var retentionDays, minFull int
	if err = tx.QueryRowContext(ctx, `SELECT name,backup_type,retention_days,min_full FROM jobs WHERE id=?`, id).Scan(&name, &backupType, &retentionDays, &minFull); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	// 已删除对象的记录不再被任何运行引用；保留它们只会让外键把任务永久钉住，因此随任务一并清理。
	// 删除范围已由审计 detail 记录，文件本身在删除前已逐个核对摘要。
	if _, err = tx.ExecContext(ctx, `DELETE FROM artifacts WHERE job_id=? AND state='deleted'`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM run_logs WHERE run_id IN (SELECT id FROM runs WHERE job_id=?)`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM runs WHERE job_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM cleanup_previews WHERE job_id=?`, id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id=?`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	// 任务名在删除后已不可查，写入审计以保留可追溯的删除范围。
	detail := fmt.Sprintf("%s|%s|retention=%dd|min_full=%d", name, backupType, retentionDays, minFull)
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(actor,action,target,result,request_id,created_at) VALUES(?,?,?,?,?,?)`,
		actor, "job.delete", id, "success:"+detail, "", formatTime(time.Now())); err != nil {
		return fmt.Errorf("写入删除审计: %w", err)
	}
	return tx.Commit()
}

// DeleteRunLogs 清理运行日志；用于删除动作未完成时回收临时运行记录。
func (s *Store) DeleteRunLogs(ctx context.Context, runID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM run_logs WHERE run_id=?`, runID)
	return err
}

const jobSelect = `SELECT j.id,j.name,j.source_id,j.target_id,j.location_kind,j.server_dir,j.subdir,j.timezone,j.schedule_type,j.time_of_day,j.weekday,j.interval_minutes,j.enabled,j.backup_type,j.backup_method,j.retention_days,j.min_full,j.auto_cleanup,j.next_run_at,j.revision,j.created_at,j.updated_at FROM jobs j`

func scanJob(row rowScanner) (Job, error) {
	var job Job
	var next, targetID, locationKind sql.NullString
	var created, updated string
	err := row.Scan(&job.ID, &job.Name, &job.SourceID, &targetID, &locationKind, &job.ServerDir, &job.Subdir, &job.Timezone, &job.ScheduleType, &job.TimeOfDay, &job.Weekday, &job.IntervalMinutes, &job.Enabled, &job.BackupType, &job.BackupMethod, &job.RetentionDays, &job.MinFull, &job.AutoCleanup, &next, &job.Revision, &created, &updated)
	if err != nil {
		return Job{}, err
	}
	job.TargetID = targetID.String
	if job.LocationKind = locationKind.String; job.LocationKind == "" {
		// 迁移前的历史任务一律是存储位置模式。
		job.LocationKind = "storage"
	}
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

// nullableString 把空字符串写入为 NULL；target_id 等可空外键使用。
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
