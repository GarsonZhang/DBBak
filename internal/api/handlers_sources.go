package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"dbbak/internal/backup"
	"dbbak/internal/metadata"
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
	// TrustServerCertificate 只有 SQL Server 使用；其他引擎提交该字段会被忽略，
	// 避免把无关的布尔值带到别的适配器上造成误解。
	TrustServerCertificate bool  `json:"trust_server_certificate"`
	Revision               int64 `json:"revision"`
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
	source := metadata.Source{ID: metadata.NewID("src"), Name: strings.TrimSpace(input.Name), Engine: input.Engine, Host: strings.TrimSpace(input.Host), Port: input.Port, Database: input.Database, Username: strings.TrimSpace(input.Username), TrustServerCertificate: input.Engine == "sqlserver" && input.TrustServerCertificate, SecretRef: metadata.NewID("sec"), Revision: 1, CreatedAt: now, UpdatedAt: now}
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
	source.TrustServerCertificate = input.Engine == "sqlserver" && input.TrustServerCertificate
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
	// route 可选：MySQL 有逻辑与物理两条路线，连接测试必须能分别验证，
	// 否则物理路线的前提（同机 datadir、版本对齐）在创建任务前无从确认。
	route := strings.TrimSpace(r.URL.Query().Get("route"))
	adapter, err := s.adapters.ForRoute(source.Engine, route)
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "ADAPTER_UNIMPLEMENTED", err.Error())
		return
	}
	version, err := adapter.Probe(r.Context(), source, password)
	if err != nil {
		s.audit(r, "source.probe", source.ID, "failed")
		// 工具路径、原始 stderr 只进服务日志：界面显示短句，排障时才需要这些细节。
		log.Printf("来源 %s 连接测试失败: %s", source.ID, backup.DiagnosticError(err))
		s.fail(w, r, http.StatusUnprocessableEntity, backup.ErrorCode(err), backup.SafeErrorMessage(err))
		return
	}
	s.audit(r, "source.probe", source.ID, "success")
	backupType := "logical_full"
	switch {
	case source.Engine == "postgresql":
		backupType = "physical_cluster_full"
	case source.Engine == "mysql" && route == "physical":
		backupType = "physical_full"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "supported", "server_version": version, "method": source.Engine, "route": route, "backup_type": backupType})
}

// sourceAdapterAndPassword 取出数据源的适配器与解密后的密码，供连接测试和能力查询共用。
//
// 密码只在调用栈内传递，不写日志、不写响应；解密失败必须显式失败，
// 不能拿空密码去连接——那会把「主密钥不可用」误报成「数据库认证失败」。
func (s *Server) sourceAdapterAndPassword(r *http.Request, source metadata.Source, route string) (backup.Adapter, string, error) {
	adapter, err := s.adapters.ForRoute(source.Engine, route)
	if err != nil {
		return nil, "", err
	}
	ciphertext, err := s.store.SourcePassword(r.Context(), source.ID)
	if err != nil {
		return nil, "", err
	}
	password, err := s.keys.Decrypt(ciphertext)
	if err != nil {
		return nil, "", fmt.Errorf("凭据解密失败，请检查主密钥")
	}
	return adapter, password, nil
}

