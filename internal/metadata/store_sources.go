package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CreateSource 在同一事务中保存加密凭据和数据源。
func (s *Store) CreateSource(ctx context.Context, source Source, ciphertext string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO secrets(id,ciphertext,created_at) VALUES(?,?,?)`, source.SecretRef, ciphertext, formatTime(time.Now())); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sources(id,name,engine,host,port,database_name,username,trust_server_certificate,secret_ref,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		source.ID, source.Name, source.Engine, source.Host, source.Port, source.Database, source.Username, boolToInt(source.TrustServerCertificate), source.SecretRef, source.Revision, formatTime(source.CreatedAt), formatTime(source.UpdatedAt))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateSource 更新连接信息；ciphertext 为空时保留原凭据。
func (s *Store) UpdateSource(ctx context.Context, source Source, ciphertext string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if ciphertext != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE secrets SET ciphertext=? WHERE id=?`, ciphertext, source.SecretRef); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE sources SET name=?,host=?,port=?,database_name=?,username=?,trust_server_certificate=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`,
		source.Name, source.Host, source.Port, source.Database, source.Username, boolToInt(source.TrustServerCertificate), formatTime(source.UpdatedAt), source.ID, source.Revision)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrConflict
	}
	return tx.Commit()
}

// sourceColumns 统一列出扫描字段顺序，避免各查询与 scanSource 错位。
const sourceColumns = `id,name,engine,host,port,database_name,username,trust_server_certificate,secret_ref,revision,created_at,updated_at`

// ListSources 返回不包含密文的数据源列表。
func (s *Store) ListSources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sourceColumns+` FROM sources ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Source, 0)
	for rows.Next() {
		source, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, source)
	}
	return result, rows.Err()
}

// GetSource 按 ID 获取数据源。
func (s *Store) GetSource(ctx context.Context, id string) (Source, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sourceColumns+` FROM sources WHERE id=?`, id)
	source, err := scanSource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	return source, err
}

// SourcePassword 只在执行器内部取得解密后的凭据。
func (s *Store) SourcePassword(ctx context.Context, sourceID string) (string, error) {
	var ciphertext string
	err := s.db.QueryRowContext(ctx, `SELECT sec.ciphertext FROM sources src JOIN secrets sec ON sec.id=src.secret_ref WHERE src.id=?`, sourceID).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return ciphertext, err
}

type rowScanner interface{ Scan(dest ...any) error }

func scanSource(row rowScanner) (Source, error) {
	var source Source
	var trust, created, updated string
	err := row.Scan(&source.ID, &source.Name, &source.Engine, &source.Host, &source.Port, &source.Database, &source.Username, &trust, &source.SecretRef, &source.Revision, &created, &updated)
	if err != nil {
		return Source{}, err
	}
	source.TrustServerCertificate = trust == "1" || trust == "true"
	if source.CreatedAt, err = parseTime(created); err != nil {
		return Source{}, fmt.Errorf("解析数据源创建时间: %w", err)
	}
	source.UpdatedAt, err = parseTime(updated)
	return source, err
}

// storageColumns 统一列出扫描字段顺序，避免各查询与 scanStorage 错位。
const storageColumns = `id,name,kind,root,server_root,revision,created_at,host,port,username,secret_ref,passive,bucket,region,use_ssl,path_style`

// CreateStorageTarget 登记执行器本地目录、共享目录、FTP 或对象存储。
func (s *Store) CreateStorageTarget(ctx context.Context, target StorageTarget) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO storage_targets(`+storageColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		target.ID, target.Name, target.Kind, target.Root, target.ServerRoot, target.Revision, formatTime(target.CreatedAt), target.Host, target.Port, target.Username, nullableString(target.SecretRef), boolToInt(target.Passive), target.Bucket, target.Region, boolToInt(target.UseSSL), boolToInt(target.PathStyle))
	return err
}

// UpdateStorageTarget 修改名称与连接配置；已有备份时禁止改根目录，避免旧文件失去定位。
func (s *Store) UpdateStorageTarget(ctx context.Context, target StorageTarget) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE target_id=?`, target.ID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		var oldRoot, oldServerRoot, oldKind string
		if err := s.db.QueryRowContext(ctx, `SELECT root,server_root,kind FROM storage_targets WHERE id=?`, target.ID).Scan(&oldRoot, &oldServerRoot, &oldKind); err != nil {
			return err
		}
		if oldRoot != target.Root || oldServerRoot != target.ServerRoot || oldKind != target.Kind {
			return fmt.Errorf("%w: 已有备份的仓库不能修改根目录或存储类型", ErrConflict)
		}
	}
	result, err := s.db.ExecContext(ctx, `UPDATE storage_targets SET name=?,root=?,server_root=?,revision=revision+1,host=?,port=?,username=?,secret_ref=?,passive=?,bucket=?,region=?,use_ssl=?,path_style=? WHERE id=? AND revision=?`,
		target.Name, target.Root, target.ServerRoot, target.Host, target.Port, target.Username, nullableString(target.SecretRef), boolToInt(target.Passive), target.Bucket, target.Region, boolToInt(target.UseSSL), boolToInt(target.PathStyle), target.ID, target.Revision)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrConflict
	}
	return nil
}

