//go:build windows

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"dbbak/internal/config"
	"dbbak/internal/security"
)

// 卸载登记位置：安装后在“设置 → 应用”中可见，卸载命令指向安装目录内的可执行文件。
const uninstallRegistryPath = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\DBBak`

// installPlan 是安装/卸载共用的路径与运行参数快照，打印、执行和回滚都以同一份数据为准。
type installPlan struct {
	InstallDir string
	ExePath    string
	DataRoot   string
	DBPath     string
	BackupRoot string
	KeyFile    string
	LogDir     string
	Addr       string
	Account    string
	// MySQLPath 为空表示不写入服务环境，服务按自身 PATH 查找 mysql/mysqldump。
	MySQLPath string
}

// setupLogFile 是 --log-file 打开的日志句柄，由 run() 在写完最终错误后关闭。
//
// 关键点：失败结论（"错误: ..."）是 run() 在 setupFromArgs 返回之后才写进 logger 的。
// 若在 setupFromArgs 里 defer 关闭句柄，安装器读到的日志必然为空，用户只看到
// "服务注册或启动失败（退出码 1）"却没有任何原因——排查成本极高。
var setupLogFile *os.File

// closeSetupLog 在进程结束前把日志刷盘并关闭；可重复调用。
func closeSetupLog() {
	if setupLogFile == nil {
		return
	}
	_ = setupLogFile.Sync()
	_ = setupLogFile.Close()
	setupLogFile = nil
}

// setupOptions 是仅在安装阶段生效的交互选项。
type setupOptions struct {
	AdminUser      string
	AdminPass      string
	NoAdmin        bool
	NoStart        bool
	NoBrowser      bool
	NoUninstallKey bool
	Yes            bool
	MySQLPath      string
	// Repair 为 true 表示服务已存在：只刷新程序、服务命令行与环境，保留数据、管理员和 ACL。
	Repair bool
}

// runWithoutArgs 处理“不带任何参数”的启动，也就是在资源管理器里双击安装包。
//
// 只有“双击”才进入向导：要求独占控制台（系统为它新建一个）且标准输入可交互。
// 从 cmd / PowerShell 无参运行、或被计划任务拉起时都退回打印用法，
// 否则会把命令行探测或无人值守调用变成一次 UAC 弹窗和等待输入。
func runWithoutArgs() int {
	if !stdinIsTerminal() || !ownsConsole() {
		usage()
		return 2
	}
	if err := setupFromArgs([]string{"--wizard"}); err != nil {
		log.Printf("错误: %v", err)
		return 1
	}
	return 0
}

var (
	kernel32Proc              = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32Proc.NewProc("GetConsoleProcessList")
)

// ownsConsole 判断当前进程是否独占控制台。
//
// 双击时资源管理器会为安装程序新建控制台，列表里只有自己（返回 1）；
// 从已有的 cmd / PowerShell 运行时列表至少是 2，据此区分“双击”和“命令行调用”。
// 调用失败（无控制台）时返回 false，同样退回用法，避免误入安装流程。
func ownsConsole() bool {
	var pids [16]uint32
	count, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return int(count) == 1
}

// stdinIsTerminal 判断标准输入是否连着控制台；双击运行与命令行运行都是 true。
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// pause 等待回车，只在窗口模式下调用；读不到输入（EOF）时直接返回，避免挂住。
func pause(prompt string) {
	fmt.Print(prompt)
	var line string
	_, _ = fmt.Fscanln(os.Stdin, &line)
	fmt.Println()
}

// setupFromArgs 解析参数并执行安装。
//
// 返回值命名为 err 是为了让 --wizard 能在所有退出路径（含失败）暂停窗口：
// 双击安装时控制台随进程结束立即关闭，错误信息和一次性密码都会来不及看。
func setupFromArgs(args []string) (err error) {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		prefix       = fs.String("prefix", "", `安装目录，默认 %ProgramFiles%\DBBak`)
		dataRoot     = fs.String("data-dir", "", `数据根目录，默认 %ProgramData%\DBBak`)
		addr         = fs.String("addr", "127.0.0.1:8080", "监听地址，默认仅回环")
		account      = fs.String("account", virtualServiceAccount, `服务运行账号，默认 NT SERVICE\DBBak；可改为 LocalSystem`)
		password     = fs.String("admin-password", "", "初始管理员密码；留空则生成 24 位随机密码并只打印一次")
		passwordFile = fs.String("admin-password-file", "", "从文件读取初始管理员密码（图形安装器用，避免密码出现在命令行与进程表）")
		adminUser    = fs.String("admin-user", "admin", "初始管理员用户名")
		noAdmin      = fs.Bool("no-admin", false, "不创建管理员（之后用 init-admin 创建）")
		noStart      = fs.Bool("no-start", false, "只注册服务，不立即启动")
		noBrowser    = fs.Bool("no-browser", false, "安装完成后不打开浏览器")
		noUninstKey  = fs.Bool("no-uninstall-key", false, "不写“设置 → 应用”卸载登记（由外部安装器登记时使用）")
		logFile      = fs.String("log-file", "", "把安装输出写入该文件（图形安装器用，失败时回看）")
		mysqlPath    = fs.String("mysql-path", "", "mysql 客户端绝对路径；指定后连同所在目录写入服务环境（服务账号 PATH 与登录用户不同）")
		dryRun       = fs.Bool("dry-run", false, "只打印安装计划，不做任何更改")
		yes          = fs.Bool("yes", false, "跳过交互确认")
		wizard       = fs.Bool("wizard", false, "内部使用：双击安装的交互窗口模式")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// 图形安装器以隐藏窗口调用本命令，控制台内容看不到，因此提供一个可选日志文件。
	if *logFile != "" {
		file, ferr := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if ferr != nil {
			return fmt.Errorf("打开安装日志失败: %w", ferr)
		}
		// 句柄交给 run() 关闭：失败原因是在 setupFromArgs 返回之后才写进 logger 的，
		// 这里一 defer 关闭，安装器弹出的日志就永远是空的（只剩退出码）。
		setupLogFile = file
		os.Stdout, os.Stderr = file, file
		log.SetOutput(file)
	}
	if *wizard {
		defer func() {
			if err != nil {
				pause("\n安装未完成，按回车键关闭此窗口…")
				return
			}
			pause("\n按回车键关闭此窗口…")
		}()
	}
	plan, err := newInstallPlan(*prefix, *dataRoot, *addr, *account)
	if err != nil {
		return err
	}
	mysqlExe, err := normalizeMySQLPath(*mysqlPath)
	if err != nil {
		return err
	}
	plan.MySQLPath = mysqlExe
	if *dryRun {
		plan.print()
		return nil
	}
	// 提权必须早于任何写入：双击运行时默认只有标准用户令牌。
	// 向导模式下提权后的进程带 --yes，避免同一件事让用户确认两遍。
	relaunchArgs := append([]string{"setup"}, args...)
	if *wizard && !*yes {
		relaunchArgs = append([]string{"setup", "--yes"}, args...)
	}
	if relaunched, err := relaunchElevated(relaunchArgs); relaunched {
		if *wizard {
			fmt.Println("安装已在新的管理员窗口中继续。")
		}
		return err
	}
	existing, err := serviceExists()
	if err != nil {
		return err
	}
	if existing && !ownService(plan.ExePath) {
		// 别的程序占了同名服务时不能覆盖，否则会破坏对方服务。
		return fmt.Errorf("服务 %s 已存在且不属于 %s；如需替换请先运行 \"%s\" uninstall", serviceName, plan.InstallDir, plan.ExePath)
	}
	// 密码可以来自文件：图形安装器不便把明文密码拼进命令行，否则会留在进程表里。
	adminPassword := *password
	if *passwordFile != "" {
		raw, readErr := os.ReadFile(*passwordFile)
		if readErr != nil {
			return fmt.Errorf("读取密码文件失败: %w", readErr)
		}
		adminPassword = strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff"))
		// 读后即删，减少明文在临时目录里的停留时间；删不掉不算失败。
		if rmErr := os.Remove(*passwordFile); rmErr != nil {
			log.Printf("警告: 未能删除临时密码文件: %v", rmErr)
		}
	}
	opts := setupOptions{
		AdminUser: *adminUser, AdminPass: adminPassword, NoAdmin: *noAdmin,
		NoStart: *noStart, NoBrowser: *noBrowser, NoUninstallKey: *noUninstKey, Yes: *yes,
		MySQLPath: plan.MySQLPath, Repair: existing,
	}
	if !opts.Yes {
		plan.print()
		if existing {
			fmt.Println("  已存在同名服务：将先停止服务，再刷新服务命令行与环境，保留数据与管理员账号")
		}
		ok, err := confirm("确认按以上计划安装")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("已取消，未做任何更改。")
			return nil
		}
	}
	generated, err := installService(plan, opts)
	if err != nil {
		return err
	}
	printSummary(plan, opts, generated)
	if !opts.NoStart && !opts.NoBrowser {
		if err := openBrowser("http://" + plan.Addr + "/"); err != nil {
			log.Printf("警告: 未能自动打开浏览器: %v，请手工访问 http://%s/", err, plan.Addr)
		}
	}
	return nil
}

// newInstallPlan 校验并规范化安装路径与账号；任何越界路径都在写盘前拒绝。
func newInstallPlan(prefix, dataRoot, addr, account string) (installPlan, error) {
	if prefix == "" {
		prefix = filepath.Join(os.Getenv("ProgramFiles"), "DBBak")
	}
	if dataRoot == "" {
		dataRoot = filepath.Join(os.Getenv("ProgramData"), "DBBak")
	}
	installDir, err := normalizeDir(prefix)
	if err != nil {
		return installPlan{}, fmt.Errorf("--prefix 无效: %w", err)
	}
	rootDir, err := normalizeDir(dataRoot)
	if err != nil {
		return installPlan{}, fmt.Errorf("--data-dir 无效: %w", err)
	}
	if strings.EqualFold(installDir, rootDir) {
		return installPlan{}, errors.New("程序目录与数据目录不能相同：程序目录可被升级覆盖，数据目录必须独立")
	}
	if _, _, err = net.SplitHostPort(addr); err != nil {
		return installPlan{}, fmt.Errorf("--addr 无效（需 host:port）: %w", err)
	}
	account = strings.TrimSpace(account)
	switch {
	case strings.EqualFold(account, "LocalSystem"), strings.EqualFold(account, `NT AUTHORITY\SYSTEM`):
		account = "LocalSystem"
	case strings.EqualFold(account, virtualServiceAccount):
		account = virtualServiceAccount
	default:
		// 普通账号需要预置密码与“作为服务登录”权限，安装程序不做静默授权，避免扩大提权面。
		return installPlan{}, fmt.Errorf("--account 仅支持 %s 或 LocalSystem", virtualServiceAccount)
	}
	return installPlan{
		InstallDir: installDir,
		ExePath:    filepath.Join(installDir, "dbbak.exe"),
		DataRoot:   rootDir,
		DBPath:     filepath.Join(rootDir, "dbbak.db"),
		BackupRoot: filepath.Join(rootDir, "backups"),
		KeyFile:    filepath.Join(rootDir, "key", "master.key"),
		LogDir:     filepath.Join(rootDir, "logs"),
		Addr:       addr,
		Account:    account,
	}, nil
}

// normalizeMySQLPath 校验 --mysql-path：必须是已存在的绝对文件路径。
//
// 安装程序只检查“文件存在”，不执行它；真正的可用性由运行时连接测试给出真实结论。
func normalizeMySQLPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("--mysql-path 无效: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("--mysql-path 指向的文件不存在: %s", abs)
	}
	return abs, nil
}

func normalizeDir(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if filepath.Dir(abs) == abs {
		return "", fmt.Errorf("拒绝使用卷根目录 %s", abs)
	}
	return abs, nil
}

func (p installPlan) dirs() []string {
	return []string{p.InstallDir, p.DataRoot, filepath.Dir(p.KeyFile), p.BackupRoot, p.LogDir}
}

// serviceArgs 是写入服务 ImagePath 的参数；服务启动时由 runWindowsService 解析。
func (p installPlan) serviceArgs() []string {
	return []string{
		"serve",
		"--addr", p.Addr,
		"--data-dir", p.DataRoot,
		"--backup-root", p.BackupRoot,
		"--master-key-file", p.KeyFile,
	}
}

func (p installPlan) print() {
	fmt.Println("DBBak 安装计划：")
	fmt.Printf("  程序目录    %s\n", p.InstallDir)
	fmt.Printf("  数据目录    %s\n", p.DataRoot)
	fmt.Printf("  元数据库    %s\n", p.DBPath)
	fmt.Printf("  备份根目录  %s\n", p.BackupRoot)
	fmt.Printf("  主密钥      %s\n", p.KeyFile)
	fmt.Printf("  服务日志    %s\n", filepath.Join(p.LogDir, "dbbak.log"))
	fmt.Printf("  控制台      http://%s/\n", p.Addr)
	if p.MySQLPath != "" {
		fmt.Printf("  MySQL 工具  %s（连同所在目录写入服务环境 PATH）\n", p.MySQLPath)
	}
	fmt.Printf("  服务与账号  %s / %s\n", serviceName, p.Account)
	fmt.Printf("  服务命令行  \"%s\" %s\n", p.ExePath, strings.Join(p.serviceArgs(), " "))
}

// installService 执行安装并返回生成的初始密码（未生成时为空串）。
//
// 顺序有意为之：服务必须先注册，ACL 才能按服务名解析虚拟账号 NT SERVICE\DBBak；
// 数据目录授权必须先于管理员初始化，否则新建的元数据库对服务账号不可写。
// 任一步失败都保留真实错误；服务注册之后失败会回滚注册，避免留下起不来的服务。
//
// opts.Repair 为 true 表示服务已存在（重装用于修复或补装自带工具）：此时不删服务、
// 不重设 ACL、不重置管理员，只刷新服务命令行、环境与启动状态；失败也不回滚，
// 因为回滚会删掉客户正在使用的服务。
func installService(plan installPlan, opts setupOptions) (string, error) {
	mode := "全新安装"
	if opts.Repair {
		mode = "刷新已有服务"
	}
	log.Printf("开始%s：程序目录=%s 数据目录=%s 监听=%s 账号=%s",
		mode, plan.InstallDir, plan.DataRoot, plan.Addr, plan.Account)
	for _, dir := range plan.dirs() {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", fmt.Errorf("创建目录 %s: %w", dir, err)
		}
	}
	log.Printf("目录已就绪：%v", plan.dirs())
	if err := copySelfTo(plan.ExePath); err != nil {
		return "", err
	}
	log.Printf("程序文件已就位：%s", plan.ExePath)
	// 自带工具放进安装目录后，运行时按“exe 目录\tools”直接找到，客户机不需要任何 PATH 配置。
	if err := installBundledTools(selfExecutableDir(), plan.InstallDir); err != nil {
		return "", err
	}
	// 环境必须在注册服务之前写入：SCM 启动服务时只继承注册时可见的环境，
	// 之后再改注册表对已注册服务不一定生效（实例本身可能仍持有旧环境块）。
	if plan.MySQLPath != "" {
		if envErr := configureServiceEnvironment(plan.MySQLPath); envErr != nil {
			return "", envErr
		}
	}

	var service *mgr.Service
	var err error
	if opts.Repair {
		service, err = openExistingService(plan)
		if err != nil {
			return "", err
		}
	} else {
		service, err = createService(plan)
		if err != nil {
			return "", err
		}
	}
	defer service.Close()
	rollback := func(cause error) (string, error) {
		if opts.Repair {
			// 重装失败不删服务：客户机上可能还有正在运行的备份任务，删服务比留问题更糟。
			return "", fmt.Errorf("%w\n服务仍保留在系统中，修正问题后可重新运行 setup 或安装包", cause)
		}
		if delErr := service.Delete(); delErr != nil {
			log.Printf("警告: 回滚删除服务失败: %v", delErr)
		}
		return "", fmt.Errorf("%w\n已回滚服务注册；程序与数据保留在 %s，修正问题后可重新运行 setup", cause, plan.DataRoot)
	}
	if opts.Repair {
		fmt.Println("刷新已有服务：保留数据目录、管理员账号与目录授权。")
	}

	if !opts.Repair {
		if err = grantDataRootACL(plan.DataRoot, plan.Account); err != nil {
			return rollback(err)
		}
		log.Printf("数据目录权限已设置：%s 仅授权 SYSTEM、管理员与 %s", plan.DataRoot, plan.Account)
	}

	generated := ""
	if opts.Repair && opts.AdminPass == "" {
		fmt.Println("重装未提供新密码：保留原有管理员账号与密码。")
	}
	// 首次安装没有密码时必须生成随机密码并打印一次：静默跳过会装出一个
	// "服务在跑但没有任何管理员"的实例，控制台谁也登不进去。
	if !opts.NoAdmin {
		password := opts.AdminPass
		if password == "" {
			random, genErr := randomPassword(24)
			if genErr != nil {
				return rollback(genErr)
			}
			password = random
			generated = random
		}
		if len(password) < security.MinPasswordLength {
			return rollback(fmt.Errorf("管理员密码至少 %d 个字符", security.MinPasswordLength))
		}
		created, adminErr := initInstalledAdmin(plan, opts.AdminUser, password)
		if adminErr != nil {
			return rollback(adminErr)
		}
		if !created {
			fmt.Println("数据目录中已存在管理员账号，保留原账号与密码，不重置。")
			generated = ""
		} else {
			log.Printf("已创建初始管理员 %s", opts.AdminUser)
		}
	}

	if err = configureFailureActions(); err != nil {
		log.Printf("警告: 未能配置失败自动重启: %v", err)
	}
	if !opts.NoStart {
		// 全新安装时端口已被占用是最常见的失败原因，而它表现为"服务起来了又立刻退出"。
		// 事先试绑一次，把 90 秒超时换成一眼可见的结论。
		if !opts.Repair {
			if err = ensureListenAddrFree(plan.Addr); err != nil {
				return rollback(err)
			}
		}
		if err = startService(service, plan); err != nil {
			return rollback(err)
		}
	}
	if !opts.NoUninstallKey {
		if err = writeUninstallKey(plan); err != nil {
			log.Printf("警告: 写入卸载登记失败（不影响运行，可手工运行 uninstall）: %v", err)
		}
	}
	return generated, nil
}

func confirm(prompt string) (bool, error) {
	fmt.Printf("%s（输入 YES 继续）: ", prompt)
	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		if errors.Is(err, io.EOF) {
			return false, errors.New("未读到确认输入，已中止；无人值守安装请加 --yes")
		}
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(answer), "YES"), nil
}

// relaunchElevated 在非管理员令牌下用 runas 重新拉起自身，返回是否已移交、以及移交错误。
func relaunchElevated(args []string) (bool, error) {
	if windows.GetCurrentProcessToken().IsElevated() {
		return false, nil
	}
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return true, err
	}
	file, err := windows.UTF16PtrFromString(selfExecutable())
	if err != nil {
		return true, err
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = syscall.EscapeArg(arg)
	}
	params, err := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	if err != nil {
		return true, err
	}
	if err = windows.ShellExecute(0, verb, file, params, nil, windows.SW_SHOWNORMAL); err != nil {
		return true, fmt.Errorf("需要管理员权限，自动提权失败: %w；请右键选择「以管理员身份运行」", err)
	}
	fmt.Println("已请求管理员权限，请在 UAC 对话框中允许后继续安装。")
	return true, nil
}

func selfExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

func samePath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

// copySelfTo 先写 .new 再改名，避免中断留下半个可执行文件覆盖可用程序。
func copySelfTo(target string) error {
	source := selfExecutable()
	if samePath(source, target) {
		return nil
	}
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("读取安装源 %s: %w", source, err)
	}
	defer in.Close()
	temp := target + ".new"
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("写入 %s: %w", temp, err)
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(temp)
		return fmt.Errorf("复制程序文件失败: %w", err)
	}
	if err = out.Close(); err != nil {
		os.Remove(temp)
		return err
	}
	if err = os.Rename(temp, target); err != nil {
		os.Remove(temp)
		return fmt.Errorf("替换 %s 失败: %w", target, err)
	}
	return nil
}

// grantDataRootACL 只给 SYSTEM、管理员和服务账号授权，切断 ProgramData 的继承权限。
// 元数据库与主密钥所在目录必须同时对服务账号可写，否则服务启动即失败。
func grantDataRootACL(root, account string) error {
	args := []string{root, "/inheritance:r", "/grant:r",
		"*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F"}
	if account != "LocalSystem" {
		args = append(args, account+":(OI)(CI)M")
	}
	args = append(args, "/T", "/C")
	output, err := exec.Command("icacls.exe", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("设置数据目录权限失败: %w（icacls 输出: %s）", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func initInstalledAdmin(plan installPlan, user, password string) (bool, error) {
	for _, pair := range [][2]string{
		{"DBBAK_DATA_DIR", plan.DataRoot},
		{"DBBAK_BACKUP_ROOT", plan.BackupRoot},
		{"DBBAK_MASTER_KEY_FILE", plan.KeyFile},
	} {
		_ = os.Setenv(pair[0], pair[1])
	}
	cfg, err := config.Load()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return ensureInitialAdmin(ctx, cfg, user, password)
}

func randomPassword(length int) (string, error) {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buffer := make([]byte, length)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成随机密码失败: %w", err)
	}
	out := make([]byte, length)
	for i, value := range buffer {
		out[i] = alphabet[int(value)%len(alphabet)]
	}
	return string(out), nil
}

// connectManager 连接本机服务管理器。
// x/sys 固定申请 SC_MANAGER_ALL_ACCESS，因此除查询外都必须提权；此处把拒绝访问翻译成可直接执行的提示。
func connectManager() (*mgr.Mgr, error) {
	manager, err := mgr.Connect()
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, fmt.Errorf("连接服务管理器被拒绝，请以管理员身份运行（%w）", err)
		}
		return nil, fmt.Errorf("连接服务管理器失败: %w", err)
	}
	return manager, nil
}

func serviceExists() (bool, error) {
	manager, err := connectManager()
	if err != nil {
		return false, err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(serviceName)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return false, nil
		}
		return false, err
	}
	service.Close()
	return true, nil
}

func createService(plan installPlan) (*mgr.Service, error) {
	manager, err := connectManager()
	if err != nil {
		return nil, err
	}
	defer manager.Disconnect()
	service, err := manager.CreateService(serviceName, plan.ExePath, mgr.Config{
		DisplayName:      serviceDisplayName,
		Description:      serviceDescription,
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
		ServiceStartName: plan.Account,
	}, plan.serviceArgs()...)
	if err != nil {
		return nil, fmt.Errorf("注册服务失败: %w", err)
	}
	return service, nil
}

// configureFailureActions 让服务异常退出后由 SCM 自动重启。
// x/sys 未封装 SERVICE_FAILURE_ACTIONS，这里直接调用系统自带的 sc.exe，参数走数组而非 shell 拼接。
func configureFailureActions() error {
	output, err := exec.Command("sc.exe", "failure", serviceName, "reset=", "3600",
		"actions=", "restart/10000/restart/30000/restart/60000").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w（sc 输出: %s）", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// ensureListenAddrFree 在启动服务前确认监听地址还能绑上。
//
// 服务进程绑不上端口时会在启动后立刻退出，但 SCM 已经先返回了“启动成功”，
// 于是 setup 只能靠 readiness 超时（90 秒）才发现问题，错误信息与真正原因相差很远。
// 组装服务前试绑一次，把"端口已被占用"直接说清楚。
func ensureListenAddrFree(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err == nil {
		listener.Close()
		return nil
	}
	owner := describePortOwner(addr)
	holder := "该端口"
	if owner != "" {
		holder = "该端口（被 " + owner + " 占用）"
	}
	return fmt.Errorf("监听地址 %s 不可用：%s已被占用 (%w)\n"+
		"常见原因：另一个 DBBak 实例或旧版本服务仍在运行（例如升级前注册的旧服务），或其他程序占用了端口。\n"+
		"处理办法：在向导里换一个监听端口重新安装；或先停止占用端口的程序后重试",
		addr, holder, err)
}

// describePortOwner 尽力找出占用该端口的进程，只用于失败提示。
//
// 解析失败不当作错误：它只是把"端口可能被占用"变成"被 mysqld.exe (PID 4312) 占用"，
// 让客户不必自己去 netstat 里翻。
func describePortOwner(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return ""
	}
	output, err := exec.Command("netstat.exe", "-ano", "-p", "TCP").Output()
	if err != nil {
		return ""
	}
	pid := ""
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.EqualFold(fields[len(fields)-2], "LISTENING") {
			continue
		}
		if strings.HasSuffix(fields[1], ":"+port) {
			pid = fields[len(fields)-1]
			break
		}
	}
	if pid == "" || pid == "0" {
		return ""
	}
	if name := processName(pid); name != "" {
		return name + " (PID " + pid + ")"
	}
	return "PID " + pid
}

// processName 通过 tasklist 取 PID 对应的映像名；取不到就返回空串。
func processName(pid string) string {
	output, err := exec.Command("tasklist.exe", "/FI", "PID eq "+pid, "/FO", "CSV", "/NH").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(output))
	if !strings.HasPrefix(line, `"`) {
		return ""
	}
	fields := strings.Split(line, `","`)
	if len(fields) == 0 {
		return ""
	}
	name := strings.Trim(fields[0], `"`)
	if strings.Contains(strings.ToLower(name), "no tasks") {
		return ""
	}
	return name
}

