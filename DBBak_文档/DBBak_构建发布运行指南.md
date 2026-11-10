# DBBak 构建、发布与运行指南

> 适用版本：`0.2.0-phase5`
> 覆盖：Windows 原生、Linux 原生（systemd）、Docker / Docker Compose
> 本文只写可执行步骤和真实状态。**构建成功不等于运行验收**，各节末尾都标注了实际验证情况。

## 1 版本基线与产物形态

| 项 | 固定值 | 说明 |
| --- | --- | --- |
| Go | 1.27.3 | `go.mod` 语言版本为 1.26.0，构建使用 1.27.3 |
| MySQL 客户端 | 8.4.13 LTS | `mysql` / `mysqldump`，与已验证服务器同版本 |
| PostgreSQL 客户端 | 18.6 | `psql` / `pg_basebackup` |
| SQLite 驱动 | `modernc.org/sqlite` | 纯 Go，**不需要 CGO** |
| 前端 | `web/` 目录 | 编译期通过 `//go:embed` 打进二进制 |
| 时区 | 内嵌 `time/tzdata` | `cmd/dbbak/main.go` 空白导入；不依赖宿主机 zoneinfo |

产物是**单个静态二进制**，内含 Web 界面、SQLite 驱动和时区数据。运行期外部依赖只有数据库客户端工具（`mysql`/`mysqldump`/`psql`/`pg_basebackup`/`sqlcmd`），用于真实备份与连接探测。

> 时区数据由 `cmd/dbbak/main.go` 的 `import _ "time/tzdata"` 嵌入，二进制约增加 400 KB。缺少这个导入时，装过 Go 的开发机仍能靠 `GOROOT/lib/time/zoneinfo.zip` 回退正常工作，干净发行机会在保存任务时报“无效时区”；不要把开发机上的正常表现当作发行验证。

| 部署形态 | 服务管理 | 当前验证状态 |
| --- | --- | --- |
| Windows 原生 | 前台进程 / 原生 Windows 服务（`dbbak setup`） | 前台与构建已验证；**服务安装在本机未运行（需管理员权限）** |
| Linux 原生 | 前台进程 / systemd | 仅交叉编译；**未在 Linux 启动** |
| Docker | Compose + 持久卷 | 镜像未构建、容器未运行 |

## 2 构建

### 2.1 前置条件

- Go 1.27.3（本仓库自带便携工具链时可用 `.tools/go/bin/go.exe`，未加入 PATH）
- 无需 CGO、无需 Node.js、无需 Docker
- 仓库根目录即 Go 模块根，`go.mod` 在根下，构建命令直接在根执行

```powershell
go version          # 应为 go1.27.3
```

### 2.2 本平台构建

Windows：

```powershell
go mod download
go test ./...
go vet ./...
go build -trimpath -o .test-runtime\dbbak.exe .\cmd\dbbak
.\.test-runtime\dbbak.exe version
```

Linux / macOS：

```bash
go mod download
go test ./...
go vet ./...
go build -trimpath -o dist/dbbak ./cmd/dbbak
./dist/dbbak version
```

`-trimpath` 去掉本机路径，产物可复现且不泄露目录结构。

### 2.3 交叉构建

```bash
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -o dist/dbbak-linux-amd64   ./cmd/dbbak
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o dist/dbbak-windows-amd64.exe ./cmd/dbbak
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -o dist/dbbak-linux-arm64   ./cmd/dbbak
```

必须保持 `CGO_ENABLED=0`：SQLite 驱动是纯 Go 实现，开启 CGO 会让交叉编译失效。交叉构建只证明可编译，不能替代目标平台运行测试。

### 2.4 可复现构建脚本

两个脚本只做同一件事，输出目录和校验和格式一致：

```powershell
# Windows
.\packaging\build.ps1
.\packaging\build.ps1 -Version 0.2.0-phase2 -GOOS linux -GOARCH amd64
```

```bash
# Linux / macOS / Git Bash
./packaging/build.sh
VERSION=0.2.0-phase2 GOOS=windows GOARCH=amd64 ./packaging/build.sh
```

输出：

```text
dist/<version>/<goos>_<goarch>/dbbak[.exe]
dist/<version>/<goos>_<arch>/SHA256SUMS
```

脚本内置：`GOTOOLCHAIN=go1.27.3`、`GOWORK=off`、`CGO_ENABLED=0`、`SOURCE_DATE_EPOCH=0`，并在结束时打印“仍需在目标系统执行运行、备份和恢复验收”。先写临时文件再原子改名，失败不会留下半成品。

### 2.5 常见构建失败

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `go: cannot find main module` | 在子目录执行 | 回到仓库根，`go.mod` 在根目录 |
| `pattern web/index.html: no matching files` | `web/` 被移动或改名 | 目录必须与 `embed.go` 同级 |
| `go: download ... x509` / 超时 | 模块代理不可达 | 设 `GOPROXY=https://goproxy.cn,direct` |
| `docker build` 中 `go mod download` 超时（`dial tcp 142.250.x.x:443: i/o timeout`） | 构建容器默认走 `proxy.golang.org`，国内网络不可达 | Dockerfile 已内置 `ARG GOPROXY`（默认 `https://goproxy.cn`）；需换源时用 `--build-arg GOPROXY=...` |
| `go: toolchain not available` | 本机 Go 版本过低 | 安装 1.27.3，或让 `GOTOOLCHAIN` 自动下载 |
| Windows 下 `Access is denied` 覆盖 `dbbak.exe` | 旧进程仍在运行 | 先停止服务，再构建 |

