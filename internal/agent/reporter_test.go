package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeProtocol struct {
	pull      func(context.Context, PullRequest) (PullResponse, error)
	heartbeat func(context.Context, string, HeartbeatRequest) (HeartbeatResponse, error)
	event     func(context.Context, string, EventRequest) (EventResponse, error)
	complete  func(context.Context, string, CompleteRequest) error
}

func (fake *fakeProtocol) Pull(ctx context.Context, request PullRequest) (PullResponse, error) {
	if fake.pull == nil {
		return PullResponse{}, nil
	}
	return fake.pull(ctx, request)
}

func (fake *fakeProtocol) Heartbeat(ctx context.Context, runID string, request HeartbeatRequest) (HeartbeatResponse, error) {
	if fake.heartbeat == nil {
		return HeartbeatResponse{}, nil
	}
	return fake.heartbeat(ctx, runID, request)
}

func (fake *fakeProtocol) SendEvent(ctx context.Context, runID string, request EventRequest) (EventResponse, error) {
	if fake.event == nil {
		return EventResponse{Accepted: true}, nil
	}
	return fake.event(ctx, runID, request)
}

func (fake *fakeProtocol) Complete(ctx context.Context, runID string, request CompleteRequest) error {
	if fake.complete == nil {
		return nil
	}
	return fake.complete(ctx, runID, request)
}

func TestRunReporterOrderingDeduplicationAndFence(t *testing.T) {
	var heartbeats []HeartbeatRequest
	var events []EventRequest
	var completions []CompleteRequest
	protocol := &fakeProtocol{}
	protocol.heartbeat = func(_ context.Context, _ string, request HeartbeatRequest) (HeartbeatResponse, error) {
		heartbeats = append(heartbeats, request)
		return HeartbeatResponse{Fence: 9}, nil
	}
	protocol.event = func(_ context.Context, _ string, request EventRequest) (EventResponse, error) {
		events = append(events, request)
		return EventResponse{Accepted: true}, nil
	}
	protocol.complete = func(_ context.Context, _ string, request CompleteRequest) error {
		completions = append(completions, request)
		return nil
	}
	reporter, err := NewRunReporter(protocol, Task{RunID: "run-1", Attempt: 1, Fence: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reporter.Heartbeat(context.Background(), 10, "running"); err != nil {
		t.Fatal(err)
	}
	event := Event{ID: "event-1", Type: "progress", Data: []byte(`{"percent":10}`)}
	if _, err = reporter.SendEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if _, err = reporter.SendEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("已确认重复事件不应再次发送，实际请求 %d 次", len(events))
	}
	conflicting := event
	conflicting.Message = "different"
	if _, err = reporter.SendEvent(context.Background(), conflicting); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("相同事件 ID 不同内容应冲突，实际 %v", err)
	}
	if err = reporter.AcceptFence(8); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("旧 fence 应被拒绝，实际 %v", err)
	}
	if err = reporter.Complete(context.Background(), Completion{Status: StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	if err = reporter.Complete(context.Background(), Completion{Status: StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	if len(completions) != 1 {
		t.Fatalf("已确认完成不应重复发送，实际 %d 次", len(completions))
	}
	if heartbeats[0].Sequence != 1 || heartbeats[0].Fence != 7 {
		t.Fatalf("心跳序号/fence 错误: %+v", heartbeats[0])
	}
	if events[0].Sequence != 2 || events[0].Fence != 9 {
		t.Fatalf("事件未递增序号或使用新 fence: %+v", events[0])
	}
	if completions[0].Sequence != 3 || completions[0].Fence != 9 {
		t.Fatalf("完成序号/fence 错误: %+v", completions[0])
	}
	if _, err = reporter.Heartbeat(context.Background(), 20, "running"); !errors.Is(err, ErrAlreadyCompleted) {
		t.Fatalf("完成后心跳应拒绝，实际 %v", err)
	}
}

func TestRunReporterRetriesOriginalRequest(t *testing.T) {
	attempts := 0
	var sequences []uint64
	protocol := &fakeProtocol{}
	protocol.event = func(_ context.Context, _ string, request EventRequest) (EventResponse, error) {
		attempts++
		sequences = append(sequences, request.Sequence)
		if attempts == 1 {
			return EventResponse{}, errors.New("连接中断，结果未知")
		}
		return EventResponse{Accepted: true}, nil
	}
	reporter, err := NewRunReporter(protocol, Task{RunID: "run-1", Attempt: 2, Fence: 4})
	if err != nil {
		t.Fatal(err)
	}
	event := Event{ID: "event-retry", Type: "log", Message: "original"}
	if _, err = reporter.SendEvent(context.Background(), event); err == nil {
		t.Fatal("首次网络错误必须向上返回")
	}
	if _, err = reporter.SendEvent(context.Background(), Event{ID: "other", Type: "log"}); !errors.Is(err, ErrPendingReport) {
		t.Fatalf("结果未知时其他事件不能越过，实际 %v", err)
	}
	if err = reporter.AcceptFence(5); !errors.Is(err, ErrPendingReport) {
		t.Fatalf("结果未知时不能切换 fence，实际 %v", err)
	}
	if _, err = reporter.SendEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sequences, []uint64{1, 1}) {
		t.Fatalf("重试必须沿用序号，实际 %v", sequences)
	}
}

func TestRunReporterRequiresEventAcknowledgement(t *testing.T) {
	calls := 0
	protocol := &fakeProtocol{}
	protocol.event = func(_ context.Context, _ string, _ EventRequest) (EventResponse, error) {
		calls++
		return EventResponse{Accepted: calls > 1}, nil
	}
	reporter, err := NewRunReporter(protocol, Task{RunID: "run-1", Attempt: 1, Fence: 1})
	if err != nil {
		t.Fatal(err)
	}
	event := Event{ID: "event-1", Type: "progress"}
	if _, err = reporter.SendEvent(context.Background(), event); err == nil {
		t.Fatal("控制端未确认时不能视为成功")
	}
	response, err := reporter.SendEvent(context.Background(), event)
	if err != nil || !response.Accepted {
		t.Fatalf("重试应返回真实确认，response=%+v err=%v", response, err)
	}
}
