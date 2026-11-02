# Vault 阶段二至五交付状态

日期：2026-10-31 · 版本基线：0.2.0-phase2（部分阶段为开发实现，未提升为稳定版）

## 总体结论

- **阶段二 PostgreSQL**：真实全量与独立集群恢复已完成。
- **阶段二 SQL Server**：已完成真实备份与异名恢复验收（SQL Server 2025 + sqlcmd，2026-11-02），能力为 `supported`；任务创建由 `VAULT_SQLSERVER_ENABLED` 显式门控，默认关闭。
- **阶段三**：MySQL binlog 与 PostgreSQL WAL 时间点恢复脚本已真实验证；**尚未接入 Vault 产品 API 和调度器**。SQL Server 差异/日志未验证。
- **阶段四**：远程 Agent 的 mTLS 客户端与协议有单元/回环测试，**控制端协议接口和 Runner 执行集成未完成**；独立全量自动清理已接入；NAS 仅本地路径，**S3 未实现**。
- **阶段五**：Windows/Linux/Docker 与构建脚本已交付并通过静态检查，**服务安装、升级、systemd、Docker 运行均未执行**。

因此不能宣称阶段三至五全部完成。

## 阶段二 SQL Server 实现

已实现：

- `SQLServerClient`：sqlcmd 版本、真实连接检测、原生全量、CHECKSUM、备份 LSN 元数据。
- 存储目标新增 `server_root`，要求当前矩阵与执行器 root 指向同一实际目录，防止管理端路径被误当作数据库服务路径。
- 能力矩阵按 `VAULT_SQLSERVER_ENABLED` 返回 `unverified` 或 `supported`；开关默认关闭，避免未验收能力产生成功记录。
- `sqlserver_test.go` 验证缺失工具、服务路径必填、受信任连接与 SQL 登录参数。
- `scripts/phase2_sqlserver_verify.py` 使用 FILELISTONLY + WITH MOVE，不使用 REPLACE，强制隔离确认。

已完成（2026-11-02，本机 SQL Server 2025 17.0.1000.7）：

- 脚本 `phase2_sqlserver_verify.py` 输出 passed：备份 3,788,800 字节、SHA-256 校验、异名恢复 3 行 / 601.25 / 4 字段。
- 产品路径 `run_b66efbfff38f5076c178fc7b`：state=succeeded、command=succeeded、file=verified、restore=verified，SHA-256 与磁盘一致。
- 修复真实运行暴露的两个缺陷：`msdb.dbo.backupset` 不存在 `backup_lsn` 列；`sqlcmd -Q` 的行数统计污染最后一个 LSN 字段。

未完成：SQL Server 差异、事务日志与 LSN 恢复链。

## 阶段三真实证据

### MySQL binlog 全量与时间点恢复

脚本：`scripts/phase3_mysql_binlog_verify.py`

环境：MySQL Server/mysql/mysqldump/mysqlbinlog 8.4.13，ROW + FULL row image，server_id 33306。

结果：通过。

- 运行：`20261031T034044Z-60128-1793418044299358100`。
- 全量 SQL：2739 字节，SHA-256 `f90162423d106efe0762d17e10772e34234d80873110c4347ff968f88b4396f8`。
- 全量坐标：`vault-bin.000005:639`。
- 目标坐标：`vault-bin.000006:542`。
- binlog 证据文件均记录 SHA-256，并限定在隔离目录。
- 重放 SQL 3620 字节，SHA-256 `854c118af90b92c3f7ef02b72d17ad00d5ab40e6e3d7ccb7d1762bcfd0435921`。
- 全量恢复为 5 行，目标标记存在，表结构和数据核对通过。
- 使用 `--rewrite-db` 仅作用于独立 `vault_restore`，未覆盖源库。

### PostgreSQL WAL 目标 LSN 恢复

脚本：`scripts/phase3_postgres_wal_verify.py`

环境：PostgreSQL 18.6，源 127.0.0.1:35432，WAL archive_mode=on。

结果：通过。

