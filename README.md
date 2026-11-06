# DBBak 数据库备份管理

DBBak 当前提供真实 MySQL 逻辑全量与 PostgreSQL 整实例物理全量：Go 单实例服务、SQLite 持久化、管理员认证、数据源与本地目录、手动和后台调度、运行与文件记录、清理预览、安全删除和隔离恢复验证。现有中文界面由同一 Go 进程提供，页面数据来自真实 API。

## 当前能力边界

已实现并验证的能力仅以 `DBBak_文档/DBBak_兼容性矩阵.md`、`DBBak_文档/DBBak_阶段一验证报告.md` 和 `DBBak_文档/DBBak_阶段二PostgreSQL验证报告.md` 为准。

边界：SQL Server 原生全量已完成真实备份与异名恢复验收（SQL Server 2025 + sqlcmd），能力为 supported，任务创建由 `DBBAK_SQLSERVER_ENABLED=1` 显式开启，默认关闭。MySQL binlog 与 PostgreSQL WAL 时间点恢复已由独立脚本真实验证，但尚未接入产品调度；SQL Server 已支持全量、差异与事务日志备份，并完成链式恢复验证，链成员暂不参与自动清理。远程 Agent 已有 mTLS 客户端和协议测试，控制端接口未接入。日志链自动清理、Windows 服务安装验收、Linux systemd 和 Docker 运行验收未完成。独立全量自动清理已实现。备份落点支持本地目录、共享目录（UNC/SMB）、FTP 与对象存储（阿里云 OSS / S3 兼容），对象存储已完成代码实现与本地桩件测试，**尚未对真实阿里云 OSS 桶验收**。详细状态见 `DBBak_文档/DBBak_阶段三至五交付状态.md`。

## 构建与测试

要求 Go 1.27.3；MySQL 真实验证固定使用 MySQL Server、mysqldump 和 mysql 客户端 8.4.13 LTS。

Go 模块根就是仓库根，`go.mod`、`cmd/`、`internal/`、`web/` 与 `embed.go` 都在根下，命令直接在根执行。

```powershell
go mod download
go test ./...
go vet -p=1 ./...
go build -trimpath -o .test-runtime/dbbak.exe ./cmd/dbbak
```

Linux 构建：

```bash
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/dbbak-linux-amd64 ./cmd/dbbak
```

SQLite 驱动为纯 Go，不需要 CGO。跨平台构建不等于目标平台运行验收。

完整的构建、发布（版本同步、产物与校验和）以及 Windows / Linux / Docker 三种运行方式，见 `DBBak_文档/DBBak_构建发布运行指南.md`。

## Windows 安装包与服务

```powershell
.\packaging\build-installer.ps1
# 产物：
#   dist\0.2.0-phase2\DBBak-Setup-0.2.0-phase2-windows-amd64.exe  安装包
#   dist\0.2.0-phase2\dbbak-0.2.0-phase2-windows-amd64.zip        便携包
#   dist\0.2.0-phase2\SHA256SUMS
```

**双击安装包就是标准的 Windows 安装向导**（Inno Setup 编译，中文界面），按页面走即可：

1. **选择安装目录**（默认 `C:\Program Files\DBBak`，可浏览改到其他盘）
2. **选择数据目录**（默认 `C:\ProgramData\DBBak`，存放元数据库、主密钥与备份，必须与程序目录分开）
3. **设置监听地址、管理员用户名、初始密码**（密码至少 6 位，需确认输入）
4. 点「安装」后由 `dbbak.exe setup` 注册服务 `DBBak`（延迟自动启动、失败自动重启、账号 `NT SERVICE\DBBak`）、收紧数据目录 ACL、创建管理员，并等待 `/health/ready` 通过；失败会把安装日志尾部显示出来并中止，不会留下装了一半的目录。

**装完即用**：安装包自带 MySQL 客户端（`mysql` / `mysqldump` / `mysqlbinlog` 及必需的 OpenSSL DLL，位于 `C:\Program Files\DBBak\tools`），客户机不需要另装 MySQL 客户端、不需要改 PATH，也不需要设置任何 `DBBAK_*` 变量。查找顺序为 `DBBAK_*` 显式配置 → 程序目录 `tools` → 系统 PATH，所以要换客户端版本时设置变量即可覆盖。PostgreSQL 与 SQL Server 的工具不随包分发，缺工具时能力矩阵会点名缺失项。

