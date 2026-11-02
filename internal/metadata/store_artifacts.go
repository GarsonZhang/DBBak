package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreateArtifact 登记正在写入的备份对象。
func (s *Store) CreateArtifact(ctx context.Context, artifact Artifact) error {
	if len(artifact.EngineMetadata) == 0 {
		artifact.EngineMetadata = []byte("{}")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO artifacts(id,run_id,job_id,source_id,target_id,chain_id,parent_id,type,method,relative_path,size,sha256,state,locked,tool_version,engine_metadata,created_at,verified_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		artifact.ID, artifact.RunID, artifact.JobID, artifact.SourceID, artifact.TargetID, artifact.ChainID, artifact.ParentID, artifact.Type, artifact.Method, artifact.RelativePath, artifact.Size, artifact.SHA256, artifact.State, artifact.Locked, artifact.ToolVersion, artifact.EngineMetadata, formatTime(artifact.CreatedAt), nullableTime(artifact.VerifiedAt))
	return err
}

// UpdateArtifact 更新校验状态、大小和摘要。
func (s *Store) UpdateArtifact(ctx context.Context, artifact Artifact) error {
	if len(artifact.EngineMetadata) == 0 {
		artifact.EngineMetadata = []byte("{}")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE artifacts SET relative_path=?,size=?,sha256=?,state=?,locked=?,method=?,tool_version=?,engine_metadata=?,verified_at=? WHERE id=?`, artifact.RelativePath, artifact.Size, artifact.SHA256, artifact.State, artifact.Locked, artifact.Method, artifact.ToolVersion, artifact.EngineMetadata, nullableTime(artifact.VerifiedAt), artifact.ID)
	return err
}

// SetArtifactLocked 设置人工保留标记。
func (s *Store) SetArtifactLocked(ctx context.Context, id string, locked bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE artifacts SET locked=? WHERE id=?`, locked, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	return nil
}

// DeleteArtifactRecord 在文件删除后登记状态，不物理删除元数据。
func (s *Store) DeleteArtifactRecord(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE artifacts SET state='deleted' WHERE id=?`, id)
	return err
}

// ListArtifacts 查询文件记录；jobID 为空时返回全部。
// LatestVerifiedArtifacts 返回指定数据源与仓库下最近的已校验对象，用于确定差异与日志的基线。
func (s *Store) LatestVerifiedArtifacts(ctx context.Context, sourceID, targetID string, limit int) ([]Artifact, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, artifactSelect+
		` WHERE a.source_id=? AND a.target_id=? AND a.state='verified' ORDER BY a.created_at DESC LIMIT ?`,
		sourceID, targetID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Artifact, 0, limit)
	for rows.Next() {
		artifact, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	return result, rows.Err()
}

func (s *Store) ListArtifacts(ctx context.Context, jobID string) ([]Artifact, error) {
	query := artifactSelect
	args := []any{}
	if jobID != "" {
		query += ` WHERE a.job_id=?`
		args = append(args, jobID)
	}
	query += ` ORDER BY a.created_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Artifact, 0)
	for rows.Next() {
		artifact, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	return result, rows.Err()
}

// GetArtifact 按 ID 获取对象。
func (s *Store) GetArtifact(ctx context.Context, id string) (Artifact, error) {
	artifact, err := scanArtifact(s.db.QueryRowContext(ctx, artifactSelect+` WHERE a.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	return artifact, err
}

const artifactSelect = `SELECT a.id,a.run_id,a.job_id,a.source_id,a.target_id,a.chain_id,a.parent_id,a.type,a.method,a.relative_path,a.size,a.sha256,a.state,a.locked,a.tool_version,a.engine_metadata,a.created_at,a.verified_at FROM artifacts a`

func scanArtifact(row rowScanner) (Artifact, error) {
	var artifact Artifact
	var parent, verified sql.NullString
	var created, engineMetadata string
	err := row.Scan(&artifact.ID, &artifact.RunID, &artifact.JobID, &artifact.SourceID, &artifact.TargetID, &artifact.ChainID, &parent, &artifact.Type, &artifact.Method, &artifact.RelativePath, &artifact.Size, &artifact.SHA256, &artifact.State, &artifact.Locked, &artifact.ToolVersion, &engineMetadata, &created, &verified)
	if err != nil {
		return Artifact{}, err
	}
	artifact.EngineMetadata = []byte(engineMetadata)
	if parent.Valid {
		artifact.ParentID = &parent.String
	}
	if artifact.VerifiedAt, err = scanNullableTime(verified); err != nil {
		return Artifact{}, err
	}
	if artifact.CreatedAt, err = parseTime(created); err != nil {
		return Artifact{}, fmt.Errorf("解析备份时间: %w", err)
	}
	return artifact, nil
}

// CleanupPreview 是带配置版本和文件指纹的服务端预览。
type CleanupPreview struct {
	ID          string    `json:"id"`
	JobID       string    `json:"job_id"`
	JobRevision int64     `json:"job_revision"`
	Fingerprint string    `json:"fingerprint"`
	Payload     string    `json:"-"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// SaveCleanupPreview 保存不可由客户端伪造的候选清单。
func (s *Store) SaveCleanupPreview(ctx context.Context, preview CleanupPreview) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO cleanup_previews(id,job_id,job_revision,fingerprint,payload,expires_at,created_at) VALUES(?,?,?,?,?,?,?)`, preview.ID, preview.JobID, preview.JobRevision, preview.Fingerprint, preview.Payload, formatTime(preview.ExpiresAt), formatTime(preview.CreatedAt))
	return err
}

// GetCleanupPreview 获取服务端预览。
func (s *Store) GetCleanupPreview(ctx context.Context, id string) (CleanupPreview, error) {
	var preview CleanupPreview
	var expires, created string
	err := s.db.QueryRowContext(ctx, `SELECT id,job_id,job_revision,fingerprint,payload,expires_at,created_at FROM cleanup_previews WHERE id=?`, id).Scan(&preview.ID, &preview.JobID, &preview.JobRevision, &preview.Fingerprint, &preview.Payload, &expires, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return CleanupPreview{}, ErrNotFound
	}
	if err != nil {
		return CleanupPreview{}, err
	}
	if preview.ExpiresAt, err = parseTime(expires); err != nil {
		return CleanupPreview{}, err
	}
	preview.CreatedAt, err = parseTime(created)
	return preview, err
}
