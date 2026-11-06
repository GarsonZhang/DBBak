//go:build windows

package main

import (
	"context"
	"log"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"

	"dbbak/internal/config"
)

// Windows 服务的稳定标识。服务名同时是 SCM 键名、事件日志来源与升级脚本的定位依据，不可随意更改。
const (
	serviceName           = "DBBak"
	serviceDisplayName    = "DBBak 数据库备份服务"
	serviceDescription    = "DBBak 单实例数据库备份控制服务：Web 控制台、备份计划、恢复验证与保留清理"
	virtualServiceAccount = `NT SERVICE\DBBak`
)

// isWindowsService 判断当前进程是否被 SCM 拉起。非服务上下文返回 false，命令行模式照常执行。
func isWindowsService() bool {
	running, err := svc.IsWindowsService()
	return err == nil && running
}

// runWindowsService 在服务上下文里启动业务逻辑。
//
// 服务没有控制台，标准输出会被丢弃，因此这里先把日志接到数据目录下的文件，
// 否则启动失败只会在事件查看器里留下一个空错误码。
func runWindowsService(args []string) error {
	if len(args) > 0 && args[0] == "serve" {
		opts, err := parseServeFlags(args[1:])
		if err != nil {
			return err
		}
		opts.apply()
	}
	cfg, err := config.Load()
	if err != nil {
		// 配置都读不出来时仍要留一份日志，便于判断是环境变量还是路径问题。
		if logErr := attachServiceLog(filepath.Join(filepath.Dir(selfExecutable()), "logs")); logErr != nil {
			return err
		}
		return err
	}
	if err = attachServiceLog(filepath.Join(cfg.DataDir, "logs")); err != nil {
		return err
	}
	log.Printf("DBBak %s 以 Windows 服务方式启动，数据目录 %s，监听 %s", version, cfg.DataDir, cfg.Addr)
	// 服务账号只继承机器级环境，这里把自带工具目录补进 PATH 并留下审计线索：
	// 客户机排障时看到“找不到工具”，首先要能确认程序到底在哪个目录找过。
	cfg.ApplySearchPath()
	log.Printf("客户端工具：mysql=%s mysqldump=%s mysqlbinlog=%s sqlcmd=%s", cfg.MySQLPath, cfg.MySQLDumpPath, cfg.MySQLBinlogPath, cfg.SQLCMDPath)
	return svc.Run(serviceName, &dbbakService{cfg: cfg})
}

type dbbakService struct {
	cfg config.Config
}

// Execute 是 SCM 回调：启动业务协程、上报状态、响应停止与关机请求。
//
// 停止必须先取消 ctx 让 HTTP 服务与调度器退出，再返回，否则 SCM 会认为服务未响应。
func (s *dbbakService) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveWithConfig(ctx, s.cfg) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			cancel()
			if err != nil {
				log.Printf("服务异常退出: %v", err)
				status <- svc.Status{State: svc.Stopped}
				return false, 1
			}
			status <- svc.Status{State: svc.Stopped}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				status <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				log.Printf("收到停止请求（cmd=%d），开始优雅关闭", request.Cmd)
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case err := <-done:
					if err != nil {
						log.Printf("关闭过程中出错: %v", err)
					}
				case <-time.After(30 * time.Second):
					log.Printf("关闭超时（30s），交由 SCM 结束进程")
					status <- svc.Status{State: svc.Stopped}
					return false, 1
				}
				status <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		}
	}
}