## 3 发布

### 3.1 版本号同步点

改版本号必须同时改这五处，否则产物、镜像标签和 `version` 输出会互相矛盾：

| 位置 | 当前值 |
| --- | --- |
| `cmd/dbbak/main.go` `const version` | `0.2.0-phase5` |
| `deploy/docker/Dockerfile` `LABEL org.opencontainers.image.version` | `0.2.0-phase5` |
| `deploy/docker/compose.yaml` `image` | `dbbak:0.2.0-phase5` |
| `packaging/build.sh` 默认 `VERSION` | `0.2.0-phase5` |
| `packaging/build.ps1` 参数默认值 | `0.2.0-phase5` |

### 3.2 发布前检查清单

```bash
go vet ./...
go test ./...
go build ./...
git ls-files --eol | grep -E 'w/mixed' || echo "行尾正常"
```

- 前端改动后必须重新构建二进制，`web/` 是编译期嵌入，不是运行时读取。
- `DBBak_文档/DBBak_兼容性矩阵.md` 中标注的能力要与本次发布一致。
- 未验证的能力（SQL Server、日志链、Agent 控制端）不得在发布说明里写成“支持”。

### 3.3 产物清单

一次发布建议包含：

```text
DBBak-Setup-<version>-windows-amd64.exe    # 安装包：Inno Setup 图形向导，双击安装并注册服务
dbbak-<version>-windows-amd64.zip          # 便携包：解压后前台 dbbak.exe serve
  dbbak.exe
  README.md
  LICENSE
  deploy/windows/backup.ps1, upgrade.ps1
dbbak-<version>-linux-amd64.tar.gz
  dbbak
  README.md
  LICENSE
  deploy/linux/install.sh, upgrade.sh, uninstall.sh, backup.sh, dbbak.service, dbbak.env.example
SHA256SUMS
```

Windows 安装包由 `packaging/build-installer.ps1` 生成，实际是 `packaging/dbbak.iss` 经 Inno Setup 编译的结果：

```powershell
.\packaging\build-installer.ps1 -Version 0.2.0-phase2
```

脚本会先 `go vet`/`go test`，再构建 windows/amd64，校验“产物自报版本 == 包名版本”，然后把程序目录整理成暂存区（`dbbak.exe` + `backup.ps1` + 自带 `tools`），用 Inno Setup 编译 `.iss`，最后写 `SHA256SUMS`。构建期需要 Inno Setup 7 的 `ISCC.exe`，缺失时脚本自动下载固定版本 7.1.1 并校验 SHA-256；`-NoInstaller` 可跳过图形安装包只出便携包。安装逻辑仍在程序内部的 `setup` 子命令里，安装器只负责收集目录与密码并调用它。

自带工具的打包规则：

- `packaging/collect-tools.ps1` 从 `.tools\mysql-8.4.13-winx64\bin` 只挑必需文件（`mysql.exe`、`mysqldump.exe`、`mysqlbinlog.exe`、`libcrypto-3-x64.dll`、`libssl-3-x64.dll`，约 29 MB），而不是整个 850 MB 的 bin 目录。依赖清单来自对三个 exe 导入表的实际读取，不是猜的。
- 复制后立即执行 `mysqldump --version` 冒烟验证；工具跑不起来时构建直接失败，不会产出“装完也用不了”的安装包。
- MySQL 的 `LICENSE` 一起打包为 `LICENSE-mysql.txt`（GPLv2 再分发要求），同时生成 `README-tools.txt` 记录来源、版本与文件清单。
- 换客户端版本：`-MySQLClientBin <bin 目录>`；内网已有客户端、不需要自带时：`-NoBundledTools`。

**许可证提醒**：MySQL Community 为 GPLv2，随安装包分发二进制时需同时提供许可证，是否触发 GPL 的传染性要求需要法务确认；不想分发时可加 `-NoBundledTools`，改为要求客户机自备客户端。

### 3.4 校验与签名

当前只有 SHA-256，没有代码签名：

```bash
sha256sum -c SHA256SUMS
```

Windows 下用 `Get-FileHash -Algorithm SHA256` 比对。**没有 Authenticode 签名，也不是 MSI**：`DBBak-Setup-*.exe` 是 Inno Setup 生成的图形安装程序（未签名），首次运行可能出现 SmartScreen 提示（选“更多信息 → 仍要运行”）。Linux 需要自行 `chmod +x`。

### 3.5 回滚

- Windows 服务：`upgrade.ps1` 会停服、生成元数据快照，把旧的可执行文件复制到 `C:\ProgramData\DBBak\maintenance`，再替换并启动；回滚时停服务、换回旧文件、启服务。
- Linux：`upgrade.sh` 先停服、调用 `dbbak-backup` 做元数据快照，再把旧二进制 `cp` 成 `/var/lib/dbbak/maintenance/dbbak-binary-<UTC时间戳>`，失败时明确打印旧程序路径与归档位置。
- Docker：`docker compose down` 后把 `image:` 改回旧标签重新 `up`；数据在命名卷里，不受镜像回滚影响。

