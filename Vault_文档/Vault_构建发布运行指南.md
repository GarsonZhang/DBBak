# Vault 构建、发布与运行指南

> 适用版本：`0.2.0-phase2`
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
| 时区 | 内嵌 `time/tzdata` | 不依赖宿主机的 zoneinfo |

产物是**单个静态二进制**，内含 Web 界面、SQLite 驱动和时区数据。运行期外部依赖只有数据库客户端工具（`mysql`/`mysqldump`/`psql`/`pg_basebackup`/`sqlcmd`），用于真实备份与连接探测。

| 部署形态 | 服务管理 | 当前验证状态 |
| --- | --- | --- |
| Windows 原生 | 前台进程 / WinSW 服务 | 前台与构建已验证；**服务安装未运行** |
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
go build -trimpath -o .test-runtime\vault.exe .\cmd\vault
.\.test-runtime\vault.exe version
```

Linux / macOS：

```bash
go mod download
go test ./...
go vet ./...
go build -trimpath -o dist/vault ./cmd/vault
./dist/vault version
```

`-trimpath` 去掉本机路径，产物可复现且不泄露目录结构。

### 2.3 交叉构建

```bash
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -o dist/vault-linux-amd64   ./cmd/vault
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o dist/vault-windows-amd64.exe ./cmd/vault
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -o dist/vault-linux-arm64   ./cmd/vault
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
dist/<version>/<goos>_<goarch>/vault[.exe]
dist/<version>/<goos>_<arch>/SHA256SUMS
```

脚本内置：`GOTOOLCHAIN=go1.27.3`、`GOWORK=off`、`CGO_ENABLED=0`、`SOURCE_DATE_EPOCH=0`，并在结束时打印“仍需在目标系统执行运行、备份和恢复验收”。先写临时文件再原子改名，失败不会留下半成品。

### 2.5 常见构建失败

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `go: cannot find main module` | 在子目录执行 | 回到仓库根，`go.mod` 在根目录 |
| `pattern web/index.html: no matching files` | `web/` 被移动或改名 | 目录必须与 `embed.go` 同级 |
| `go: download ... x509` / 超时 | 模块代理不可达 | 设 `GOPROXY=https://goproxy.cn,direct` |
| `go: toolchain not available` | 本机 Go 版本过低 | 安装 1.27.3，或让 `GOTOOLCHAIN` 自动下载 |
| Windows 下 `Access is denied` 覆盖 `vault.exe` | 旧进程仍在运行 | 先停止服务，再构建 |

## 3 发布

### 3.1 版本号同步点

改版本号必须同时改这五处，否则产物、镜像标签和 `version` 输出会互相矛盾：

| 位置 | 当前值 |
| --- | --- |
| `cmd/vault/main.go` `const version` | `0.2.0-phase2` |
| `deploy/docker/Dockerfile` `LABEL org.opencontainers.image.version` | `0.2.0-phase2` |
| `deploy/docker/compose.yaml` `image` | `vault:0.2.0-phase2` |
| `packaging/build.sh` 默认 `VERSION` | `0.2.0-phase2` |
| `packaging/build.ps1` 参数默认值 | `0.2.0-phase2` |

### 3.2 发布前检查清单

```bash
go vet ./...
go test ./...
go build ./...
git ls-files --eol | grep -E 'w/mixed' || echo "行尾正常"
```

- 前端改动后必须重新构建二进制，`web/` 是编译期嵌入，不是运行时读取。
- `Vault_文档/Vault_兼容性矩阵.md` 中标注的能力要与本次发布一致。
- 未验证的能力（SQL Server、日志链、Agent 控制端）不得在发布说明里写成“支持”。

### 3.3 产物清单

一次发布建议包含：

```text
vault-<version>-windows-amd64.zip
  vault.exe
  README.md
  LICENSE
  deploy/windows/*.ps1, vault-service.xml
vault-<version>-linux-amd64.tar.gz
  vault
  README.md
  LICENSE
  deploy/linux/install.sh, upgrade.sh, uninstall.sh, backup.sh, vault.service, vault.env.example
SHA256SUMS
```

