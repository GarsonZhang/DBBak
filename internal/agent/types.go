// Package agent 实现 Vault 远程 Agent 的安全通信与有序上报协议。
// 本包不负责具体数据库备份，备份能力通过 Executor 接口注入。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	// StatusSucceeded 表示执行器真实完成且结果可提交。
	StatusSucceeded = "succeeded"
	// StatusFailed 表示执行器明确失败。
	StatusFailed = "failed"
	// StatusCancelled 表示执行器已经确认停止。
	StatusCancelled = "cancelled"
)

var (
	// ErrStaleFence 表示租约 fence 已过期，禁止继续上报。
	ErrStaleFence = errors.New("agent: 旧 fence 已拒绝")
	// ErrEventConflict 表示同一事件 ID 被用于不同内容。
	ErrEventConflict = errors.New("agent: 事件 ID 与已记录内容冲突")
	// ErrPendingReport 表示上一个请求结果未知，必须先重试该请求以保持顺序。
	ErrPendingReport = errors.New("agent: 存在结果未知的上报，必须先重试原请求")
	// ErrAlreadyCompleted 表示运行已经完成，不能再发送其他上报。
	ErrAlreadyCompleted = errors.New("agent: 运行已经完成")
)

// Task 是控制端租约返回的单个运行任务。Config 与 Access 保持不透明，
// 由后续执行适配器解释，通信层不会据此推断备份成功。
type Task struct {
	RunID   string          `json:"run_id"`
	Attempt uint64          `json:"attempt"`
	Fence   uint64          `json:"fence"`
	Config  json.RawMessage `json:"config,omitempty"`
	Access  json.RawMessage `json:"access,omitempty"`
}

// CancelInstruction 是控制端随拉取响应返回的取消指令。
type CancelInstruction struct {
	RunID   string `json:"run_id"`
	Attempt uint64 `json:"attempt"`
	Fence   uint64 `json:"fence"`
}

// PullRequest 描述一次主动拉取。RequestID 在同一次有界重试中保持不变。
type PullRequest struct {
	AgentID      string   `json:"agent_id"`
	RequestID    string   `json:"request_id"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// PullResponse 至多返回一个任务，空 Task 表示当前没有可领取任务。
type PullResponse struct {
	Task   *Task              `json:"task,omitempty"`
	Cancel *CancelInstruction `json:"cancel,omitempty"`
}

// HeartbeatRequest 延长指定租约并上报可选进度。Sequence 在同一 RunReporter 内递增。
type HeartbeatRequest struct {
	AgentID  string `json:"agent_id"`
	Attempt  uint64 `json:"attempt"`
	Fence    uint64 `json:"fence"`
	Sequence uint64 `json:"sequence"`
	Progress int    `json:"progress"`
	Message  string `json:"message,omitempty"`
}

// HeartbeatResponse 返回最新租约与取消状态；Fence 为 0 表示未变更。
type HeartbeatResponse struct {
	LeaseUntil time.Time `json:"lease_until"`
	Fence      uint64    `json:"fence,omitempty"`
	Cancel     bool      `json:"cancel"`
}

// Event 是一条具有稳定事件 ID 的结构化运行事件。
type Event struct {
	ID         string          `json:"event_id"`
	Type       string          `json:"type"`
	Level      string          `json:"level,omitempty"`
	Message    string          `json:"message,omitempty"`
	OccurredAt time.Time       `json:"occurred_at,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// EventRequest 将事件绑定到运行、attempt、fence 和递增序号。
type EventRequest struct {
	AgentID  string `json:"agent_id"`
	Attempt  uint64 `json:"attempt"`
	Fence    uint64 `json:"fence"`
	Sequence uint64 `json:"sequence"`
	Event    Event  `json:"event"`
}

// EventResponse 的 Duplicate 仅由控制端确认，客户端不会自行伪造服务端去重结果。
type EventResponse struct {
	Accepted  bool `json:"accepted"`
	Duplicate bool `json:"duplicate"`
}

// Completion 是执行器返回的最终结果；Manifest 必须来自真实执行结果。
type Completion struct {
	Status       string          `json:"status"`
	ErrorCode    string          `json:"error_code,omitempty"`
	ErrorMessage string          `json:"error_message,omitempty"`
	Manifest     json.RawMessage `json:"manifest,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
}

// CompleteRequest 是幂等完成请求，重试时沿用原 fence 与序号。
type CompleteRequest struct {
	AgentID  string     `json:"agent_id"`
	Attempt  uint64     `json:"attempt"`
	Fence    uint64     `json:"fence"`
	Sequence uint64     `json:"sequence"`
	Result   Completion `json:"result"`
}

// Protocol 是 Agent 通信接口，*Client 已实现，测试可注入内存实现。
type Protocol interface {
	Pull(context.Context, PullRequest) (PullResponse, error)
	Heartbeat(context.Context, string, HeartbeatRequest) (HeartbeatResponse, error)
	SendEvent(context.Context, string, EventRequest) (EventResponse, error)
	Complete(context.Context, string, CompleteRequest) error
}