## 4 公共运行配置

### 4.1 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DBBAK_ADDR` | `127.0.0.1:8080` | 监听地址，默认仅回环 |
| `DBBAK_DATA_DIR` | `.dbbak` | 元数据、默认密钥、默认备份根 |
| `DBBAK_DATABASE` | `<数据目录>/dbbak.db` | SQLite 文件，需与 `-wal`/`-shm` 同目录 |
| `DBBAK_BACKUP_ROOT` | `<数据目录>/backups` | 备份授权根目录 |
| `DBBAK_MASTER_KEY_FILE` | `<数据目录>/master.key` | AES-256 主密钥，**必须独立备份** |
| `DBBAK_MYSQL` / `DBBAK_MYSQLDUMP` | `mysql` / `mysqldump` | MySQL 客户端 |
| `DBBAK_PSQL` / `DBBAK_PG_BASEBACKUP` | `psql` / `pg_basebackup` | PostgreSQL 客户端 |
| `DBBAK_SQLCMD` | `sqlcmd` | SQL Server 适配器工具 |
| `DBBAK_SQLSERVER_ENABLED` | 未设置（关闭） | 设为 `1` 才允许创建 SQL Server 任务；须先完成异名恢复验收 |
| `DBBAK_SESSION_TTL` | `12h` | 会话有效期 |
| `DBBAK_SCHEDULER_INTERVAL` | `2s` | 到期任务扫描间隔 |
| `DBBAK_BACKUP_TIMEOUT` | `30m` | 单次备份超时 |
| `DBBAK_ADMIN_USERNAME` | `admin` | 初始管理员用户名；`serve` 自动建号与 `init-admin` 共用 |
| `DBBAK_ADMIN_PASSWORD` | 无 | 初始管理员密码，至少 6 个字符。`serve` 首次启动（`users` 表为空）时自动建号，已有用户则忽略；`init-admin` 也用该变量 |

### 4.2 首次初始化

两种方式，都只依赖环境变量传密码，不落盘、不写日志：

- **`serve` 自动建号**（容器化部署推荐）：`users` 表为空且已设置 `DBBAK_ADMIN_PASSWORD` 时，启动即创建首个管理员；已有用户时完全忽略该变量，因此重启不会重置密码。详见 7.4。
- **`init-admin` 手动初始化**（Windows 服务、systemd、以及给已运行实例补账号）：严格模式，重复执行会因用户已存在而失败。

```powershell
$env:DBBAK_DATA_DIR = "D:\dbbak-data"
$env:DBBAK_ADMIN_PASSWORD = Read-Host "初始管理员密码"
.\dbbak.exe init-admin
Remove-Item Env:\DBBAK_ADMIN_PASSWORD
```

```bash
export DBBAK_DATA_DIR=/var/lib/dbbak
read -rsp '初始管理员密码: ' DBBAK_ADMIN_PASSWORD; echo
./dbbak init-admin
unset DBBAK_ADMIN_PASSWORD
```

### 4.3 主密钥

`master.key` 用于解密数据源凭据，与 SQLite 分离存放。**丢失即无法解密已保存的数据库密码**，只能重新录入数据源；泄露等于所有凭据泄露。备份时必须与元数据分开保护。

### 4.4 启动后自检

```bash
curl -fsS http://127.0.0.1:8080/health/live     # 进程存活
curl -fsS http://127.0.0.1:8080/health/ready    # 依赖就绪，返回 {"status":"ready"}
```

浏览器打开 `http://127.0.0.1:8080/`，用 `admin` 登录。关闭浏览器不影响后台调度，只有停止进程才会中断。

## 5 Windows 运行

### 5.1 前台运行（开发、验证）

```powershell
.\scripts\run-dev.ps1 -Build      # 使用 .test-runtime 数据与 .tools 便携工具链
```

等价手工命令：

```powershell
$env:DBBAK_DATA_DIR = "D:\dbbak-data"
$env:DBBAK_BACKUP_ROOT = "D:\dbbak-backups"
$env:DBBAK_MYSQL = "C:\tools\mysql-8.4.13\bin\mysql.exe"
$env:DBBAK_MYSQLDUMP = "C:\tools\mysql-8.4.13\bin\mysqldump.exe"
$env:DBBAK_ADDR = "127.0.0.1:8080"
.\dbbak.exe serve
```

`serve` 是前台进程，Ctrl+C 即停止；关终端等于停服务。

密码查看：
```
Get-Content .\.test-runtime\admin-password
```

### 5.2 作为 Windows 服务（图形安装包）

Windows 服务由 `dbbak.exe` **自身**实现（`golang.org/x/sys/windows/svc`）：服务进程就是 `dbbak.exe`，服务模式与控制台模式共用同一套业务启动逻辑。

`DBBak-Setup-<version>-windows-amd64.exe` 是 Inno Setup 7 编译的**标准 Windows 安装向导**（简体中文界面），不是控制台脚本：

