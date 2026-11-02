package backup

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"vault/internal/metadata"
	"vault/internal/security"
)

// Runner 使用持久化运行记录和有界队列执行本地备份。
type Runner struct {
	store    *metadata.Store
	adapters *Registry
	secrets  *security.KeyStore
	timeout  time.Duration
	queue    chan string
}

// NewRunner 创建执行器；queue 满时新运行会明确失败而不是静默丢失。
func NewRunner(store *metadata.Store, adapters *Registry, secrets *security.KeyStore, timeout time.Duration) *Runner {
	return &Runner{store: store, adapters: adapters, secrets: secrets, timeout: timeout, queue: make(chan string, 256)}
}

// Start 先对账中断运行，再启动固定数量的本地 worker。
func (r *Runner) Start(ctx context.Context) error {
	if err := r.store.ReconcileRuns(ctx); err != nil {
		return fmt.Errorf("启动对账: %w", err)
	}
	for worker := 0; worker < 2; worker++ {
		go r.work(ctx)
	}
	return nil
}

// Trigger 原子创建运行并入队；重复幂等键返回原运行。
func (r *Runner) Trigger(ctx context.Context, jobID, triggerType, idempotencyKey string, scheduledAt *time.Time) (metadata.Run, bool, error) {
	if _, err := r.store.GetJob(ctx, jobID); err != nil {
		return metadata.Run{}, false, err
	}
	now := time.Now().UTC()
	run := metadata.Run{
		ID: metadata.NewID("run"), JobID: jobID, TriggerType: triggerType,
		ScheduledAt: scheduledAt, IdempotencyKey: idempotencyKey,
		State: "queued", Attempt: 1, CommandStatus: "not_run", FileStatus: "pending", RestoreStatus: "not_run",
		CreatedAt: now, UpdatedAt: now,
	}
	created, duplicate, err := r.store.CreateRun(ctx, run)
	if err != nil || duplicate {
		return created, duplicate, err
	}
	select {
	case r.queue <- run.ID:
	default:
		created.State = "failed"
		created.ErrorCode = "QUEUE_FULL"
		created.ErrorMessage = "执行队列已满，请稍后重试"
		finished := time.Now().UTC()
		created.FinishedAt = &finished
		if updateErr := r.store.UpdateRun(ctx, created); updateErr != nil {
			return created, false, updateErr
		}
		return created, false, fmt.Errorf("%w: 执行队列已满", metadata.ErrConflict)
	}
	return created, false, nil
}

func (r *Runner) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case runID := <-r.queue:
			r.execute(ctx, runID)
		}
	}
}

