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
	"time"
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

// NormalizeSubdir 规范化任务在存储根目录下的相对子目录，空值表示根目录。
//
// 校验拒绝绝对路径、盘符、UNC、`.`/`..` 段和控制字符，防止子目录把备份写出授权根目录；
// 存储统一使用正斜杠，避免同一目录在 Windows 与其他平台产生两种写法。
func NormalizeSubdir(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	if strings.ContainsRune(trimmed, '\x00') || strings.ContainsAny(trimmed, "\r\n") {
		return "", fmt.Errorf("子目录包含控制字符")
	}
	if len([]rune(trimmed)) > 120 {
		return "", fmt.Errorf("子目录不超过 120 个字符")
	}
	normalized := strings.ReplaceAll(trimmed, "\\", "/")
	if strings.HasPrefix(normalized, "/") || filepath.IsAbs(normalized) || filepath.VolumeName(filepath.FromSlash(normalized)) != "" {
		return "", fmt.Errorf("子目录必须是存储根目录内的相对路径，不能是绝对路径或盘符路径")
	}
	segments := strings.Split(normalized, "/")
	// 首尾与重复斜杠在规范化时折叠；安全判断交给 Clean 之后的越界检查：
	// `./` 与中间的 `.` 都是冗余写法，而 `..` 一旦让结果离开根目录，下面的检查会拒绝。
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		switch segment {
		case "", ".", "..":
			continue
		default:
			if len([]rune(segment)) > 60 {
				return "", fmt.Errorf("子目录的单个层级不超过 60 个字符")
			}
			if strings.ContainsAny(segment, "<>:\"|?*") {
				return "", fmt.Errorf("子目录不能包含 < > : \" | ? * 等字符")
			}
		}
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(normalized)))
	if cleaned == "." || cleaned == "" {
		return "", nil
	}
	// 相对路径规范化后仍以 .. 开头，说明目标位于存储根目录之外。
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("子目录不能包含越出存储根目录的 .. 段")
	}
	return cleaned, nil
}

// ArtifactRelativePath 计算备份对象相对授权根目录的位置。
//
// 结构为 <子目录>/<数据库>/<YYYY/MM>/<文件名>：子目录由任务维护，用于区分同一根目录下的不同实例；
// 数据库段与日期段保证不同源、不同月份的对象不会互相覆盖。
func ArtifactRelativePath(subdir, database, fileName string) string {
	parts := make([]string, 0, 4)
	if subdir != "" {
		parts = append(parts, subdir)
	}
	parts = append(parts, database, fileName)
	return strings.Join(parts, "/")
}

// ArtifactFileName 生成备份文件名（不含扩展名）：<数据库名>-<YYYYMMDDHHmmss>。
//
// 时间取运行开始时冻结的 UTC 时间，与 runs.started_at 同源，界面显示按任务时区换算；
// 不用本地时间，避免服务迁移时区后新旧文件名基准不一致。数据库名只保留可安全用于
// Windows/Linux/FTP 对象的字符，中文名保留可读性，非法字符统一替换为下划线。
// 同一数据库在同一秒内触发两次（并发或手动连点）会撞名：落盘侧在 Move 前检查目标存在并拒绝覆盖，
// 运行记录仍以 run_id 区分，不会出现“两个运行共用一个文件”的假象。
func ArtifactFileName(database string, startedAt time.Time) string {
	return SafeNameSegment(database) + "-" + startedAt.UTC().Format("20060102150405")
}

// SafeNameSegment 把任意标识清洗成适合做文件名的一段：替换路径分隔符与保留字符，
// 去掉首尾的点与空格（Windows 无法创建以空格/点结尾的文件），空值退化为 database。
func SafeNameSegment(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\x00', '\r', '\n', '\t':
			return '_'
		}
		return r
	}, value)
	cleaned = strings.TrimRight(cleaned, ". ")
	cleaned = strings.TrimSpace(cleaned)
	// 设备名（CON、PRN、NUL、COM1 等）在 Windows 上是保留名，加前缀避免创建失败。
	if cleaned == "" {
		return "database"
	}
	if reservedName(cleaned) {
		return "_" + cleaned
	}
	return cleaned
}

func reservedName(name string) bool {
	upper := strings.ToUpper(name)
	for _, base := range []string{"CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"} {
		if upper == base {
			return true
		}
	}
	return false
}