| 页面 | 内容 |
| --- | --- |
| 欢迎 | 版本号与安装提示 |
| 选择目标位置 | 安装目录，默认 `C:\Program Files\DBBak`，可用「浏览」改到其他磁盘 |
| 选择数据目录 | 默认 `C:\ProgramData\DBBak`，存元数据库、主密钥与备份；与程序目录相同时拒绝继续 |
| 管理员与监听设置 | 监听地址（默认 `127.0.0.1:8080`）、管理员用户名、初始密码、确认密码；首次安装少于 6 个字符或两次不一致时拒绝继续；重装时密码可留空表示保留原密码 |
| 准备安装 → 安装 | 复制程序与自带工具后调用 `dbbak.exe setup --yes ...` 注册并启动服务，并等待 `/health/ready` |
| 完成 | 可勾选打开 DBBak 控制台 |

安装器把密码写入临时文件，用 `--admin-password-file` 传给安装程序，避免明文出现在进程命令行；`setup` 读完即删。服务注册或就绪等待失败时，安装器显示安装日志尾部并中止，不会留下装了一半、没有服务的目录。

系统里已存在 `DBBak` 服务时，安装器改为覆盖重装：先提示影响范围，确认后先 `net stop` 停服（不依赖快照脚本版本），再调用 `backup.ps1 -Yes` 做停服快照，然后覆盖程序与 `tools`、重新注册并启动服务。数据目录、任务、备份文件与管理员密码都保留；向导会预填已登记的数据目录、监听地址与管理员用户名。这既是升级路径，也是“程序或工具损坏后的修复”路径。

停服发生在**文件替换之前**（`CurStepChanged(ssInstall)`），而不是启动阶段：`{app}` 常量要到用户过完目录页才建立，在 `InitializeSetup` 里展开会直接报 `An attempt was made to expand the "app" constant before it was initialized`。停服脚本的目录取自注册表 `ImagePath` 而不是 `{app}`，所以即使这次把安装目录改到别处，也会先停掉旧目录里那个服务。`/VERYSILENT` 下确认框会自动按“继续”处理，适合批量升级。

构建依赖 Inno Setup 7（`ISCC.exe`）：`packaging/build-installer.ps1` 按 `.tools\innosetup` → `%ProgramFiles%\Inno Setup 7` → `Inno Setup 6` 的顺序查找，都没有就下载固定版本 7.1.1 到 `.tools\innosetup`、校验 SHA-256 后静默安装。加 `-NoInstaller` 可只产出便携包。

命令行与无人值守仍然可用（便携包或安装后的 `C:\Program Files\DBBak\dbbak.exe` 具备同样能力）：

```powershell
dbbak.exe setup --dry-run
dbbak.exe setup --yes --admin-password <至少6字符>
dbbak.exe setup --dry-run --prefix D:\Apps\DBBak --data-dir D:\DBBakData
# 服务已存在时用于修复/补装工具：保留数据与管理员账号
dbbak.exe setup --yes --no-browser
```

无参数运行 `dbbak.exe` 时按“是否独占控制台”区分：双击进入控制台向导，从 cmd / PowerShell 无参运行只打印用法，避免脚本调用意外弹出 UAC。

#### 安装失败怎么定位

安装失败时向导显示退出码与安装日志尾部，并把日志副本保存到 `<数据目录>\logs\setup-error.log`（默认 `C:\ProgramData\DBBak\logs\setup-error.log`）。这份日志按步骤记录：创建目录 → 复制程序与自带工具 → 数据目录授权 → 创建管理员 → 注册服务 → 启动服务 → readiness 检查，第一条失败就是原因所在。服务进程自身起不来的细节在 `<数据目录>\logs\dbbak.log`。

按出现频率排列的常见原因：

- **监听端口被占用**：安装前会试绑一次监听地址，报错直接给出占用端口的进程名与 PID。换一个监听端口重装，或结束占用进程后重试。
- **旧服务仍在运行**：注册表里的 `DBBak` 服务仍指向旧安装目录（换盘、改目录名后重装）时，安装器按 `ImagePath` 停掉旧服务、刷新命令行接管，不需要先手工卸载。
- **程序文件被占用**：`dbbak.exe` 被前台实例或旧服务占用，先 `dbbak.exe service stop` 或结束进程再装。
- **日志为空**：说明 `dbbak.exe setup` 在写日志前就退出了，通常是被安全软件拦截或程序文件损坏，用便携包里的 `dbbak.exe setup --dry-run` 手工跑一次即可看到真实报错。

