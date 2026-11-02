package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound 表示请求资源不存在。
var ErrNotFound = errors.New("资源不存在")

// ErrConflict 表示唯一键、活动运行或版本冲突。
var ErrConflict = errors.New("资源状态冲突")

// HasUsers 判断是否仍需初始化管理员。
func (s *Store) HasUsers(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	return count > 0, err
}

// CreateUser 保存已哈希的管理员密码。
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (User, error) {
	user := User{ID: NewID("usr"), Username: username, CreatedAt: time.Now().UTC()}
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,created_at) VALUES(?,?,?,?)`, user.ID, user.Username, passwordHash, formatTime(user.CreatedAt))
	return user, err
}

// Authenticate 返回匹配用户名的用户与密码哈希；调用方负责 bcrypt 校验。
func (s *Store) Authenticate(ctx context.Context, username string) (User, string, error) {
	var user User
	var hash, created string
	err := s.db.QueryRowContext(ctx, `SELECT id,username,password_hash,created_at FROM users WHERE username=?`, username).Scan(&user.ID, &user.Username, &hash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", ErrNotFound
	}
	if err != nil {
		return User{}, "", err
	}
	user.CreatedAt, err = parseTime(created)
	return user, hash, err
}

// CreateSession 持久化会话摘要与过期时间。
func (s *Store) CreateSession(ctx context.Context, tokenHash, userID string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at,created_at) VALUES(?,?,?,?)`, tokenHash, userID, formatTime(expiresAt), formatTime(time.Now()))
	return err
}

// SessionUser 查询未过期会话对应的用户。
func (s *Store) SessionUser(ctx context.Context, tokenHash string, now time.Time) (User, error) {
	var user User
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.username,u.created_at FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>?`, tokenHash, formatTime(now)).Scan(&user.ID, &user.Username, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	user.CreatedAt, err = parseTime(created)
	return user, err
}

// DeleteSession 注销单个会话。
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

// Audit 追加不包含敏感载荷的审计事件。
func (s *Store) Audit(ctx context.Context, actor, action, target, result, requestID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_events(actor,action,target,result,request_id,created_at) VALUES(?,?,?,?,?,?)`, actor, action, target, result, requestID, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("写入审计: %w", err)
	}
	return nil
}