// sourceCompressionCapabilities 返回某数据源可用的备份压缩选项。
//
// 界面在渲染任务表单时需要知道哪些选项可选，因此这里按数据源实时查询数据库实例：
// 压缩能力取决于 Edition（Express/Web 无备份压缩许可）与主版本（ZSTD 需要 2025 及以上），
// 任何静态结论都会在别的实例上变成假承诺。探测失败时返回 unverified，
// 前端据此禁用选项并显示原因，而不是让它先写进任务、等到备份时才报错。
func (s *Server) sourceCompressionCapabilities(w http.ResponseWriter, r *http.Request) {
	source, err := s.store.GetSource(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	response := map[string]any{
		"engine":       source.Engine,
		"supported":    false,
		"capabilities": map[string]backup.CompressionCapability{},
	}
	if source.Engine != "sqlserver" {
		// 其他引擎没有 BACKUP COMPRESSION 语义，压缩由各自工具参数控制；
		// 明确返回空能力集，避免界面渲染出一个对 MySQL 毫无意义的开关。
		writeJSON(w, http.StatusOK, response)
		return
	}
	response["supported"] = true
	adapter, password, err := s.sourceAdapterAndPassword(r, source, "")
	if err != nil {
		// 探测本身失败不应让界面崩掉：退化为全部 unverified，用户仍能看到原因。
		response["capabilities"] = backup.UnverifiedCompressionCapabilities("无法读取数据源凭据：" + err.Error())
		writeJSON(w, http.StatusOK, response)
		return
	}
	prober, ok := adapter.(backup.CompressionSupported)
	if !ok {
		response["capabilities"] = backup.UnverifiedCompressionCapabilities(source.Engine + " 适配器未实现压缩能力探测")
		writeJSON(w, http.StatusOK, response)
		return
	}
	capabilities, err := prober.CompressionCapabilities(r.Context(), source, password)
	if err != nil {
		log.Printf("数据源 %s 压缩能力探测失败: %s", source.ID, backup.DiagnosticError(err))
		message := backup.SafeErrorMessage(err)
		if message == "" {
			message = "无法连接数据库实例，不能确认压缩能力"
		}
		response["capabilities"] = backup.UnverifiedCompressionCapabilities(message)
		writeJSON(w, http.StatusOK, response)
		return
	}
	response["capabilities"] = capabilities
	// 实例默认压缩行为要展示给用户：选「不指定」时实际会不会压缩，取决于这个值，
	// 把它藏起来会让「不指定」变成一个结果不可预期的选项。
	if provider, ok := prober.(backup.CompressionDefaultProvider); ok {
		if value, defaultErr := provider.CompressionDefault(r.Context(), source, password); defaultErr == nil {
			response["compression_default"] = value
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// validateCompression 在保存任务时复核压缩选项，并把非 SQL Server 引擎的值清空。
//
// 只校验取值合法与引擎匹配，不在这里重新连接数据库实例：保存任务不应该因为
// 实例临时不可达而失败。真正的能力拦截发生在界面（选项不可选）和运行时的
// BACKUP 语句上——实例不支持时引擎自己会报错，不会产生虚假的成功记录。
func (s *Server) validateCompression(source metadata.Source, job *metadata.Job) error {
	if job.BackupCompression == "" {
		return nil
	}
	if source.Engine != "sqlserver" {
		job.BackupCompression = ""
		return nil
	}
	if !backup.KnownCompression(job.BackupCompression) {
		return fmt.Errorf("备份压缩选项必须为 off、on、zstd_low、zstd_medium、zstd_high 或留空")
	}
	return nil
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
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	Root     string  `json:"root"`
	Revision int64   `json:"revision"`
	ServerRoot string  `json:"server_root"`
	// FTP 专用：local/unc 必须留空。Password 在创建时必填、编辑时留空表示保留原凭据。
	Host     string  `json:"host"`
	Port     int     `json:"port"`
	Username string  `json:"username"`
	Password *string `json:"password"`
	Passive  *bool   `json:"passive"`
	// OSS/S3 专用：Endpoint 为服务地址，Bucket 为存储桶，Region 为签名地域；
	// Username 复用为 AccessKey ID，Password 复用为 AccessKey Secret。
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	Endpoint  string `json:"endpoint"`
	UseSSL    *bool  `json:"use_ssl"`
	PathStyle bool   `json:"path_style"`
}

func (s *Server) listStorage(w http.ResponseWriter, r *http.Request) {
	targets, err := s.store.ListStorageTargets(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, targets)
}

// validateStorageInput 校验存储位置输入并返回可持久化的目标与凭据密文。
// 返回的 ciphertext 为空表示沿用已有凭据（仅编辑场景）。
// 写检查是真实的：local/unc 落盘探测，FTP 走「上传-回读-删除」探测。
func (s *Server) validateStorageInput(ctx context.Context, input storageInput, existing *metadata.StorageTarget) (metadata.StorageTarget, string, error) {
	target := metadata.StorageTarget{
		Name: strings.TrimSpace(input.Name), Kind: input.Kind,
		Revision: input.Revision, CreatedAt: time.Now().UTC(),
	}
	if target.Name == "" || len(target.Name) > 80 {
		return target, "", fmt.Errorf("名称必填且不超过 80 字符")
	}
	switch target.Kind {
	case "local", "unc":
		if input.Host != "" || input.Port != 0 || input.Username != "" || (input.Password != nil && *input.Password != "") {
			return target, "", fmt.Errorf("本地目录与共享目录不使用 FTP 连接参数")
		}
		if input.Bucket != "" || input.Region != "" || input.Endpoint != "" || input.PathStyle {
			return target, "", fmt.Errorf("本地目录与共享目录不使用对象存储参数")
		}
		root, err := backup.PrepareRoot(input.Root)
		if err == nil {
			err = backup.CheckWritable(root)
		}
		if err != nil {
			return target, "", fmt.Errorf("根目录真实写检查失败: %w", err)
		}
		serverRoot, err := validateServerRoot(root, input.ServerRoot)
		if err != nil {
			return target, "", fmt.Errorf("服务端路径无效: %w", err)
		}
		target.Root, target.ServerRoot, target.Passive = root, serverRoot, true
	case "ftp":
		if input.ServerRoot != "" {
			return target, "", fmt.Errorf("FTP 存储不使用 SQL Server 服务端路径")
		}
		if input.Bucket != "" || input.Region != "" || input.Endpoint != "" || input.PathStyle {
			return target, "", fmt.Errorf("FTP 存储不使用对象存储参数")
		}
		if strings.TrimSpace(input.Host) == "" {
			return target, "", fmt.Errorf("FTP 主机必填")
		}
		port := input.Port
		if port == 0 {
			port = 21
		}
		if port < 1 || port > 65535 {
			return target, "", fmt.Errorf("FTP 端口必须在 1~65535")
		}
		root := strings.TrimSpace(input.Root)
		if !strings.HasPrefix(root, "/") {
			return target, "", fmt.Errorf("FTP 远程根目录必须是以 / 开头的绝对路径（如 /backups）")
		}
		target.Root = strings.TrimSuffix(root, "/")
		target.Host = strings.TrimSpace(input.Host)
		target.Port = port
		target.Username = strings.TrimSpace(input.Username)
		target.Passive = input.Passive == nil || *input.Passive
		password := ""
		if input.Password != nil {
			password = *input.Password
		}
		if password == "" && existing != nil && existing.SecretRef != "" {
			// 编辑且未填新密码：沿用旧凭据做写检查。
			cipher, err := s.store.StoragePassword(ctx, existing.ID)
			if err != nil {
				return target, "", fmt.Errorf("读取原 FTP 凭据: %w", err)
			}
			password, err = s.keys.Decrypt(cipher)
			if err != nil {
				return target, "", fmt.Errorf("解密原 FTP 凭据: %w", err)
			}
		} else if password == "" {
			return target, "", fmt.Errorf("FTP 密码必填（匿名账号可填任意非空值）")
		}
		if err := backup.FTPWriteTest(target, password); err != nil {
			return target, "", fmt.Errorf("FTP 连接或写检查失败: %w", err)
		}
		if input.Password != nil && *input.Password != "" {
			cipher, err := s.keys.Encrypt(*input.Password)
			if err != nil {
				return target, "", fmt.Errorf("加密存储凭据: %w", err)
			}
			return target, cipher, nil
		}
	case "oss":
		// 对象存储（S3 兼容，含阿里云 OSS）。保存时同样做真实写检查：
		// 写探测对象 → 读回长度 → 删除，缺任一权限都在这里失败。
		if input.ServerRoot != "" {
			return target, "", fmt.Errorf("对象存储不使用 SQL Server 服务端路径")
		}
		if input.Host != "" || input.Port != 0 {
			return target, "", fmt.Errorf("对象存储不需要 FTP 主机与端口；端点请填在「服务端点」")
		}
		if strings.TrimSpace(input.Bucket) == "" {
			return target, "", fmt.Errorf("对象存储桶（Bucket）必填")
		}
		endpoint := strings.TrimSpace(input.Endpoint)
		if endpoint == "" {
			return target, "", fmt.Errorf("对象存储端点必填（阿里云 OSS 形如 oss-cn-hangzhou.aliyuncs.com）")
		}
		if err := validateOSSEndpoint(endpoint); err != nil {
			return target, "", err
		}
		accessKey := strings.TrimSpace(input.Username)
		if accessKey == "" {
			return target, "", fmt.Errorf("AccessKey ID 必填")
		}
		target.Root = normalizeOSSPrefixInput(input.Root)
		target.Bucket = strings.TrimSpace(input.Bucket)
		target.Region = strings.TrimSpace(input.Region)
		target.Endpoint = stripEndpointScheme(endpoint)
		target.Username = accessKey
		target.UseSSL = input.UseSSL == nil || *input.UseSSL
		target.PathStyle = input.PathStyle
		target.Passive = true
		secret := ""
		if input.Password != nil {
			secret = *input.Password
		}
		if secret == "" && existing != nil && existing.SecretRef != "" {
			cipher, err := s.store.StoragePassword(ctx, existing.ID)
			if err != nil {
				return target, "", fmt.Errorf("读取原 AccessKey Secret: %w", err)
			}
			secret, err = s.keys.Decrypt(cipher)
			if err != nil {
				return target, "", fmt.Errorf("解密原 AccessKey Secret: %w", err)
			}
		} else if secret == "" {
			return target, "", fmt.Errorf("AccessKey Secret 必填")
		}
		if err := backup.OSSWriteTest(target, secret); err != nil {
			return target, "", fmt.Errorf("对象存储写检查失败: %w", err)
		}
		if input.Password != nil && *input.Password != "" {
			cipher, err := s.keys.Encrypt(*input.Password)
			if err != nil {
				return target, "", fmt.Errorf("加密存储凭据: %w", err)
			}
			return target, cipher, nil
		}
	default:
		return target, "", fmt.Errorf("存储类型必须为 local、unc、ftp 或 oss")
	}
	return target, "", nil
}

// normalizeOSSPrefixInput 把用户填写的前缀统一成「不含首尾斜杠的相对路径」。
// 对象存储没有目录概念，允许留空表示直接写在桶根。
func normalizeOSSPrefixInput(raw string) string {
	return strings.Trim(strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/"), "/")
}

// stripEndpointScheme 去掉用户可能粘贴进来的 http:// / https:// 前缀，
// 协议由 UseSSL 单独控制，避免两处配置互相矛盾。
func stripEndpointScheme(endpoint string) string {
	lower := strings.ToLower(endpoint)
	switch {
	case strings.HasPrefix(lower, "https://"):
		return endpoint[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		return endpoint[len("http://"):]
	}
	return endpoint
}

// validateOSSEndpoint 拒绝带路径、带查询串或带空格的端点，
// 这类输入通常是把完整对象 URL 误贴进来，放行只会得到一个更难排查的签名错误。
func validateOSSEndpoint(endpoint string) error {
	trimmed := stripEndpointScheme(strings.TrimSpace(endpoint))
	if trimmed == "" {
		return fmt.Errorf("对象存储端点不能为空")
	}
	if strings.ContainsAny(trimmed, " \t\r\n/?") {
		return fmt.Errorf("对象存储端点只填主机名（可带端口），不要带路径、参数或空格；如 oss-cn-hangzhou.aliyuncs.com")
	}
	if strings.Contains(trimmed, "/") {
		return fmt.Errorf("对象存储端点不能包含路径；桶与前缀请分别填写")
	}
	return nil
}

func (s *Server) createStorage(w http.ResponseWriter, r *http.Request) {
	var input storageInput
	if !decode(w, r, &input) {
		return
	}
	if input.Kind == "" {
		input.Kind = "local"
	}
	target, cipher, err := s.validateStorageInput(r.Context(), input, nil)
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "STORAGE_NOT_WRITABLE", err.Error())
		return
	}
	target.ID = metadata.NewID("repo")
	target.Revision = 1
	if cipher != "" {
		ref := metadata.NewID("sec")
		if err = s.store.CreateSecret(r.Context(), ref, cipher); err != nil {
			s.internalError(w, r, err)
			return
		}
		target.SecretRef = ref
	}
	if err = s.store.CreateStorageTarget(r.Context(), target); err != nil {
		s.internalError(w, r, err)
		return
	}
	target.SecretRef = ""
	s.audit(r, "storage.create", target.ID, "success")
	writeJSON(w, http.StatusCreated, target)
}

func (s *Server) updateStorage(w http.ResponseWriter, r *http.Request) {
	var input storageInput
	if !decode(w, r, &input) {
		return
	}
	existing, err := s.store.GetStorageTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if input.Revision < 1 {
		s.fail(w, r, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "revision 必填")
		return
	}
	target, cipher, err := s.validateStorageInput(r.Context(), input, &existing)
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "STORAGE_NOT_WRITABLE", err.Error())
		return
	}
	target.ID = existing.ID
	target.CreatedAt = existing.CreatedAt
	if cipher != "" {
		if existing.SecretRef != "" {
			if err = s.store.UpdateSecret(r.Context(), existing.SecretRef, cipher); err != nil {
				s.internalError(w, r, err)
				return
			}
			target.SecretRef = existing.SecretRef
		} else {
			ref := metadata.NewID("sec")
			if err = s.store.CreateSecret(r.Context(), ref, cipher); err != nil {
				s.internalError(w, r, err)
				return
			}
			target.SecretRef = ref
		}
	} else {
		target.SecretRef = existing.SecretRef
	}
	if err = s.store.UpdateStorageTarget(r.Context(), target); err != nil {
		if errors.Is(err, metadata.ErrConflict) {
			s.fail(w, r, http.StatusConflict, "STORAGE_IN_USE", err.Error())
			return
		}
		s.internalError(w, r, err)
		return
	}
	target, _ = s.store.GetStorageTarget(r.Context(), target.ID)
	s.audit(r, "storage.update", target.ID, "success")
	writeJSON(w, http.StatusOK, target)
}

// storageRemovalPreview 描述删除存储位置前的范围与阻塞项。
// 存储位置本身不持有文件：它的根目录可能被多个任务共用，也可能放着手工文件，
// 因此删除位置只解除登记，绝不碰磁盘内容，这一点必须让调用方明确知道。
type storageRemovalPreview struct {
	ID        string                    `json:"id"`
	Name      string                    `json:"name"`
	Root      string                    `json:"root"`
	Usage     metadata.StorageTargetUsage `json:"usage"`
	Blockers  []backup.RemovalBlocker   `json:"blockers"`
	Warnings  []string                  `json:"warnings"`
	Deletable bool                      `json:"deletable"`
}

// storageRemovalPlan 由服务端重新评估，预览与执行共用同一判定，避免前端自行推算范围。
func (s *Server) storageRemovalPlan(r *http.Request, id string) (storageRemovalPreview, error) {
	target, err := s.store.GetStorageTarget(r.Context(), id)
	if err != nil {
		return storageRemovalPreview{}, err
	}
	usage, err := s.store.StorageTargetUsage(r.Context(), id)
	if err != nil {
		return storageRemovalPreview{}, err
	}
	plan := storageRemovalPreview{
		ID: target.ID, Name: target.Name, Root: target.Root, Usage: usage,
		Blockers: []backup.RemovalBlocker{}, Warnings: []string{},
	}
	if usage.Jobs > 0 {
		plan.Blockers = append(plan.Blockers, backup.RemovalBlocker{
			Code: "STORAGE_IN_USE_BY_JOB", Message: fmt.Sprintf("%d 个备份任务正在使用该存储位置", usage.Jobs),
			Remediation: "在「备份任务」中修改这些任务的输出存储位置，或先删除任务",
		})
	}
	if usage.Artifacts > 0 {
		plan.Blockers = append(plan.Blockers, backup.RemovalBlocker{
			Code: "STORAGE_HAS_ARTIFACTS", Message: fmt.Sprintf("仍有 %d 个备份对象登记在该存储位置", usage.Artifacts),
			Remediation: "先删除对应备份文件（任务删除或「保留与清理」），使对象记录清理完毕",
		})
	}
	// 根目录如果被其他位置或任务共用，删掉登记不会动文件，但要提醒文件仍会留在磁盘上。
	others := 0
	if targets, listErr := s.store.ListStorageTargets(r.Context()); listErr == nil {
		for _, item := range targets {
			if item.ID != target.ID && strings.EqualFold(filepath.Clean(item.Root), filepath.Clean(target.Root)) {
				others++
			}
		}
	}
	if others > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("另有 %d 个存储位置共用同一根目录，删除后它们的登记与文件不受影响", others))
	}
	plan.Warnings = append(plan.Warnings, "仅解除登记：不会删除磁盘上的任何文件或目录，如需回收空间请另行手动清理")
	plan.Deletable = len(plan.Blockers) == 0
	return plan, nil
}

