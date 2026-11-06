package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// attemptResiduePattern 匹配 runner 自己创建的工作目录名。
// 见 runner.go 的 workRelative：<subdir>/<database>/<YYYY/MM>/.run_<hex>.attempt-<n>。
// 小写点前缀来自 runner，历史数据里也存在不带点的同类目录，因此两者都接受。
var attemptResiduePattern = regexp.MustCompile(`^\.?run_[0-9a-f]{16,64}\.attempt-\d+$`)

const (
	// residueMaxEntries 与 residueMaxDepth 是防呆上限：超过就整体放弃，不做部分清理。
	residueMaxEntries = 5000
	residueMaxDepth   = 8
)

// isAttemptResidueDir 判断目标是否是备份尝试留下的一次性工作目录。
// 只有这类目录才允许递归删除；正式备份对象永远不会命中这个命名。
func isAttemptResidueDir(path string) bool {
	return attemptResiduePattern.MatchString(filepath.Base(path))
}

// residueEntryProblem 判断残留目录中的条目是否允许递归删除。
// 只允许普通文件与普通目录：符号链接、设备、管道、socket 一律拒绝，
// 避免顺着链接删到授权根目录之外，或误删特殊设备。
func residueEntryProblem(name string, mode os.FileMode) error {
	switch {
	case mode&os.ModeSymlink != 0:
		return fmt.Errorf("%s 是符号链接", name)
	case mode.IsDir() || mode.IsRegular():
		return nil
	default:
		return fmt.Errorf("%s 不是普通文件（%s）", name, mode.Type())
	}
}

// removeAttemptResidue 清理一次失败备份留下的工作目录树，返回删除的文件数。
//
// 安全约束：
//   - 调用方必须先用 isAttemptResidueDir 限定范围，本函数不再重复判断；
//   - 先整棵树校验再删除，遇到符号链接、设备/管道文件、超限条目都整体放弃，
//     不会出现“删了一半”的中间状态；
//   - 从 ReadDir 拿到的名字直接拼接，不做路径猜测，因此不会越出该目录。
func removeAttemptResidue(dir string) (int, error) {
	root := filepath.Clean(dir)
	files := make([]string, 0, 16)
	dirs := make([]string, 0, 8)
	entries := 0

	var walk func(current string, depth int) error
	walk = func(current string, depth int) error {
		if depth > residueMaxDepth {
			return fmt.Errorf("目录层级超过 %d 层", residueMaxDepth)
		}
		children, err := os.ReadDir(current)
		if err != nil {
			return fmt.Errorf("读取目录失败: %w", err)
		}
		for _, child := range children {
			entries++
			if entries > residueMaxEntries {
				return fmt.Errorf("目录条目超过 %d 项", residueMaxEntries)
			}
			childPath := filepath.Join(current, child.Name())
			if filepath.Dir(childPath) != current {
				return fmt.Errorf("子项路径异常: %s", child.Name())
			}
			// DirEntry.Info 取的是链接自身信息，不会跟随符号链接。
			info, err := child.Info()
			if err != nil {
				return fmt.Errorf("读取 %s 属性失败: %w", child.Name(), err)
			}
			if err := residueEntryProblem(child.Name(), info.Mode()); err != nil {
				return err
			}
			if info.IsDir() {
				dirs = append(dirs, childPath)
				if err := walk(childPath, depth+1); err != nil {
					return err
				}
				continue
			}
			files = append(files, childPath)
		}
		return nil
	}
	if err := walk(root, 0); err != nil {
		return 0, err
	}

	removed := 0
	for _, file := range files {
		if err := os.Remove(file); err != nil {
			return removed, fmt.Errorf("删除 %s 失败: %w", filepath.Base(file), err)
		}
		removed++
	}
	// dirs 是父在前、子在后，倒序删除保证每层目录被删除时已为空。
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Remove(dirs[i]); err != nil {
			return removed, fmt.Errorf("删除目录 %s 失败: %w", filepath.Base(dirs[i]), err)
		}
	}
	if err := os.Remove(root); err != nil {
		return removed, fmt.Errorf("删除目录 %s 失败: %w", filepath.Base(root), err)
	}
	return removed, nil
}