// StorageTargetUsage 汇总一个仓库仍被引用的位置，用于删除前给出可执行的解除建议。
type StorageTargetUsage struct {
	Jobs      int `json:"job_count"`
	Artifacts int `json:"artifact_count"`
	Previews  int `json:"preview_count"`
}

// InUse 表示仍有任务、备份对象或清理预览指向该仓库。
func (u StorageTargetUsage) InUse() bool {
	return u.Jobs > 0 || u.Artifacts > 0 || u.Previews > 0
}

// Describe 生成面向用户的解除建议，说明每一类引用该怎么处理。
func (u StorageTargetUsage) Describe() string {
	parts := make([]string, 0, 3)
	if u.Jobs > 0 {
		parts = append(parts, fmt.Sprintf("%d 个备份任务仍使用该目录，需先修改这些任务的存储位置或删除任务", u.Jobs))
	}
	if u.Artifacts > 0 {
		parts = append(parts, fmt.Sprintf("仍有 %d 个备份对象记录指向该目录，需先删除对应备份文件", u.Artifacts))
	}
	if u.Previews > 0 {
		parts = append(parts, fmt.Sprintf("存在 %d 条清理预览引用该目录，需重新生成预览后再试", u.Previews))
	}
	return strings.Join(parts, "；")
}

// StorageTargetUsage 统计仓库引用。删除前必须调用，避免外键失败被当成内部错误。
func (s *Store) StorageTargetUsage(ctx context.Context, id string) (StorageTargetUsage, error) {
	var usage StorageTargetUsage
	queries := []struct {
		sql   string
		count *int
	}{
		{`SELECT COUNT(*) FROM jobs WHERE target_id=?`, &usage.Jobs},
		{`SELECT COUNT(*) FROM artifacts WHERE target_id=?`, &usage.Artifacts},
		// 清理预览表没有 target_id 列，按任务反查：任务改换仓库后旧预览不再构成引用。
		{`SELECT COUNT(*) FROM cleanup_previews WHERE job_id IN (SELECT id FROM jobs WHERE target_id=?)`, &usage.Previews},
	}
	for _, item := range queries {
		if err := s.db.QueryRowContext(ctx, item.sql, id).Scan(item.count); err != nil {
			return StorageTargetUsage{}, err
		}
	}
	return usage, nil
}

// DeleteStorageTarget 删除不再被引用的仓库。引用检查与删除在同一事务内完成，
// 避免检查后被并发创建的任务穿透成外键错误。不会删除磁盘上的任何文件。
func (s *Store) DeleteStorageTarget(ctx context.Context, id, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name, root, kind string
	var secretRef sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT name,root,kind,secret_ref FROM storage_targets WHERE id=?`, id).Scan(&name, &root, &kind, &secretRef); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var jobs, artifacts int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE target_id=?`, id).Scan(&jobs); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE target_id=?`, id).Scan(&artifacts); err != nil {
		return err
	}
	if jobs > 0 || artifacts > 0 {
		return fmt.Errorf("%w: %s", ErrConflict, StorageTargetUsage{Jobs: jobs, Artifacts: artifacts}.Describe())
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM storage_targets WHERE id=?`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	// FTP 凭据只被这个位置引用；位置删除后保留密文只会留下不可解密的孤儿数据。
	if secretRef.Valid && secretRef.String != "" {
		if _, err = tx.ExecContext(ctx, `DELETE FROM secrets WHERE id=?`, secretRef.String); err != nil {
			return err
		}
	}
	// 删除后名称与路径已不可查，写审计保留可追溯的删除对象。
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(actor,action,target,result,request_id,created_at) VALUES(?,?,?,?,?,?)`,
		actor, "storage.delete", id, "success:"+name+"|"+root, "", formatTime(time.Now())); err != nil {
		return fmt.Errorf("写入删除审计: %w", err)
	}
	return tx.Commit()
}

// DeleteSource 删除不再被任务引用的数据源，并连同其加密凭据一并清除。
// 引用检查与删除在同一事务内完成；凭据删除后无法恢复，重新添加数据源需重新录入密码。
func (s *Store) DeleteSource(ctx context.Context, id, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name, engine, secretRef string
	if err = tx.QueryRowContext(ctx, `SELECT name,engine,secret_ref FROM sources WHERE id=?`, id).Scan(&name, &engine, &secretRef); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var jobs int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE source_id=?`, id).Scan(&jobs); err != nil {
		return err
	}
	if jobs > 0 {
		return fmt.Errorf("%w: %d 个备份任务仍使用该数据源，需先修改这些任务的数据库或删除任务", ErrConflict, jobs)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM sources WHERE id=?`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	// 凭据只被这个数据源引用，源删除后保留密文只会留下不可解密的孤儿数据。
	if _, err = tx.ExecContext(ctx, `DELETE FROM secrets WHERE id=?`, secretRef); err != nil {
		return err
	}
	// 删除后连接信息已不可查，写审计保留可追溯的删除对象。
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(actor,action,target,result,request_id,created_at) VALUES(?,?,?,?,?,?)`,
		actor, "source.delete", id, "success:"+name+"|"+engine, "", formatTime(time.Now())); err != nil {
		return fmt.Errorf("写入删除审计: %w", err)
	}
	return tx.Commit()
}