常用参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--prefix` | `%ProgramFiles%\DBBak` | 程序目录，拒绝卷根目录 |
| `--data-dir` | `%ProgramData%\DBBak` | 数据根目录，必须与程序目录不同 |
| `--addr` | `127.0.0.1:8080` | 监听地址，写入服务命令行的 ImagePath |
| `--account` | `NT SERVICE\DBBak` | 仅支持虚拟账号或 `LocalSystem` |
| `--admin-user` | `admin` | 初始管理员用户名 |
| `--admin-password` | 随机生成 | 少于 12 字符直接拒绝；不推荐（会出现在进程表） |
| `--admin-password-file` | 无 | 从文件读取密码，图形安装器用，读完即删 |
| `--no-admin` | 否 | 跳过管理员初始化，之后用 `init-admin` |
| `--no-start` | 否 | 只注册服务不启动（排障） |
| `--no-browser` | 否 | 安装完成后不打开浏览器 |
| `--no-uninstall-key` | 否 | 不写“设置 → 应用”卸载登记，由外部安装器登记时使用 |
| `--log-file` | 无 | 把安装输出写入文件，图形安装器失败时回看 |
| `--mysql-path` | 无 | `mysql.exe` 绝对路径；连同所在目录写入系统环境 `Path`，并设置 `DBBAK_MYSQL` / `DBBAK_MYSQLDUMP`（仅在不用自带工具时需要） |
| `--dry-run` | 否 | 只打印计划，不需要管理员权限 |

安装后布局：

| 路径 | 内容 |
| --- | --- |
| `C:\Program Files\DBBak\dbbak.exe` | 程序本体，同时是服务进程 |
| `C:\Program Files\DBBak\tools\` | 自带的 MySQL 客户端（mysql / mysqldump / mysqlbinlog + OpenSSL DLL + 许可证） |
| `C:\Program Files\DBBak\backup.ps1` | 停服一致性快照脚本，安装器重装前也会调用它 |
| `C:\ProgramData\DBBak\dbbak.db` | SQLite 元数据（含 `-wal`/`-shm`） |
| `C:\ProgramData\DBBak\key\master.key` | 独立主密钥 |
| `C:\ProgramData\DBBak\backups\` | 备份仓库 |
| `C:\ProgramData\DBBak\logs\dbbak.log` | 服务日志，10 MiB 轮转，保留 5 份 |
| `C:\ProgramData\DBBak\maintenance\` | `backup.ps1` 产生的停服快照与旧二进制 |

服务名 `DBBak`，延迟自动启动，失败后由 SCM 按 10s/30s/60s 重启。默认账号 `NT SERVICE\DBBak`；数据目录 ACL 只保留 SYSTEM、Administrators 与该账号，切断 `ProgramData` 的继承权限。凭据不写入服务命令行，也不写入注册表。

服务命令行（`sc qc DBBak` 可见）：

```text
"C:\Program Files\DBBak\dbbak.exe" serve --addr 127.0.0.1:8080 --data-dir C:\ProgramData\DBBak --backup-root C:\ProgramData\DBBak\backups --master-key-file C:\ProgramData\DBBak\key\master.key
```

日常操作：

```powershell
Get-Service DBBak                              # 或用 dbbak.exe service status
dbbak.exe service start | stop | restart       # 需管理员权限

# 升级：停服 → 元数据快照 → 保留旧二进制 → 替换 → 启动 → readiness 校验
.\deploy\windows\upgrade.ps1 -BinaryPath <新 dbbak.exe> -Yes

# 停服一致性快照（含主密钥，注意异地保护）
.\deploy\windows\backup.ps1 -Yes
# 服务还在跑、又想一条命令停服并快照时加 -StopService；数据目录非默认时用 -DataRoot 指定
.\deploy\windows\backup.ps1 -Yes -StopService -DataRoot D:\DBBakData

# 卸载：停止并删除服务与程序，保留数据、主密钥与备份
"C:\Program Files\DBBak\dbbak.exe" uninstall

