// Package api 提供 Vault JSON API、认证和静态 Web 服务。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	vault "vault"
	"vault/internal/backup"
	"vault/internal/config"
	"vault/internal/metadata"
	"vault/internal/security"
)

type contextKey string

const userKey contextKey = "user"

// Server 聚合阶段一 API 所需的真实服务。
type Server struct {
	store    *metadata.Store
	keys     *security.KeyStore
	adapters *backup.Registry
	runner   *backup.Runner
	cleanup  *backup.CleanupService
	config   config.Config
	started  time.Time
	handler  http.Handler
}

// NewServer 注册 REST 路由和现有中文静态界面。
func NewServer(store *metadata.Store, keys *security.KeyStore, adapters *backup.Registry, runner *backup.Runner, cleanup *backup.CleanupService, cfg config.Config) (*Server, error) {
	s := &Server{store: store, keys: keys, adapters: adapters, runner: runner, cleanup: cleanup, config: cfg, started: time.Now().UTC()}
	static, err := fs.Sub(vault.StaticFS, "web")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", s.live)
	mux.HandleFunc("GET /health/ready", s.ready)
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("POST /api/v1/auth/logout", s.authenticated(s.logout))
	mux.HandleFunc("GET /api/v1/auth/me", s.authenticated(s.me))
	mux.HandleFunc("GET /api/v1/overview", s.authenticated(s.overview))
	mux.HandleFunc("GET /api/v1/capabilities", s.authenticated(s.capabilities))
	mux.HandleFunc("GET /api/v1/settings", s.authenticated(s.settings))
	mux.HandleFunc("GET /api/v1/sources", s.authenticated(s.listSources))
	mux.HandleFunc("POST /api/v1/sources", s.authenticated(s.createSource))
	mux.HandleFunc("PATCH /api/v1/sources/{id}", s.authenticated(s.updateSource))
	mux.HandleFunc("POST /api/v1/sources/{id}/probe", s.authenticated(s.probeSource))
	mux.HandleFunc("GET /api/v1/storage-targets", s.authenticated(s.listStorage))
	mux.HandleFunc("POST /api/v1/storage-targets", s.authenticated(s.createStorage))
	mux.HandleFunc("PATCH /api/v1/storage-targets/{id}", s.authenticated(s.updateStorage))
	mux.HandleFunc("POST /api/v1/storage-targets/{id}/check", s.authenticated(s.checkStorage))
	mux.HandleFunc("GET /api/v1/jobs", s.authenticated(s.listJobs))
	mux.HandleFunc("POST /api/v1/jobs", s.authenticated(s.createJob))
	mux.HandleFunc("PATCH /api/v1/jobs/{id}", s.authenticated(s.updateJob))
	mux.HandleFunc("POST /api/v1/jobs/{id}/runs", s.authenticated(s.triggerRun))
	mux.HandleFunc("POST /api/v1/jobs/{id}/cleanup-previews", s.authenticated(s.previewCleanup))
	mux.HandleFunc("GET /api/v1/runs", s.authenticated(s.listRuns))
	mux.HandleFunc("GET /api/v1/runs/{id}", s.authenticated(s.getRun))
	mux.HandleFunc("GET /api/v1/runs/{id}/logs", s.authenticated(s.getRunLogs))
	mux.HandleFunc("POST /api/v1/runs/{id}/restore-verification", s.authenticated(s.recordRestoreVerification))
	mux.HandleFunc("GET /api/v1/artifacts", s.authenticated(s.listArtifacts))
	mux.HandleFunc("PATCH /api/v1/artifacts/{id}/hold", s.authenticated(s.holdArtifact))
	mux.HandleFunc("POST /api/v1/cleanup-runs", s.authenticated(s.executeCleanup))
	mux.Handle("GET /", s.securityHeaders(http.FileServer(http.FS(static))))
	s.handler = s.requestID(mux)
	return s, nil
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := metadata.NewID("req")
		w.Header().Set("X-Request-ID", id)
		r.Header.Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, fromCookie := requestToken(r)
		if token == "" {
			s.fail(w, r, http.StatusUnauthorized, "AUTH_REQUIRED", "请先登录")
			return
		}
		user, err := s.store.SessionUser(r.Context(), security.TokenHash(token), time.Now().UTC())
		if err != nil {
			s.fail(w, r, http.StatusUnauthorized, "SESSION_INVALID", "登录已失效，请重新登录")
			return
		}
		if fromCookie && r.Method != http.MethodGet && r.Header.Get("X-Vault-Request") != "1" {
			s.fail(w, r, http.StatusForbidden, "CSRF_REJECTED", "缺少同源请求确认头")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	}
}

