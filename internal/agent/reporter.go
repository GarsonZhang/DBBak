package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

type pendingReport struct {
	kind        string
	key         string
	fingerprint []byte
	fence       uint64
	sequence    uint64
}

type acceptedEvent struct {
	fingerprint []byte
	response    EventResponse
}

// RunReporter 串行化一个运行的心跳、事件和完成请求，统一分配 fence 与递增序号。
// 网络结果未知时保留原请求，阻止其他请求越过它，避免重试乱序。
type RunReporter struct {
	protocol Protocol
	task     Task

	mu                    sync.Mutex
	fence                 uint64
	nextSequence          uint64
	pending               *pendingReport
	events                map[string]acceptedEvent
	completed             bool
	completionFingerprint []byte
}

// NewRunReporter 为一个有效租约创建有序上报器。
func NewRunReporter(protocol Protocol, task Task) (*RunReporter, error) {
	if protocol == nil {
		return nil, errors.New("agent: Protocol 不能为空")
	}
	if task.RunID == "" || task.Attempt == 0 || task.Fence == 0 {
		return nil, errors.New("agent: 任务 run_id、attempt 和 fence 必须非零")
	}
	return &RunReporter{
		protocol:     protocol,
		task:         task,
		fence:        task.Fence,
		nextSequence: 1,
		events:       make(map[string]acceptedEvent),
	}, nil
}

// Fence 返回当前上报使用的 fence。
func (reporter *RunReporter) Fence() uint64 {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	return reporter.fence
}

// AcceptFence 接受控制端下发的新 fence，明确拒绝旧 fence。结果未知的请求
// 尚未重试完成时不能切换 fence，调用方应停止旧租约或先完成原请求重试。
func (reporter *RunReporter) AcceptFence(fence uint64) error {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	return reporter.acceptFenceLocked(fence)
}

func (reporter *RunReporter) acceptFenceLocked(fence uint64) error {
	if fence < reporter.fence {
		return fmt.Errorf("%w: 当前=%d，收到=%d", ErrStaleFence, reporter.fence, fence)
	}
	if fence == reporter.fence {
		return nil
	}
	if reporter.pending != nil {
		return ErrPendingReport
	}
	reporter.fence = fence
	return nil
}

// Heartbeat 上报进度并接受控制端返回的新 fence。失败后应使用相同进度重试；
// 不同上报会返回 ErrPendingReport，而不是跳过结果未知的序号。
func (reporter *RunReporter) Heartbeat(ctx context.Context, progress int, message string) (HeartbeatResponse, error) {
	if progress < 0 || progress > 100 {
		return HeartbeatResponse{}, errors.New("agent: 心跳进度必须在 0 到 100 之间")
	}
	fingerprint, err := json.Marshal(struct {
		Progress int    `json:"progress"`
		Message  string `json:"message"`
	}{Progress: progress, Message: message})
	if err != nil {
		return HeartbeatResponse{}, err
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	pending, err := reporter.prepareLocked("heartbeat", "", fingerprint)
	if err != nil {
		return HeartbeatResponse{}, err
	}
	response, err := reporter.protocol.Heartbeat(ctx, reporter.task.RunID, HeartbeatRequest{
		Attempt: reporter.task.Attempt, Fence: pending.fence, Sequence: pending.sequence,
		Progress: progress, Message: message,
	})
	if err != nil {
		return HeartbeatResponse{}, err
	}
	reporter.finishPendingLocked()
	if response.Fence != 0 {
		if err = reporter.acceptFenceLocked(response.Fence); err != nil {
			return HeartbeatResponse{}, err
		}
	}
	return response, nil
}

// SendEvent 发送事件并按事件 ID 去重。只有控制端已确认的事件才会本地去重；
// 失败后的相同事件使用原 fence 和序号重试，不会伪造接受结果。
func (reporter *RunReporter) SendEvent(ctx context.Context, event Event) (EventResponse, error) {
	if event.ID == "" || event.Type == "" {
		return EventResponse{}, errors.New("agent: 事件 ID 和类型不能为空")
	}
	event = cloneEvent(event)
	fingerprint, err := json.Marshal(event)
	if err != nil {
		return EventResponse{}, fmt.Errorf("agent: 编码事件指纹: %w", err)
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if accepted, ok := reporter.events[event.ID]; ok {
		if !equalBytes(accepted.fingerprint, fingerprint) {
			return EventResponse{}, ErrEventConflict
		}
		return accepted.response, nil
	}
	pending, err := reporter.prepareLocked("event", event.ID, fingerprint)
	if err != nil {
		return EventResponse{}, err
	}
	response, err := reporter.protocol.SendEvent(ctx, reporter.task.RunID, EventRequest{
		Attempt: reporter.task.Attempt, Fence: pending.fence, Sequence: pending.sequence, Event: event,
	})
	if err != nil {
		return EventResponse{}, err
	}
	if !response.Accepted {
		return EventResponse{}, errors.New("agent: 控制端未确认接受事件")
	}
	reporter.events[event.ID] = acceptedEvent{fingerprint: fingerprint, response: response}
	reporter.finishPendingLocked()
	return response, nil
}

// Complete 幂等提交完成结果。已确认的相同结果可直接返回，不同结果或其他
// 后续上报会被拒绝；未收到控制端确认时绝不会标记本地完成。
func (reporter *RunReporter) Complete(ctx context.Context, result Completion) error {
	switch result.Status {
	case StatusSucceeded, StatusFailed, StatusCancelled:
	default:
		return errors.New("agent: 完成状态必须为 succeeded、failed 或 cancelled")
	}
	fingerprint, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("agent: 编码完成结果指纹: %w", err)
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if reporter.completed {
		if equalBytes(reporter.completionFingerprint, fingerprint) {
			return nil
		}
		return ErrAlreadyCompleted
	}
	pending, err := reporter.prepareLocked("complete", "", fingerprint)
	if err != nil {
		return err
	}
	if err = reporter.protocol.Complete(ctx, reporter.task.RunID, CompleteRequest{
		Attempt: reporter.task.Attempt, Fence: pending.fence, Sequence: pending.sequence, Result: result,
	}); err != nil {
		return err
	}
	reporter.completed = true
	reporter.completionFingerprint = fingerprint
	reporter.finishPendingLocked()
	return nil
}

func (reporter *RunReporter) prepareLocked(kind, key string, fingerprint []byte) (*pendingReport, error) {
	if reporter.completed {
		return nil, ErrAlreadyCompleted
	}
	if reporter.pending != nil {
		if reporter.pending.kind != kind || reporter.pending.key != key || !equalBytes(reporter.pending.fingerprint, fingerprint) {
			return nil, ErrPendingReport
		}
		return reporter.pending, nil
	}
	reporter.pending = &pendingReport{
		kind: kind, key: key, fingerprint: fingerprint,
		fence: reporter.fence, sequence: reporter.nextSequence,
	}
	return reporter.pending, nil
}

func (reporter *RunReporter) finishPendingLocked() {
	reporter.pending = nil
	reporter.nextSequence++
}

func cloneEvent(event Event) Event {
	if event.Data != nil {
		event.Data = append(json.RawMessage(nil), event.Data...)
	}
	return event
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
