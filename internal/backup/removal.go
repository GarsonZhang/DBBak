package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"dbbak/internal/metadata"
	"dbbak/internal/security"
)

// 删除对象类型：job 为任务配置，artifact 为单个备份对象，job_files 为任务下的备份文件。
const (
	RemovalTargetJob      = "job"
	RemovalTargetArtifact = "artifact"
	RemovalTargetJobFiles = "job_files"
)

// RemovalBlocker 说明删除无法进行的具体原因与解除建议。
type RemovalBlocker struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}

// RemovalResult 是删除前的服务端清理范围评估；不修改任何文件或记录。
type RemovalResult struct {
	Target     string              `json:"target"`
	TargetID   string              `json:"target_id"`
	Force      bool                `json:"force"`
	Artifacts  []metadata.Artifact `json:"artifacts"`
	FileCount  int                 `json:"file_count"`
	TotalSize  int64               `json:"total_size"`
	RunCount   int                 `json:"run_count"`
	Runs       []metadata.Run      `json:"runs"`
	ActiveRuns []metadata.Run      `json:"active_runs"`
	Blockers   []RemovalBlocker    `json:"blockers"`
	Warnings   []string            `json:"warnings"`
	JobID      string              `json:"job_id,omitempty"`
	JobName    string              `json:"job_name,omitempty"`
	JobEnabled bool                `json:"job_enabled"`
}

// RemovalExecution 记录每个对象的实际删除结果；错误按对象独立返回，不整体回滚。
type RemovalExecution struct {
	DeleteRunID  string             `json:"delete_run_id,omitempty"`
	Files        []CleanupExecution `json:"files"`
	DeletedFiles int                `json:"deleted_files"`
	FailedFiles  int                `json:"failed_files"`
	RunsRemoved  int                `json:"runs_removed"`
	JobDeleted   bool               `json:"job_deleted"`
	Message      string             `json:"message"`
}

var (
	// ErrRemovalBlocked 表示目标不满足删除前提，必须先由人工解除阻塞。
	ErrRemovalBlocked = errors.New("删除前提未满足")
	// ErrConfirmationRequired 表示强制删除缺少显式确认口令。
	ErrConfirmationRequired = errors.New("强制删除需要显式确认")
)

// RemovalService 负责任务与备份对象的受控删除：先评估范围，再重新核对并逐个删除。
// 与保留清理（CleanupService）的区别：清理按保留策略筛选候选，删除按用户显式指定的目标执行；
// 两者共用路径约束、SHA-256 复核和「先删文件、后改记录」的顺序。
type RemovalService struct {
	store  *metadata.Store
	secrets *security.KeyStore
}

// NewRemovalService 创建删除服务；secrets 用于解密 FTP 等存储凭据。
func NewRemovalService(store *metadata.Store, secrets *security.KeyStore) *RemovalService {
	return &RemovalService{store: store, secrets: secrets}
}