func (r *Runner) execute(parent context.Context, runID string) {
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	run, err := r.store.GetRun(ctx, runID)
	if err != nil || run.State != "queued" {
		return
	}
	job, err := r.store.GetJob(ctx, run.JobID)
	if err != nil {
		r.failRun(ctx, &run, nil, "JOB_NOT_FOUND", "任务不存在或已删除")
		return
	}
	source, err := r.store.GetSource(ctx, job.SourceID)
	if err != nil {
		r.failRun(ctx, &run, nil, "SOURCE_NOT_FOUND", "数据源不存在")
		return
	}
	target, err := r.store.GetStorageTarget(ctx, job.TargetID)
	if err != nil {
		r.failRun(ctx, &run, nil, "STORAGE_NOT_FOUND", "存储位置不存在")
		return
	}
	adapter, err := r.adapters.ForEngine(source.Engine)
	if err != nil {
		r.failRun(ctx, &run, nil, "ADAPTER_UNIMPLEMENTED", err.Error())
		return
	}
	now := time.Now().UTC()
	run.State = "running"
	run.CommandStatus = "running"
	run.StartedAt = &now
	if err = r.store.UpdateRun(ctx, run); err != nil {
		log.Printf("更新运行 %s 开始状态失败: %v", run.ID, err)
		return
	}
	r.log(ctx, run.ID, "INFO", "开始 "+source.Engine+" 全量备份")

	root, err := PrepareRoot(target.Root)
	if err != nil {
		r.failRun(ctx, &run, nil, "STORAGE_NOT_WRITABLE", err.Error())
		return
	}
	toolVersion, err := adapter.Version(ctx)
	if err != nil {
		r.failRun(ctx, &run, nil, ErrorCode(err), SafeErrorMessage(err))
		return
	}
	passwordCiphertext, err := r.store.SourcePassword(ctx, source.ID)
	if err != nil {
		r.failRun(ctx, &run, nil, "SECRET_NOT_FOUND", "数据源凭据不可用")
		return
	}
	password, err := r.secrets.Decrypt(passwordCiphertext)
	if err != nil {
		r.failRun(ctx, &run, nil, "SECRET_UNAVAILABLE", "数据源凭据解密失败，请检查独立主密钥")
		return
	}

	artifact := metadata.Artifact{
		ID: metadata.NewID("art"), RunID: run.ID, JobID: job.ID, SourceID: source.ID, TargetID: target.ID,
		Type: "full", State: "writing", ToolVersion: toolVersion, CreatedAt: now, EngineMetadata: []byte("{}"),
	}
	artifact.ChainID = artifact.ID
	dateDir := now.Format("2006/01")
	workRelative := source.Database + "/" + dateDir + "/." + run.ID + ".attempt-1"
	artifact.RelativePath = workRelative
	if err = r.store.CreateArtifact(ctx, artifact); err != nil {
		r.failRun(ctx, &run, nil, "METADATA_ERROR", "无法登记备份对象")
		return
	}
	placeholder, err := SecurePath(root, workRelative+"/artifact.pending", false)
	if err != nil {
		r.failRun(ctx, &run, &artifact, "STORAGE_PATH_REJECTED", err.Error())
		return
	}
	workPath := filepath.Dir(placeholder)
	result, err := adapter.Execute(ctx, source, target, password, workPath)
	if err != nil {
		r.failRun(ctx, &run, &artifact, ErrorCode(err), SafeErrorMessage(err))
		return
	}
	artifact.Method = result.Method
	artifact.EngineMetadata = result.EngineMetadata
	run.CommandStatus = "succeeded"
	run.State = "verifying"
	run.FileStatus = "verifying"
	if err = r.store.UpdateRun(ctx, run); err != nil {
		log.Printf("更新运行 %s 校验状态失败: %v", run.ID, err)
		return
	}
	r.log(ctx, run.ID, "INFO", result.Method+" 已完成，开始文件校验")

	size, digest, err := HashFile(result.ProducedPath)
	if err != nil {
		r.failRun(ctx, &run, &artifact, "FILE_VERIFICATION_FAILED", err.Error())
		return
	}
	finalRelative := source.Database + "/" + dateDir + "/" + run.ID + result.Extension
	finalPath, err := SecurePath(root, finalRelative, false)
	if err != nil {
		r.failRun(ctx, &run, &artifact, "STORAGE_PATH_REJECTED", err.Error())
		return
	}
	if _, err = os.Lstat(finalPath); err == nil {
		r.failRun(ctx, &run, &artifact, "BACKUP_ALREADY_EXISTS", "目标备份文件已存在，拒绝覆盖")
		return
	} else if !os.IsNotExist(err) {
		r.failRun(ctx, &run, &artifact, "STORAGE_ERROR", err.Error())
		return
	}
	if err = os.Rename(result.ProducedPath, finalPath); err != nil {
		r.failRun(ctx, &run, &artifact, "STORAGE_ERROR", "无法提交备份文件: "+err.Error())
		return
	}
	if removeErr := os.RemoveAll(workPath); removeErr != nil {
		r.log(ctx, run.ID, "WARN", "临时工作目录待对账: "+removeErr.Error())
	}
	verifiedAt := time.Now().UTC()
	artifact.RelativePath = finalRelative
	artifact.Size = size
	artifact.SHA256 = digest
	artifact.State = "verified"
	artifact.VerifiedAt = &verifiedAt
	if err = r.store.UpdateArtifact(ctx, artifact); err != nil {
		r.failRun(ctx, &run, &artifact, "METADATA_ERROR", "备份文件已生成，但校验元数据提交失败，请人工核查")
		return
	}
	finished := time.Now().UTC()
	run.State = "succeeded"
	run.FileStatus = "verified"
	run.FinishedAt = &finished
	if err = r.store.UpdateRun(ctx, run); err != nil {
		log.Printf("提交运行 %s 成功状态失败: %v", run.ID, err)
		return
	}
	r.log(ctx, run.ID, "INFO", fmt.Sprintf("备份文件校验通过：%d 字节，SHA-256 %s", size, digest))
}

func (r *Runner) failRun(ctx context.Context, run *metadata.Run, artifact *metadata.Artifact, code, message string) {
	finished := time.Now().UTC()
	run.State = "failed"
	run.ErrorCode = code
	run.ErrorMessage = message
	run.FinishedAt = &finished
	if run.CommandStatus == "running" || run.CommandStatus == "not_run" {
		run.CommandStatus = "failed"
	}
	if run.FileStatus != "verified" {
		run.FileStatus = "quarantined"
	}
	if artifact != nil {
		artifact.State = "quarantined"
		if err := r.store.UpdateArtifact(ctx, *artifact); err != nil {
			log.Printf("隔离备份对象 %s 失败: %v", artifact.ID, err)
		}
	}
	if err := r.store.UpdateRun(ctx, *run); err != nil {
		log.Printf("记录运行 %s 失败状态出错: %v", run.ID, err)
	}
	r.log(ctx, run.ID, "ERROR", code+": "+message)
}

func (r *Runner) log(ctx context.Context, runID, level, message string) {
	if err := r.store.AddRunLog(ctx, runID, level, message); err != nil {
		log.Printf("写入运行 %s 日志失败: %v", runID, err)
	}
}