### 3.4 校验与签名

当前只有 SHA-256，没有代码签名：

```bash
sha256sum -c SHA256SUMS
```

Windows 下用 `Get-FileHash -Algorithm SHA256` 比对。**没有 Authenticode 签名，也没有 MSI/deb/rpm 包**，Windows 首次运行可能出现 SmartScreen 提示，Linux 需要自行 `chmod +x`。

### 3.5 回滚

- Windows 服务：`upgrade.ps1` 会把旧的可执行文件复制到 `C:\ProgramData\Vault\maintenance`，回滚时停服务、换回旧文件、启服务。
- Linux：`upgrade.sh` 先停服、调用 `vault-backup` 做元数据快照，再把旧二进制 `cp` 成 `/var/lib/vault/maintenance/vault-binary-<UTC时间戳>`，失败时明确打印旧程序路径与归档位置。
- Docker：`docker compose down` 后把 `image:` 改回旧标签重新 `up`；数据在命名卷里，不受镜像回滚影响。

## 4 公共运行配置

### 4.1 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `VAULT_ADDR` | `127.0.0.1:8080` | 监听地址，默认仅回环 |
| `VAULT_DATA_DIR` | `.vault` | 元数据、默认密钥、默认备份根 |
| `VAULT_DATABASE` | `<数据目录>/vault.db` | SQLite 文件，需与 `-wal`/`-shm` 同目录 |
| `VAULT_BACKUP_ROOT` | `<数据目录>/backups` | 备份授权根目录 |
| `VAULT_MASTER_KEY_FILE` | `<数据目录>/master.key` | AES-256 主密钥，**必须独立备份** |
| `VAULT_MYSQL` / `VAULT_MYSQLDUMP` | `mysql` / `mysqldump` | MySQL 客户端 |
| `VAULT_PSQL` / `VAULT_PG_BASEBACKUP` | `psql` / `pg_basebackup` | PostgreSQL 客户端 |
| `VAULT_SQLCMD` | `sqlcmd` | SQL Server 适配器工具 |
| `VAULT_SQLSERVER_ENABLED` | 未设置（关闭） | 设为 `1` 才允许创建 SQL Server 任务；须先完成异名恢复验收 |
| `VAULT_SESSION_TTL` | `12h` | 会话有效期 |
| `VAULT_SCHEDULER_INTERVAL` | `2s` | 到期任务扫描间隔 |
| `VAULT_BACKUP_TIMEOUT` | `30m` | 单次备份超时 |
| `VAULT_ADMIN_USERNAME` | `admin` | 仅 `init-admin` 使用 |
| `VAULT_ADMIN_PASSWORD` | 无 | 仅 `init-admin` 使用，至少 12 字节 |

### 4.2 首次初始化

`init-admin` 只执行一次，重复执行会因用户已存在而失败。密码只通过环境变量传入，不落盘、不写日志。

```powershell
$env:VAULT_DATA_DIR = "D:\vault-data"
$env:VAULT_ADMIN_PASSWORD = Read-Host "初始管理员密码"
.\vault.exe init-admin
Remove-Item Env:\VAULT_ADMIN_PASSWORD
```

```bash
export VAULT_DATA_DIR=/var/lib/vault
read -rsp '初始管理员密码: ' VAULT_ADMIN_PASSWORD; echo
./vault init-admin
unset VAULT_ADMIN_PASSWORD
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
$env:VAULT_DATA_DIR = "D:\vault-data"
$env:VAULT_BACKUP_ROOT = "D:\vault-backups"
$env:VAULT_MYSQL = "C:\tools\mysql-8.4.13\bin\mysql.exe"
$env:VAULT_MYSQLDUMP = "C:\tools\mysql-8.4.13\bin\mysqldump.exe"
$env:VAULT_ADDR = "127.0.0.1:8080"
.\vault.exe serve
```

`serve` 是前台进程，Ctrl+C 即停止；关终端等于停服务。

密码查看：
```
Get-Content .\.test-runtime\admin-password
```

### 5.2 作为 Windows 服务（WinSW）