// Plan 计算删除范围并给出阻塞原因；force 只放宽「已验证备份」这一项，不绕过活动运行与恢复链依赖。
func (r *RemovalService) Plan(ctx context.Context, target, id string, force bool) (RemovalResult, error) {
	result := RemovalResult{Target: target, TargetID: id, Force: force, Artifacts: []metadata.Artifact{}, Runs: []metadata.Run{}, ActiveRuns: []metadata.Run{}, Blockers: []RemovalBlocker{}, Warnings: []string{}}
	switch target {
	case RemovalTargetArtifact:
		artifact, err := r.store.GetArtifact(ctx, id)
		if err != nil {
			return result, err
		}
		job, err := r.store.GetJob(ctx, artifact.JobID)
		if err != nil {
			return result, err
		}
		result.JobID, result.JobName, result.JobEnabled = job.ID, job.Name, job.Enabled
		result.Artifacts = append(result.Artifacts, artifact)
		all, err := r.store.ListArtifacts(ctx, job.ID)
		if err != nil {
			return result, err
		}
		blockers := r.artifactBlockers(ctx, artifact, all, force)
		result.Blockers = append(result.Blockers, blockers...)
	case RemovalTargetJob, RemovalTargetJobFiles:
		job, err := r.store.GetJob(ctx, id)
		if err != nil {
			return result, err
		}
		result.JobID, result.JobName, result.JobEnabled = job.ID, job.Name, job.Enabled
		artifacts, err := r.store.ListArtifacts(ctx, job.ID)
		if err != nil {
			return result, err
		}
		for _, artifact := range artifacts {
			if artifact.State == "deleted" {
				continue
			}
			result.Artifacts = append(result.Artifacts, artifact)
			result.FileCount++
			result.TotalSize += artifact.Size
			if artifact.State != "verified" {
				result.Warnings = append(result.Warnings, fmt.Sprintf("%s 状态为 %s，删除的是未通过校验的文件", artifact.RelativePath, artifact.State))
			}
		}
		if target == RemovalTargetJob && job.Enabled {
			// 删除任务是破坏性配置操作：直接拒绝，要求先显式暂停，避免计划在删除途中再次入队。
			result.Blockers = append(result.Blockers, RemovalBlocker{Code: "JOB_ENABLED", Message: "任务仍处于启用状态", Remediation: "先在「备份任务」中暂停该任务，再执行删除"})
		}
		runs, err := r.store.ListRuns(ctx, job.ID, 500)
		if err != nil {
			return result, err
		}
		for _, run := range runs {
			if isActiveRunState(run.State) {
				result.ActiveRuns = append(result.ActiveRuns, run)
				continue
			}
			result.Runs = append(result.Runs, run)
		}
		result.RunCount = len(result.Runs)
		if len(result.ActiveRuns) > 0 {
			result.Blockers = append(result.Blockers, RemovalBlocker{Code: "ACTIVE_RUN", Message: fmt.Sprintf("任务存在 %d 个未结束运行", len(result.ActiveRuns)), Remediation: "等待运行结束或在执行日志中确认中断后再删除"})
		}
		if len(result.Blockers) == 0 && !force {
			for _, artifact := range result.Artifacts {
				if artifact.State == "verified" || artifact.Locked {
					result.Blockers = append(result.Blockers, RemovalBlocker{Code: "CONFIRMATION_REQUIRED", Message: "范围包含已校验备份或人工锁定对象", Remediation: "勾选强制删除并输入目标名称确认，或先解除锁定"})
					break
				}
			}
		}
	default:
		return result, fmt.Errorf("%w: 不支持的删除对象类型 %s", metadata.ErrConflict, target)
	}
	return result, nil
}

// artifactBlockers 只检查单个对象：活动运行、恢复链依赖与强制确认。
func (r *RemovalService) artifactBlockers(ctx context.Context, artifact metadata.Artifact, all []metadata.Artifact, force bool) []RemovalBlocker {
	blockers := []RemovalBlocker{}
	if artifact.State == "deleted" {
		return append(blockers, RemovalBlocker{Code: "ALREADY_DELETED", Message: "该备份对象已标记删除", Remediation: "无需重复操作"})
	}
	if active, err := r.store.ActiveRunForJob(ctx, artifact.JobID); err == nil && active {
		blockers = append(blockers, RemovalBlocker{Code: "ACTIVE_RUN", Message: "所属任务存在未结束运行", Remediation: "等待运行结束后再删除"})
	}
	for _, other := range all {
		if other.State != "deleted" && other.ParentID != nil && *other.ParentID == artifact.ID {
			blockers = append(blockers, RemovalBlocker{Code: "CHAIN_DEPENDENT", Message: "仍有差异或日志备份依赖该恢复点", Remediation: "先删除依赖该基线的新增备份，再删除本对象"})
			break
		}
	}
	if !force && (artifact.State == "verified" || artifact.Locked) {
		blockers = append(blockers, RemovalBlocker{Code: "CONFIRMATION_REQUIRED", Message: "删除已校验备份需要强制确认", Remediation: "勾选强制删除并输入对象名称确认"})
	}
	return blockers
}

