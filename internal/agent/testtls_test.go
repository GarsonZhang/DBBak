package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testPKI struct {
	caCertificate *x509.Certificate
	caPrivateKey  *ecdsa.PrivateKey
	caFile        string
	server        tls.Certificate
	clientCert    string
	clientKey     string
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	directory := t.TempDir()
	caKey := newTestKey(t)
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Vault Test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caFile := filepath.Join(directory, "ca.crt")
	writeTestFile(t, caFile, caPEM)

	serverKey := newTestKey(t)
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCertificate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverCertificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		encodeTestPrivateKey(t, serverKey),
	)
	if err != nil {
		t.Fatal(err)
	}
	clientKey := newTestKey(t)
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "agent-1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caCertificate, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientCert := filepath.Join(directory, "client.crt")
	clientKeyFile := filepath.Join(directory, "client.key")
	writeTestFile(t, clientCert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}))
	writeTestFile(t, clientKeyFile, encodeTestPrivateKey(t, clientKey))
	return &testPKI{
		caCertificate: caCertificate, caPrivateKey: caKey, caFile: caFile,
		server: serverCertificate, clientCert: clientCert, clientKey: clientKeyFile,
	}
}

func (pki *testPKI) startServer(t *testing.T, handler http.Handler, requireClientCertificate bool) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	clientAuth := tls.VerifyClientCertIfGiven
	if requireClientCertificate {
		clientAuth = tls.RequireAndVerifyClientCert
	}
	pool := x509.NewCertPool()
	pool.AddCert(pki.caCertificate)
	server.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pki.server},
		ClientCAs: pool, ClientAuth: clientAuth,
	}
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func (pki *testPKI) newClient(t *testing.T, serverURL string, retry RetryPolicy) *Client {
	t.Helper()
	client, err := NewClient(ClientConfig{
		ServerURL: serverURL, AgentID: "agent-1", CAFile: pki.caFile,
		CertificateFile: pki.clientCert, PrivateKeyFile: pki.clientKey,
		HTTPTimeout: 2 * time.Second, Retry: retry,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func fastRetry(attempts int) RetryPolicy {
	return RetryPolicy{MaxAttempts: attempts, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
}

func newTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func encodeTestPrivateKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