func requestToken(r *http.Request) (string, bool) {
	if value := r.Header.Get("Authorization"); strings.HasPrefix(value, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(value, "Bearer ")), false
	}
	cookie, err := r.Cookie("vault_session")
	if err != nil {
		return "", false
	}
	return cookie.Value, true
}

func actor(ctx context.Context) string {
	if user, ok := ctx.Value(userKey).(metadata.User); ok {
		return user.Username
	}
	return "anonymous"
}

func (s *Server) audit(r *http.Request, action, target, result string) {
	if err := s.store.Audit(r.Context(), actor(r.Context()), action, target, result, r.Header.Get("X-Request-ID")); err != nil {
		log.Printf("审计写入失败: %v", err)
	}
}

func decode(w http.ResponseWriter, r *http.Request, destination any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "请求 JSON 无效或包含未知字段")
		return false
	}
	if decoder.More() {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "请求体只能包含一个 JSON 对象")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"code": code, "message": message, "field_errors": map[string]string{}, "retryable": false})
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, map[string]any{"code": code, "message": message, "field_errors": map[string]string{}, "retryable": status >= 500, "request_id": r.Header.Get("X-Request-ID")})
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("请求失败 %s %s: %v", r.Method, r.URL.Path, err)
	switch {
	case errors.Is(err, metadata.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "NOT_FOUND", "请求资源不存在")
	case errors.Is(err, metadata.ErrConflict):
		s.fail(w, r, http.StatusConflict, "STATE_CONFLICT", err.Error())
	default:
		s.fail(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "服务端处理失败")
	}
}

func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "METADATA_UNAVAILABLE", "元数据库不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, r.Context().Value(userKey))
}

func (s *Server) settings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"data_dir": s.config.DataDir, "backup_root": s.config.BackupRoot,
		"mysql_path": s.config.MySQLPath, "mysqldump_path": s.config.MySQLDumpPath,
		"psql_path": s.config.PostgreSQLPath, "pg_basebackup_path": s.config.PGBaseBackupPath,
		"sqlcmd_path":     s.config.SQLCMDPath,
		"master_key_path": s.keys.KeyPath(), "backup_timeout": s.config.BackupTimeout.String(),
		"session_ttl": s.config.SessionTTL.String(), "started_at": s.started,
	})
}

func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	executorStatus := "supported"
	versions := map[string]string{}
	messages := []string{}
	capabilities := []map[string]string{}
	for _, engine := range s.adapters.Engines() {
		if engine == "sqlserver" {
			continue // 实现已注册，但真实恢复未验收，不得标为 supported。
		}
		adapter, _ := s.adapters.ForEngine(engine)
		version, err := adapter.Version(r.Context())
		status := "supported"
		method := map[string]string{"mysql": "mysqldump", "postgresql": "pg_basebackup"}[engine]
		if err != nil {
			status = "prerequisite_missing"
			executorStatus = status
			messages = append(messages, backup.SafeErrorMessage(err))
			version = ""
		}
		versions[engine] = version
		capabilities = append(capabilities, map[string]string{"engine": engine, "method": method, "type": "full", "status": status})
	}
	capabilities = append(capabilities,
		map[string]string{"engine": "mysql", "method": "binlog", "type": "log", "status": "unimplemented"},
		map[string]string{"engine": "postgresql", "method": "WAL", "type": "log", "status": "unimplemented"},
		map[string]string{"engine": "postgresql", "method": "pgBackRest", "type": "full", "status": "unimplemented"},
		map[string]string{"engine": "sqlserver", "method": "native", "type": "full", "status": "unverified"},
	)
	writeJSON(w, http.StatusOK, map[string]any{"local_executor": map[string]any{"status": executorStatus, "tool_versions": versions, "message": strings.Join(messages, "；")}, "capabilities": capabilities})
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ListJobs(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	runs, err := s.store.ListRuns(r.Context(), "", 100)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	artifacts, err := s.store.ListArtifacts(r.Context(), "")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var bytes int64
	verified := 0
	for _, artifact := range artifacts {
		if artifact.State == "verified" {
			bytes += artifact.Size
			verified++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"job_count": len(jobs), "recent_runs": runs[:min(8, len(runs))], "verified_artifacts": verified, "verified_bytes": bytes})
}

func validationError(message string) error {
	return fmt.Errorf("%w: %s", metadata.ErrConflict, message)
}
