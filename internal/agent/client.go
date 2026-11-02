package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const maxResponseBytes = 4 << 20

// RetryPolicy 限制单次协议请求的尝试次数和退避时间。
type RetryPolicy struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// ClientConfig 配置控制端地址和双向 TLS 身份。CAFile、CertificateFile、
// PrivateKeyFile 均为必填，不提供关闭证书验证的选项。
type ClientConfig struct {
	ServerURL       string
	AgentID         string
	CAFile          string
	CertificateFile string
	PrivateKeyFile  string
	HTTPTimeout     time.Duration
	Retry           RetryPolicy
}

// Client 是主动连接控制端的 HTTPS/mTLS 协议客户端。
type Client struct {
	baseURL string
	agentID string
	http    *http.Client
	retry   RetryPolicy
}

// ProtocolError 保留控制端的结构化错误，调用方可检查状态码和错误码。
type ProtocolError struct {
	StatusCode int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	RequestID  string `json:"request_id"`
	Retryable  bool   `json:"retryable"`
}

// Error 返回不包含请求正文的明确协议错误。
func (e *ProtocolError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("agent: 控制端返回 HTTP %d", e.StatusCode)
	}
	if e.RequestID != "" {
		return fmt.Sprintf("agent: 控制端返回 HTTP %d (%s, request_id=%s): %s", e.StatusCode, e.Code, e.RequestID, e.Message)
	}
	return fmt.Sprintf("agent: 控制端返回 HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
}

// Is 让 errors.Is(err, ErrStaleFence) 能识别控制端的标准旧租约错误。
func (e *ProtocolError) Is(target error) bool {
	return target == ErrStaleFence && (e.Code == "STALE_FENCE" || e.Code == "FENCE_STALE")
}

// NewClient 创建强制服务端证书校验和客户端证书身份的客户端。
func NewClient(cfg ClientConfig) (*Client, error) {
	baseURL, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.AgentID) == "" {
		return nil, errors.New("agent: AgentID 不能为空")
	}
	pool, err := loadCAPool(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	if cfg.CertificateFile == "" || cfg.PrivateKeyFile == "" {
		return nil, errors.New("agent: 客户端证书和私钥文件必须同时配置")
	}
	certificate, err := tls.LoadX509KeyPair(cfg.CertificateFile, cfg.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("agent: 加载客户端证书和私钥: %w", err)
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
	transport.TLSClientConfig = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      pool,
		Certificates: []tls.Certificate{certificate},
		// 不设置 InsecureSkipVerify，Go 默认必须验证服务端证书链和主机名。
	}
	return &Client{
		baseURL: baseURL,
		agentID: cfg.AgentID,
		http:    &http.Client{Transport: transport, Timeout: timeout},
		retry:   retry,
	}, nil
}

// Pull 主动领取一个任务；网络重试沿用同一 RequestID。
func (c *Client) Pull(ctx context.Context, request PullRequest) (PullResponse, error) {
	var response PullResponse
	request.AgentID = c.agentID
	if request.RequestID == "" {
		requestID, err := randomRequestID()
		if err != nil {
			return response, fmt.Errorf("agent: 生成拉取请求 ID: %w", err)
		}
		request.RequestID = requestID
	}
	if err := c.doJSON(ctx, http.MethodPost, "/agent/v1/leases/pull", request, &response, ""); err != nil {
		return PullResponse{}, fmt.Errorf("agent: 拉取任务: %w", err)
	}
	if response.Task != nil {
		if response.Task.RunID == "" || response.Task.Attempt == 0 || response.Task.Fence == 0 {
			return PullResponse{}, errors.New("agent: 拉取任务: 控制端返回无效 run_id、attempt 或 fence")
		}
	}
	return response, nil
}

// Heartbeat 延长 runID 对应租约并上报进度。
func (c *Client) Heartbeat(ctx context.Context, runID string, request HeartbeatRequest) (HeartbeatResponse, error) {
	var response HeartbeatResponse
	if err := validateReport(runID, request.Attempt, request.Fence, request.Sequence); err != nil {
		return response, err
	}
	if request.Progress < 0 || request.Progress > 100 {
		return response, errors.New("agent: 心跳进度必须在 0 到 100 之间")
	}
	request.AgentID = c.agentID
	path := "/agent/v1/leases/" + url.PathEscape(runID) + "/heartbeat"
	if err := c.doJSON(ctx, http.MethodPost, path, request, &response, ""); err != nil {
		return HeartbeatResponse{}, fmt.Errorf("agent: 心跳: %w", err)
	}
	return response, nil
}

// SendEvent 发送有序事件；相同请求的自动重试不会更改序号。
func (c *Client) SendEvent(ctx context.Context, runID string, request EventRequest) (EventResponse, error) {
	var response EventResponse
	if err := validateReport(runID, request.Attempt, request.Fence, request.Sequence); err != nil {
		return response, err
	}
	if request.Event.ID == "" || request.Event.Type == "" {
		return response, errors.New("agent: 事件 ID 和类型不能为空")
	}
	request.AgentID = c.agentID
	path := "/agent/v1/runs/" + url.PathEscape(runID) + "/events"
	if err := c.doJSON(ctx, http.MethodPost, path, request, &response, ""); err != nil {
		return EventResponse{}, fmt.Errorf("agent: 上报事件: %w", err)
	}
	return response, nil
}

