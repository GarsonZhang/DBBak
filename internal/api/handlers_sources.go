package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"vault/internal/backup"
	"vault/internal/metadata"
)

var databaseNamePattern = regexp.MustCompile(`^[A-Za-z0-9_$-]{1,64}$`)

type sourceInput struct {
	Name     string  `json:"name"`
	Engine   string  `json:"engine"`
	Host     string  `json:"host"`
	Port     int     `json:"port"`
	Database string  `json:"database"`
	Username string  `json:"username"`
	Password *string `json:"password"`
	Revision int64   `json:"revision"`
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	sources, err := s.store.ListSources(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sources)
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	var input sourceInput
	if !decode(w, r, &input) {
		return
	}
	if input.Password == nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "PASSWORD_REQUIRED", "新建数据源必须提交密码；空密码可显式提交空字符串")
		return
	}
	if err := validateSource(input); err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
		return
	}
	ciphertext, err := s.keys.Encrypt(*input.Password)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	now := time.Now().UTC()
	source := metadata.Source{ID: metadata.NewID("src"), Name: strings.TrimSpace(input.Name), Engine: input.Engine, Host: strings.TrimSpace(input.Host), Port: input.Port, Database: input.Database, Username: strings.TrimSpace(input.Username), SecretRef: metadata.NewID("sec"), Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = s.store.CreateSource(r.Context(), source, ciphertext); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "source.create", source.ID, "success")
	writeJSON(w, http.StatusCreated, source)
}

func (s *Server) updateSource(w http.ResponseWriter, r *http.Request) {
	var input sourceInput
	if !decode(w, r, &input) {
		return
	}
	if input.Revision < 1 {
		s.fail(w, r, http.StatusUnprocessableEntity, "REVISION_REQUIRED", "更新必须携带当前 revision")
		return
	}
	if err := validateSource(input); err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
		return
	}
	source, err := s.store.GetSource(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if input.Engine != source.Engine {
		s.fail(w, r, http.StatusUnprocessableEntity, "ENGINE_CHANGE_REJECTED", "已有数据源不能修改引擎，请新建数据源")
		return
	}
	source.Name = strings.TrimSpace(input.Name)
	source.Host = strings.TrimSpace(input.Host)
	source.Port = input.Port
	source.Database = input.Database
	source.Username = strings.TrimSpace(input.Username)
	source.UpdatedAt = time.Now().UTC()
	var ciphertext string
	if input.Password != nil {
		if ciphertext, err = s.keys.Encrypt(*input.Password); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	if err = s.store.UpdateSource(r.Context(), source, ciphertext); err != nil {
		s.internalError(w, r, err)
		return
	}
	source, _ = s.store.GetSource(r.Context(), source.ID)
	s.audit(r, "source.update", source.ID, "success")
	writeJSON(w, http.StatusOK, source)
}

func (s *Server) probeSource(w http.ResponseWriter, r *http.Request) {
	source, err := s.store.GetSource(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	ciphertext, err := s.store.SourcePassword(r.Context(), source.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	password, err := s.keys.Decrypt(ciphertext)
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "SECRET_UNAVAILABLE", "凭据解密失败，请检查主密钥")
		return
	}
	adapter, err := s.adapters.ForEngine(source.Engine)
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "ADAPTER_UNIMPLEMENTED", err.Error())
		return
	}
	version, err := adapter.Probe(r.Context(), source, password)
	if err != nil {
		s.audit(r, "source.probe", source.ID, "failed")
		s.fail(w, r, http.StatusUnprocessableEntity, backup.ErrorCode(err), backup.SafeErrorMessage(err))
		return
	}
	s.audit(r, "source.probe", source.ID, "success")
	backupType := "logical_full"
	if source.Engine == "postgresql" {
		backupType = "physical_cluster_full"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "supported", "server_version": version, "method": source.Engine, "backup_type": backupType})
}