func startService(service *mgr.Service, plan installPlan) error {
	log.Printf("正在启动服务 %s ...", serviceName)
	if err := service.Start(); err != nil {
		return fmt.Errorf("启动服务失败: %w（详见 %s）", err, filepath.Join(plan.LogDir, "dbbak.log"))
	}
	if err := waitReady(service, plan, 90*time.Second); err != nil {
		return err
	}
	log.Printf("服务已启动并通过 readiness 检查：http://%s/", plan.Addr)
	return nil
}

// waitReady 轮询 readiness 接口，确认服务真的在对外提供服务，而不只是进程还在。
//
// 同时盯着 SCM 状态：服务启动后崩溃会停在 Stopped，此时再等满 90 秒毫无意义，
// 直接把服务日志尾部带出来，用户才知道是端口、权限还是主密钥的问题。
func waitReady(service *mgr.Service, plan installPlan, timeout time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second}
	url := "http://" + plan.Addr + "/health/ready"
	serviceLog := filepath.Join(plan.LogDir, "dbbak.log")
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if status, queryErr := service.Query(); queryErr == nil && status.State == svc.Stopped {
			return fmt.Errorf("服务启动后立即停止（%s），未能对外提供服务。\n服务日志 %s 末尾:\n%s",
				stateName(status.State), serviceLog, tailLog(serviceLog, 20))
		}
		response, err := client.Get(url)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("readiness 返回 HTTP %d", response.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("服务在 %s 内未通过 readiness 检查: %v\n服务日志 %s 末尾:\n%s",
		timeout, lastErr, serviceLog, tailLog(serviceLog, 20))
}