# 彻底删除（同时删除数据目录，不可恢复）
"C:\Program Files\DBBak\dbbak.exe" uninstall --purge --yes
```

卸载也会移除“设置 → 应用”里的卸载登记。`--purge` 必须同时给 `--yes`，且仅当目录名含 `dbbak` 时才递归删除。

> 未验证：本机当前会话无管理员权限，`setup` 的**真实安装、ACL 授权、服务启动、readiness 通过与卸载流程均未执行**。已验证的是：安装计划生成（`--dry-run`）、参数与环境变量优先级、前台 `serve` 启动与 `/health/live`、`/health/ready`、首页返回 200、未提权时的错误提示。首次在目标机安装时请按第 9 节逐项确认，重点确认 `NT SERVICE\DBBak` 的 ACL 授权是否成功。

自带客户端工具（装完即用）：

安装包把 MySQL 客户端一起装进程序目录，客户机**不需要另装 mysql/mysqldump，也不需要改 PATH 或设置 `DBBAK_*`**：

| 路径 | 内容 |
| --- | --- |
| `C:\Program Files\DBBak\tools\mysql.exe` | 连接测试与恢复执行 |
| `C:\Program Files\DBBak\tools\mysqldump.exe` | 逻辑全量 |
| `C:\Program Files\DBBak\tools\mysqlbinlog.exe` | binlog 归档（物理路线） |
| `C:\Program Files\DBBak\tools\libcrypto-3-x64.dll`、`libssl-3-x64.dll` | 上述工具运行时必需的同目录 OpenSSL 库 |
| `C:\Program Files\DBBak\tools\LICENSE-mysql.txt`、`README-tools.txt` | MySQL GPLv2 许可证与工具清单 |

工具查找顺序固定为：`DBBAK_*` 显式配置 → 程序目录 `tools` → 命令名交给系统 `PATH`。所以运维要换客户端版本时，设置 `DBBAK_MYSQL` / `DBBAK_MYSQLDUMP` 即可覆盖自带版本。启动日志会写一行实际使用的工具路径，能力矩阵也会显示识别到的版本，可用于核对。

`psql` / `pg_basebackup` / `sqlcmd` 目前**不随包分发**（PostgreSQL 客户端需要 VC++ 运行库和较新的 Windows，sqlcmd 属微软专有组件）。缺这些工具时能力矩阵显示 `prerequisite_missing` 并点名缺失的工具，需要时用 `DBBAK_PSQL` / `DBBAK_PG_BASEBACKUP` / `DBBAK_SQLCMD` 指定路径。

命令行安装也可以显式指定客户端：

```powershell
dbbak.exe setup --dry-run --mysql-path D:\tools\mysql-8.4.13-winx64\bin\mysql.exe
```

`--mysql-path` 会把路径写入机器级 `DBBAK_MYSQL` / `DBBAK_MYSQLDUMP` 并把 bin 目录追加到机器 `Path`。只有在不用自带工具时才需要它。

重装与修复：程序或 `tools` 目录损坏、需要补装客户端时，直接再次运行安装包即可。安装器会先停服并做停服快照（`backup.ps1`），再覆盖程序与工具、重新注册服务；数据目录、已有任务、备份文件和管理员密码都保持不变（密码留空表示不重置）。旧版本安装包只能先卸载再装，新版本起支持覆盖重装。

排障要点：

- `icacls` 报「找不到账号」时，说明服务名与账号映射未建立；先确认服务已注册（`sc qc DBBak`），或改用 `--account LocalSystem` 重装（权限更宽，需自行评估）。
- `setup` 在启动阶段失败会自动回滚服务注册，并打印 `dbbak.log` 末尾 20 行；数据目录与主密钥保留，修正后可重跑。
- 出现 `TOOL_MISSING` 时看两处提示：「部署与执行器」页会写清缺哪个工具、服务实际用的路径；`C:\ProgramData\DBBak\logs\dbbak.log` 记录完整路径与工具原始 stderr。
- 只想换程序时不要重跑 `setup`（服务已存在会被拒绝），用 `upgrade.ps1` 或手工停服替换。

## 6 Linux 运行

### 6.1 手工前台运行

二进制就位（例如 `/usr/local/bin/dbbak`）后，可以用专用账号前台启动，便于观察日志：

```bash
sudo install -d -o dbbak -g dbbak -m 0700 /var/lib/dbbak
sudo -u dbbak env \
  DBBAK_DATA_DIR=/var/lib/dbbak \
  DBBAK_BACKUP_ROOT=/var/lib/dbbak/backups \
  DBBAK_MASTER_KEY_FILE=/var/lib/dbbak/key/master.key \
  DBBAK_ADDR=127.0.0.1:8080 \
  /usr/local/bin/dbbak serve
```

### 6.2 一键安装（systemd）

```bash
sudo ./deploy/linux/install.sh /path/to/dbbak        # 交互确认
sudo ./deploy/linux/install.sh --yes /path/to/dbbak  # 无人值守
```

脚本做的事：

1. 创建无登录 shell 的系统用户/组 `dbbak`。
2. 创建 `/var/lib/dbbak`、`backups`、`key`、`maintenance`，权限 `0700`，属主 `dbbak`。
3. 安装 `/usr/local/bin/dbbak`、`/usr/local/sbin/dbbak-backup`（停服快照工具）、`/etc/systemd/system/dbbak.service`。
4. 若 `/etc/dbbak/dbbak.env` 不存在则从样例安装（`0600` root），**已存在则保留不覆盖**。
5. `systemctl enable dbbak.service` 后再 `systemctl start`，并在 30 秒内轮询 `/health/ready`，失败直接报错退出。

若已存在安装，脚本会拒绝执行并提示改用 `upgrade.sh`。

### 6.3 配置

```bash
sudo vi /etc/dbbak/dbbak.env      # 0600 root:root
sudo systemctl restart dbbak
```

`dbbak.env` 里**不要写** `DBBAK_ADMIN_PASSWORD` 或任何数据库密码，样例文件已注明。若把 `DBBAK_BACKUP_ROOT` 移出 `/var/lib/dbbak`，必须同步在 unit 里加 `ReadWritePaths=`，否则 `ProtectSystem=strict` 会拒绝写入。

unit 已启用 `NoNewPrivileges`、`ProtectSystem=strict`、`PrivateTmp`、`CapabilityBoundingSet=`（清空）等加固项，并限制 `LimitNOFILE=65536`。

### 6.4 运维命令

```bash
systemctl status dbbak
journalctl -u dbbak -f
curl -fsS http://127.0.0.1:8080/health/ready
```

### 6.5 升级、快照与卸载

```bash
# 升级：停服 → 备份 SQLite/WAL/主密钥 → 保留旧二进制 → 装新二进制 → readiness 校验
sudo ./deploy/linux/upgrade.sh /path/to/new/dbbak

# 停服一致性快照（服务必须已停止，运行中复制 dbbak.db 不满足 WAL 一致性）
sudo systemctl stop dbbak
sudo /usr/local/sbin/dbbak-backup /var/lib/dbbak/maintenance
sudo systemctl start dbbak