func (s *Server) previewStorageRemoval(w http.ResponseWriter, r *http.Request) {
	plan, err := s.storageRemovalPlan(r, r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// deleteStorage 解除一个存储位置的登记。用确认名称防止误点：名称由服务端重新读取并比对。
func (s *Server) deleteStorage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	plan, err := s.storageRemovalPlan(r, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !plan.Deletable {
		messages := make([]string, 0, len(plan.Blockers))
		for _, item := range plan.Blockers {
			messages = append(messages, item.Message+"（"+item.Remediation+"）")
		}
		s.fail(w, r, http.StatusConflict, "REMOVAL_BLOCKED", strings.Join(messages, "；"))
		return
	}
	if confirm := removalConfirm(r); confirm != plan.Name {
		s.fail(w, r, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "用于确认的名称与存储位置不一致")
		return
	}
	if err = s.store.DeleteStorageTarget(r.Context(), id, actor(r.Context())); err != nil {
		switch {
		case errors.Is(err, metadata.ErrConflict):
			s.fail(w, r, http.StatusConflict, "STORAGE_IN_USE", err.Error())
		case errors.Is(err, metadata.ErrNotFound):
			s.internalError(w, r, err)
		default:
			s.audit(r, "storage.delete", id, "failed")
			s.internalError(w, r, err)
		}
		return
	}
	s.audit(r, "storage.delete", id, "success")
	writeJSON(w, http.StatusOK, map[string]any{
		"id": plan.ID, "name": plan.Name, "root": plan.Root, "deleted": true,
		"message": "存储位置登记已解除，磁盘文件未做任何改动",
	})
}

func (s *Server) checkStorage(w http.ResponseWriter, r *http.Request) {
	target, err := s.store.GetStorageTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if target.Kind == "ftp" {
		cipher, secretErr := s.store.StoragePassword(r.Context(), target.ID)
		if secretErr != nil {
			s.fail(w, r, http.StatusUnprocessableEntity, "STORAGE_NOT_WRITABLE", "读取存储凭据失败: "+secretErr.Error())
			return
		}
		password, decryptErr := s.keys.Decrypt(cipher)
		if decryptErr != nil {
			s.fail(w, r, http.StatusUnprocessableEntity, "STORAGE_NOT_WRITABLE", "存储凭据解密失败，请检查独立主密钥")
			return
		}
		if err = backup.FTPWriteTest(target, password); err != nil {
			s.fail(w, r, http.StatusUnprocessableEntity, "STORAGE_NOT_WRITABLE", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "writable", "root": target.Root, "kind": "ftp"})
		return
	}
	if target.Kind == "oss" {
		cipher, secretErr := s.store.StoragePassword(r.Context(), target.ID)
		if secretErr != nil {
			s.fail(w, r, http.StatusUnprocessableEntity, "STORAGE_NOT_WRITABLE", "读取存储凭据失败: "+secretErr.Error())
			return
		}
		secret, decryptErr := s.keys.Decrypt(cipher)
		if decryptErr != nil {
			s.fail(w, r, http.StatusUnprocessableEntity, "STORAGE_NOT_WRITABLE", "存储凭据解密失败，请检查独立主密钥")
			return
		}
		if err = backup.OSSWriteTest(target, secret); err != nil {
			s.fail(w, r, http.StatusUnprocessableEntity, "STORAGE_NOT_WRITABLE", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "writable", "root": target.Root, "kind": "oss", "bucket": target.Bucket})
		return
	}
	if err = backup.CheckWritable(target.Root); err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "STORAGE_NOT_WRITABLE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "writable", "root": target.Root})
}

// sourceRemovalPreview 描述删除数据源前的范围与阻塞项。
// 数据源持有加密凭据：删除后凭据一并清除，重新添加需重新录入密码，必须提前告知。
type sourceRemovalPreview struct {
	ID        string                    `json:"id"`
	Name      string                    `json:"name"`
	Engine    string                    `json:"engine"`
	Endpoint  string                    `json:"endpoint"`
	Database  string                    `json:"database"`
	Usage     metadata.SourceUsage      `json:"usage"`
	Blockers  []backup.RemovalBlocker   `json:"blockers"`
	Warnings  []string                  `json:"warnings"`
	Deletable bool                      `json:"deletable"`
}

// sourceRemovalPlan 由服务端统一评估，预览与执行共用同一判定。
func (s *Server) sourceRemovalPlan(r *http.Request, id string) (sourceRemovalPreview, error) {
	source, err := s.store.GetSource(r.Context(), id)
	if err != nil {
		return sourceRemovalPreview{}, err
	}
	usage, err := s.store.SourceUsage(r.Context(), id)
	if err != nil {
		return sourceRemovalPreview{}, err
	}
	plan := sourceRemovalPreview{
		ID: source.ID, Name: source.Name, Engine: source.Engine,
		Endpoint: fmt.Sprintf("%s:%d", source.Host, source.Port), Database: source.Database, Usage: usage,
		Blockers: []backup.RemovalBlocker{}, Warnings: []string{},
	}
	if usage.Jobs > 0 {
		plan.Blockers = append(plan.Blockers, backup.RemovalBlocker{
			Code: "SOURCE_IN_USE_BY_JOB", Message: fmt.Sprintf("%d 个备份任务正在使用该数据源", usage.Jobs),
			Remediation: "在「备份任务」中修改这些任务的数据库，或先删除任务",
		})
	}
	if usage.Artifacts > 0 {
		plan.Blockers = append(plan.Blockers, backup.RemovalBlocker{
			Code: "SOURCE_HAS_ARTIFACTS", Message: fmt.Sprintf("仍有 %d 个备份对象登记在该数据源下", usage.Artifacts),
			Remediation: "先删除对应任务的备份文件，使对象记录清理完毕",
		})
	}
	plan.Warnings = append(plan.Warnings,
		"仅解除登记：不会连接数据库，也不会改动数据库内的任何数据",
		"该数据源的加密凭据将一并删除且不可恢复，重新添加时需重新录入密码",
	)
	plan.Deletable = len(plan.Blockers) == 0
	return plan, nil
}

func (s *Server) previewSourceRemoval(w http.ResponseWriter, r *http.Request) {
	plan, err := s.sourceRemovalPlan(r, r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// deleteSource 解除数据源登记（含加密凭据）。确认名称由服务端重新读取并比对。
func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	plan, err := s.sourceRemovalPlan(r, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !plan.Deletable {
		messages := make([]string, 0, len(plan.Blockers))
		for _, item := range plan.Blockers {
			messages = append(messages, item.Message+"（"+item.Remediation+"）")
		}
		s.fail(w, r, http.StatusConflict, "REMOVAL_BLOCKED", strings.Join(messages, "；"))
		return
	}
	if confirm := removalConfirm(r); confirm != plan.Name {
		s.fail(w, r, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "用于确认的名称与数据源不一致")
		return
	}
	if err = s.store.DeleteSource(r.Context(), id, actor(r.Context())); err != nil {
		switch {
		case errors.Is(err, metadata.ErrConflict):
			s.fail(w, r, http.StatusConflict, "SOURCE_IN_USE", err.Error())
		case errors.Is(err, metadata.ErrNotFound):
			s.internalError(w, r, err)
		default:
			s.audit(r, "source.delete", id, "failed")
			s.internalError(w, r, err)
		}
		return
	}
	s.audit(r, "source.delete", id, "success")
	writeJSON(w, http.StatusOK, map[string]any{
		"id": plan.ID, "name": plan.Name, "deleted": true,
		"message": "数据源与加密凭据已删除；数据库本身未受任何影响",
	})
}