程序或 `tools` 目录损坏、需要补装工具时，**直接再运行一次安装包**即可：安装器会停服并做停服快照，再覆盖程序与工具并重新注册服务，数据、任务、备份与管理员密码都保留。

安装完成后可在「开始」菜单打开 DBBak 控制台，卸载在「设置 → 应用」中，默认保留数据与主密钥。

需要命令行或无人值守时，便携包里的 `dbbak.exe` 保留同样的能力：

```powershell
dbbak.exe setup --dry-run                                       # 只看安装计划，无需管理员
dbbak.exe setup --yes --admin-password <至少12位>                # 无人值守安装
dbbak.exe service status | start | stop | restart
.\deploy\windows\upgrade.ps1 -BinaryPath <新 dbbak.exe> -Yes     # 停服 → 快照 → 换程序 → 启动 → 验活
dbbak.exe uninstall                                             # 保留数据、主密钥与备份
dbbak.exe uninstall --purge --yes                               # 连数据目录一起删除，不可恢复
```

> 未验证：安装、ACL 授权与服务启动需管理员权限，本机尚未实际执行完整安装；已验证向导页面结构（逐页枚举控件确认目录选择、数据目录、两个密码框均存在）、`--dry-run`、前台 `serve`、健康检查与提权提示。首次安装请按构建发布运行指南第 9 节逐项确认。

## 初始化与启动

先准备独立数据目录和至少 6 个字符的管理员密码。密码只通过环境变量传给初始化命令，不使用默认密码。

```powershell
$env:DBBAK_DATA_DIR = "D:\dbbak-data"
$env:DBBAK_ADMIN_PASSWORD = Read-Host "输入初始管理员密码"
.\.test-runtime\dbbak.exe init-admin
Remove-Item Env:\DBBAK_ADMIN_PASSWORD

$env:DBBAK_BACKUP_ROOT = "D:\dbbak-backups"
$env:DBBAK_MYSQL = "C:\tools\mysql-8.4.13\bin\mysql.exe"
$env:DBBAK_MYSQLDUMP = "C:\tools\mysql-8.4.13\bin\mysqldump.exe"
$env:DBBAK_PSQL = "C:\tools\postgresql-18.6\bin\psql.exe"
$env:DBBAK_PG_BASEBACKUP = "C:\tools\postgresql-18.6\bin\pg_basebackup.exe"
.\.test-runtime\dbbak.exe serve
```

默认监听 `127.0.0.1:8080`。访问 `http://127.0.0.1:8080/`，健康检查为 `/health/live` 和 `/health/ready`。

主密钥默认位于 `$DBBAK_DATA_DIR/master.key`，与 SQLite 文件分离。必须持久化并限制读权限；丢失主密钥将无法解密数据源凭据。详细配置见 `DBBak_文档/DBBak_部署说明.md`。

MySQL 默认走逻辑全量（`mysqldump`）。需要物理全量时可在任务里选择「物理（Percona XtraBackup）」，但要求：执行器为 Linux 且与 MySQL 实例同机、XtraBackup 与 MySQL 主版本对齐、设置 `DBBAK_MYSQL_PHYSICAL_ENABLED=1`。相关变量为 `DBBAK_XTRABACKUP`（默认 `xtrabackup`）与 `DBBAK_MYSQLBINLOG`（默认 `mysqlbinlog`）。该路线**尚未在真实环境运行**，启用前必须先按 `DBBak_文档/DBBak_MySQL物理备份验证说明.md` 完成全量、`--prepare`、隔离恢复与 binlog 重放。`DBBAK_BACKUP_TIMEOUT` 默认 6 小时，大库按实际耗时上调。

### 开发环境一键启动

双击仓库根目录的 `start.cmd`，或在 PowerShell 中：