// Execute 重新评估目标，逐个删除文件并登记结果；文件删除成功但元数据提交失败时保留原状并报人工对账。
func (r *RemovalService) Execute(ctx context.Context, target, id, confirm string, force bool, actor string) (RemovalExecution, error) {
	execution := RemovalExecution{Files: []CleanupExecution{}}
	plan, err := r.Plan(ctx, target, id, force)
	if err != nil {
		return execution, err
	}
	if len(plan.Blockers) > 0 {
		messages := make([]string, 0, len(plan.Blockers))
		for _, blocker := range plan.Blockers {
			messages = append(messages, blocker.Message+"；"+blocker.Remediation)
		}
		return execution, fmt.Errorf("%w：%s", ErrRemovalBlocked, strings.Join(messages, "；"))
	}
	expected := plan.JobName
	if target == RemovalTargetArtifact && len(plan.Artifacts) == 1 {
		expected = plan.Artifacts[0].RelativePath
	}
	if force && strings.TrimSpace(confirm) != expected {
		return execution, fmt.Errorf("%w：用于确认的名称与目标不一致", ErrConfirmationRequired)
	}
	deleteRunID := ""
	if target == RemovalTargetJob {
		// 真实删除动作本身也要留痕：先写一条删除运行记录，避免删掉任务后无迹可查。
		deleteRunID = r.recordDeleteRun(ctx, plan)
	}
	targets, err := r.store.ListStorageTargets(ctx)
	if err != nil {
		return execution, err
	}
	storageByIDs := make(map[string]metadata.StorageTarget, len(targets))
	for _, item := range targets {
		storageByIDs[item.ID] = item
	}
	for _, artifact := range plan.Artifacts {
		execution.Files = append(execution.Files, r.deleteArtifactFile(ctx, artifact, storageByIDs[artifact.TargetID]))
	}
	for _, item := range execution.Files {
		if item.Deleted {
			execution.DeletedFiles++
			continue
		}
		execution.FailedFiles++
	}
	execution.DeleteRunID = deleteRunID
	if execution.FailedFiles > 0 {
		// 只报“部分文件未删除”用户无法知道自己该处理什么，这里附上前几条真实原因。
		reasons := make([]string, 0, 3)
		for _, item := range execution.Files {
			if item.Deleted {
				continue
			}
			if len(reasons) == 3 {
				reasons = append(reasons, "……")
				break
			}
			reasons = append(reasons, item.Path+"："+item.Message)
		}
		execution.Message = "部分文件未删除，记录与文件状态保持不变，可重试"
		if len(reasons) > 0 {
			execution.Message += "（" + strings.Join(reasons, "；") + "）"
		}
		if deleteRunID != "" {
			// 删除动作未完成，不留下具有误导性的运行记录。
			_ = r.store.DeleteRun(ctx, deleteRunID)
			execution.DeleteRunID = ""
		}
		return execution, nil
	}
	if target == RemovalTargetJob {
		if err = r.store.DeleteJob(ctx, plan.JobID, actor); err != nil {
			return execution, err
		}
		execution.JobDeleted = true
		execution.RunsRemoved = plan.RunCount + 1
	}
	execution.Message = fmt.Sprintf("已删除 %d 个备份文件", execution.DeletedFiles)
	return execution, nil
}