# 卸载：保留 /var/lib/dbbak 与 /etc/dbbak
sudo ./deploy/linux/uninstall.sh --yes
```

快照产物是 `0600` 的 `tar.gz` 加 `.sha256`，内含 SQLite、`-wal`/`-shm` 与独立主密钥。**归档含主密钥，必须与备份文件分开异地保护。**

> 未验证：Linux 仅完成 `GOOS=linux` 交叉编译。**systemd 单元、安装脚本、权限、真实 MySQL 备份均未在 Linux 上运行过。**

## 7 Docker 运行

### 7.1 镜像构成

`deploy/docker/Dockerfile` 两阶段：

| 阶段 | 基础镜像 | 作用 |
| --- | --- | --- |
| build | `golang:1.27.3-bookworm` | 只复制 `go.mod`/`go.sum`、`embed.go`、`cmd/`、`internal/`、`web/`，`CGO_ENABLED=0` 构建 |
| runtime | `mysql:8.4.13` | 自带 `mysql`/`mysqldump`，安装 curl 与 `sqlcmd`（微软 mssql-tools18），创建 UID/GID 10001 非 root 用户 |

两个基础镜像都带 `${BASE_REGISTRY}` 前缀，默认空值即 `docker.io/library/...`。构建主机配置了 `registry-mirrors`（镜像代理返回 503 会直接让构建失败）或使用内网私有仓库时，用 `--build-arg BASE_REGISTRY=<前缀>/` 改走可达来源，不必改 dockerd 也不必改版本标签。

运行时目录：`/var/lib/dbbak`（数据）、`/backups`（仓库）、`/etc/dbbak-key`（主密钥），三者都声明为 `VOLUME`。默认 `CMD ["serve"]`，监听 `0.0.0.0:8080`。

SQL Server 路线完全依赖 `sqlcmd`（`internal/backup/sqlserver.go`：连接探测 `SELECT @@VERSION`、`BACKUP DATABASE`、`msdb` 元数据查询都走它），而 `mysql:8.4.13` 只带 MySQL 客户端。所以镜像在 runtime 阶段从微软 RHEL 9 源装 `msodbcsql18` 与 `mssql-tools18`，并把 `/opt/mssql-tools18/bin/sqlcmd` 软链到 `/usr/local/bin/sqlcmd`（`config.Load` 不做 tools 目录解析，只按 PATH 查找）。

- 该步骤需访问 `packages.microsoft.com`，国内网络可能很慢；构建失败先确认容器能否出网，不要改成跳过校验。
- `ACCEPT_EULA=Y` 表示接受微软命令行工具许可条款；再分发镜像前需单独做许可审查。
- 只用 MySQL/PostgreSQL 的部署可用 `--build-arg INSTALL_SQLCMD=0` 跳过，镜像小约 100MB。

### 7.2 构建镜像

```bash
docker build -f deploy/docker/Dockerfile -t dbbak:0.2.0-phase2 .
```

`build` 阶段的 `GOPROXY` 默认是 `https://goproxy.cn`（不带 `direct` 回退：镜像缺失模块时立即失败，不退回 VCS 拉取），不依赖宿主机的 Go 环境配置。若网络直连官方源或使用企业 Go 私服：

```bash
docker build --build-arg GOPROXY=https://proxy.golang.org,direct \
  -f deploy/docker/Dockerfile -t dbbak:0.2.0-phase2 .
```

模块代理仍不可达时，先确认构建容器能否出网。`docker build --network host` 只共用宿主网络命名空间，代理地址本身不通时无效；这种情况应改为配置可达的 `GOPROXY`，而不是关闭校验。

基础镜像拉取失败（如 `unexpected status from HEAD request to https://<mirror>/v2/library/mysql/manifests/...: 503 Service Unavailable`）属于镜像代理不可用，不是 Dockerfile 问题。改用可达来源：

```bash
docker build --build-arg BASE_REGISTRY=192.168.10.221/ \
  -f deploy/docker/Dockerfile -t dbbak:0.2.0-phase2 .
```

私有仓库只做缓存、没有反向代理官方库时应写全路径，例如 `--build-arg BASE_REGISTRY=192.168.10.221/library/`，并按实际镜像名调整两个 `FROM` 的仓库层级。

runtime 阶段的 `sqlcmd` 默认安装，需要关闭时：

```bash
docker build --build-arg INSTALL_SQLCMD=0 -f deploy/docker/Dockerfile -t dbbak:mysql-only .
```

### 7.3 Compose 启动

```bash
cd deploy/docker
docker compose up -d --build
docker compose ps
docker compose logs -f dbbak
```

`compose.yaml` 的固定行为：

- 只把 `127.0.0.1:8080` 映射到宿主，不对外暴露。
- `read_only: true` + `tmpfs /tmp`，根文件系统只读；可写位置只有三个命名卷。
- `user: 10001:10001`、`cap_drop: ALL`、`no-new-privileges`。
- `healthcheck` 调 `/health/ready`，`start_period: 15s`。
- 卷名固定：`dbbak-production-data`、`dbbak-production-backups`、`dbbak-production-master-key`。

### 7.4 初始化管理员

**推荐：首次启动自动建号。** 在 compose 环境里给出 `DBBAK_ADMIN_PASSWORD`，服务启动时会检查 `users` 表：

