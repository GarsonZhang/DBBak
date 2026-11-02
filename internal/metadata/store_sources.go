package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	_, err = tx.ExecContext(ctx, `INSERT INTO sources(id,name,engine,host,port,database_name,username,secret_ref,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		source.ID, source.Name, source.Engine, source.Host, source.Port, source.Database, source.Username, source.SecretRef, source.Revision, formatTime(source.CreatedAt), formatTime(source.UpdatedAt))
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
	result, err := tx.ExecContext(ctx, `UPDATE sources SET name=?,host=?,port=?,database_name=?,username=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`,
		source.Name, source.Host, source.Port, source.Database, source.Username, formatTime(source.UpdatedAt), source.ID, source.Revision)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrConflict
	}
	return tx.Commit()
}

// ListSources 返回不包含密文的数据源列表。
func (s *Store) ListSources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,engine,host,port,database_name,username,secret_ref,revision,created_at,updated_at FROM sources ORDER BY created_at`)
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
	row := s.db.QueryRowContext(ctx, `SELECT id,name,engine,host,port,database_name,username,secret_ref,revision,created_at,updated_at FROM sources WHERE id=?`, id)
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
	var created, updated string
	err := row.Scan(&source.ID, &source.Name, &source.Engine, &source.Host, &source.Port, &source.Database, &source.Username, &source.SecretRef, &source.Revision, &created, &updated)
	if err != nil {
		return Source{}, err
	}
	if source.CreatedAt, err = parseTime(created); err != nil {
		return Source{}, fmt.Errorf("解析数据源创建时间: %w", err)
	}
	source.UpdatedAt, err = parseTime(updated)
	return source, err
}

// CreateStorageTarget 登记执行器本地目录。
func (s *Store) CreateStorageTarget(ctx context.Context, target StorageTarget) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO storage_targets(id,name,kind,root,server_root,revision,created_at) VALUES(?,?,?,?,?,?,?)`, target.ID, target.Name, target.Kind, target.Root, target.ServerRoot, target.Revision, formatTime(target.CreatedAt))
	return err
}

// UpdateStorageTarget 修改名称；已有备份时禁止改根目录，避免旧文件失去定位。
func (s *Store) UpdateStorageTarget(ctx context.Context, target StorageTarget) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE target_id=?`, target.ID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		var oldRoot, oldServerRoot string
		if err := s.db.QueryRowContext(ctx, `SELECT root,server_root FROM storage_targets WHERE id=?`, target.ID).Scan(&oldRoot, &oldServerRoot); err != nil {
			return err
		}
		if oldRoot != target.Root || oldServerRoot != target.ServerRoot {
			return fmt.Errorf("%w: 已有备份的仓库不能修改根目录或服务路径映射", ErrConflict)
		}
	}
	result, err := s.db.ExecContext(ctx, `UPDATE storage_targets SET name=?,root=?,server_root=?,revision=revision+1 WHERE id=? AND revision=?`, target.Name, target.Root, target.ServerRoot, target.ID, target.Revision)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrConflict
	}
	return nil
}

// ListStorageTargets 列出仓库。
func (s *Store) ListStorageTargets(ctx context.Context) ([]StorageTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,kind,root,server_root,revision,created_at FROM storage_targets ORDER BY created_at`)
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
	target, err := scanStorage(s.db.QueryRowContext(ctx, `SELECT id,name,kind,root,server_root,revision,created_at FROM storage_targets WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return StorageTarget{}, ErrNotFound
	}
	return target, err
}

func scanStorage(row rowScanner) (StorageTarget, error) {
	var target StorageTarget
	var created string
	err := row.Scan(&target.ID, &target.Name, &target.Kind, &target.Root, &target.ServerRoot, &target.Revision, &created)
	if err != nil {
		return StorageTarget{}, err
	}
	target.CreatedAt, err = parseTime(created)
	return target, err
}