```powershell
.\scripts\start-dev.ps1                 # 自检后启动，就绪后打开浏览器
.\scripts\start-dev.ps1 -Restart        # 已在运行时先停旧进程再启动
.\scripts\start-dev.ps1 -Build          # 强制重新构建
.\scripts\start-dev.ps1 -Port 38081 -NoBrowser
```

`start-dev.ps1` 会先做这些准备：端口已被 DBBak 占用时直接打开页面，不重复拉起实例；`dbbak-data` 缺失时从 `.test-runtime/vault-data` 复制迁移（原目录不动），两者都没有则引导设置管理员密码并执行 `init-admin`；二进制缺失或 `cmd/`、`internal/`、`web/`、`embed.go`、`go.mod` 比二进制新时自动重建；启动后轮询 `/health/ready`，通过才报告成功。

`run-dev.ps1` 保留为最小启动器，只检查数据目录与端口，不做自愈和就绪等待：

```powershell
.\scripts\run-dev.ps1
.\scripts\run-dev.ps1 -Port 38080
```

两个脚本共用 `scripts/dev-env.ps1` 里的环境定义，数据目录、客户端路径和调度间隔只在该文件维护。脚本只监督 DBBak 进程，不启动数据库实例；要跑真实备份，需先按下文启动隔离 MySQL/PostgreSQL。管理员账号为 `admin`，密码在 `.test-runtime/admin-password`。

## 隔离 MySQL 验证

Windows 便携 MySQL：

```powershell
python scripts/bootstrap_mysql_windows.py init    # 仅首次
python scripts/bootstrap_mysql_windows.py start   # 前台运行，另开终端
python scripts/bootstrap_mysql_windows.py seed    # 仅首次初始化样本
```

`start` 是前台进程，必须单独占用一个终端。**`seed` 会重新生成密码并 `ALTER USER`**，重复执行会让 DBBak 中已保存的数据源凭据失效（连接测试报 `AUTH_FAILED`）；数据目录已存在时只跑 `start` 即可。PostgreSQL 的 `seed` 复用已有密码文件，不受此影响，但样本数据会被重置。

Docker 测试库配置位于 `deploy/test/compose.yaml`；当前环境未安装 Docker，因此该路线未运行。不要把测试脚本指向生产数据库。

真实 API、备份和隔离恢复验证：

```powershell
python scripts/phase1_api_verify.py
python scripts/phase1_schedule_verify.py
python scripts/bootstrap_postgres_windows.py init
python scripts/bootstrap_postgres_windows.py start-source
# 在另一个终端初始化样本并验证：
python scripts/bootstrap_postgres_windows.py seed
python scripts/phase2_postgres_verify.py
```

脚本使用 `.test-runtime/admin-password` 和 `.test-runtime/mysql-password` 中的隔离测试凭据，不打印密码或会话令牌。恢复只写入独立 `dbbak_restore`，并核对表结构、3 行数据、金额合计和中文内容。

## 目录

应用代码在仓库根，文档、构建与运维资产分列：

```text
cmd/dbbak/              服务与管理员初始化入口
cmd/agent/              远程 Agent 客户端入口
internal/api/           认证、REST API 和静态 Web 服务
internal/backup/        引擎适配器、执行器、调度、校验与清理
internal/metadata/      SQLite 迁移、持久化模型与并发约束
internal/security/      bcrypt、会话令牌和 AES-GCM 凭据加密
internal/agent/         远程 Agent 协议与 mTLS 客户端
web/                    生产前端源码（HTML/CSS/JS），由 Go embed 提供
embed.go                前端资源嵌入声明
go.mod  go.sum          Go 模块定义
scripts/                隔离环境、真实备份恢复和调度验证脚本
deploy/                 Docker、systemd、Windows 服务与测试数据库配置
packaging/              跨平台可复现构建脚本
DBBak_文档/             需求、API、部署、兼容矩阵和验证报告
AGENTS.md               项目规则
ARCHITECTURE.md         模块边界、数据流与已知约束
TASKS.md                完成、待办、阻塞和未运行项
```

`internal/backup/` 同时包含 MySQL、PostgreSQL 与 SQL Server 适配器；SQL Server 已完成真实备份与异名恢复，需用 `DBBAK_SQLSERVER_ENABLED=1` 开启任务创建，差异与日志未实现。