// SourceUsage 汇总一个数据源仍被引用的位置，用于删除前给出可执行的解除建议。
type SourceUsage struct {
	Jobs      int `json:"job_count"`
	Artifacts int `json:"artifact_count"`
}

// Describe 生成面向用户的解除建议。
func (u SourceUsage) Describe() string {
	parts := make([]string, 0, 2)
	if u.Jobs > 0 {
		parts = append(parts, fmt.Sprintf("%d 个备份任务仍使用该数据源，需先修改这些任务的数据库或删除任务", u.Jobs))
	}
	if u.Artifacts > 0 {
		parts = append(parts, fmt.Sprintf("仍有 %d 个备份对象登记在该数据源下，需先删除对应备份文件", u.Artifacts))
	}
	return strings.Join(parts, "；")
}

// SourceUsage 统计数据源引用。删除前必须调用，避免外键失败被当成内部错误。
func (s *Store) SourceUsage(ctx context.Context, id string) (SourceUsage, error) {
	var usage SourceUsage
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE source_id=?`, id).Scan(&usage.Jobs); err != nil {
		return SourceUsage{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE source_id=?`, id).Scan(&usage.Artifacts); err != nil {
		return SourceUsage{}, err
	}
	return usage, nil
}

// StoragePassword 返回存储位置（FTP）的加密凭据密文，由调用方解密；未配置凭据时返回空串。
func (s *Store) StoragePassword(ctx context.Context, targetID string) (string, error) {
	var ref sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT secret_ref FROM storage_targets WHERE id=?`, targetID).Scan(&ref); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if !ref.Valid {
		return "", nil
	}
	var ciphertext string
	if err := s.db.QueryRowContext(ctx, `SELECT ciphertext FROM secrets WHERE id=?`, ref.String).Scan(&ciphertext); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("存储凭据 %s 不存在", ref.String)
		}
		return "", err
	}
	return ciphertext, nil
}

// CreateSecret 写入一条新的加密凭据；调用方持有 ID 并在 storage_targets.secret_ref 中引用。
func (s *Store) CreateSecret(ctx context.Context, id, ciphertext string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO secrets(id,ciphertext,created_at) VALUES(?,?,?)`, id, ciphertext, formatTime(time.Now()))
	return err
}

// UpdateSecret 轮换已有凭据的密文。
func (s *Store) UpdateSecret(ctx context.Context, id, ciphertext string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE secrets SET ciphertext=? WHERE id=?`, ciphertext, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	return nil
}

// ListStorageTargets 列出仓库。
func (s *Store) ListStorageTargets(ctx context.Context) ([]StorageTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+storageColumns+` FROM storage_targets ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]StorageTarget, 0)
	for rows.Next() {
		target, err := scanStorage(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, target)
	}
	return result, rows.Err()
}

// GetStorageTarget 按 ID 获取仓库。
func (s *Store) GetStorageTarget(ctx context.Context, id string) (StorageTarget, error) {
	target, err := scanStorage(s.db.QueryRowContext(ctx, `SELECT `+storageColumns+` FROM storage_targets WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return StorageTarget{}, ErrNotFound
	}
	return target, err
}

func scanStorage(row rowScanner) (StorageTarget, error) {
	var target StorageTarget
	var created string
	var host, username, secretRef sql.NullString
	var port, passive, useSSL, pathStyle int
	err := row.Scan(&target.ID, &target.Name, &target.Kind, &target.Root, &target.ServerRoot, &target.Revision, &created, &host, &port, &username, &secretRef, &passive, &target.Bucket, &target.Region, &useSSL, &pathStyle)
	if err != nil {
		return StorageTarget{}, err
	}
	target.Host, target.Username, target.SecretRef = host.String, username.String, secretRef.String
	target.Port, target.Passive = port, passive != 0
	target.UseSSL, target.PathStyle = useSSL != 0, pathStyle != 0
	target.CreatedAt, err = parseTime(created)
	return target, err
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
