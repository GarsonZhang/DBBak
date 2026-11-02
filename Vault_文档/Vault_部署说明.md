# Vault 阶段一运行与部署说明

> 构建、发布与 Windows / Linux / Docker 三种运行方式的完整步骤，见 `Vault_构建发布运行指南.md`。
> 本文只保留配置项、隔离验证环境、安全规则和备份运行手册，避免与那份文档重复。

本文覆盖当前可运行的开发/隔离部署。Windows 服务、Linux systemd 和 Docker 正式发行验收属于阶段五，未实现或未运行部分不得视为生产部署结论。

## 1 配置项

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| VAULT_ADDR | 127.0.0.1:8080 | HTTP 监听地址；默认仅回环 |
| VAULT_DATA_DIR | .vault | 元数据、默认密钥和默认备份的根目录 |
| VAULT_DATABASE | <数据目录>/vault.db | SQLite 文件 |
| VAULT_BACKUP_ROOT | <数据目录>/backups | 默认备份授权目录 |
| VAULT_MASTER_KEY_FILE | <数据目录>/master.key | 独立 AES-256 主密钥文件 |
| VAULT_MYSQL | mysql | 真实 mysql 客户端路径 |
| VAULT_MYSQLDUMP | mysqldump | 真实 mysqldump 路径 |
| VAULT_PSQL | psql | PostgreSQL 真实连接与能力检测客户端 |
| VAULT_PG_BASEBACKUP | pg_basebackup | PostgreSQL 整实例物理全量工具 |
| VAULT_SQLCMD | sqlcmd | SQL Server 适配器工具 |
| VAULT_SQLSERVER_ENABLED | 未设置 | 设为 1 才允许创建 SQL Server 任务；默认关闭，须先完成真实备份与异名恢复验收 |
| VAULT_SESSION_TTL | 12h | 登录会话有效期 |
| VAULT_SCHEDULER_INTERVAL | 2s | 到期任务检查间隔 |
| VAULT_BACKUP_TIMEOUT | 30m | 单次备份超时 |
| VAULT_ADMIN_USERNAME | admin | 仅初始化管理员使用 |
| VAULT_ADMIN_PASSWORD | 无 | 仅初始化管理员使用，至少 12 字节且不记录到日志 |

配置、SQLite 和主密钥必须持久化。备份目录应与元数据目录一样纳入权限管理。不得把唯一主密钥只放在容器临时层或待删除备份目录中。

## 2 Windows 隔离运行

固定 Go 1.27.3 和 MySQL 8.4.13 工具。Go 模块根位于仓库根目录，构建：

```powershell
go mod download
go test ./...
go build -trimpath -o .test-runtime/vault.exe ./cmd/vault
```

初始化管理员：

```powershell
$env:VAULT_DATA_DIR = "D:\vault-data"
$env:VAULT_ADMIN_PASSWORD = Read-Host "初始管理员密码"
.\.test-runtime\vault.exe init-admin
Remove-Item Env:\VAULT_ADMIN_PASSWORD
```

已有用户时 `init-admin` 会拒绝覆盖。启动：

```powershell
$env:VAULT_BACKUP_ROOT = "D:\vault-backups"
$env:VAULT_MYSQL = "C:\tools\mysql-8.4.13-winx64\bin\mysql.exe"
$env:VAULT_MYSQLDUMP = "C:\tools\mysql-8.4.13-winx64\bin\mysqldump.exe"
.\.test-runtime\vault.exe serve
```

访问 `http://127.0.0.1:8080/`。关闭浏览器不影响调度；停止 Vault 进程会中断或延迟执行，重启后活动运行会进入对账/中断状态。

当前只是前台进程。Windows 服务安装、服务账号、ProgramData、签名安装包和卸载尚未实现。

## 3 隔离 MySQL 便携实例

项目脚本只操作 `.test-runtime` 和 127.0.0.1:33306：

```powershell
python scripts/bootstrap_mysql_windows.py init
python scripts/bootstrap_mysql_windows.py start
# 另开终端：
python scripts/bootstrap_mysql_windows.py seed
```

