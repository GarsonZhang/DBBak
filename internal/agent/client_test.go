package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientRequiresSecureConfiguration(t *testing.T) {
	pki := newTestPKI(t)
	valid := ClientConfig{
		ServerURL: "https://localhost", AgentID: "agent-1", CAFile: pki.caFile,
		CertificateFile: pki.clientCert, PrivateKeyFile: pki.clientKey,
	}
	tests := []struct {
		name   string
		mutate func(*ClientConfig)
	}{
		{name: "拒绝 HTTP", mutate: func(config *ClientConfig) { config.ServerURL = "http://localhost" }},
		{name: "必须 CA", mutate: func(config *ClientConfig) { config.CAFile = "" }},
		{name: "必须客户端证书", mutate: func(config *ClientConfig) { config.CertificateFile = "" }},
		{name: "必须客户端私钥", mutate: func(config *ClientConfig) { config.PrivateKeyFile = "" }},
		{name: "必须 Agent 身份", mutate: func(config *ClientConfig) { config.AgentID = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := NewClient(config); err == nil {
				t.Fatal("不安全或不完整配置必须失败")
			}
		})
	}
}

func TestClientMTLSProtocol(t *testing.T) {
	pki := newTestPKI(t)
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 || request.TLS.PeerCertificates[0].Subject.CommonName != "agent-1" {
			http.Error(response, "missing verified client identity", http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/agent/v1/leases/pull":
			var body PullRequest
			if !decodeTestJSON(t, response, request, &body) {
				return
			}
			if body.AgentID != "agent-1" || body.RequestID == "" {
				t.Errorf("拉取身份或请求 ID 缺失: %+v", body)
			}
			writeTestJSON(t, response, http.StatusOK, PullResponse{Task: &Task{RunID: "run-1", Attempt: 2, Fence: 3}})
		case "/agent/v1/leases/run-1/heartbeat":
			var body HeartbeatRequest
			if !decodeTestJSON(t, response, request, &body) {
				return
			}
			if body.AgentID != "agent-1" || body.Attempt != 2 || body.Fence != 3 || body.Sequence != 4 {
				t.Errorf("心跳协议字段错误: %+v", body)
			}
			writeTestJSON(t, response, http.StatusOK, HeartbeatResponse{LeaseUntil: time.Now().UTC(), Fence: 3})
		case "/agent/v1/runs/run-1/events":
			var body EventRequest
			if !decodeTestJSON(t, response, request, &body) {
				return
			}
			if body.Event.ID != "event-1" || body.Sequence != 5 || body.Fence != 3 {
				t.Errorf("事件协议字段错误: %+v", body)
			}
			writeTestJSON(t, response, http.StatusOK, EventResponse{Accepted: true})
		case "/agent/v1/runs/run-1/complete":
			var body CompleteRequest
			if !decodeTestJSON(t, response, request, &body) {
				return
			}
			if body.Result.Status != StatusSucceeded || body.Sequence != 6 {
				t.Errorf("完成协议字段错误: %+v", body)
			}
			response.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(response, request)
		}
	})
	server := pki.startServer(t, handler, true)
	client := pki.newClient(t, server.URL, fastRetry(1))
	pulled, err := client.Pull(context.Background(), PullRequest{Capabilities: []string{"mysql"}})
	if err != nil || pulled.Task == nil {
		t.Fatalf("mTLS 拉取失败: task=%+v err=%v", pulled.Task, err)
	}
	if _, err = client.Heartbeat(context.Background(), "run-1", HeartbeatRequest{Attempt: 2, Fence: 3, Sequence: 4, Progress: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.SendEvent(context.Background(), "run-1", EventRequest{
		Attempt: 2, Fence: 3, Sequence: 5, Event: Event{ID: "event-1", Type: "progress"},
	}); err != nil {
		t.Fatal(err)
	}
	if err = client.Complete(context.Background(), "run-1", CompleteRequest{
		Attempt: 2, Fence: 3, Sequence: 6, Result: Completion{Status: StatusSucceeded},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsUntrustedServer(t *testing.T) {
	clientPKI := newTestPKI(t)
	serverPKI := newTestPKI(t)
	var calls atomic.Int32
	server := serverPKI.startServer(t, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeTestJSON(t, response, http.StatusOK, PullResponse{})
	}), false)
	client := clientPKI.newClient(t, server.URL, fastRetry(1))
	if _, err := client.Pull(context.Background(), PullRequest{}); err == nil {
		t.Fatal("不受信任的服务端证书必须导致失败")
	}
	if calls.Load() != 0 {
		t.Fatalf("TLS 校验失败前不应到达应用处理器，实际 %d 次", calls.Load())
	}
}

func TestClientBoundedRetryAndPermanentError(t *testing.T) {
	t.Run("暂时错误后成功且请求 ID 不变", func(t *testing.T) {
		pki := newTestPKI(t)
		var attempts atomic.Int32
		var mutex sync.Mutex
		var requestIDs []string
		server := pki.startServer(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			var body PullRequest
			if !decodeTestJSON(t, response, request, &body) {
				return
			}
			mutex.Lock()
			requestIDs = append(requestIDs, body.RequestID)
			mutex.Unlock()
			if attempts.Add(1) < 3 {
				writeTestJSON(t, response, http.StatusServiceUnavailable, ProtocolError{Code: "TEMPORARY", Message: "temporary", Retryable: true})
				return
			}
			writeTestJSON(t, response, http.StatusOK, PullResponse{})
		}), true)
		client := pki.newClient(t, server.URL, fastRetry(3))
		if _, err := client.Pull(context.Background(), PullRequest{}); err != nil {
			t.Fatal(err)
		}
		if attempts.Load() != 3 {
			t.Fatalf("重试次数错误: %d", attempts.Load())
		}
		mutex.Lock()
		defer mutex.Unlock()
		if len(requestIDs) != 3 || requestIDs[0] == "" || requestIDs[0] != requestIDs[1] || requestIDs[1] != requestIDs[2] {
			t.Fatalf("重试请求 ID 必须稳定: %v", requestIDs)
		}
	})
	t.Run("永久错误不重试", func(t *testing.T) {
		pki := newTestPKI(t)
		var attempts atomic.Int32
		server := pki.startServer(t, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			writeTestJSON(t, response, http.StatusConflict, ProtocolError{Code: "STALE_FENCE", Message: "old fence"})
		}), true)
		client := pki.newClient(t, server.URL, fastRetry(3))
		_, err := client.Pull(context.Background(), PullRequest{})
		if !errors.Is(err, ErrStaleFence) {
			t.Fatalf("应保留旧 fence 错误，实际 %v", err)
		}
		if attempts.Load() != 1 {
			t.Fatalf("永久 HTTP 409 不应重试，实际 %d 次", attempts.Load())
		}
	})
	t.Run("重试耗尽可能返回明确失败", func(t *testing.T) {
		pki := newTestPKI(t)
		var attempts atomic.Int32
		server := pki.startServer(t, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			writeTestJSON(t, response, http.StatusServiceUnavailable, ProtocolError{Code: "UNAVAILABLE", Message: "down", Retryable: true})
		}), true)
		client := pki.newClient(t, server.URL, fastRetry(2))
		_, err := client.Pull(context.Background(), PullRequest{})
		if err == nil || !strings.Contains(err.Error(), "UNAVAILABLE") {
			t.Fatalf("耗尽重试应返回控制端错误，实际 %v", err)
		}
		if attempts.Load() != 2 {
			t.Fatalf("必须有界重试，实际 %d 次", attempts.Load())
		}
	})
}

func decodeTestJSON(t *testing.T, response http.ResponseWriter, request *http.Request, output any) bool {
	t.Helper()
	if err := json.NewDecoder(request.Body).Decode(output); err != nil {
		t.Errorf("解码请求失败: %v", err)
		http.Error(response, "bad json", http.StatusBadRequest)
		return false
	}
	return true
}

func writeTestJSON(t *testing.T, response http.ResponseWriter, status int, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Errorf("编码响应失败: %v", err)
	}
}