- 运行：`phase3-dhpvlsv_`。
- base.tar：33,506,304 字节，SHA-256 `a0dab9aabf6ed102971bacd8459e3fa21221b0f8eaf466a3178286e1428fa577`。
- 目标 LSN：`0/24000788`。
- WAL 段：`000000010000000000000024`，真实归档并重放。
- 恢复后 last replay LSN：`0/240007F8`。
- 目标标记存在、目标后标记不存在，sample_records 精确快照一致。
- system identifier `7702385004936704968` 一致；恢复后提升时间线为 2。
- 恢复使用受控 `postgres.exe` 子进程，解决 Windows `pg_ctl start` 挂起；所有等待和命令有明确超时。

### 尚未完成

- 两个脚本是隔离恢复验证，不是 Vault 产品内的日志备份、归档、链管理和一键 PITR。
- SQL Server 差异/日志、LSN 链和恢复验证未运行。
- 压缩、加密、跨主版本、大日志量性能未验证。

## 阶段四实现状态

远程 Agent（`internal/agent/**`、`cmd/agent/**`）：

- HTTPS/mTLS，最低 TLS 1.2，强制 CA 和客户端证书，无关闭校验选项。
- 一次性注册令牌 + CSR，私钥本地生成，令牌不写入身份文件。
- fence、递增序号、事件去重、旧 fence 拒绝、有界重试。
- 11 组测试、httptest 回环 mTLS、错误证书拒绝、重试和幂等测试通过；`-race` 因 CGO 未启用未运行。
- 控制端尚未实现 `/agent/v1/leases/*` 等服务端接口，`cmd/agent` 仅开放 enroll，未接入 Runner。真实网络和独立证书服务未验证。

自动清理：

- 调度器在任务 `auto_cleanup=true` 时，每天本地时间 05:00 后执行一次。
- 复用人工清理预览、任务 revision、文件指纹、路径、哈希和最低份数保护。
- Go 测试验证到期全量被删除、最低份数受保护。
- 仅适用于独立全量；日志链感知清理未实现。

存储：

- 本地/NAS 挂载目录仍通过受控本地路径处理。
- S3 兼容对象存储未实现。
- 远程 Agent 上传与对象存储尚无产品端接口。

## 阶段五交付物

已新增但**未运行**：

- Windows：WinSW 服务 XML、安装/升级/卸载/停服备份脚本；要求确认、服务专用账号、保留数据与主密钥。
- Linux：systemd unit、环境样例、安装/升级/卸载/备份脚本；专用用户、状态目录、SQLite WAL 一致性归档。
- Docker：非 root、只读根文件系统、持久卷、固定 Go/MySQL 版本的 Compose 与镜像文件。
- 构建：Windows/Linux amd64/arm64 构建脚本和 SHA-256 清单模板。

静态检查：Bash 语法、PowerShell AST、XML、Compose/Dockerfile/unit 关键结构通过；未执行 `docker compose config`、`systemd-analyze verify`、ShellCheck，未安装任何服务。

## 最终回归（2026-10-31）

- `go test ./...` 与 `go vet -p=1 ./...` 全部通过。
- 覆盖率：agent 76.5%、api 28.3%、backup 51.9%、metadata 27.5%、security 71.1%；两个入口无直接覆盖率。
- Vault 与 Agent 的 Windows amd64、Linux amd64 构建通过；Linux 未运行。
- Python 阶段二/三脚本和 Bash 发行脚本语法通过；PowerShell 5 个脚本 AST 解析、WinSW XML 解析通过。
- Chrome 登录后概览、任务、数据库、部署和清理页面验证通过，JavaScript 异常 0。
- 最终真实回归：MySQL `run_6814aa8dcaafe28efb61aa5f`（2296 字节，3 行/601.25）；PostgreSQL `run_02ea265fe4d7e55452e6beef`（50,284,544 字节，3 行/601.25）。
- 项目维护的 97 个文本文件完成 CRLF 与尾随空白检查。

## 下一步硬性条件

1. 在明确隔离且获授权的 SQL Server 环境运行脚本，留存真实证据后再开放能力。
2. 将 MySQL binlog/PostgreSQL WAL 归档和恢复链接入产品任务状态、调度、预览与审计。
3. 实现 Agent 控制端接口和 Runner 集成，使用真实证书链做双向验证。
4. 实现 S3 对象存储及依赖闭包自动清理。
5. 在目标 Windows/Linux/Docker 环境执行安装、升级、重启、卸载、持久化和恢复验收。
