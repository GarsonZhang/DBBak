package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnrollmentConfig 配置无需客户端证书的一次性注册通道；服务端 TLS 校验仍强制启用。
type EnrollmentConfig struct {
	ServerURL   string
	CAFile      string
	AgentName   string
	HTTPTimeout time.Duration
	Retry       RetryPolicy
}

// Enroller 使用短期注册令牌换取控制端签发的客户端证书。
type Enroller struct {
	client *Client
	name   string
}

// Identity 是注册得到的长期身份。私钥始终在 Agent 本地生成，令牌不属于身份内容。
type Identity struct {
	AgentID        string
	CertificatePEM []byte
	PrivateKeyPEM  []byte
}

// IdentityPaths 是 Identity.Save 使用的跨平台文件路径。
type IdentityPaths struct {
	AgentIDFile     string
	CertificateFile string
	PrivateKeyFile  string
}

// NewEnroller 创建注册客户端。注册只信任显式 CA，不会跳过证书验证。
func NewEnroller(cfg EnrollmentConfig) (*Enroller, error) {
	baseURL, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.AgentName) == "" {
		return nil, errors.New("agent: AgentName 不能为空")
	}
	pool, err := loadCAPool(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	retry, err := normalizeRetry(cfg.Retry)
	if err != nil {
		return nil, err
	}
	timeout := cfg.HTTPTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if timeout < 0 {
		return nil, errors.New("agent: HTTPTimeout 不能为负数")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	return &Enroller{
		name: cfg.AgentName,
		client: &Client{
			baseURL: baseURL,
			http:    &http.Client{Transport: transport, Timeout: timeout},
			retry:   retry,
		},
	}, nil
}

// Enroll 用一次性令牌和 CSR 换取身份。令牌仅作为本次 Authorization 请求头
// 使用，不写入 Identity、文件或错误；重复注册是否成功完全由控制端决定。
func (e *Enroller) Enroll(ctx context.Context, token string) (Identity, error) {
	if token == "" {
		return Identity{}, errors.New("agent: 一次性注册令牌不能为空")
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("agent: 生成注册私钥: %w", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: e.name},
	}, privateKey)
	if err != nil {
		return Identity{}, fmt.Errorf("agent: 生成注册 CSR: %w", err)
	}
	request := struct {
		CSRPEM string `json:"csr_pem"`
	}{CSRPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))}
	var response struct {
		AgentID        string `json:"agent_id"`
		CertificatePEM string `json:"certificate_pem"`
	}
	err = e.client.doJSON(ctx, http.MethodPost, "/agent/v1/enroll", request, &response, token)
	if err != nil {
		// 即使异常控制端在错误中回显令牌，也不把它传播到日志。
		message := strings.ReplaceAll(err.Error(), token, "[已隐藏]")
		return Identity{}, fmt.Errorf("agent: 一次性注册失败: %s", message)
	}
	if strings.TrimSpace(response.AgentID) == "" {
		return Identity{}, errors.New("agent: 注册响应缺少 agent_id")
	}
	certificatePEM := []byte(response.CertificatePEM)
	block, _ := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return Identity{}, errors.New("agent: 注册响应缺少有效 PEM 证书")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return Identity{}, fmt.Errorf("agent: 解析注册证书: %w", err)
	}
	if !privateKey.PublicKey.Equal(certificate.PublicKey) {
		return Identity{}, errors.New("agent: 注册证书与本地私钥不匹配")
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return Identity{}, fmt.Errorf("agent: 编码注册私钥: %w", err)
	}
	identity := Identity{
		AgentID:        response.AgentID,
		CertificatePEM: certificatePEM,
		PrivateKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER}),
	}
	if _, err = tls.X509KeyPair(identity.CertificatePEM, identity.PrivateKeyPEM); err != nil {
		return Identity{}, fmt.Errorf("agent: 校验注册身份: %w", err)
	}
	return identity, nil
}

// Save 将身份写入 dir，私钥权限请求为 0600。已有文件不会被覆盖；
// agent.id 最后写入，作为三个身份文件均已完整落盘的提交标记。
func (identity Identity) Save(dir string) (IdentityPaths, error) {
	if identity.AgentID == "" || len(identity.CertificatePEM) == 0 || len(identity.PrivateKeyPEM) == 0 {
		return IdentityPaths{}, errors.New("agent: 不能保存不完整身份")
	}
	if _, err := tls.X509KeyPair(identity.CertificatePEM, identity.PrivateKeyPEM); err != nil {
		return IdentityPaths{}, fmt.Errorf("agent: 保存前校验身份: %w", err)
	}
	if dir == "" {
		return IdentityPaths{}, errors.New("agent: 身份目录不能为空")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return IdentityPaths{}, fmt.Errorf("agent: 创建身份目录: %w", err)
	}
	paths := IdentityPaths{
		AgentIDFile:     filepath.Join(dir, "agent.id"),
		CertificateFile: filepath.Join(dir, "agent.crt"),
		PrivateKeyFile:  filepath.Join(dir, "agent.key"),
	}
	created := make([]string, 0, 3)
	rollback := func() {
		for _, path := range created {
			_ = os.Remove(path)
		}
	}
	if err := writeNewFile(paths.CertificateFile, identity.CertificatePEM, 0644); err != nil {
		return IdentityPaths{}, err
	}
	created = append(created, paths.CertificateFile)
	if err := writeNewFile(paths.PrivateKeyFile, identity.PrivateKeyPEM, 0600); err != nil {
		rollback()
		return IdentityPaths{}, err
	}
	created = append(created, paths.PrivateKeyFile)
	if err := writeNewFile(paths.AgentIDFile, []byte(identity.AgentID+"\n"), 0644); err != nil {
		rollback()
		return IdentityPaths{}, err
	}
	return paths, nil
}

func writeNewFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("agent: 创建身份文件 %s: %w", filepath.Base(path), err)
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err = file.Chmod(mode); err != nil {
		return fmt.Errorf("agent: 设置身份文件权限 %s: %w", filepath.Base(path), err)
	}
	if _, err = file.Write(data); err != nil {
		return fmt.Errorf("agent: 写入身份文件 %s: %w", filepath.Base(path), err)
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("agent: 同步身份文件 %s: %w", filepath.Base(path), err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("agent: 关闭身份文件 %s: %w", filepath.Base(path), err)
	}
	ok = true
	return nil
}