func tailLog(path string, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "（无法读取日志: " + err.Error() + "）"
	}
	parts := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

// ownService 判断已注册的同名服务是否属于本产品，允许安装程序接管。
//
// 不能只比对目录：用户换盘、改目录名（例如从 C:\Program Files\DBak 改成 DBBak）、
// 或用不同版本的安装包重装时，旧服务的 ImagePath 仍指向老目录。此时若一律拒绝，
// 升级路径直接失败，用户只能先手工卸载——与"装完即用"的目标相悖。
// Inno 阶段已按 ImagePath 停掉旧服务并拍过快照，这里按可执行文件名识别产品即可。
func ownService(installDir string) bool {
	manager, err := connectManager()
	if err != nil {
		return false
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(serviceName)
	if err != nil {
		return false
	}
	defer service.Close()
	config, err := service.Config()
	if err != nil {
		return false
	}
	installed := exeFromImagePath(config.BinaryPathName)
	if installed == "" {
		return false
	}
	if strings.EqualFold(filepath.Base(installed), "dbbak.exe") {
		return true
	}
	return strings.EqualFold(filepath.Dir(filepath.Clean(installed)), filepath.Clean(installDir))
}

// exeFromImagePath 从服务 ImagePath 中取出可执行文件的完整路径。
//
// 两种形态都要处理：带引号的 `"C:\Program Files\X\dbbak.exe" serve --addr ...`
// 与不带引号的 `C:\X\dbbak.exe serve`。不带引号时不能按第一个空格切分，
// 因为路径本身可能含空格，只能按 ".exe" 截断。
func exeFromImagePath(imagePath string) string {
	trimmed := strings.TrimSpace(imagePath)
	if trimmed == "" {
		return ""
	}
	if trimmed[0] == '"' {
		if end := strings.Index(trimmed[1:], `"`); end >= 0 {
			return trimmed[1 : end+1]
		}
		return strings.Trim(trimmed, `"`)
	}
	if end := strings.Index(strings.ToLower(trimmed), ".exe"); end >= 0 {
		return trimmed[:end+4]
	}
	return trimmed
}

// openExistingService 打开已注册服务并刷新为本次安装计划。
//
// 重装可能更换了监听地址、数据目录或升级了程序，服务命令行的 ImagePath 必须同步，
// 否则会出现“程序换了但服务仍按旧参数启动”的假成功。
func openExistingService(plan installPlan) (*mgr.Service, error) {
	manager, err := connectManager()
	if err != nil {
		return nil, err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(serviceName)
	if err != nil {
		return nil, fmt.Errorf("打开已有服务 %s: %w", serviceName, err)
	}
	status, statusErr := service.Query()
	if statusErr == nil && status.State != svc.Stopped {
		if err = stopAndWait(service, 60*time.Second); err != nil {
			service.Close()
			return nil, err
		}
	}
	config, err := service.Config()
	if err != nil {
		service.Close()
		return nil, fmt.Errorf("读取服务配置: %w", err)
	}
	config.BinaryPathName = quoteExePath(plan.ExePath, plan.serviceArgs())
	if err = service.UpdateConfig(config); err != nil {
		service.Close()
		return nil, fmt.Errorf("刷新服务命令行: %w", err)
	}
	return service, nil
}

// stopAndWait 停止服务并等待 SCM 报告已停止；超时不隐藏，直接报错。
func stopAndWait(service *mgr.Service, timeout time.Duration) error {
	if _, err := service.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return fmt.Errorf("停止已有服务失败（文件正在被占用时无法替换程序）: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := service.Query()
		if err != nil {
			return fmt.Errorf("查询服务状态失败: %w", err)
		}
		if status.State == svc.Stopped {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("服务在 %s 内未停止，请手动停止 %s 服务后重试", timeout, serviceName)
}

// installBundledTools 把安装包自带的客户端工具放到安装目录，未自带时不动已有目录。
//
// setup 从暂存区运行时（安装包内层目录）自带 tools；被直接执行时 sourceDir 为空，
// 保持原有 tools 不动，避免“用旧 exe 刷新服务”把已装好的工具删掉。
func installBundledTools(sourceDir, installDir string) error {
	if sourceDir == "" {
		return nil
	}
	source := filepath.Join(sourceDir, toolsDirName)
	info, err := os.Stat(source)
	if err != nil || !info.IsDir() {
		return nil
	}
	target := filepath.Join(installDir, toolsDirName)
	// 图形安装器已经把 tools 解压到安装目录，此时源与目标相同：
	// 再走“复制到 .new 再改名”等于把每个文件覆盖自己，没必要也可能与正在使用的文件冲突。
	if samePath(source, target) {
		return nil
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("创建工具目录 %s: %w", target, err)
	}
	count := 0
	err = filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if info.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		// 与 copySelfTo 同样的策略：先写临时文件再改名，避免中断留下半个可执行文件。
		temp := destination + ".new"
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		if _, err = io.Copy(out, in); err != nil {
			out.Close()
			os.Remove(temp)
			return err
		}
		if err = out.Close(); err != nil {
			os.Remove(temp)
			return err
		}
		if err = os.Rename(temp, destination); err != nil {
			os.Remove(temp)
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return fmt.Errorf("复制自带工具到 %s: %w", target, err)
	}
	log.Printf("已安装 %d 个自带客户端工具文件到 %s", count, target)
	return nil
}

// toolsDirName 与 config/toolenv 中的 tools 子目录名一致。
const toolsDirName = "tools"

// selfExecutableDir 返回当前 dbbak.exe 所在目录；安装包内层调用时即暂存区目录。
func selfExecutableDir() string {
	return filepath.Dir(selfExecutable())
}

// quoteExePath 按 Windows 服务命令行规则拼出可执行文件与参数的 ImagePath。
func quoteExePath(exe string, args []string) string {
	parts := []string{syscall.EscapeArg(exe)}
	for _, arg := range args {
		parts = append(parts, syscall.EscapeArg(arg))
	}
	return strings.Join(parts, " ")
}

// configureServiceEnvironment 把 MySQL 工具路径写入服务专属环境。
//
// 为什么不能只依赖系统 PATH：服务以虚拟账号 NT SERVICE\DBBak 启动，读的是机器 PATH，
// 而安装者往往只在当前用户 PATH 里放入 MySQL，装完连接测试就报“找不到 mysqldump”。
// 这里同时写：
//   - DBBAK_MYSQL / DBBAK_MYSQLDUMP 指向同目录下同名可执行文件，配置显式可见；
//   - 系统 PATH 追加 bin 目录，避免 MySQL 客户端依赖 libssl 等 DLL 时因搜索路径缺失而加载失败。
//
// 写入后广播 WM_SETTINGCHANGE，让服务控制管理器与资源管理器感知环境变化。
func configureServiceEnvironment(mysqlExe string) error {
	binDir := filepath.Dir(mysqlExe)
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, registry.ALL_ACCESS)
	if err != nil {
		return fmt.Errorf("打开系统环境注册表: %w", err)
	}
	defer key.Close()

	changed := false
	for _, pair := range [][2]string{
		{"DBBAK_MYSQL", mysqlExe},
		{"DBBAK_MYSQLDUMP", filepath.Join(binDir, "mysqldump.exe")},
	} {
		current, _, readErr := key.GetStringValue(pair[0])
		if readErr != nil && !errors.Is(readErr, registry.ErrNotExist) {
			return fmt.Errorf("读取 %s: %w", pair[0], readErr)
		}
		if current == pair[1] {
			continue
		}
		if err = key.SetStringValue(pair[0], pair[1]); err != nil {
			return fmt.Errorf("写入 %s: %w", pair[0], err)
		}
		changed = true
	}

	machinePath, _, err := key.GetStringValue("Path")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("读取系统 PATH: %w", err)
	}
	if !pathListContains(machinePath, binDir) {
		updated := machinePath
		if strings.TrimSpace(updated) != "" && !strings.HasSuffix(updated, ";") {
			updated += ";"
		}
		if err = key.SetStringValue("Path", updated+binDir); err != nil {
			return fmt.Errorf("写入系统 PATH: %w", err)
		}
		changed = true
	}
	if changed {
		broadcastEnvironmentChange()
	}
	return nil
}

// pathListContains 按分号拆分比较，避免把 “C:\mysql\bin2” 误判成已包含 “C:\mysql\bin”。
func pathListContains(list, dir string) bool {
	for _, item := range strings.Split(list, ";") {
		if strings.EqualFold(strings.TrimRight(strings.TrimSpace(item), `\`), strings.TrimRight(dir, `\`)) {
			return true
		}
	}
	return false
}

// broadcastEnvironmentChange 通知系统环境已变；失败只影响新进程是否立刻看到，不影响配置写入。
func broadcastEnvironmentChange() {
	const hwndBroadcast = 0xffff
	const wmSettingChange = 0x001A
	const smtoAbortIfHung = 0x0002
	message, _ := syscall.UTF16PtrFromString("Environment")
	if _, _, callErr := procSendMessageTimeout.Call(hwndBroadcast, wmSettingChange, 0,
		uintptr(unsafe.Pointer(message)), smtoAbortIfHung, 5000, 0); callErr != nil {
		log.Printf("警告: 广播环境变量变更失败（配置已写入，重启服务后生效）: %v", callErr)
	}
}

var procSendMessageTimeout = func() *windows.LazyProc {
	return windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")
}()

func writeUninstallKey(plan installPlan) error {
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, uninstallRegistryPath, registry.WRITE)
	if err != nil {
		return err
	}
	defer key.Close()
	stringsToSet := map[string]string{
		"DisplayName":          serviceDisplayName,
		"DisplayVersion":       version,
		"Publisher":            "DBBak",
		"DisplayIcon":          plan.ExePath,
		"InstallLocation":      plan.InstallDir,
		"UninstallString":      fmt.Sprintf("\"%s\" uninstall", plan.ExePath),
		"QuietUninstallString": fmt.Sprintf("\"%s\" uninstall --yes", plan.ExePath),
		"Comments":             "卸载默认保留数据目录中的元数据库、主密钥与备份文件",
	}
	for name, value := range stringsToSet {
		if err = key.SetStringValue(name, value); err != nil {
			return err
		}
	}
	if err = key.SetDWordValue("NoModify", 1); err != nil {
		return err
	}
	return key.SetDWordValue("NoRepair", 1)
}

func openBrowser(url string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(url)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, target, nil, nil, windows.SW_SHOWNORMAL)
}

func printSummary(plan installPlan, opts setupOptions, generated string) {
	fmt.Println()
	if opts.Repair {
		fmt.Println("DBBak 服务已刷新（数据与管理员账号保持不变）。")
	} else {
		fmt.Println("DBBak 安装完成。")
	}
	fmt.Printf("  控制台      http://%s/\n", plan.Addr)
	if !opts.NoAdmin {
		fmt.Printf("  管理员      %s\n", opts.AdminUser)
	}
	if generated != "" {
		fmt.Printf("  初始密码    %s\n", generated)
		fmt.Println("              该密码只显示这一次，请立即保存到密码管理器。")
	} else if opts.Repair {
		fmt.Println("  初始密码    保留原有密码（本次未重置）")
	}
	fmt.Printf("  程序目录    %s\n", plan.InstallDir)
	fmt.Printf("  数据目录    %s\n", plan.DataRoot)
	fmt.Printf("  服务日志    %s\n", filepath.Join(plan.LogDir, "dbbak.log"))
	fmt.Printf("  服务状态    dbbak.exe service status\n")
	fmt.Printf("  卸载        \"%s\" uninstall\n", plan.ExePath)
	fmt.Println("数据目录同时保存主密钥与元数据：请用停服快照方式备份，并将 master.key 与备份文件分开保护。")
}

func uninstallFromArgs(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		prefix   = fs.String("prefix", "", `安装目录，默认 %ProgramFiles%\DBBak`)
		dataRoot = fs.String("data-dir", "", `数据根目录，默认 %ProgramData%\DBBak`)
		purge    = fs.Bool("purge", false, "同时删除数据目录（元数据库、主密钥、备份文件），不可恢复")
		yes      = fs.Bool("yes", false, "跳过交互确认")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	plan, err := newInstallPlan(*prefix, *dataRoot, "127.0.0.1:8080", virtualServiceAccount)
	if err != nil {
		return err
	}
	if *purge && !*yes {
		return errors.New("--purge 会删除元数据库、主密钥和备份文件，必须同时加 --yes 确认")
	}
	if relaunched, err := relaunchElevated(append([]string{"uninstall"}, args...)); relaunched {
		return err
	}
	fmt.Println("即将卸载 DBBak：")
	fmt.Printf("  停止并删除服务 %s\n", serviceName)
	fmt.Printf("  删除程序文件   %s\n", plan.InstallDir)
	if *purge {
		files, bytes := dirStats(plan.DataRoot)
		fmt.Printf("  删除数据目录   %s（%d 个文件，%.1f MiB，不可恢复）\n", plan.DataRoot, files, float64(bytes)/(1<<20))
	} else {
		fmt.Printf("  保留数据目录   %s（元数据库、主密钥、备份文件、日志）\n", plan.DataRoot)
	}
	if !*yes {
		ok, err := confirm("确认卸载")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("已取消，未做任何更改。")
			return nil
		}
	}
	if err = stopAndDeleteService(); err != nil {
		return err
	}
	if err = removeInstalledFiles(plan.InstallDir); err != nil {
		return err
	}
	if err = registry.DeleteKey(registry.LOCAL_MACHINE, uninstallRegistryPath); err != nil && !errors.Is(err, registry.ErrNotExist) {
		log.Printf("警告: 删除卸载登记失败: %v", err)
	}
	if *purge {
		if err = purgeDataRoot(plan.DataRoot); err != nil {
			return err
		}
		fmt.Printf("数据目录已删除: %s\n", plan.DataRoot)
	} else {
		fmt.Printf("服务与程序已移除；数据、主密钥与备份仍保留在 %s\n", plan.DataRoot)
	}
	return nil
}

func stopAndDeleteService() error {
	manager, err := connectManager()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(serviceName)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			fmt.Printf("服务 %s 未注册，跳过停止。\n", serviceName)
			return nil
		}
		return err
	}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return err
	}
	if status.State != svc.Stopped {
		if _, err = service.Control(svc.Stop); err != nil {
			return fmt.Errorf("停止服务失败: %w", err)
		}
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if status, err = service.Query(); err != nil {
				break
			}
			if status.State == svc.Stopped {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if status.State != svc.Stopped {
			// 未停止就删服务会让文件句柄仍被占用，宁可中止也不留半卸载状态。
			return fmt.Errorf("服务在 90 秒内未停止（当前 %s），已中止卸载；请检查日志后重试", stateName(status.State))
		}
	}
	if err = service.Delete(); err != nil {
		return fmt.Errorf("删除服务失败: %w", err)
	}
	fmt.Printf("服务 %s 已停止并删除。\n", serviceName)
	return nil
}

func removeInstalledFiles(dir string) error {
	for _, name := range []string{"dbbak.exe", "dbbak.exe.new", "backup.ps1", "DBBakService.exe", "DBBakService.xml"} {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除 %s 失败: %w（若提示占用，请确认服务已停止）", path, err)
		}
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		log.Printf("提示: 安装目录 %s 仍有其他文件，已保留该目录: %v", dir, err)
	}
	return nil
}

// purgeDataRoot 只在目录明确属于 DBBak 时递归删除，避免误删用户自定义的上级目录。
func purgeDataRoot(root string) error {
	if !strings.Contains(strings.ToLower(filepath.Base(root)), "dbbak") {
		return fmt.Errorf("拒绝删除 %s：目录名不含 dbbak，无法确认是 DBBak 数据目录", root)
	}
	return os.RemoveAll(root)
}

func dirStats(root string) (files int, bytes int64) {
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	if err != nil {
		return files, bytes
	}
	return files, bytes
}

func serviceFromArgs(args []string) error {
	if len(args) != 1 {
		return errors.New("用法: dbbak service status|start|stop|restart")
	}
	manager, err := connectManager()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(serviceName)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return fmt.Errorf("服务 %s 未安装；请先运行 dbbak.exe setup", serviceName)
		}
		return err
	}
	defer service.Close()
	switch args[0] {
	case "status":
		status, err := service.Query()
		if err != nil {
			return err
		}
		fmt.Printf("服务 %s：%s（PID %d）\n", serviceName, stateName(status.State), status.ProcessId)
		return nil
	case "start":
		if err := service.Start(); err != nil {
			return fmt.Errorf("启动服务失败: %w", err)
		}
		fmt.Printf("服务 %s 已请求启动。\n", serviceName)
		return nil
	case "stop":
		if _, err := service.Control(svc.Stop); err != nil {
			return fmt.Errorf("停止服务失败: %w", err)
		}
		fmt.Printf("服务 %s 已请求停止。\n", serviceName)
		return nil
	case "restart":
		if status, err := service.Query(); err == nil && status.State != svc.Stopped {
			if _, err := service.Control(svc.Stop); err != nil {
				return fmt.Errorf("停止服务失败: %w", err)
			}
			deadline := time.Now().Add(90 * time.Second)
			for time.Now().Before(deadline) {
				if status, err = service.Query(); err != nil {
					break
				}
				if status.State == svc.Stopped {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if status.State != svc.Stopped {
				return fmt.Errorf("服务在 90 秒内未停止（当前 %s）", stateName(status.State))
			}
		}
		if err := service.Start(); err != nil {
			return fmt.Errorf("启动服务失败: %w", err)
		}
		fmt.Printf("服务 %s 已重启。\n", serviceName)
		return nil
	default:
		return fmt.Errorf("未知操作 %q；用法: dbbak service status|start|stop|restart", args[0])
	}
}

func stateName(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "已停止"
	case svc.StartPending:
		return "启动中"
	case svc.StopPending:
		return "停止中"
	case svc.Running:
		return "运行中"
	case svc.Paused:
		return "已暂停"
	case svc.PausePending:
		return "暂停中"
	case svc.ContinuePending:
		return "恢复中"
	default:
		return fmt.Sprintf("未知状态(%d)", uint32(state))
	}
}