`seed` 创建 vault_source、vault_restore、sample_records 和随机测试密码。密码仅写入 `.test-runtime/mysql-password`，不打印。Vault 管理员的隔离密码保存在 `.test-runtime/admin-password`。

启动 Vault 后运行：

```powershell
python scripts/phase1_api_verify.py
python scripts/phase1_schedule_verify.py
```

恢复脚本只删除并重建隔离 `vault_restore`，不得修改脚本指向其他数据库。

## 4 PostgreSQL 隔离物理全量与恢复

固定 PostgreSQL 18.6 Windows 便携工具，并配置：

```powershell
$env:VAULT_PSQL = "C:\tools\postgresql-18.6\bin\psql.exe"
$env:VAULT_PG_BASEBACKUP = "C:\tools\postgresql-18.6\bin\pg_basebackup.exe"
```

初始化和启动独立源集群：

```powershell
python scripts/bootstrap_postgres_windows.py init
python scripts/bootstrap_postgres_windows.py start-source
# 另开终端：
python scripts/bootstrap_postgres_windows.py seed
python scripts/phase2_postgres_verify.py
```

源实例为 127.0.0.1:35432，恢复实例为 127.0.0.1:35433，数据目录分离。备份为整实例 base.tar，包含必要 WAL；恢复脚本检查 tar 路径与链接、启动独立集群、核对样本和 system identifier 后停止恢复实例。PostgreSQL 维护库只用于连接，不代表只备份该库。

## 5 Docker 测试数据库

需要 Docker 与 Compose。设置仅用于隔离环境的随机密码：

```bash
export VAULT_TEST_ROOT_PASSWORD='<隔离-root-密码>'
export VAULT_TEST_MYSQL_PASSWORD='<隔离-vault_test-密码>'
docker compose -f deploy/test/compose.yaml up -d --wait
```

Compose 固定 `mysql:8.4.13`，只把 33306 绑定到 127.0.0.1，初始化样本和授权。Vault 服务仍需按第 2 节在宿主机运行，并配置 8.4.13 客户端。

当前执行环境没有 Docker，因此该 Compose **未运行**。`deploy/docker/Dockerfile` 提供 Go 1.27.3 构建阶段和 MySQL 8.4.13 工具运行阶段，但也**未构建、未进行 Linux 运行、持久卷、升级或非 root 验证**。

## 6 Linux 构建

```bash
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/vault-linux-amd64 ./cmd/vault
```

SQLite 不需要 CGO，二进制内嵌时区数据。交叉编译只证明可构建；Linux 启动、systemd、文件权限和真实 MySQL 验证仍未运行。

## 7 凭据与目录安全

- 管理员密码以 bcrypt 保存；数据库密码用 AES-GCM 加密，密钥独立于 SQLite。
- MySQL 密码仅通过子进程 `MYSQL_PWD` 传递；不得出现在命令行、API 响应或普通日志中。
- 主密钥文件应仅允许服务账号读取。备份主密钥时与元数据备份分开保护，并提供独立恢复流程。
- 所有备份和清理路径都解析到授权根目录，拒绝绝对路径、`..`、越界符号链接和非普通文件。
- 已有备份的仓库禁止修改根目录，避免旧文件失去定位。
- 清理执行前重新验证预览、运行状态、路径、大小和 SHA-256。

## 8 备份与恢复运行手册

1. 添加 MySQL 数据源并执行连接测试，确认实际版本为已验证组合。
2. 添加本地目录并执行写权限检查。
3. 创建全量任务，设置时区、调度、保留天数和最低份数。
4. 手动执行并检查运行的 command/file/restore 三个状态。
5. 在独立目标库恢复；不得默认覆盖源库或生产实例。
6. 只有真实恢复并核对后，才把 restore_status 登记为 verified。
7. 清理前检查预览；过期预览或任何配置/文件状态变化都必须重新预览。

RPO/RTO、吞吐量和大数据量性能尚未基准测量，不作承诺。