// Complete 提交真实执行结果；只有收到控制端成功响应才返回 nil。
func (c *Client) Complete(ctx context.Context, runID string, request CompleteRequest) error {
	if err := validateReport(runID, request.Attempt, request.Fence, request.Sequence); err != nil {
		return err
	}
	switch request.Result.Status {
	case StatusSucceeded, StatusFailed, StatusCancelled:
	default:
		return errors.New("agent: 完成状态必须为 succeeded、failed 或 cancelled")
	}
	request.AgentID = c.agentID
	path := "/agent/v1/runs/" + url.PathEscape(runID) + "/complete"
	if err := c.doJSON(ctx, http.MethodPost, path, request, nil, ""); err != nil {
		return fmt.Errorf("agent: 提交完成: %w", err)
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, input, output any, token string) error {
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return fmt.Errorf("编码请求: %w", err)
		}
	}
	var lastErr error
	for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var responseBody []byte
		var status int
		responseBody, status, err = c.roundTrip(ctx, method, path, body, token)
		if err == nil {
			if status >= 200 && status < 300 {
				if output == nil || status == http.StatusNoContent || len(bytes.TrimSpace(responseBody)) == 0 {
					return nil
				}
				if decodeErr := json.Unmarshal(responseBody, output); decodeErr != nil {
					return fmt.Errorf("解码控制端成功响应: %w", decodeErr)
				}
				return nil
			}
			err = decodeProtocolError(status, responseBody)
		}
		lastErr = err
		if attempt == c.retry.MaxAttempts {
			return fmt.Errorf("请求在 %d 次尝试后失败: %w", attempt, err)
		}
		if !shouldRetry(err, status) {
			return err
		}
		if waitErr := waitBackoff(ctx, c.retry, attempt); waitErr != nil {
			return waitErr
		}
	}
	return fmt.Errorf("请求在 %d 次尝试后失败: %w", c.retry.MaxAttempts, lastErr)
}

func (c *Client) roundTrip(ctx context.Context, method, path string, body []byte, token string) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("创建请求: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("HTTPS 请求: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("读取控制端响应: %w", err)
	}
	if len(responseBody) > maxResponseBytes {
		return nil, response.StatusCode, errors.New("控制端响应超过 4 MiB 限制")
	}
	return responseBody, response.StatusCode, nil
}

func decodeProtocolError(status int, body []byte) error {
	protocolError := &ProtocolError{StatusCode: status}
	if len(bytes.TrimSpace(body)) != 0 && json.Unmarshal(body, protocolError) != nil {
		protocolError.Message = "控制端返回了非 JSON 错误响应"
	}
	if protocolError.Message == "" {
		protocolError.Message = http.StatusText(status)
	}
	return protocolError
}

func shouldRetry(err error, status int) bool {
	var protocolError *ProtocolError
	if errors.As(err, &protocolError) {
		if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
			return true
		}
		return protocolError.Retryable && status >= 500
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func waitBackoff(ctx context.Context, retry RetryPolicy, attempt int) error {
	delay := retry.InitialBackoff
	for index := 1; index < attempt; index++ {
		delay *= 2
		if delay >= retry.MaxBackoff {
			delay = retry.MaxBackoff
			break
		}
	}
	if delay > retry.MaxBackoff {
		delay = retry.MaxBackoff
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func normalizeRetry(retry RetryPolicy) (RetryPolicy, error) {
	if retry.MaxAttempts == 0 {
		retry.MaxAttempts = 3
	}
	if retry.InitialBackoff == 0 {
		retry.InitialBackoff = 100 * time.Millisecond
	}
	if retry.MaxBackoff == 0 {
		retry.MaxBackoff = 2 * time.Second
	}
	if retry.MaxAttempts < 1 || retry.MaxAttempts > 10 {
		return RetryPolicy{}, errors.New("agent: MaxAttempts 必须在 1 到 10 之间")
	}
	if retry.InitialBackoff < 0 || retry.MaxBackoff <= 0 || retry.InitialBackoff > retry.MaxBackoff {
		return RetryPolicy{}, errors.New("agent: 重试退避时间无效")
	}
	return retry, nil
}

func normalizeServerURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("agent: 控制端地址无效: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("agent: 控制端地址必须使用 https 并包含主机")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("agent: 控制端地址不能包含用户信息、查询参数或片段")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, errors.New("agent: 必须显式配置 CA 文件")
	}
	pemData, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agent: 读取 CA 文件: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, errors.New("agent: CA 文件不包含有效 PEM 证书")
	}
	return pool, nil
}

func validateReport(runID string, attempt, fence, sequence uint64) error {
	if runID == "" || attempt == 0 || fence == 0 || sequence == 0 {
		return errors.New("agent: run_id、attempt、fence 和 sequence 必须非零")
	}
	return nil
}

func randomRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
