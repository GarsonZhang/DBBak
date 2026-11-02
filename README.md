# Vault 数据库备份管理

Vault 当前提供真实 MySQL 逻辑全量与 PostgreSQL 整实例物理全量：Go 单实例服务、SQLite 持久化、管理员认证、数据源与本地目录、手动和后台调度、运行与文件记录、清理预览、安全删除和隔离恢复验证。现有中文界面由同一 Go 进程提供，页面数据来自真实 API。

## 当前能力边界

已实现并验证的能力仅以 `Vault_文档/Vault_兼容性矩阵.md`、`Vault_文档/Vault_阶段一验证报告.md` 和 `Vault_文档/Vault_阶段二PostgreSQL验证报告.md` 为准。

边界：SQL Server 原生全量已完成真实备份与异名恢复验收（SQL Server 2025 + sqlcmd），能力为 supported，任务创建由 `VAULT_SQLSERVER_ENABLED=1` 显式开启，默认关闭。MySQL binlog 与 PostgreSQL WAL 时间点恢复已由独立脚本真实验证，但尚未接入产品调度；SQL Server 已支持全量、差异与事务日志备份，并完成链式恢复验证，链成员暂不参与自动清理。远程 Agent 已有 mTLS 客户端和协议测试，控制端接口未接入。S3、日志链自动清理、正式 Windows 服务、Linux systemd 和 Docker 运行验收未完成。独立全量自动清理已实现。详细状态见 `Vault_文档/Vault_阶段三至五交付状态.md`。

## 构建与测试

要求 Go 1.27.3；MySQL 真实验证固定使用 MySQL Server、mysqldump 和 mysql 客户端 8.4.13 LTS。

Go 模块根就是仓库根，`go.mod`、`cmd/`、`internal/`、`web/` 与 `embed.go` 都在根下，命令直接在根执行。

```powershell
go mod download
go test ./...
go vet -p=1 ./...
go build -trimpath -o .test-runtime/vault.exe ./cmd/vault
```

Linux 构建：

```bash
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/vault-linux-amd64 ./cmd/vault
```

SQLite 驱动为纯 Go，不需要 CGO。跨平台构建不等于目标平台运行验收。

完整的构建、发布（版本同步、产物与校验和）以及 Windows / Linux / Docker 三种运行方式，见 `Vault_文档/Vault_构建发布运行指南.md`。

## 初始化与启动

先准备独立数据目录和至少 12 字节的管理员密码。密码只通过环境变量传给初始化命令，不使用默认密码。

```powershell
$env:VAULT_DATA_DIR = "D:\vault-data"
$env:VAULT_ADMIN_PASSWORD = Read-Host "输入初始管理员密码"
.\.test-runtime\vault.exe init-admin
Remove-Item Env:\VAULT_ADMIN_PASSWORD

$env:VAULT_BACKUP_ROOT = "D:\vault-backups"
$env:VAULT_MYSQL = "C:\tools\mysql-8.4.13\bin\mysql.exe"
$env:VAULT_MYSQLDUMP = "C:\tools\mysql-8.4.13\bin\mysqldump.exe"
$env:VAULT_PSQL = "C:\tools\postgresql-18.6\bin\psql.exe"
$env:VAULT_PG_BASEBACKUP = "C:\tools\postgresql-18.6\bin\pg_basebackup.exe"
.\.test-runtime\vault.exe serve
```

默认监听 `127.0.0.1:8080`。访问 `http://127.0.0.1:8080/`，健康检查为 `/health/live` 和 `/health/ready`。

主密钥默认位于 `$VAULT_DATA_DIR/master.key`，与 SQLite 文件分离。必须持久化并限制读权限；丢失主密钥将无法解密数据源凭据。详细配置见 `Vault_文档/Vault_部署说明.md`。

### 开发环境一键启动

仓库已用 `.test-runtime` 做过隔离运行，可直接复用其数据目录与 `.tools` 便携工具链：

```powershell
.\scripts\run-dev.ps1              # 使用 .test-runtime 现有数据启动
.\scripts\run-dev.ps1 -Build       # 先重新构建再启动
.\scripts\run-dev.ps1 -Port 38080  # 指定端口
```

脚本会先检查数据目录与端口占用，端口被占时直接报出占用进程号，不会等到 bind 失败。它只监督 Vault 进程，不启动数据库实例；要跑真实备份，需先按下文启动隔离 MySQL/PostgreSQL。管理员账号为 `admin`，密码在 `.test-runtime/admin-password`。

## 隔离 MySQL 验证

Windows 便携 MySQL：

```powershell
python scripts/bootstrap_mysql_windows.py init    # 仅首次
python scripts/bootstrap_mysql_windows.py start   # 前台运行，另开终端
python scripts/bootstrap_mysql_windows.py seed    # 仅首次初始化样本
```

`start` 是前台进程，必须单独占用一个终端。**`seed` 会重新生成密码并 `ALTER USER`**，重复执行会让 Vault 中已保存的数据源凭据失效（连接测试报 `AUTH_FAILED`）；数据目录已存在时只跑 `start` 即可。PostgreSQL 的 `seed` 复用已有密码文件，不受此影响，但样本数据会被重置。

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

脚本使用 `.test-runtime/admin-password` 和 `.test-runtime/mysql-password` 中的隔离测试凭据，不打印密码或会话令牌。恢复只写入独立 `vault_restore`，并核对表结构、3 行数据、金额合计和中文内容。

## 目录

应用代码在仓库根，文档、构建与运维资产分列：

```text
cmd/vault/              服务与管理员初始化入口
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
Vault_文档/             需求、API、部署、兼容矩阵和验证报告
AGENTS.md               项目规则
ARCHITECTURE.md         模块边界、数据流与已知约束
TASKS.md                完成、待办、阻塞和未运行项
```

`internal/backup/` 同时包含 MySQL、PostgreSQL 与 SQL Server 适配器；SQL Server 已完成真实备份与异名恢复，需用 `VAULT_SQLSERVER_ENABLED=1` 开启任务创建，差异与日志未实现。
