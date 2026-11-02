// Package backup 实现数据库工具执行、调度、文件校验与安全清理。
package backup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// PrepareRoot 规范化并创建授权根目录，返回解析符号链接后的绝对路径。
func PrepareRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("备份根目录不能为空")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(absolute, 0o700); err != nil {
		return "", fmt.Errorf("创建备份根目录: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("解析备份根目录: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("备份根路径不是目录")
	}
	return resolved, nil
}

// SecurePath 将相对对象路径限制在授权根目录内，并检查父目录或文件的真实路径。
// 该函数不执行删除；清理还必须重新核对对象状态。
func SecurePath(root, relative string, mustExist bool) (string, error) {
	resolvedRoot, err := PrepareRoot(root)
	if err != nil {
		return "", err
	}
	if relative == "" || strings.ContainsRune(relative, '\x00') || strings.HasPrefix(relative, "/") || strings.HasPrefix(relative, "\\") || filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" {
		return "", fmt.Errorf("对象路径必须是授权目录内的相对路径")
	}
	normalized := filepath.Clean(filepath.FromSlash(relative))
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("对象路径包含非法上级目录")
	}
	candidate := filepath.Join(resolvedRoot, normalized)
	if err = ensureContained(resolvedRoot, candidate); err != nil {
		return "", err
	}
	if mustExist {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return "", fmt.Errorf("解析对象路径: %w", err)
		}
		if err = ensureContained(resolvedRoot, resolved); err != nil {
			return "", err
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("备份对象不是普通文件")
		}
		return resolved, nil
	}
	parent := filepath.Dir(candidate)
	if err = os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("创建对象父目录: %w", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	if err = ensureContained(resolvedRoot, resolvedParent); err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(candidate)), nil
}

func ensureContained(root, candidate string) error {
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("对象路径越过授权根目录")
	}
	return nil
}

// CheckWritable 通过创建并删除随机临时文件验证真实写权限。
func CheckWritable(root string) error {
	resolved, err := PrepareRoot(root)
	if err != nil {
		return err
	}
	random := make([]byte, 8)
	if _, err = rand.Read(random); err != nil {
		return err
	}
	path := filepath.Join(resolved, ".vault-write-check-"+hex.EncodeToString(random))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("目录不可写: %w", err)
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Remove(path)
}

// HashFile 计算普通文件大小与 SHA-256。
func HashFile(path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", err
	}
	if size == 0 {
		return 0, "", fmt.Errorf("备份文件为空")
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}
