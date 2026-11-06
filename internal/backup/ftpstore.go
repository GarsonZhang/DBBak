package backup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"path"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"

	"dbbak/internal/metadata"
)

// FTP 路径语义与本地 SecurePath 不同：没有符号链接可解析，但仍必须阻止
// 相对路径越出授权根目录，并拒绝任何可能改变控制语义的字符。
func remoteJoin(root, relative string) (string, error) {
	if strings.TrimSpace(root) == "" || !strings.HasPrefix(root, "/") {
		return "", fmt.Errorf("FTP 远程根目录必须以 / 开头的绝对路径表示")
	}
	cleaned := path.Clean("/" + strings.ReplaceAll(relative, "\\", "/"))
	if cleaned == "/" || strings.HasPrefix(cleaned, "/../") {
		return "", fmt.Errorf("对象路径包含非法上级目录")
	}
	for _, segment := range strings.Split(strings.TrimPrefix(cleaned, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("对象路径包含非法目录段: %q", segment)
		}
	}
	return strings.TrimSuffix(root, "/") + cleaned, nil
}

// dialFTP 建立 FTP 控制连接并完成登录；被动模式由库默认使用，
// 连接与登录超时固定为 15 秒，避免调度线程被无响应主机长时间挂住。
func dialFTP(target metadata.StorageTarget, password string) (*ftp.ServerConn, error) {
	if target.Host == "" {
		return nil, fmt.Errorf("FTP 主机未配置")
	}
	addr := fmt.Sprintf("%s:%d", target.Host, target.Port)
	conn, err := ftp.Dial(addr, ftp.DialWithTimeout(15*time.Second))
	if err != nil {
		return nil, fmt.Errorf("连接 FTP %s 失败: %w", addr, err)
	}
	if err = conn.Login(target.Username, password); err != nil {
		_ = conn.Quit()
		return nil, fmt.Errorf("FTP 登录失败（账号 %s）: %w", target.Username, err)
	}
	return conn, nil
}

// ensureRemoteDirs 按「根目录 + 相对路径」逐段创建目录。
// 多数 FTP 服务对已存在目录返回 550，这里以 ChangeDir 探测代替解析错误文本。
func ensureRemoteDirs(conn *ftp.ServerConn, root, relative string) error {
	dir := path.Dir(strings.TrimPrefix(strings.ReplaceAll(relative, "\\", "/"), "/"))
	if dir == "." || dir == "/" {
		return nil
	}
	current := strings.TrimSuffix(root, "/")
	for _, segment := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		if segment == "" {
			continue
		}
		current += "/" + segment
		if err := conn.ChangeDir(current); err == nil {
			continue
		}
		if makeErr := conn.MakeDir(current); makeErr != nil {
			if probeErr := conn.ChangeDir(current); probeErr != nil {
				return fmt.Errorf("创建远程目录 %s 失败: %w", current, makeErr)
			}
		}
	}
	return nil
}

// ftpUploadVerified 上传本地文件并在上传后立即整文件回读比对 SHA-256。
// FTP 协议没有远端哈希命令，回读是唯一诚实的校验方式；代价是双倍传输量，
// 这一点已在界面与文档中说明。
func ftpUploadVerified(conn *ftp.ServerConn, target metadata.StorageTarget, localPath, relative string) (string, error) {
	remotePath, err := remoteJoin(target.Root, relative)
	if err != nil {
		return "", err
	}
	if err = ensureRemoteDirs(conn, target.Root, relative); err != nil {
		return "", err
	}
	// 本地哈希在上传前计算；上传过程不再改动该文件。
	wantSize, wantDigest, err := HashFile(localPath)
	if err != nil {
		return "", fmt.Errorf("计算本地文件摘要: %w", err)
	}
	localFile, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("读取本地备份文件: %w", err)
	}
	if err = conn.Stor(remotePath, localFile); err != nil {
		localFile.Close()
		return "", fmt.Errorf("上传 %s 失败: %w", remotePath, err)
	}
	localFile.Close()

	remote, err := conn.Retr(remotePath)
	if err != nil {
		return "", fmt.Errorf("回读 %s 校验失败: %w", remotePath, err)
	}
	defer remote.Close()
	digest := sha256.New()
	size, err := io.Copy(digest, remote)
	if err != nil {
		return "", fmt.Errorf("回读 %s 传输中断: %w", remotePath, err)
	}
	if size != wantSize || hex.EncodeToString(digest.Sum(nil)) != wantDigest {
		return "", fmt.Errorf("回读校验不一致：本地 %d/%s，远端 %d/%s", wantSize, wantDigest, size, hex.EncodeToString(digest.Sum(nil)))
	}
	return remotePath, nil
}

// ftpDelete 删除远程对象；返回错误时调用方保持记录不动，允许安全重试。
func ftpDelete(conn *ftp.ServerConn, target metadata.StorageTarget, relative string) (string, error) {
	remotePath, err := remoteJoin(target.Root, relative)
	if err != nil {
		return "", err
	}
	if err = conn.Delete(remotePath); err != nil {
		return remotePath, err
	}
	return remotePath, nil
}

// ftpWriteTest 对 FTP 根目录做真实写检查：上传探测文件、确认 SIZE、删除。
func FTPWriteTest(target metadata.StorageTarget, password string) error {
	conn, err := dialFTP(target, password)
	if err != nil {
		return err
	}
	defer conn.Quit()
	probe := make([]byte, 8)
	if _, err = rand.Read(probe); err != nil {
		return err
	}
	name := "/dbbak-write-test-" + hex.EncodeToString(probe) + ".tmp"
	remotePath, err := remoteJoin(target.Root, name)
	if err != nil {
		return err
	}
	if err = conn.Stor(remotePath, strings.NewReader("dbbak probe")); err != nil {
		return fmt.Errorf("写测试失败（根目录 %s 不可写或不存在）: %w", target.Root, err)
	}
	size, sizeErr := conn.FileSize(remotePath)
	if sizeErr != nil || size <= 0 {
		_ = conn.Delete(remotePath)
		return fmt.Errorf("写测试文件无法回读（SIZE=%d, err=%v）", size, sizeErr)
	}
	if err = conn.Delete(remotePath); err != nil {
		return fmt.Errorf("写测试文件删除失败，请检查账号权限: %w", err)
	}
	return nil
}

// ftpReadError 保留远端路径与原始错误，供调用方区分「不存在」与「读取失败」。
type ftpReadError struct {
	path string
	err  error
}

func (e *ftpReadError) Error() string { return fmt.Sprintf("回读 %s 失败: %v", e.path, e.err) }

func (e *ftpReadError) Unwrap() error { return e.err }

// isFTPNotExist 判断回读失败是否因为远端对象不存在（550）。
// 库把服务器返回码包进 *textproto.Error，这里按码判断而不是匹配文案。
func isFTPNotExist(err error) bool {
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		return protoErr.Code == ftp.StatusFileUnavailable
	}
	return false
}

// ftpDownloadHash 回读远程文件并计算 SHA-256，用于清理前的身份复核。
func ftpDownloadHash(conn *ftp.ServerConn, target metadata.StorageTarget, relative string) (string, error) {
	remotePath, err := remoteJoin(target.Root, relative)
	if err != nil {
		return "", err
	}
	remote, err := conn.Retr(remotePath)
	if err != nil {
		return "", &ftpReadError{path: remotePath, err: err}
	}
	defer remote.Close()
	digest := sha256.New()
	if _, err = io.Copy(digest, remote); err != nil {
		return "", fmt.Errorf("回读 %s 传输中断: %w", remotePath, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