需要管理员 PowerShell 和 **WinSW v2.12.0**（脚本会校验它的 SHA-256）。

```powershell
.\deploy\windows\install.ps1 `
  -BinaryPath .\dist\0.2.0-phase2\windows_amd64\vault.exe `
  -WinSWPath  C:\tools\WinSW.exe `
  -WinSWSha256 <WinSW.exe 的 64 位十六进制 SHA-256>
```

安装后布局：

| 路径 | 内容 |
| --- | --- |
| `C:\Program Files\Vault\` | `vault.exe`、`VaultService.exe`（WinSW 包装器） |
| `C:\ProgramData\Vault\data\` | `vault.db` 及 `-wal`/`-shm` |
| `C:\ProgramData\Vault\backups\` | 备份仓库 |
| `C:\ProgramData\Vault\key\` | `master.key` |
| `C:\ProgramData\Vault\logs\` | WinSW 滚动日志 |
| `C:\ProgramData\Vault\maintenance\` | 停服快照与旧二进制 |

服务名 `Vault`，延迟自动启动，默认以虚拟账号 `NT SERVICE\Vault` 运行，并限制 ProgramData 的 ACL。脚本不会初始化管理员、不写数据库凭据、不删除任何现有备份。

日常操作：

```powershell
Get-Service Vault
Restart-Service Vault
# 升级
.\deploy\windows\upgrade.ps1 -BinaryPath <新 vault.exe> -Yes
# 停服一致性快照（含主密钥，注意异地保护）
.\deploy\windows\backup.ps1 -Yes
# 卸载：保留 ProgramData 下的数据、密钥、备份与日志
.\deploy\windows\uninstall.ps1 -Yes
```

> 未验证：本机未执行服务安装、ACL 设置与卸载流程。上述命令来自脚本实现，**尚未在真实 Windows 服务环境跑通**。

## 6 Linux 运行

### 6.1 手工前台运行

二进制就位（例如 `/usr/local/bin/vault`）后，可以用专用账号前台启动，便于观察日志：

```bash
sudo install -d -o vault -g vault -m 0700 /var/lib/vault
sudo -u vault env \
  VAULT_DATA_DIR=/var/lib/vault \
  VAULT_BACKUP_ROOT=/var/lib/vault/backups \
  VAULT_MASTER_KEY_FILE=/var/lib/vault/key/master.key \
  VAULT_ADDR=127.0.0.1:8080 \
  /usr/local/bin/vault serve
```

### 6.2 一键安装（systemd）

```bash
sudo ./deploy/linux/install.sh /path/to/vault        # 交互确认
sudo ./deploy/linux/install.sh --yes /path/to/vault  # 无人值守
```

脚本做的事：

1. 创建无登录 shell 的系统用户/组 `vault`。
2. 创建 `/var/lib/vault`、`backups`、`key`、`maintenance`，权限 `0700`，属主 `vault`。
3. 安装 `/usr/local/bin/vault`、`/usr/local/sbin/vault-backup`（停服快照工具）、`/etc/systemd/system/vault.service`。
4. 若 `/etc/vault/vault.env` 不存在则从样例安装（`0600` root），**已存在则保留不覆盖**。
5. `systemctl enable vault.service` 后再 `systemctl start`，并在 30 秒内轮询 `/health/ready`，失败直接报错退出。

若已存在安装，脚本会拒绝执行并提示改用 `upgrade.sh`。

### 6.3 配置

```bash
sudo vi /etc/vault/vault.env      # 0600 root:root
sudo systemctl restart vault
```

`vault.env` 里**不要写** `VAULT_ADMIN_PASSWORD` 或任何数据库密码，样例文件已注明。若把 `VAULT_BACKUP_ROOT` 移出 `/var/lib/vault`，必须同步在 unit 里加 `ReadWritePaths=`，否则 `ProtectSystem=strict` 会拒绝写入。

unit 已启用 `NoNewPrivileges`、`ProtectSystem=strict`、`PrivateTmp`、`CapabilityBoundingSet=`（清空）等加固项，并限制 `LimitNOFILE=65536`。

### 6.4 运维命令

```bash
systemctl status vault
journalctl -u vault -f
curl -fsS http://127.0.0.1:8080/health/ready
```

### 6.5 升级、快照与卸载

```bash
# 升级：停服 → 备份 SQLite/WAL/主密钥 → 保留旧二进制 → 装新二进制 → readiness 校验
sudo ./deploy/linux/upgrade.sh /path/to/new/vault

