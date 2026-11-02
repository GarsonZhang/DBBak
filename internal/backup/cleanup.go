package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"vault/internal/metadata"
)

// CleanupItem 同时描述候选与保护原因。
type CleanupItem struct {
	Artifact  metadata.Artifact `json:"artifact"`
	Candidate bool              `json:"candidate"`
	Reason    string            `json:"reason"`
}

// CleanupResult 是服务端生成并可复核的清理评估。
type CleanupResult struct {
	JobID         string        `json:"job_id"`
	JobRevision   int64         `json:"job_revision"`
	Fingerprint   string        `json:"fingerprint"`
	CandidateSize int64         `json:"candidate_size"`
	Items         []CleanupItem `json:"items"`
}

// CleanupExecution 记录单个对象的实际删除结果。
type CleanupExecution struct {
	ArtifactID string `json:"artifact_id"`
	Path       string `json:"path"`
	Deleted    bool   `json:"deleted"`
	Message    string `json:"message"`
}

// CleanupService 计算全量恢复点保留集合并执行受控清理。
type CleanupService struct {
	store *metadata.Store
}

// NewCleanupService 创建清理服务。
func NewCleanupService(store *metadata.Store) *CleanupService { return &CleanupService{store: store} }

// Preview 生成绑定任务 revision 和对象指纹的十分钟预览。
func (c *CleanupService) Preview(ctx context.Context, jobID string) (CleanupResult, metadata.CleanupPreview, error) {
	job, err := c.store.GetJob(ctx, jobID)
	if err != nil {
		return CleanupResult{}, metadata.CleanupPreview{}, err
	}
	result, err := c.evaluate(ctx, job, time.Now().UTC())
	if err != nil {
		return CleanupResult{}, metadata.CleanupPreview{}, err
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return CleanupResult{}, metadata.CleanupPreview{}, err
	}
	now := time.Now().UTC()
	preview := metadata.CleanupPreview{
		ID: metadata.NewID("preview"), JobID: jobID, JobRevision: job.Revision,
		Fingerprint: result.Fingerprint, Payload: string(payload), ExpiresAt: now.Add(10 * time.Minute), CreatedAt: now,
	}
	if err = c.store.SaveCleanupPreview(ctx, preview); err != nil {
		return CleanupResult{}, metadata.CleanupPreview{}, err
	}
	return result, preview, nil
}