func validateServerRoot(root, serverRoot string) (string, error) {
	if serverRoot == "" {
		return "", nil
	}
	if !filepath.IsAbs(serverRoot) {
		return "", fmt.Errorf("server_root 必须是数据库服务可访问的绝对路径")
	}
	resolved, err := backup.PrepareRoot(serverRoot)
	if err != nil {
		return "", err
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	serverInfo, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !os.SameFile(rootInfo, serverInfo) {
		return "", fmt.Errorf("当前矩阵要求 server_root 与 root 指向同一实际目录；远程 SQL Server 路径映射未实现")
	}
	return resolved, nil
}

func validateSource(input sourceInput) error {
	if strings.TrimSpace(input.Name) == "" || len(input.Name) > 80 {
		return fmt.Errorf("名称必填且不超过 80 字符")
	}
	if input.Engine != "mysql" && input.Engine != "postgresql" && input.Engine != "sqlserver" {
		return fmt.Errorf("不支持的数据库引擎 %s", input.Engine)
	}
	host := strings.TrimSpace(input.Host)
	if host == "" || len(host) > 255 || strings.ContainsAny(host, " \t\r\n") {
		return fmt.Errorf("主机地址无效")
	}
	if input.Port < 1 || input.Port > 65535 {
		return fmt.Errorf("端口必须在 1 到 65535 之间")
	}
	if !databaseNamePattern.MatchString(input.Database) {
		return fmt.Errorf("数据库名或维护库名仅允许字母、数字、下划线、$ 和连字符，最长 64 字符")
	}
	if input.Engine != "sqlserver" && strings.TrimSpace(input.Username) == "" || len(input.Username) > 128 || strings.ContainsAny(input.Username, "\r\n\x00") {
		return fmt.Errorf("数据库用户名无效")
	}
	return nil
}

type storageInput struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Root       string `json:"root"`
	Revision   int64  `json:"revision"`
	ServerRoot string `json:"server_root"`
}

func (s *Server) listStorage(w http.ResponseWriter, r *http.Request) {
	targets, err := s.store.ListStorageTargets(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, targets)
}

func (s *Server) createStorage(w http.ResponseWriter, r *http.Request) {
	var input storageInput
	if !decode(w, r, &input) {
		return
	}
	if input.Kind != "local" || strings.TrimSpace(input.Name) == "" {
		s.fail(w, r, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "名称必填，阶段一存储类型仅支持 local")
		return
	}
	root, err := backup.PrepareRoot(input.Root)
	if err == nil {
		err = backup.CheckWritable(root)
	}
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "STORAGE_NOT_WRITABLE", err.Error())
		return
	}
	serverRoot, err := validateServerRoot(root, input.ServerRoot)
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "SERVER_PATH_REJECTED", err.Error())
		return
	}
	target := metadata.StorageTarget{ID: metadata.NewID("repo"), Name: strings.TrimSpace(input.Name), Kind: "local", Root: root, ServerRoot: serverRoot, Revision: 1, CreatedAt: time.Now().UTC()}
	if err = s.store.CreateStorageTarget(r.Context(), target); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "storage.create", target.ID, "success")
	writeJSON(w, http.StatusCreated, target)
}

func (s *Server) updateStorage(w http.ResponseWriter, r *http.Request) {
	var input storageInput
	if !decode(w, r, &input) {
		return
	}
	target, err := s.store.GetStorageTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if input.Revision < 1 || input.Kind != "local" || strings.TrimSpace(input.Name) == "" {
		s.fail(w, r, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "名称、local 类型和 revision 必填")
		return
	}
	root, err := backup.PrepareRoot(input.Root)
	if err == nil {
		err = backup.CheckWritable(root)
	}
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "STORAGE_NOT_WRITABLE", err.Error())
		return
	}
	serverRoot, err := validateServerRoot(root, input.ServerRoot)
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "SERVER_PATH_REJECTED", err.Error())
		return
	}
	target.Name = strings.TrimSpace(input.Name)
	target.Root = root
	target.ServerRoot = serverRoot
	target.Revision = input.Revision
	if err = s.store.UpdateStorageTarget(r.Context(), target); err != nil {
		s.internalError(w, r, err)
		return
	}
	target, _ = s.store.GetStorageTarget(r.Context(), target.ID)
	s.audit(r, "storage.update", target.ID, "success")
	writeJSON(w, http.StatusOK, target)
}

func (s *Server) checkStorage(w http.ResponseWriter, r *http.Request) {
	target, err := s.store.GetStorageTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if err = backup.CheckWritable(target.Root); err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "STORAGE_NOT_WRITABLE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "writable", "root": target.Root})
}