# 停服一致性快照（服务必须已停止，运行中复制 vault.db 不满足 WAL 一致性）
sudo systemctl stop vault
sudo /usr/local/sbin/vault-backup /var/lib/vault/maintenance
sudo systemctl start vault

# 卸载：保留 /var/lib/vault 与 /etc/vault
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
| runtime | `mysql:8.4.13` | 自带 `mysql`/`mysqldump`，安装 curl，创建 UID/GID 10001 非 root 用户 |

运行时目录：`/var/lib/vault`（数据）、`/backups`（仓库）、`/etc/vault-key`（主密钥），三者都声明为 `VOLUME`。默认 `CMD ["serve"]`，监听 `0.0.0.0:8080`。

### 7.2 构建镜像

```bash
docker build -f deploy/docker/Dockerfile -t vault:0.2.0-phase2 .
```

### 7.3 Compose 启动

```bash
cd deploy/docker
docker compose up -d --build
docker compose ps
docker compose logs -f vault
```

`compose.yaml` 的固定行为：

- 只把 `127.0.0.1:8080` 映射到宿主，不对外暴露。
- `read_only: true` + `tmpfs /tmp`，根文件系统只读；可写位置只有三个命名卷。
- `user: 10001:10001`、`cap_drop: ALL`、`no-new-privileges`。
- `healthcheck` 调 `/health/ready`，`start_period: 15s`。
- 卷名固定：`vault-production-data`、`vault-production-backups`、`vault-production-master-key`。

### 7.4 容器内初始化管理员

初始化必须复用同一组卷，否则会写到另一个空数据库：

```bash
read -rsp '初始管理员密码: ' VAULT_ADMIN_PASSWORD; echo
export VAULT_ADMIN_PASSWORD
docker compose -f deploy/docker/compose.yaml run --rm \
  -e VAULT_ADMIN_PASSWORD vault init-admin
unset VAULT_ADMIN_PASSWORD
```

### 7.5 路径语义（容易踩）

- 容器内的 `/backups` 是**执行器**路径，`mysqldump`/`pg_basebackup` 由容器进程写出，没问题。
- **SQL Server 例外**：`BACKUP DATABASE` 的目标路径必须能被 SQL Server 实例访问，容器内路径对它不可见。必须用 `storage_targets.server_root` 映射到数据库主机上的真实目录。
- 容器内没有 `sqlcmd` 时，SQL Server 适配器会报 `TOOL_MISSING`，不会伪造成功。

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
| 服务管理 | WinSW + `Get-Service` | `systemctl` | Compose / 容器运行时 |
| 数据目录 | `C:\ProgramData\Vault` | `/var/lib/vault` | 命名卷 |
| 主密钥 | `ProgramData\Vault\key` | `/var/lib/vault/key` | 独立卷 `/etc/vault-key` |
| 配置 | WinSW XML 内联 `env` | `/etc/vault/vault.env` | 环境变量 + 卷 |
| 日志 | `ProgramData\Vault\logs` | `journalctl -u vault` | `docker compose logs` |
| 停服快照 | `backup.ps1` | `vault-backup` | 停容器后备份卷 |
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

- 未提供 MSI/deb/rpm 安装包，也没有代码签名与自动更新。
- Windows 服务、Linux systemd、Docker 三种正式形态**均未在本机完成运行验收**。
- SQL Server 适配器为 `unverified`，服务端禁止创建任务；差异、增量、binlog/WAL 产品化未实现。
- 远程 Agent 只有客户端，`/agent/v1` 服务端接口未实现。
- 单实例架构：无高可用、无多租户、无分布式调度。
- 未做容量预检与配额，清理释放空间为估算值。
- RPO/RTO、吞吐量、大数据量性能未做基准测量。
