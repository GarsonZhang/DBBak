//go:build !windows

package main

import "errors"

// errWindowsOnly 明确说明命令只在 Windows 生效，避免其他平台静默跳过安装步骤。
var errWindowsOnly = errors.New("该命令仅在 Windows 上可用")

func isWindowsService() bool { return false }

func runWindowsService([]string) error { return errWindowsOnly }

func setupFromArgs([]string) error { return errWindowsOnly }

// closeSetupLog 在非 Windows 上没有安装日志句柄，保持同名空实现以便 run() 统一收尾。
func closeSetupLog() {}

// runWithoutArgs 在非 Windows 上没有双击安装的语义，保持打印用法。
func runWithoutArgs() int {
	usage()
	return 2
}

func uninstallFromArgs([]string) error { return errWindowsOnly }

func serviceFromArgs([]string) error { return errWindowsOnly }