func (r *RemovalService) recordDeleteRun(ctx context.Context, plan RemovalResult) string {
	now := time.Now().UTC()
	run := metadata.Run{
		ID: metadata.NewID("run"), JobID: plan.JobID, TriggerType: "manual", IdempotencyKey: "delete:" + metadata.NewID("request"),
		State: "running", Attempt: 1, CommandStatus: "running", FileStatus: "pending", RestoreStatus: "not_run",
		StartedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	created, _, err := r.store.CreateRun(ctx, run)
	if err != nil {
		return ""
	}
	_ = r.store.AddRunLog(ctx, created.ID, "INFO", "开始删除任务配置及其备份文件，范围由服务端重新评估")
	return created.ID
}

// serverPathOf 从引擎元数据中提取数据库服务器上的实际文件路径；缺失时退回登记的相对路径。
func serverPathOf(artifact metadata.Artifact) string {
	var meta map[string]any
	if err := json.Unmarshal(artifact.EngineMetadata, &meta); err == nil {
		if path, ok := meta["server_path"].(string); ok && path != "" {
			return path
		}
	}
	return artifact.RelativePath
}

// deleteArtifactFile 只处理已登记、位于授权根目录内的对象；
// 递归删除仅限于 runner 自建的 .run_<id>.attempt-N 工作目录，其他目录一律不递归。
func (r *RemovalService) deleteArtifactFile(ctx context.Context, artifact metadata.Artifact, target metadata.StorageTarget) CleanupExecution {
	execution := CleanupExecution{ArtifactID: artifact.ID, Path: artifact.RelativePath}
	if artifact.TargetID == "" {
		// 数据库服务器本地备份：执行器接触不到文件，删除只清记录；
		// 远程文件必须由人在数据库服务器上处理，绝不假装已删。
		if err := r.store.DeleteArtifactRecord(ctx, artifact.ID); err != nil {
			execution.Message = "记录登记失败，远程文件与记录均未处理"
			return execution
		}
		execution.Deleted = true
		execution.Message = "记录已删除；文件在数据库服务器上，需人工删除：" + serverPathOf(artifact)
		return execution
	}
	if target.Kind == "ftp" {
		// FTP：先回读比对哈希确认身份，再删除远端文件；任一步失败都保持记录原状以便重试。
		cipher, secretErr := r.store.StoragePassword(ctx, target.ID)
		if secretErr != nil {
			execution.Message = "读取存储凭据失败: " + secretErr.Error()
			return execution
		}
		password, decryptErr := r.secrets.Decrypt(cipher)
		if decryptErr != nil {
			execution.Message = "存储凭据解密失败，请检查独立主密钥"
			return execution
		}
		conn, dialErr := dialFTP(target, password)
		if dialErr != nil {
			execution.Message = dialErr.Error()
			return execution
		}
		defer conn.Quit()
		sum, hashErr := ftpDownloadHash(conn, target, artifact.RelativePath)
		if hashErr != nil {
			// 与本地路径保持一致：远端已不存在时登记删除，避免记录永久悬挂。
			// 只有「文件仍存在但内容不符」才拒绝，不能把两种情况混为一谈。
			if isFTPNotExist(hashErr) {
				if err := r.store.DeleteArtifactRecord(ctx, artifact.ID); err != nil {
					execution.Message = "远端文件已不存在，但元数据登记失败"
					return execution
				}
				execution.Deleted = true
				execution.Message = "远端文件已不存在，仅登记删除状态"
				return execution
			}
			execution.Message = hashErr.Error()
			return execution
		}
		if artifact.SHA256 != "" && sum != artifact.SHA256 {
			execution.Message = "远端文件 SHA-256 与登记不一致，停止删除"
			return execution
		}
		remotePath, deleteErr := ftpDelete(conn, target, artifact.RelativePath)
		if deleteErr != nil {
			execution.Message = "删除远端文件失败: " + deleteErr.Error()
			return execution
		}
		if err := r.store.DeleteArtifactRecord(ctx, artifact.ID); err != nil {
			execution.Message = "远端文件已删除，但元数据状态提交失败，需要人工对账"
			return execution
		}
		execution.Deleted = true
		execution.Message = "已删除远端文件 " + remotePath
		return execution
	}
	if target.Kind == "oss" {
		// 对象存储：先确认对象存在且长度一致，再删除；S3 的删除是幂等的，
		// 但"远端已不存在"与"权限不足"必须区分，不能把失败当成功登记。
		cipher, secretErr := r.store.StoragePassword(ctx, target.ID)
		if secretErr != nil {
			execution.Message = "读取存储凭据失败: " + secretErr.Error()
			return execution
		}
		secret, decryptErr := r.secrets.Decrypt(cipher)
		if decryptErr != nil {
			execution.Message = "存储凭据解密失败，请检查独立主密钥"
			return execution
		}
		remoteSize, objectKey, headErr := ossHead(ctx, target, secret, artifact.RelativePath)
		if headErr != nil {
			if errors.Is(headErr, errObjectNotFound) {
				if err := r.store.DeleteArtifactRecord(ctx, artifact.ID); err != nil {
					execution.Message = "远端对象已不存在，但元数据登记失败"
					return execution
				}
				execution.Deleted = true
				execution.Message = "远端对象已不存在，仅登记删除状态"
				return execution
			}
			execution.Message = headErr.Error()
			return execution
		}
		if artifact.Size > 0 && remoteSize != artifact.Size {
			execution.Message = fmt.Sprintf("远端对象长度 %d 与登记 %d 不一致，停止删除", remoteSize, artifact.Size)
			return execution
		}
		// 小对象额外回读重算 SHA-256 确认身份；大对象以长度为准（回读成本按流量计费）。
		if digest, ok, hashErr := ossDownloadHash(ctx, target, secret, artifact.RelativePath, remoteSize); hashErr != nil {
			execution.Message = hashErr.Error()
			return execution
		} else if ok && artifact.SHA256 != "" && artifact.SHA256 != "pending" && digest != artifact.SHA256 {
			execution.Message = "远端对象 SHA-256 与登记不一致，停止删除"
			return execution
		}
		if _, deleteErr := ossDelete(ctx, target, secret, artifact.RelativePath); deleteErr != nil && !errors.Is(deleteErr, errObjectNotFound) {
			execution.Message = "删除远端对象失败: " + deleteErr.Error()
			return execution
		}
		if err := r.store.DeleteArtifactRecord(ctx, artifact.ID); err != nil {
			execution.Message = "远端对象已删除，但元数据状态提交失败，需要人工对账"
			return execution
		}
		execution.Deleted = true
		execution.Message = "已删除对象存储中的 " + objectKey
		return execution
	}
	root := target.Root
	if root == "" {
		execution.Message = "存储位置已不存在，无法确认授权根目录，暂停删除"
		return execution
	}
	path, err := SecureRemovalPath(root, artifact.RelativePath)
	if err != nil {
		execution.Message = err.Error()
		return execution
	}
	info, statErr := os.Lstat(path)
	switch {
	case os.IsNotExist(statErr):
		// 文件已不在磁盘：登记为删除，避免记录长期悬挂。
		if err = r.store.DeleteArtifactRecord(ctx, artifact.ID); err != nil {
			execution.Message = "文件不存在，但元数据登记失败"
			return execution
		}
		execution.Deleted = true
		execution.Message = "磁盘文件已不存在，仅登记删除状态"
		return execution
	case statErr != nil:
		execution.Message = "读取文件失败: " + statErr.Error()
		return execution
	case info.IsDir():
		// runner 自己命名的 .run_<id>.attempt-N 是单次备份尝试的工作目录，里面只有中间产物，
		// 中断后必须能整棵清理，否则任务会永久卡在“部分文件未删除”。
		// 该命名不可能指向正式备份对象，且已校验状态不允许是 verified。
		if artifact.State != "verified" && isAttemptResidueDir(path) {
			removed, removeErr := removeAttemptResidue(path)
			if removeErr != nil {
				execution.Message = "清理中断残留失败：" + removeErr.Error()
				return execution
			}
			if err = r.store.DeleteArtifactRecord(ctx, artifact.ID); err != nil {
				execution.Message = "残留已清理，但元数据状态提交失败，需要人工对账"
				return execution
			}
			execution.Deleted = true
			execution.Message = fmt.Sprintf("已清理中断残留目录（%d 个文件）", removed)
			return execution
		}
		// 其他目录：只删除空目录，绝不递归，避免误删整棵备份树。
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			execution.Message = "读取目录失败: " + readErr.Error()
			return execution
		}
		if len(entries) > 0 {
			execution.Message = fmt.Sprintf("目录非空（%d 项），不做递归删除，需人工确认后清理", len(entries))
			return execution
		}
		if err = os.Remove(path); err != nil {
			execution.Message = "删除空目录失败: " + err.Error()
			return execution
		}
		if err = r.store.DeleteArtifactRecord(ctx, artifact.ID); err != nil {
			execution.Message = "空目录已删除，但元数据状态提交失败，需要人工对账"
			return execution
		}
		execution.Deleted = true
		execution.Message = "已删除空目录残留"
		return execution
	case !info.Mode().IsRegular():
		execution.Message = "目标不是普通文件，拒绝删除"
		return execution
	}
	if artifact.State == "verified" {
		size, digest, hashErr := HashFile(path)
		if hashErr != nil || size != artifact.Size || digest != artifact.SHA256 {
			execution.Message = "文件大小或 SHA-256 与登记信息不一致，停止删除"
			return execution
		}
	}
	if err = os.Remove(path); err != nil {
		execution.Message = "删除失败: " + err.Error()
		return execution
	}
	if err = r.store.DeleteArtifactRecord(ctx, artifact.ID); err != nil {
		execution.Message = "文件已删除，但元数据状态提交失败，需要人工对账"
		return execution
	}
	execution.Deleted = true
	execution.Message = "已删除并登记"
	return execution
}

func isActiveRunState(state string) bool {
	switch state {
	case "queued", "running", "verifying", "reconciling":
		return true
	}
	return false
}
