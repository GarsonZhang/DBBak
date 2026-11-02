package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentRunOnceEmitsEventAndCompletes(t *testing.T) {
	protocol := &fakeProtocol{}
	protocol.pull = func(_ context.Context, request PullRequest) (PullResponse, error) {
		if len(request.Capabilities) != 1 || request.Capabilities[0] != "test" {
			t.Errorf("能力声明错误: %v", request.Capabilities)
		}
		return PullResponse{Task: &Task{RunID: "run-1", Attempt: 1, Fence: 2}}, nil
	}
	var eventRequests []EventRequest
	protocol.event = func(_ context.Context, _ string, request EventRequest) (EventResponse, error) {
		eventRequests = append(eventRequests, request)
		return EventResponse{Accepted: true}, nil
	}
	var completion *CompleteRequest
	protocol.complete = func(_ context.Context, _ string, request CompleteRequest) error {
		completion = &request
		return nil
	}
	executor := ExecutorFunc(func(_ context.Context, task Task, emit func(Event) error) Completion {
		if task.RunID != "run-1" {
			t.Errorf("任务未透传: %+v", task)
		}
		if err := emit(Event{ID: "started", Type: "state"}); err != nil {
			t.Errorf("发送事件失败: %v", err)
		}
		return Completion{Status: StatusSucceeded, Manifest: []byte(`{"files":1}`)}
	})
	agent, err := NewAgent(protocol, executor, AgentConfig{HeartbeatInterval: time.Hour, Capabilities: []string{"test"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = agent.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(eventRequests) != 1 || completion == nil || completion.Result.Status != StatusSucceeded {
		t.Fatalf("事件或完成协议缺失: events=%d completion=%+v", len(eventRequests), completion)
	}
	if eventRequests[0].Sequence != 1 || completion.Sequence != 2 {
		t.Fatalf("运行内序号未递增: event=%d complete=%d", eventRequests[0].Sequence, completion.Sequence)
	}
}

func TestAgentCancellationConfirmsOnlyAfterExecutorStops(t *testing.T) {
	protocol := &fakeProtocol{}
	protocol.pull = func(_ context.Context, _ PullRequest) (PullResponse, error) {
		return PullResponse{Task: &Task{RunID: "run-cancel", Attempt: 1, Fence: 1}}, nil
	}
	protocol.heartbeat = func(_ context.Context, _ string, _ HeartbeatRequest) (HeartbeatResponse, error) {
		return HeartbeatResponse{Cancel: true}, nil
	}
	var completedStatus string
	protocol.complete = func(_ context.Context, _ string, request CompleteRequest) error {
		completedStatus = request.Result.Status
		return nil
	}
	executorStopped := atomic.Bool{}
	executor := ExecutorFunc(func(ctx context.Context, _ Task, _ func(Event) error) Completion {
		select {
		case <-ctx.Done():
			executorStopped.Store(true)
			return Completion{Status: StatusFailed, ErrorCode: "STOPPED"}
		case <-time.After(2 * time.Second):
			return Completion{Status: StatusFailed, ErrorCode: "DID_NOT_STOP"}
		}
	})
	agent, err := NewAgent(protocol, executor, AgentConfig{HeartbeatInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err = agent.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !executorStopped.Load() || completedStatus != StatusCancelled {
		t.Fatalf("取消必须在执行器停止后确认: stopped=%v status=%q", executorStopped.Load(), completedStatus)
	}
}

func TestAgentDoesNotCompleteAfterHeartbeatFailure(t *testing.T) {
	protocol := &fakeProtocol{}
	protocol.pull = func(_ context.Context, _ PullRequest) (PullResponse, error) {
		return PullResponse{Task: &Task{RunID: "run-error", Attempt: 1, Fence: 1}}, nil
	}
	protocol.heartbeat = func(_ context.Context, _ string, _ HeartbeatRequest) (HeartbeatResponse, error) {
		return HeartbeatResponse{}, errors.New("heartbeat unavailable")
	}
	var completions atomic.Int32
	protocol.complete = func(_ context.Context, _ string, _ CompleteRequest) error {
		completions.Add(1)
		return nil
	}
	executor := ExecutorFunc(func(ctx context.Context, _ Task, _ func(Event) error) Completion {
		<-ctx.Done()
		return Completion{Status: StatusSucceeded}
	})
	agent, err := NewAgent(protocol, executor, AgentConfig{HeartbeatInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err = agent.RunOnce(context.Background()); err == nil {
		t.Fatal("心跳失败必须使运行返回错误")
	}
	if completions.Load() != 0 {
		t.Fatal("心跳结果未知时不得提交成功完成")
	}
}
