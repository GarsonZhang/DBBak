package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CreateRun 原子创建运行；幂等键重复时返回原运行，活动冲突返回 ErrConflict。
func (s *Store) CreateRun(ctx context.Context, run Run) (Run, bool, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs(id,job_id,trigger_type,scheduled_at,idempotency_key,state,attempt,command_status,file_status,restore_status,error_code,error_message,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, run.JobID, run.TriggerType, nullableTime(run.ScheduledAt), run.IdempotencyKey, run.State, run.Attempt, run.CommandStatus, run.FileStatus, run.RestoreStatus, "", "", formatTime(run.CreatedAt), formatTime(run.UpdatedAt))
	if err == nil {
		return run, false, nil
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return Run{}, false, err
	}
	if run.IdempotencyKey != "" {
		existing, getErr := s.GetRunByIdempotency(ctx, run.JobID, run.IdempotencyKey)
		if getErr == nil {
			return existing, true, nil
		}
	}
	return Run{}, false, fmt.Errorf("%w: 任务已有活动运行或重复触发", ErrConflict)
}

// GetRunByIdempotency 查询重复手动触发对应的原运行。
func (s *Store) GetRunByIdempotency(ctx context.Context, jobID, key string) (Run, error) {
	run, err := scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE r.job_id=? AND r.idempotency_key=?`, jobID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return run, err
}

// GetRun 按 ID 获取运行。
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	run, err := scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE r.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return run, err
}

// ListRuns 按可选任务筛选并限制数量。
func (s *Store) ListRuns(ctx context.Context, jobID string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := runSelect
	args := []any{}
	if jobID != "" {
		query += ` WHERE r.job_id=?`
		args = append(args, jobID)
	}
	query += ` ORDER BY r.created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return result, rows.Err()
}

// UpdateRun 持久化完整状态，由执行器在状态转换时调用。
func (s *Store) UpdateRun(ctx context.Context, run Run) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET state=?,command_status=?,file_status=?,restore_status=?,error_code=?,error_message=?,started_at=?,finished_at=?,updated_at=? WHERE id=?`,
		run.State, run.CommandStatus, run.FileStatus, run.RestoreStatus, run.ErrorCode, run.ErrorMessage, nullableTime(run.StartedAt), nullableTime(run.FinishedAt), formatTime(time.Now()), run.ID)
	return err
}

// ActiveRunForJob 判断任务是否已有未结束运行。
func (s *Store) ActiveRunForJob(ctx context.Context, jobID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE job_id=? AND state IN ('queued','running','verifying','reconciling')`, jobID).Scan(&count)
	return count > 0, err
}

// ReconcileRuns 在服务启动时把无法继续确认的活动运行标记中断。
func (s *Store) ReconcileRuns(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := formatTime(time.Now())
	if _, err = tx.ExecContext(ctx, `UPDATE runs SET state='interrupted',error_code='SERVICE_RESTARTED',error_message='服务重启，原运行状态无法继续确认，需要人工核查',finished_at=?,updated_at=? WHERE state IN ('queued','running','verifying','reconciling')`, now, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE artifacts SET state='quarantined' WHERE state IN ('writing','uploaded')`); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteRun 删除单条运行记录及其日志；仅用于删除动作未完成时回收临时运行。
func (s *Store) DeleteRun(ctx context.Context, runID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM run_logs WHERE run_id=?`, runID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM runs WHERE id=?`, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// AddRunLog 添加已脱敏日志。
func (s *Store) AddRunLog(ctx context.Context, runID, level, message string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_logs(run_id,ts,level,message) VALUES(?,?,?,?)`, runID, formatTime(time.Now()), level, message)
	return err
}

// ListRunLogs 按写入顺序返回运行日志。
func (s *Store) ListRunLogs(ctx context.Context, runID string) ([]RunLog, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,run_id,ts,level,message FROM run_logs WHERE run_id=? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RunLog, 0)
	for rows.Next() {
		var log RunLog
		var ts string
		if err := rows.Scan(&log.ID, &log.RunID, &ts, &log.Level, &log.Message); err != nil {
			return nil, err
		}
		var err error
		if log.Timestamp, err = parseTime(ts); err != nil {
			return nil, err
		}
		result = append(result, log)
	}
	return result, rows.Err()
}

const runSelect = `SELECT r.id,r.job_id,r.trigger_type,r.scheduled_at,r.idempotency_key,r.state,r.attempt,r.command_status,r.file_status,r.restore_status,r.error_code,r.error_message,r.started_at,r.finished_at,r.created_at,r.updated_at FROM runs r`

func scanRun(row rowScanner) (Run, error) {
	var run Run
	var scheduled, started, finished sql.NullString
	var created, updated string
	err := row.Scan(&run.ID, &run.JobID, &run.TriggerType, &scheduled, &run.IdempotencyKey, &run.State, &run.Attempt, &run.CommandStatus, &run.FileStatus, &run.RestoreStatus, &run.ErrorCode, &run.ErrorMessage, &started, &finished, &created, &updated)
	if err != nil {
		return Run{}, err
	}
	if run.ScheduledAt, err = scanNullableTime(scheduled); err != nil {
		return Run{}, err
	}
	if run.StartedAt, err = scanNullableTime(started); err != nil {
		return Run{}, err
	}
	if run.FinishedAt, err = scanNullableTime(finished); err != nil {
		return Run{}, err
	}
	if run.CreatedAt, err = parseTime(created); err != nil {
		return Run{}, err
	}
	run.UpdatedAt, err = parseTime(updated)
	return run, err
}
