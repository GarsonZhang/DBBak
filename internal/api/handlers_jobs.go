package api

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"dbbak/internal/backup"
	"dbbak/internal/metadata"
)

type jobInput struct {
	Name     string `json:"name"`
	SourceID string `json:"source_id"`
	TargetID string `json:"target_id"`
	// StorageRoot 是任务直接填写的本地备份目录；填写后按目录自动建/复用存储目标，
	// 不再要求先在存储位置页维护授权根目录。target_id 仅保留兼容旧客户端。
	StorageRoot string `json:"storage_root"`
	// LocationKind 区分备份落点：'storage'（默认，执行器可达目录）或
	// 'database_server'（数据库服务器本地目录，仅 SQL Server，文件不经过执行器）。
	LocationKind string `json:"location_kind"`
	ServerDir    string `json:"server_dir"`
	Subdir       string `json:"subdir"`
	Timezone        string `json:"timezone"`
	ScheduleType    string `json:"schedule_type"`
	TimeOfDay       string `json:"time_of_day"`
	Weekday         int    `json:"weekday"`
	IntervalMinutes int    `json:"interval_minutes"`
	Enabled         bool   `json:"enabled"`
	BackupType      string `json:"backup_type"`
	// BackupMethod 选择备份路线：空为引擎默认（MySQL 逻辑全量），logical 为 mysqldump，
	// physical 为 Percona XtraBackup。路线一旦写入任务就固定，恢复语义随之一致。
	BackupMethod  string `json:"backup_method"`
	RetentionDays int    `json:"retention_days"`
	MinFull       int    `json:"min_full"`
	AutoCleanup   bool   `json:"auto_cleanup"`
	Revision      int64  `json:"revision"`
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
		LocationKind: strings.TrimSpace(input.LocationKind), ServerDir: strings.TrimSpace(input.ServerDir),
		Subdir: strings.TrimSpace(input.Subdir), Timezone: input.Timezone, ScheduleType: input.ScheduleType, TimeOfDay: input.TimeOfDay,
		Weekday: input.Weekday, IntervalMinutes: input.IntervalMinutes, Enabled: input.Enabled,
		BackupType:    strings.TrimSpace(input.BackupType),
		BackupMethod:  strings.TrimSpace(input.BackupMethod),
		RetentionDays: input.RetentionDays, MinFull: input.MinFull, AutoCleanup: input.AutoCleanup,
	}
	if job.LocationKind == "" {
		job.LocationKind = "storage"
	}
	if job.BackupType == "" {
		job.BackupType = "full"
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
	// 备份路线必须与引擎匹配：路线决定恢复语义，配错会让任务产出无法恢复的产物。
	switch job.BackupMethod {
	case "":
		// 沿用引擎默认路线（MySQL 为逻辑全量，其余引擎只有一条路线）。
	case "logical", "physical":
		if source.Engine != "mysql" {
			return job, fmt.Errorf("备份路线只对 MySQL 有效；%s 没有可选路线", source.Engine)
		}
	default:
		return job, fmt.Errorf("备份路线必须为 logical 或 physical")
	}
	physical := job.BackupMethod == "physical"
	if physical {
		if !s.config.MySQLPhysicalEnabled {
			return job, fmt.Errorf("MySQL 物理备份（Percona XtraBackup）已实现但未在目标环境验收；请先执行 DBBak_文档/DBBak_MySQL物理备份验证说明.md 的脚本，再设置 DBBAK_MYSQL_PHYSICAL_ENABLED=1 重启")
		}
		if runtime.GOOS == "windows" {
			return job, fmt.Errorf("Percona XtraBackup 不提供 Windows 二进制；MySQL 物理备份只有在 Linux 执行器与 MySQL 同机时才可创建")
		}
	}
	switch {
	case job.BackupType == "full":
	case job.BackupType == "differential":
		// 目前只有 SQL Server 适配器实现差异；MySQL 物理增量链未实现，不能降级成全量。
		if source.Engine != "sqlserver" {
			return job, fmt.Errorf("差异备份当前只支持 SQL Server，%s 尚不支持", source.Engine)
		}
	case job.BackupType == "log":
		// binlog 归档依附于物理全量记录的一致位点；逻辑路线没有该位点，不能提供。
		if source.Engine != "sqlserver" && !physical {
			return job, fmt.Errorf("MySQL 的 binlog 归档只在物理备份路线上可用；逻辑全量不记录 binlog 位点")
		}
	default:
		return job, fmt.Errorf("备份类型必须为 full、differential 或 log")
	}
	if source.Engine == "sqlserver" && !s.config.SQLServerEnabled {
		// 未验收的备份路径不得默认启用；完成验证后由 DBBAK_SQLSERVER_ENABLED=1 显式放开。
		return job, fmt.Errorf("SQL Server 适配器已实现，但尚未通过真实备份与异名恢复验收；先执行 DBBak_文档/DBBak_SQLServer验证脚本说明.md 中的脚本，再设置 DBBAK_SQLSERVER_ENABLED=1 重启")
	}
	// 备份位置类型决定目标解析方式：数据库服务器本地目录不经过存储位置，
	// 执行器无法预验证远程目录可写，真实结果由运行时的数据库服务给出。
	switch job.LocationKind {
	case "database_server":
		if source.Engine != "sqlserver" {
			return job, fmt.Errorf("数据库服务器本地备份当前只支持 SQL Server 原生备份")
		}
		if input.StorageRoot != "" || job.TargetID != "" {
			return job, fmt.Errorf("数据库服务器本地备份不使用存储位置，请清除存储目录或改回存储位置类型")
		}
		if filepath.VolumeName(job.ServerDir) == "" || !filepath.IsAbs(job.ServerDir) {
			return job, fmt.Errorf("服务器目录必须是数据库服务器上的本地绝对路径（如 D:\\bak）")
		}
		if strings.ContainsRune(job.ServerDir, '\'') {
			return job, fmt.Errorf("服务器目录包含不安全字符")
		}
		job.TargetID = ""
	case "", "storage":
		job.LocationKind = "storage"
		job.ServerDir = ""
		if root := strings.TrimSpace(input.StorageRoot); root != "" {
			prepared, err := backup.PrepareRoot(root)
			if err == nil {
				err = backup.CheckWritable(prepared)
			}
			if err != nil {
				return job, fmt.Errorf("本地存储目录无效：%w", err)
			}
			targetID, err := s.ensureStorageTarget(r.Context(), prepared)
			if err != nil {
				return job, err
			}
			job.TargetID = targetID
		} else {
			if _, err := s.store.GetStorageTarget(r.Context(), job.TargetID); err != nil {
				return job, fmt.Errorf("存储位置不存在")
			}
		}
	default:
		return job, fmt.Errorf("备份位置类型必须为 storage 或 database_server")
	}
	// 子目录属于任务级路径配置：保存时就校验并规范化，执行器运行前还会再复核一次。
	subdir, err := backup.NormalizeSubdir(job.Subdir)
	if err != nil {
		return job, fmt.Errorf("输出子目录无效：%w", err)
	}
	job.Subdir = subdir
	if err := backup.ValidateSchedule(job); err != nil {
		return job, err
	}
	return job, nil
}

// ensureStorageTarget 按目录路径查找已有存储目标；不存在则自动创建一条登记。
// 同一目录多个任务共用一个目标，避免为每个任务重复登记相同根目录。
// 存储目标仅作为目录登记与清理挂接点，不再要求预先配置；本地目录的
// 规范化与可写性校验在任务保存时完成。
func (s *Server) ensureStorageTarget(ctx context.Context, root string) (string, error) {
	targets, err := s.store.ListStorageTargets(ctx)
	if err != nil {
		return "", err
	}
	for _, item := range targets {
		if strings.EqualFold(filepath.Clean(item.Root), filepath.Clean(root)) {
			return item.ID, nil
		}
	}
	target := metadata.StorageTarget{
		ID: metadata.NewID("repo"), Name: "本地目录 " + root, Kind: "local",
		Root: root, Revision: 1, CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateStorageTarget(ctx, target); err != nil {
		return "", err
	}
	return target.ID, nil
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