// SecurePath 将相对对象路径限制在授权根目录内，并检查父目录或文件的真实路径。
// 该函数不执行删除；清理还必须重新核对对象状态。
//
// run_dir=true 表示 relative 是本次运行独占的目录（如 <数据库>/2026/11/<run_id>）：
// 允许该目录已存在（重试、进程重启后重跑同一 run 复用目录），但仍要求它是根目录内的真实目录，
// 不能是指向根目录外的符号链接。
func SecurePath(root, relative string, mustExist, runDir bool) (string, error) {
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
		if runDir {
			info, err := os.Stat(resolved)
			if err != nil || !info.IsDir() {
				return "", fmt.Errorf("运行目录不可用")
			}
			return resolved, nil
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

// SecureRemovalPath 解析授权根目录内待删除对象的路径。
//
// 与 SecurePath 的区别：不要求目标是普通文件，也不要求目标仍存在。
// 备份中断会留下空的临时目录（例如 pg_basebackup 的 <run_id>），
// 文件也可能被外部清理掉，这两种情况都必须能被登记为已删除，否则任务会永久卡在
// “部分文件未删除”。存在时仍解析符号链接并复核是否越出根目录；
// 是否真的删除、以及目录是否允许删除，由调用方按类型判断。
func SecureRemovalPath(root, relative string) (string, error) {
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
	if resolved, linkErr := filepath.EvalSymlinks(candidate); linkErr == nil {
		if err = ensureContained(resolvedRoot, resolved); err != nil {
			return "", err
		}
		return resolved, nil
	}
	// 目标已不存在：退到父目录解析，保持与必须存在时相同的越界约束，且不创建任何目录。
	parent := filepath.Dir(candidate)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("解析对象上级目录: %w", err)
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

// NormalizeServerDir 规范化数据库服务器本机的绝对目录（如 SQL Server 所在主机的 D:\bak）。
//
// 与 NormalizeSubdir 不同，这个路径属于**数据库服务器**而不是执行器：执行器可能运行在
// Linux、Docker 或 Kubernetes 中，此时 filepath.IsAbs / filepath.VolumeName 会把合法的
// Windows 盘符路径判成相对路径（Linux 上 E:\bak 不是绝对路径），导致任务无法保存。
// 因此这里按 Windows 语义做纯文本校验，不做任何文件系统访问：
// 接受驱动器路径（C:\bak、D:/bak）与 UNC（\\host\share\bak）。
// 拒绝相对路径、rooted 路径、盘符相对写法（C:bak）、设备命名空间（\\.\、\\?\）、通配符和控制字符。
// 目录是否存在、服务账号是否有写权限只有数据库服务自己知道，必须留给运行时反馈。
func NormalizeServerDir(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("数据库服务器目录必填：请填写数据库服务器上的本地绝对路径（如 D:\\bak）")
	}
	if len([]rune(trimmed)) > 300 {
		return "", fmt.Errorf("数据库服务器目录不超过 300 个字符")
	}
	if strings.ContainsRune(trimmed, '\x00') || strings.ContainsAny(trimmed, "\r\n") {
		return "", fmt.Errorf("数据库服务器目录包含控制字符")
	}
	if strings.ContainsRune(trimmed, '\'') {
		// 该目录会拼进 SQL Server 的单引号字符串字面量，单引号既可用于注入也说明路径本身异常。
		return "", fmt.Errorf("数据库服务器目录包含不安全字符（单引号），请检查是否粘贴时混入了引号")
	}
	// 统一成分隔符后校验，存储时保留一份稳定的写法。
	normalized := strings.ReplaceAll(trimmed, "/", `\`)
	if strings.ContainsAny(normalized, `*?"<>|`) {
		return "", fmt.Errorf("数据库服务器目录不能包含 * ? \" < > | 等通配或非法字符")
	}
	segments := strings.Split(normalized, `\`)
	for _, segment := range segments {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("数据库服务器目录不能包含 . 或 .. 段")
		}
	}
	unc := strings.HasPrefix(normalized, `\\`)
	isDrive := len(normalized) >= 2 && normalized[1] == ':' &&
		((normalized[0] >= 'a' && normalized[0] <= 'z') || (normalized[0] >= 'A' && normalized[0] <= 'Z'))
	switch {
	case unc:
		// \\服务器\共享\目录 → segments 为 ["", "", "服务器", "共享", "目录"...]
		if len(segments) < 4 || segments[2] == "" || segments[3] == "" {
			return "", fmt.Errorf("数据库服务器目录 %q 不是有效的 UNC 路径：应为 \\\\服务器\\共享名\\目录", trimmed)
		}
	case isDrive:
		// C:\bak 或 D:bak；C: 后面直接跟字符（如 C:bak 以外的 C:1）也视为非法写法。
		if len(normalized) > 2 && normalized[2] != '\\' {
			return "", fmt.Errorf("数据库服务器目录 %q 格式不正确：盘符后应接反斜杠，如 D:\\bak", trimmed)
		}
	default:
		return "", fmt.Errorf("数据库服务器目录 %q 不是绝对路径：请填写数据库服务器本机的绝对路径（如 D:\\bak 或 \\\\服务器\\共享\\目录）", trimmed)
	}
	// 结尾反斜杠去掉（根目录 D:\ 与 UNC 根 \\host\share 保留），避免与文件名拼接时出现双反斜杠。
	if len(normalized) > 3 && strings.HasSuffix(normalized, `\`) {
		normalized = strings.TrimRight(normalized, `\`)
	}
	return normalized, nil
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
	path := filepath.Join(resolved, ".dbbak-write-check-"+hex.EncodeToString(random))
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
