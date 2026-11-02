// vault-agent 提供远程 Agent 的一次性注册入口。
// 任务执行入口需要在后续接入现有本地 Runner 的已验证适配器后开放。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"vault/internal/agent"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) != 1 || arguments[0] != "enroll" {
		return errors.New("用法: vault-agent enroll；任务执行适配器尚未接入，当前仅开放独立注册流程")
	}
	token := os.Getenv("VAULT_AGENT_ENROLLMENT_TOKEN")
	defer os.Unsetenv("VAULT_AGENT_ENROLLMENT_TOKEN")
	if token == "" {
		return errors.New("请通过 VAULT_AGENT_ENROLLMENT_TOKEN 提供一次性令牌；不要放入命令行参数")
	}
	stateDir := os.Getenv("VAULT_AGENT_STATE_DIR")
	if stateDir == "" {
		return errors.New("请通过 VAULT_AGENT_STATE_DIR 指定身份目录")
	}
	enroller, err := agent.NewEnroller(agent.EnrollmentConfig{
		ServerURL: os.Getenv("VAULT_AGENT_SERVER"),
		CAFile:    os.Getenv("VAULT_AGENT_CA"),
		AgentName: os.Getenv("VAULT_AGENT_NAME"),
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	identity, err := enroller.Enroll(ctx, token)
	if err != nil {
		return err
	}
	paths, err := identity.Save(stateDir)
	if err != nil {
		return err
	}
	fmt.Printf("Agent %s 注册身份已保存；证书=%s，私钥=%s\n", identity.AgentID, paths.CertificateFile, paths.PrivateKeyFile)
	return nil
}
