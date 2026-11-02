package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"vault/internal/backup"
	"vault/internal/metadata"
)

type jobInput struct {
	Name            string `json:"name"`
	SourceID        string `json:"source_id"`
	TargetID        string `json:"target_id"`
	Timezone        string `json:"timezone"`
	ScheduleType    string `json:"schedule_type"`
	TimeOfDay       string `json:"time_of_day"`
	Weekday         int    `json:"weekday"`
	IntervalMinutes int    `json:"interval_minutes"`
	Enabled         bool   `json:"enabled"`
	RetentionDays   int    `json:"retention_days"`
	MinFull         int    `json:"min_full"`
	AutoCleanup     bool   `json:"auto_cleanup"`
	Revision        int64  `json:"revision"`
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ListJobs(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var input jobInput
	if !decode(w, r, &input) {
		return
	}
	job, err := s.jobFromInput(r, input)
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
		return
	}
	now := time.Now().UTC()
	job.ID = metadata.NewID("job")
	job.Revision = 1
	job.CreatedAt = now
	job.UpdatedAt = now
	if job.Enabled {
		next, nextErr := backup.NextRun(job, now)
		if nextErr != nil {
			s.fail(w, r, http.StatusUnprocessableEntity, "SCHEDULE_INVALID", nextErr.Error())
			return
		}
		job.NextRunAt = &next
	}
	if err = s.store.CreateJob(r.Context(), job); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "job.create", job.ID, "success")
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) updateJob(w http.ResponseWriter, r *http.Request) {
	var input jobInput
	if !decode(w, r, &input) {
		return
	}
	if input.Revision < 1 {
		s.fail(w, r, http.StatusUnprocessableEntity, "REVISION_REQUIRED", "更新必须携带当前 revision")
		return
	}
	job, err := s.jobFromInput(r, input)
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
		return
	}
	job.ID = r.PathValue("id")
	job.Revision = input.Revision
	job.UpdatedAt = time.Now().UTC()
	if job.Enabled {
		next, nextErr := backup.NextRun(job, time.Now().UTC())
		if nextErr != nil {
			s.fail(w, r, http.StatusUnprocessableEntity, "SCHEDULE_INVALID", nextErr.Error())
			return
		}
		job.NextRunAt = &next
	}
	if err = s.store.UpdateJob(r.Context(), job); err != nil {
		s.internalError(w, r, err)
		return
	}
	job, _ = s.store.GetJob(r.Context(), job.ID)
	s.audit(r, "job.update", job.ID, "success")
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) jobFromInput(r *http.Request, input jobInput) (metadata.Job, error) {
	job := metadata.Job{
		Name: strings.TrimSpace(input.Name), SourceID: input.SourceID, TargetID: input.TargetID,
		Timezone: input.Timezone, ScheduleType: input.ScheduleType, TimeOfDay: input.TimeOfDay,
		Weekday: input.Weekday, IntervalMinutes: input.IntervalMinutes, Enabled: input.Enabled,
		RetentionDays: input.RetentionDays, MinFull: input.MinFull, AutoCleanup: input.AutoCleanup,
	}
	if job.Name == "" || len(job.Name) > 100 {
		return job, fmt.Errorf("任务名称必填且不超过 100 字符")
	}
	if job.RetentionDays < 1 || job.RetentionDays > 3650 || job.MinFull < 1 || job.MinFull > 100 {
		return job, fmt.Errorf("保留天数为 1~3650，最低全量份数为 1~100")
	}
	// 独立全量的自动清理复用服务端预览与执行复查；日志链自动清理仍未开放。
	source, err := s.store.GetSource(r.Context(), job.SourceID)
	if err != nil {
		return job, fmt.Errorf("数据源不存在")
	}
	if source.Engine == "sqlserver" {
		return job, fmt.Errorf("SQL Server 适配器尚未完成真实备份恢复验收，当前禁止创建任务")
	}
	if _, err := s.store.GetStorageTarget(r.Context(), job.TargetID); err != nil {
		return job, fmt.Errorf("存储位置不存在")
	}
	if err := backup.ValidateSchedule(job); err != nil {
		return job, err
	}
	return job, nil
}

func (s *Server) triggerRun(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		key = "manual:" + metadata.NewID("request")
	}
	if len(key) > 200 {
		s.fail(w, r, http.StatusBadRequest, "IDEMPOTENCY_KEY_TOO_LONG", "Idempotency-Key 不超过 200 字符")
		return
	}
	run, duplicate, err := s.runner.Trigger(r.Context(), r.PathValue("id"), "manual", key, nil)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "run.trigger", run.ID, "accepted")
	if duplicate {
		writeJSON(w, http.StatusOK, run)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListRuns(r.Context(), r.URL.Query().Get("job_id"), 100)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) getRunLogs(w http.ResponseWriter, r *http.Request) {
	logs, err := s.store.ListRunLogs(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, logs)
}

// recordRestoreVerification 登记在隔离目标上实际完成的恢复演练；该接口本身不伪造或执行恢复。
func (s *Server) recordRestoreVerification(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Status  string `json:"status"`
		Target  string `json:"target"`
		Details string `json:"details"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Status != "verified" && input.Status != "failed" || strings.TrimSpace(input.Target) == "" || len(input.Details) > 1200 {
		s.fail(w, r, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "status 必须为 verified 或 failed，target 必填，details 不超过 1200 字符")
		return
	}
	run, err := s.store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if run.State != "succeeded" || run.FileStatus != "verified" {
		s.fail(w, r, http.StatusConflict, "RESTORE_SOURCE_NOT_VERIFIED", "只有文件校验通过的备份运行可以登记恢复验证")
		return
	}
	run.RestoreStatus = input.Status
	if err = s.store.UpdateRun(r.Context(), run); err != nil {
		s.internalError(w, r, err)
		return
	}
	message := "隔离恢复验证" + input.Status + "，目标：" + input.Target
	if input.Details != "" {
		message += "；" + input.Details
	}
	if err = s.store.AddRunLog(r.Context(), run.ID, "INFO", message); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "restore.verification", run.ID, input.Status)
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	artifacts, err := s.store.ListArtifacts(r.Context(), r.URL.Query().Get("job_id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, artifacts)
}

func (s *Server) holdArtifact(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Locked bool `json:"locked"`
	}
	if !decode(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if err := s.store.SetArtifactLocked(r.Context(), id, input.Locked); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "artifact.hold", id, "success")
	artifact, _ := s.store.GetArtifact(r.Context(), id)
	writeJSON(w, http.StatusOK, artifact)
}
