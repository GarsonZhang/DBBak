package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Executor 执行一个不透明任务并通过 emit 发送真实事件。返回 Completion 前
// 必须已经停止相关工具进程；返回结果本身不代表控制端已接受。
type Executor interface {
	Execute(context.Context, Task, func(Event) error) Completion
}

// ExecutorFunc 将函数适配为 Executor，便于接入现有 Runner 或编写隔离测试。
type ExecutorFunc func(context.Context, Task, func(Event) error) Completion

// Execute 调用被适配的函数。
func (function ExecutorFunc) Execute(ctx context.Context, task Task, emit func(Event) error) Completion {
	return function(ctx, task, emit)
}

// AgentConfig 配置主动轮询、租约心跳与能力声明。
type AgentConfig struct {
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	Capabilities      []string
}

// Agent 串行拉取并执行任务，任一通信失败都会明确返回，不会继续宣称成功。
type Agent struct {
	protocol Protocol
	executor Executor
	config   AgentConfig
}

// NewAgent 创建单任务 Agent。具体备份执行器必须显式注入。
func NewAgent(protocol Protocol, executor Executor, config AgentConfig) (*Agent, error) {
	if protocol == nil || executor == nil {
		return nil, errors.New("agent: Protocol 和 Executor 不能为空")
	}
	if config.PollInterval == 0 {
		config.PollInterval = 5 * time.Second
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = 15 * time.Second
	}
	if config.PollInterval < 0 || config.HeartbeatInterval <= 0 {
		return nil, errors.New("agent: 轮询间隔不能为负，心跳间隔必须为正")
	}
	config.Capabilities = append([]string(nil), config.Capabilities...)
	return &Agent{protocol: protocol, executor: executor, config: config}, nil
}

// Run 持续主动拉取，直到上下文取消或某次有界协议请求最终失败。
func (agent *Agent) Run(ctx context.Context) error {
	for {
		if err := agent.RunOnce(ctx); err != nil {
			return err
		}
		timer := time.NewTimer(agent.config.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// RunOnce 执行一次拉取；有任务时运行到完成提交，无任务时立即返回。
func (agent *Agent) RunOnce(ctx context.Context) error {
	response, err := agent.protocol.Pull(ctx, PullRequest{Capabilities: agent.config.Capabilities})
	if err != nil {
		return err
	}
	if response.Cancel != nil {
		// 当前独立执行循环没有可关联的旧进程，不能虚假确认已经停止。
		return fmt.Errorf("agent: 收到运行 %s 的取消指令，但没有可确认停止的活动任务", response.Cancel.RunID)
	}
	if response.Task == nil {
		return nil
	}
	reporter, err := NewRunReporter(agent.protocol, *response.Task)
	if err != nil {
		return err
	}
	return agent.execute(ctx, reporter, *response.Task)
}

func (agent *Agent) execute(ctx context.Context, reporter *RunReporter, task Task) error {
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	var asynchronousError error
	var errorOnce sync.Once
	recordError := func(err error) {
		if err == nil {
			return
		}
		errorOnce.Do(func() {
			asynchronousError = err
			cancel()
		})
	}
	emit := func(event Event) error {
		_, err := reporter.SendEvent(runContext, event)
		recordError(err)
		return err
	}
	executionDone := make(chan Completion, 1)
	var executorFinished atomic.Bool
	go func() {
		result := agent.executor.Execute(runContext, task, emit)
		executorFinished.Store(true)
		executionDone <- result
	}()
	var cancelledByServer atomic.Bool
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(agent.config.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runContext.Done():
				return
			case <-ticker.C:
			}
			response, err := reporter.Heartbeat(runContext, 0, "running")
			if err != nil {
				if runContext.Err() == nil {
					recordError(err)
				}
				return
			}
			if response.Cancel {
				// 已完成的执行不能被随后到达的取消响应改写成 cancelled。
				if !executorFinished.Load() {
					cancelledByServer.Store(true)
					cancel()
				}
				return
			}
			select {
			case <-runContext.Done():
				return
			default:
			}
		}
	}()
	result := <-executionDone
	cancel()
	<-heartbeatDone
	if asynchronousError != nil {
		return fmt.Errorf("agent: 运行 %s 上报失败，未提交完成: %w", task.RunID, asynchronousError)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cancelledByServer.Load() {
		// Executor 返回表示工具已经实际停止，此时才允许确认 cancelled。
		result.Status = StatusCancelled
	}
	switch result.Status {
	case StatusSucceeded, StatusFailed, StatusCancelled:
	default:
		return fmt.Errorf("agent: 执行器返回无效完成状态 %q", result.Status)
	}
	if err := reporter.Complete(ctx, result); err != nil {
		return fmt.Errorf("agent: 运行 %s 完成提交失败: %w", task.RunID, err)
	}
	return nil
}