| 情况 | 行为 |
| --- | --- |
| 未设置 `DBBAK_ADMIN_PASSWORD` | 跳过，不碰 `users` 表；容器正常启动但无法登录 |
| 元数据库无任何用户 | 用该密码建首个管理员（用户名取 `DBBAK_ADMIN_USERNAME`，默认 `admin`），日志打印「已创建初始管理员」 |
| 已有用户 | 完全忽略，不比较也不覆盖密码；日志打印「管理员已存在，忽略 DBBAK_ADMIN_PASSWORD」 |
| 密码不合规（少于 6 字符或超过 72 字节） | 启动失败并退出，不会建弱口令账号 |

```bash
cd deploy/docker
# 用宿主机环境或 .env 注入，不要写进 compose.yaml 明文
export DBBAK_ADMIN_PASSWORD='你的密码'
docker compose up -d
docker compose logs dbbak | grep 管理员
```

因为「已有用户就忽略」，这个变量可以长期保留而不会在重启时重置密码。**但密码会以明文存在于容器环境变量中**（`docker inspect` 可见），首次登录后建议从 compose 或 `.env` 中删除。

**备选：手动执行 `init-admin`。** 需要在已运行的部署上补建账号时使用，必须复用同一组卷，否则会写到另一个空数据库：

```bash
read -rsp '初始管理员密码: ' DBBAK_ADMIN_PASSWORD; echo
export DBBAK_ADMIN_PASSWORD
docker compose -f deploy/docker/compose.yaml run --rm \
  -e DBBAK_ADMIN_PASSWORD dbbak init-admin
unset DBBAK_ADMIN_PASSWORD
```

`init-admin` 是严格模式：已有用户时直接报错退出，不做覆盖。

### 7.5 路径语义（容易踩）

- 容器内的 `/backups` 是**执行器**路径，`mysqldump`/`pg_basebackup` 由容器进程写出，没问题。
- **SQL Server 例外**：`BACKUP DATABASE` 的目标路径必须能被 SQL Server 实例访问，容器内路径对它不可见。必须用 `storage_targets.server_root` 映射到数据库主机上的真实目录。
- 镜像默认带 `sqlcmd`；用 `INSTALL_SQLCMD=0` 构建的镜像没有它，SQL Server 适配器会报 `TOOL_MISSING`，不会伪造成功。用 `docker exec dbbak sqlcmd -?` 可确认工具是否在 PATH 中。

### 7.6 升级与数据保留

```bash
docker compose down            # 不删卷
docker compose up -d --build   # 重新构建启动
```

`down -v` 会**删除全部卷**，包括主密钥和备份，等同于不可恢复的数据丢失，除非你确知自己在做什么。

> 未验证：本机没有 Docker，**镜像未构建、容器未启动**；非 root、只读根、卷持久化、healthcheck 全部未实测。

## 8 三种方式对照

| 维度 | Windows 服务 | Linux systemd | Docker |
| --- | --- | --- | --- |
| 服务管理 | 原生 Windows 服务 + `dbbak.exe service` | `systemctl` | Compose / 容器运行时 |
| 数据目录 | `C:\ProgramData\DBBak` | `/var/lib/dbbak` | 命名卷 |
| 主密钥 | `ProgramData\DBBak\key` | `/var/lib/dbbak/key` | 独立卷 `/etc/dbbak-key` |
| 配置 | 服务命令行参数（`serve --data-dir …`） | `/etc/dbbak/dbbak.env` | 环境变量 + 卷 |
| 日志 | `ProgramData\DBBak\logs` | `journalctl -u dbbak` | `docker compose logs` |
| 停服快照 | `backup.ps1` | `dbbak-backup` | 停容器后备份卷 |
| 默认监听 | `127.0.0.1:8080` | `127.0.0.1:8080` | 容器内 `0.0.0.0:8080`，宿主仅回环映射 |
| 数据库客户端 | 需另装 | 需另装 | 镜像内含 MySQL 客户端 |

## 9 验收自检

每个环境部署后至少确认：

1. `/health/ready` 返回 `{"status":"ready"}`。
2. 能登录，概览页数据来自 SQLite 而非前端样例。
3. 重启服务后，数据源、任务、运行记录仍在。
4. 关闭浏览器后，定时任务仍按计划执行。
5. 配置错误的数据库密码时，返回 `AUTH_FAILED`，不产生成功记录。
6. 客户端工具缺失时，返回 `TOOL_MISSING` 或 `prerequisite_missing`。
7. 真实备份产生文件，并在独立数据库恢复后核对行数与校验值。

第 5～7 项需要真实数据库，不能用 mock 代替。

## 10 已知限制

- 未提供 MSI/deb/rpm 安装包，也没有代码签名与自动更新；Windows 安装包是 Inno Setup 图形安装程序（非 MSI）。
- Windows 服务、Linux systemd、Docker 三种正式形态**均未在本机完成运行验收**。
- SQL Server 适配器为 `unverified`，服务端禁止创建任务；差异、增量、binlog/WAL 产品化未实现。
- 远程 Agent 只有客户端，`/agent/v1` 服务端接口未实现。
- 单实例架构：无高可用、无多租户、无分布式调度。
- 未做容量预检与配额，清理释放空间为估算值。
- RPO/RTO、吞吐量、大数据量性能未做基准测量。
