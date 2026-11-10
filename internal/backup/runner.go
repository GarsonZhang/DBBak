package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"dbbak/internal/metadata"
	"dbbak/internal/security"
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
	// 备份位置类型在任务保存时已校验；运行前按类型准备目标，
	// 不让错误配置在调度时刻才暴露成模糊失败。
	var target metadata.StorageTarget
	serverLocal := job.LocationKind == "database_server"
	if serverLocal {
		if source.Engine != "sqlserver" {
			r.failRun(ctx, &run, nil, "UNSUPPORTED_LOCATION", "数据库服务器本地备份当前只支持 SQL Server 原生备份")
			return
		}
		// 执行器可能运行在 Linux / Docker 中，不能用 filepath.IsAbs 判断数据库服务器上的
		// Windows 盘符路径；保存时已按同一规则校验，这里复核以防配置被直接改库绕过。
		if _, normalizeErr := NormalizeServerDir(job.ServerDir); normalizeErr != nil {
			r.failRun(ctx, &run, nil, "SERVER_PATH_INVALID", normalizeErr.Error())
			return
		}
	} else {
		var err error
		target, err = r.store.GetStorageTarget(ctx, job.TargetID)
		if err != nil {
			r.failRun(ctx, &run, nil, "STORAGE_NOT_FOUND", "存储位置不存在")
			return
		}
	}
	// 路线由任务固定：同一引擎的逻辑与物理路线恢复语义不同，不能按运行环境临时挑一个。
	// 显式指定但未实现的路线直接失败，不静默回退到默认路线。
	adapter, err := r.adapters.ForRoute(source.Engine, job.BackupMethod)
	if err != nil {
		r.failRun(ctx, &run, nil, "ADAPTER_UNIMPLEMENTED", err.Error())
		return
	}
	routeMethod := RouteMethod(adapter)
	backupType := job.BackupType
	if backupType == "" {
		backupType = "full"
	}
	// 差异与日志必须先有已验证的全量基线，否则会生成无法恢复的孤立文件。
	var base *metadata.Artifact
	chainID := ""
	if backupType != "full" {
		var recent []metadata.Artifact
		var listErr error
		if serverLocal {
			recent, listErr = r.store.LatestServerLocalArtifacts(ctx, source.ID, routeMethod, 50)
		} else {
			recent, listErr = r.store.LatestVerifiedArtifacts(ctx, source.ID, target.ID, routeMethod, 50)
		}
		if listErr != nil {
			r.failRun(ctx, &run, nil, "METADATA_ERROR", "无法读取恢复链基线")
			return
		}
		var full *metadata.Artifact
		for index := range recent {
			if recent[index].Type == "full" {
				full = &recent[index]
				break
			}
		}
		if full == nil {
			r.failRun(ctx, &run, nil, "NO_BASE_BACKUP", "差异与日志备份需要至少一份已验证的全量基线，请先执行全量备份")
			return
		}
		chainID = full.ChainID
		if chainID == "" {
			chainID = full.ID
		}
		base = full
		if backupType == "log" {
			// 日志接在链上最新对象之后，而不是固定接全量。
			for index := range recent {
				if recent[index].ChainID == chainID {
					base = &recent[index]
					break
				}
			}
		}
	}

	now := time.Now().UTC()
	run.State = "running"
	run.CommandStatus = "running"
	run.StartedAt = &now
	if err = r.store.UpdateRun(ctx, run); err != nil {
		log.Printf("更新运行 %s 开始状态失败: %v", run.ID, err)
		return
	}
	r.log(ctx, run.ID, "INFO", "开始 "+source.Engine+" "+backupTypeLabel(backupType)+"备份")

	var root string
	ftpStorage := false
	ossStorage := false
	if serverLocal {
		// 服务器本地模式：执行器不落盘，工作目录只承载给 sqlcmd 的脚本文件。
		tempDir, mkErr := os.MkdirTemp("", "dbbak-run-")
		if mkErr != nil {
			r.failRun(ctx, &run, nil, "STORAGE_ERROR", "无法创建临时工作目录: "+mkErr.Error())
			return
		}
		defer os.RemoveAll(tempDir)
		root = tempDir
	} else if target.Kind == "ftp" {
		// FTP 模式：备份先落在临时目录，完成校验后上传并回读比对，本地副本随即删除。
		if source.Engine == "sqlserver" {
			r.failRun(ctx, &run, nil, "UNSUPPORTED_LOCATION", "SQL Server 原生备份无法写入 FTP，请使用数据库服务器本地目录或共享目录")
			return
		}
		tempDir, mkErr := os.MkdirTemp("", "dbbak-ftp-")
		if mkErr != nil {
			r.failRun(ctx, &run, nil, "STORAGE_ERROR", "无法创建临时工作目录: "+mkErr.Error())
			return
		}
		defer os.RemoveAll(tempDir)
		root = tempDir
		ftpStorage = true
	} else if target.Kind == "oss" {
		// 对象存储（S3 兼容 / 阿里云 OSS）：备份先落在执行器临时目录，上传校验通过后删除本地副本。
		// SQL Server 原生备份由数据库服务写盘，执行器无法把产物上传，必须提前拒绝。
		if source.Engine == "sqlserver" {
			r.failRun(ctx, &run, nil, "UNSUPPORTED_LOCATION", "SQL Server 原生备份无法写入对象存储，请使用数据库服务器本地目录或共享目录")
			return
		}
		tempDir, mkErr := os.MkdirTemp("", "dbbak-oss-")
		if mkErr != nil {
			r.failRun(ctx, &run, nil, "STORAGE_ERROR", "无法创建临时工作目录: "+mkErr.Error())
			return
		}
		defer os.RemoveAll(tempDir)
		root = tempDir
		ossStorage = true
	} else {
		var err error
		root, err = PrepareRoot(target.Root)
		if err != nil {
			r.failRun(ctx, &run, nil, "STORAGE_NOT_WRITABLE", err.Error())
			return
		}
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
		ID: metadata.NewID("art"), RunID: run.ID, JobID: job.ID, SourceID: source.ID,
		Type: backupType, BackupMethod: routeMethod, State: "writing", ToolVersion: toolVersion, CreatedAt: now, EngineMetadata: []byte("{}"),
	}
	if !serverLocal {
		artifact.TargetID = target.ID
	}
	if backupType == "full" {
		artifact.ChainID = artifact.ID
	} else {
		artifact.ChainID = chainID
		parent := base.ID
		artifact.ParentID = &parent
	}
	dateDir := now.Format("2006/01")
	// 备份文件名固定为 <数据库名>-<YYYYMMDDHHmmss>：时间取运行开始冻结的 UTC 时刻，
	// 与 run.started_at 同源，不会因为写入耗时不同而产生两套基准。
	// 对象路径为 <子目录>/<数据库>/<YYYY/MM>/<文件名>，仍按数据库与月份分目录。
	artifactName := ArtifactFileName(source.Database, now)
	// 子目录来自任务配置，运行前再规范化一次：即使配置被绕过校验写库，也不会把对象写出授权根目录。
	subdir, err := NormalizeSubdir(job.Subdir)
	if err != nil {
		r.failRun(ctx, &run, nil, "SUBDIR_INVALID", err.Error())
		return
	}
	workRelative := ArtifactRelativePath(subdir, source.Database, dateDir+"/"+run.ID)
	artifact.RelativePath = workRelative
	if err = r.store.CreateArtifact(ctx, artifact); err != nil {
		r.failRun(ctx, &run, nil, "METADATA_ERROR", "无法登记备份对象")
		return
	}
	var workPath string
	if serverLocal || ftpStorage || ossStorage {
		workPath = root // 远程模式的临时工作目录，defer 已负责清理
	} else {
		// 每次运行的工作目录是 <子目录>/<数据库>/<YYYY/MM>/run_<id>：先解析占位文件路径，
		// 父目录会在解析时一并创建；再复核该目录确实是根目录内的真实目录（防止被替换成
		// 越过授权根的符号链接），并直接作为适配器的工作目录。
		if _, pathErr := SecurePath(root, workRelative+"/artifact.pending", false, false); pathErr != nil {
			r.failRun(ctx, &run, &artifact, "STORAGE_PATH_REJECTED", pathErr.Error())
			return
		}
		dirPath, dirErr := SecurePath(root, workRelative, true, true)
		if dirErr != nil {
			r.failRun(ctx, &run, &artifact, "STORAGE_PATH_REJECTED", dirErr.Error())
			return
		}
		workPath = dirPath
	}
	adapterTarget := target
	if serverLocal {
		// 只把服务器目录与模式交给适配器：它据此拼 BACKUP 的 DISK 目标，不触碰执行器文件系统。
		adapterTarget = metadata.StorageTarget{ID: "", Kind: "database_server", ServerRoot: job.ServerDir}
	}
	result, err := adapter.Execute(ctx, source, adapterTarget, password, workPath, BackupSpec{Type: backupType, Base: base, FileName: artifactName})
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

	if result.ProducedPath == "" {
		// 数据库服务器本地备份：文件不经过执行器，无法哈希校验。
		// 以 msdb 登记的 backup_size 与 LSN 作为真实性凭证，产物标记为 uploaded（未校验），
		// 删除与清理只清记录，远程文件需在数据库服务器上人工处理。
		var meta map[string]any
		if jsonErr := json.Unmarshal(result.EngineMetadata, &meta); jsonErr != nil {
			r.failRun(ctx, &run, &artifact, "METADATA_ERROR", "无法解析备份元数据: "+jsonErr.Error())
			return
		}
		size, _ := meta["backup_size"].(float64)
		artifact.RelativePath = ArtifactRelativePath(subdir, source.Database, dateDir+"/"+artifactName+result.Extension)
		artifact.Size = int64(size)
		artifact.State = "uploaded"
		if err = r.store.UpdateArtifact(ctx, artifact); err != nil {
			r.failRun(ctx, &run, &artifact, "METADATA_ERROR", "备份已完成，但元数据提交失败，请人工核查")
			return
		}
		finished := time.Now().UTC()
		run.State = "succeeded"
		run.FileStatus = "uploaded"
		run.FinishedAt = &finished
		if err = r.store.UpdateRun(ctx, run); err != nil {
			log.Printf("提交运行 %s 成功状态失败: %v", run.ID, err)
			return
		}
		r.log(ctx, run.ID, "INFO", fmt.Sprintf("数据库服务器本地备份完成：%d 字节（未校验，文件位于服务器目录 %s）", artifact.Size, job.ServerDir))
		return
	}

	size, digest, err := HashFile(result.ProducedPath)
	if err != nil {
		r.failRun(ctx, &run, &artifact, "FILE_VERIFICATION_FAILED", err.Error())
		return
	}
	finalRelative := ArtifactRelativePath(subdir, source.Database, dateDir+"/"+artifactName+result.Extension)

	if ftpStorage {
		// FTP：上传后整文件回读比对哈希，通过才登记 verified；本地副本随即删除。
		cipher, secretErr := r.store.StoragePassword(ctx, target.ID)
		if secretErr != nil {
			r.failRun(ctx, &run, &artifact, "SECRET_NOT_FOUND", "读取存储凭据失败: "+secretErr.Error())
			return
		}
		ftpPassword, decryptErr := r.secrets.Decrypt(cipher)
		if decryptErr != nil {
			r.failRun(ctx, &run, &artifact, "SECRET_UNAVAILABLE", "存储凭据解密失败，请检查独立主密钥")
			return
		}
		conn, dialErr := dialFTP(target, ftpPassword)
		if dialErr != nil {
			r.failRun(ctx, &run, &artifact, "STORAGE_ERROR", dialErr.Error())
			return
		}
		remotePath, uploadErr := ftpUploadVerified(conn, target, result.ProducedPath, finalRelative)
		_ = conn.Quit()
		if uploadErr != nil {
			r.failRun(ctx, &run, &artifact, "STORAGE_ERROR", uploadErr.Error())
			return
		}
		_ = os.Remove(result.ProducedPath)
		verifiedAt := time.Now().UTC()
		artifact.RelativePath = finalRelative
		artifact.Size = size
		artifact.SHA256 = digest
		artifact.State = "verified"
		artifact.VerifiedAt = &verifiedAt
		if err = r.store.UpdateArtifact(ctx, artifact); err != nil {
			r.failRun(ctx, &run, &artifact, "METADATA_ERROR", "备份文件已上传，但元数据提交失败，请人工核查")
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
		r.log(ctx, run.ID, "INFO", fmt.Sprintf("备份已上传并回读校验通过：%d 字节，SHA-256 %s，远端 %s", size, digest, remotePath))
		return
	}

	if ossStorage {
		// 对象存储：上传后以服务端返回的长度与 ETag 复核，通过才登记 verified；本地副本随即删除。
		// 注意校验等级：单次 PUT 的 ETag 是 MD5，可确认内容；多段上传的 ETag 不是整文件摘要，
		// 只能确认长度，这一点必须写进运行日志，不能含糊成"校验通过"。
		cipher, secretErr := r.store.StoragePassword(ctx, target.ID)
		if secretErr != nil {
			r.failRun(ctx, &run, &artifact, "SECRET_NOT_FOUND", "读取存储凭据失败: "+secretErr.Error())
			return
		}
		secret, decryptErr := r.secrets.Decrypt(cipher)
		if decryptErr != nil {
			r.failRun(ctx, &run, &artifact, "SECRET_UNAVAILABLE", "存储凭据解密失败，请检查独立主密钥")
			return
		}
		objectKey, verifyNote, uploadErr := ossUploadVerified(ctx, target, secret, result.ProducedPath, finalRelative)
		if uploadErr != nil {
			r.failRun(ctx, &run, &artifact, "STORAGE_ERROR", uploadErr.Error())
			return
		}
		_ = os.Remove(result.ProducedPath)
		verifiedAt := time.Now().UTC()
		artifact.RelativePath = finalRelative
		artifact.Size = size
		artifact.SHA256 = digest
		artifact.State = "verified"
		artifact.VerifiedAt = &verifiedAt
		if err = r.store.UpdateArtifact(ctx, artifact); err != nil {
			r.failRun(ctx, &run, &artifact, "METADATA_ERROR", "备份文件已上传，但元数据提交失败，请人工核查")
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
		r.log(ctx, run.ID, "INFO", fmt.Sprintf("备份已上传对象存储：%d 字节，SHA-256 %s，%s，对象 %s/%s", size, digest, verifyNote, target.Bucket, objectKey))
		return
	}

	finalPath, err := SecurePath(root, finalRelative, false, false)
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
	// 工作目录里只剩本次运行的中间产物，最终文件已拷贝到 finalPath；删掉它避免留下垃圾。
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

// backupTypeLabel 用于运行日志，避免把差异或日志写成“全量”。
func backupTypeLabel(kind string) string {
	switch kind {
	case "differential":
		return "差异"
	case "log":
		return "事务日志"
	default:
		return "全量"
	}
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