// Execute 重新核对预览版本、对象状态、真实路径和文件摘要后逐个删除。
func (c *CleanupService) Execute(ctx context.Context, previewID string) ([]CleanupExecution, error) {
	preview, err := c.store.GetCleanupPreview(ctx, previewID)
	if err != nil {
		return nil, err
	}
	if time.Now().UTC().After(preview.ExpiresAt) {
		return nil, fmt.Errorf("%w: 清理预览已过期", metadata.ErrConflict)
	}
	job, err := c.store.GetJob(ctx, preview.JobID)
	if err != nil {
		return nil, err
	}
	if job.Revision != preview.JobRevision {
		return nil, fmt.Errorf("%w: 任务配置已变化，请重新预览", metadata.ErrConflict)
	}
	current, err := c.evaluate(ctx, job, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if current.Fingerprint != preview.Fingerprint {
		return nil, fmt.Errorf("%w: 备份或运行状态已变化，请重新预览", metadata.ErrConflict)
	}
	var original CleanupResult
	if err = json.Unmarshal([]byte(preview.Payload), &original); err != nil {
		return nil, fmt.Errorf("清理预览内容无效: %w", err)
	}
	target, err := c.store.GetStorageTarget(ctx, job.TargetID)
	if err != nil {
		return nil, err
	}
	var executions []CleanupExecution
	for _, item := range original.Items {
		if !item.Candidate {
			continue
		}
		execution := CleanupExecution{ArtifactID: item.Artifact.ID, Path: item.Artifact.RelativePath}
		// 即使指纹一致，也按对象重新读取，防止后续引入批量删除时扩大范围。
		artifact, getErr := c.store.GetArtifact(ctx, item.Artifact.ID)
		if getErr != nil || artifact.State != "verified" || artifact.Locked {
			execution.Message = "对象状态已变化，停止删除该对象"
			executions = append(executions, execution)
			continue
		}
		path, pathErr := SecurePath(target.Root, artifact.RelativePath, true)
		if pathErr != nil {
			execution.Message = pathErr.Error()
			executions = append(executions, execution)
			continue
		}
		size, digest, hashErr := HashFile(path)
		if hashErr != nil || size != artifact.Size || digest != artifact.SHA256 {
			execution.Message = "文件大小或 SHA-256 与登记信息不一致，停止删除"
			executions = append(executions, execution)
			continue
		}
		if removeErr := os.Remove(path); removeErr != nil {
			execution.Message = "删除失败: " + removeErr.Error()
			executions = append(executions, execution)
			continue
		}
		if updateErr := c.store.DeleteArtifactRecord(ctx, artifact.ID); updateErr != nil {
			execution.Message = "文件已删除，但元数据状态提交失败，需要人工对账"
			executions = append(executions, execution)
			continue
		}
		execution.Deleted = true
		execution.Message = "已删除并登记"
		executions = append(executions, execution)
	}
	return executions, nil
}

func (c *CleanupService) evaluate(ctx context.Context, job metadata.Job, now time.Time) (CleanupResult, error) {
	artifacts, err := c.store.ListArtifacts(ctx, job.ID)
	if err != nil {
		return CleanupResult{}, err
	}
	sort.SliceStable(artifacts, func(i, j int) bool { return artifacts[i].CreatedAt.After(artifacts[j].CreatedAt) })
	active, err := c.store.ActiveRunForJob(ctx, job.ID)
	if err != nil {
		return CleanupResult{}, err
	}
	result := CleanupResult{JobID: job.ID, JobRevision: job.Revision}
	verifiedFull := 0
	for _, artifact := range artifacts {
		if artifact.State == "deleted" {
			continue
		}
		item := CleanupItem{Artifact: artifact}
		switch {
		case artifact.State != "verified":
			item.Reason = "未通过文件校验，保留待核查"
		case artifact.Locked:
			item.Reason = "人工锁定保留"
		case artifact.ChainID == "" || artifact.ParentID != nil:
			item.Reason = "恢复依赖信息不属于阶段一独立全量模型，停止自动判断"
		case active:
			item.Reason = "任务存在活动运行，整个预览保持保守"
		case verifiedFull < job.MinFull:
			item.Reason = fmt.Sprintf("最低保留 %d 份全量", job.MinFull)
			verifiedFull++
		case artifact.CreatedAt.After(now.Add(-time.Duration(job.RetentionDays) * 24 * time.Hour)):
			item.Reason = fmt.Sprintf("仍在 %d 天保留窗口内", job.RetentionDays)
		default:
			item.Candidate = true
			item.Reason = "已过保留期且不属于最低保留集合"
			result.CandidateSize += artifact.Size
		}
		// 只有已验证全量计入最低份数；其他状态不参与序号计算。
		if item.Candidate {
			verifiedFull++
		}
		result.Items = append(result.Items, item)
	}
	fingerprint := sha256.New()
	fmt.Fprintf(fingerprint, "job:%s:%d\n", job.ID, job.Revision)
	for _, item := range result.Items {
		a := item.Artifact
		fmt.Fprintf(fingerprint, "%s|%s|%t|%s|%d|%s|%s|%t\n", a.ID, a.State, a.Locked, a.RelativePath, a.Size, a.SHA256, a.RunID, item.Candidate)
	}
	fmt.Fprintf(fingerprint, "active:%t", active)
	result.Fingerprint = hex.EncodeToString(fingerprint.Sum(nil))
	return result, nil
}
