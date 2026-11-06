// Package toolenv 负责确定数据库客户端工具的路径。
//
// 发行形态的目标是“装完即用”：客户机不该手动配 DBBAK_* 环境变量或改 PATH。
// 因此查找顺序固定为：
//  1. 显式配置（DBBAK_* 或 setup 写入的机器级变量）——运维有意覆盖时优先；
//  2. 安装目录自带的 tools 子目录——无需任何外部依赖；
//  3. 命令名交给操作系统解析——仅在未打包工具（开发、便携包）时需要。
//
// 本包不执行工具、不做可用性判断：路径不存在时由适配器返回 TOOL_MISSING，
// 不能在这里静默换成别的候选，否则运维看到的“找不到”与实际使用的路径不一致。
package toolenv

import (
	"os"
	"path/filepath"
)

// ToolsSubdir 是随程序分发的客户端工具子目录，相对于可执行文件所在目录。
const ToolsSubdir = "tools"

// DefaultSearchPath 返回未打包工具的发行形态下的默认 PATH。
//
// Windows 服务以虚拟账号启动时，系统级 PATH 可能不包含 Common Files 下的
// OLE DB 目录，而 SQL Server 的 sqlcmd 依赖该目录里的 sqlncli/sqloledb 组件。
const DefaultSearchPath = `C:\Program Files\Common Files\Microsoft Shared\OLE DB\OLEDB\148`

// ExecutableDir 返回当前可执行文件所在目录。
func ExecutableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return filepath.Dir(resolved)
	}
	return filepath.Dir(exe)
}

// BundledToolsDir 返回安装目录下的 tools 子目录；exeDir 为空时返回空串。
func BundledToolsDir(exeDir string) string {
	if exeDir == "" {
		return ""
	}
	return filepath.Join(exeDir, ToolsSubdir)
}

// Resolve 依次在 bundledDirs 中查找 name，未命中时返回 fallback。
//
// bundledDirs 的顺序由调用方决定（通常是安装目录 tools、再是旧版目录），
// 找到的第一个已存在文件即返回其绝对路径。
func Resolve(name string, fallback string, bundledDirs ...string) string {
	for _, dir := range bundledDirs {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return fallback
}

// ResolveAll 只返回已打包且仍在的候选路径，便于能力矩阵说明“工具来自安装包”。
func ResolveAll(name string, bundledDirs ...string) []string {
	found := []string{}
	for _, dir := range bundledDirs {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			found = append(found, candidate)
		}
	}
	return found
}
