package agent

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEnrollmentOneTimeTokenAndMTLSIdentity(t *testing.T) {
	pki := newTestPKI(t)
	const token = "one-time-secret-token"
	var mutex sync.Mutex
	used := false
	enrollCalls := 0
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/agent/v1/enroll":
			mutex.Lock()
			enrollCalls++
			alreadyUsed := used
			if request.Header.Get("Authorization") == "Bearer "+token {
				used = true
			}
			mutex.Unlock()
			if request.Header.Get("Authorization") != "Bearer "+token || alreadyUsed {
				writeTestJSON(t, response, http.StatusUnauthorized, ProtocolError{Code: "ENROLLMENT_TOKEN_USED", Message: "invalid token " + token})
				return
			}
			var body struct {
				CSRPEM string `json:"csr_pem"`
			}
			if !decodeTestJSON(t, response, request, &body) {
				return
			}
			block, _ := pem.Decode([]byte(body.CSRPEM))
			if block == nil {
				t.Error("注册请求缺少 CSR")
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			csr, err := x509.ParseCertificateRequest(block.Bytes)
			if err != nil || csr.CheckSignature() != nil {
				t.Errorf("CSR 无效: %v", err)
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			now := time.Now()
			template := &x509.Certificate{
				SerialNumber: big.NewInt(10), Subject: pkix.Name{CommonName: "agent-1"},
				NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
				KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			}
			certificateDER, err := x509.CreateCertificate(rand.Reader, template, pki.caCertificate, csr.PublicKey, pki.caPrivateKey)
			if err != nil {
				t.Errorf("签发证书失败: %v", err)
				response.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeTestJSON(t, response, http.StatusCreated, map[string]string{
				"agent_id": "agent-1", "certificate_pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})),
			})
		case "/agent/v1/leases/pull":
			if request.TLS == nil || len(request.TLS.PeerCertificates) != 1 {
				t.Error("注册后的协议请求未提供已验证客户端证书")
				response.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeTestJSON(t, response, http.StatusOK, PullResponse{})
		default:
			http.NotFound(response, request)
		}
	})
	server := pki.startServer(t, handler, false)
	enroller, err := NewEnroller(EnrollmentConfig{
		ServerURL: server.URL, CAFile: pki.caFile, AgentName: "agent-1", Retry: fastRetry(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := enroller.Enroll(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := identity.Save(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.AgentIDFile, paths.CertificateFile, paths.PrivateKeyFile} {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(content), token) {
			t.Fatalf("身份文件 %s 泄露了明文注册令牌", path)
		}
	}
	client, err := NewClient(ClientConfig{
		ServerURL: server.URL, AgentID: identity.AgentID, CAFile: pki.caFile,
		CertificateFile: paths.CertificateFile, PrivateKeyFile: paths.PrivateKeyFile, Retry: fastRetry(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Pull(context.Background(), PullRequest{}); err != nil {
		t.Fatalf("注册身份不能用于 mTLS: %v", err)
	}
	_, err = enroller.Enroll(context.Background(), token)
	if err == nil {
		t.Fatal("同一令牌第二次注册必须失败")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("注册错误泄露明文令牌: %v", err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if enrollCalls != 2 {
		t.Fatalf("注册请求次数错误: %d", enrollCalls)
	}
}
