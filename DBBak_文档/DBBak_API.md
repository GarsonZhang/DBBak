# DBBak 当前 API 说明

版本：`0.2.0-phase2` · 前缀：`/api/v1` · JSON：UTF-8 · 时间：RFC 3339 UTC

## 认证与请求约束

管理员通过 `dbbak init-admin` 创建，或在容器化部署时由 `serve` 在 `users` 表为空且设置了 `DBBAK_ADMIN_PASSWORD` 的首次启动中自动创建；无默认账号或默认密码。登录成功后服务设置 HttpOnly、SameSite=Strict 会话 Cookie。所有需要认证的接口支持 Cookie；也支持 `Authorization: Bearer <会话令牌>`。

Cookie 会话的写请求必须带 `X-DBBak-Request: 1`，否则返回 403。前端不得把数据库密码或主密钥写入 localStorage。

```powershell
curl -c cookies.txt -H "Content-Type: application/json" -H "X-DBBak-Request: 1" `
  -d '{"username":"admin","password":"<初始密码>"}' `
  http://127.0.0.1:8080/api/v1/auth/login
curl -b cookies.txt http://127.0.0.1:8080/api/v1/jobs
```

响应错误包含 `code`、`message`、`field_errors`、`retryable` 和可用的 `request_id`。常见状态：400 JSON 无效，401 未认证，403 同源确认或权限失败，404 不存在，409 版本或状态冲突，422 配置/连接/能力前提不满足。

## 接口清单

| 方法与路径 | 已实现用途 | 成功状态 |
| --- | --- | --- |
| GET `/health/live` | 进程存活，无需登录 | 200 |
| GET `/health/ready` | SQLite 可用性 | 200/503 |
| POST `/api/v1/auth/login` | 管理员登录 | 200 |
| POST `/api/v1/auth/logout` | 注销当前会话 | 200 |
| GET `/api/v1/auth/me` | 当前用户 | 200 |
| GET `/api/v1/overview` | 持久化统计与最近运行 | 200 |
| GET `/api/v1/capabilities` | 本机工具与未实现能力矩阵 | 200 |
| GET `/api/v1/settings` | 非敏感运行配置和密钥路径 | 200 |
| GET/POST `/api/v1/sources` | MySQL/PostgreSQL/SQL Server 数据源。SQL Server 支持 `trust_server_certificate`（默认 false，为 true 时 sqlcmd 带 `-C` 跳过证书链校验） | 200/201 |
| PATCH `/api/v1/sources/{id}` | 编辑数据源，需 revision | 200 |
| POST `/api/v1/sources/{id}/probe` | 真实连接与版本检测；可选 `?route=physical` 单独验证 MySQL 物理路线前提（执行器能否看到 `datadir`、XtraBackup 与 MySQL 主版本是否对齐、`log_bin` 状态） | 200/422 |
| GET/POST `/api/v1/storage-targets` | 存储位置列表/新增。`kind` 取 `local`（执行器本地目录）、`unc`（共享目录 SMB，`root` 填 `\\host\share\path`，路径语义与本地一致）、`ftp`（`host`/`port`/`username`/`password`/`root` 为远程绝对路径）。创建时执行真实写检查：本地/共享落盘探测，FTP 走「上传-回读-删除」探测 | 200/201 |
| PATCH `/api/v1/storage-targets/{id}` | 编辑仓库；有备份时禁止改根目录/存储类型。FTP 编辑时 `password` 留空表示沿用原凭据 | 200 |
| POST `/api/v1/storage-targets/{id}/check` | 真实写检查：本地/共享创建并删除测试文件，FTP 上传-回读-删除 | 200/422 |
| GET `/api/v1/storage-targets/{id}/removal` | 存储位置删除预览：引用统计（任务/对象/预览）与阻塞原因 | 200 |
| DELETE `/api/v1/storage-targets/{id}?confirm=<名称>` | 解除存储位置登记；**不删除磁盘文件**；仍被任务或备份对象引用时返回 409 | 200/409/422 |
| GET `/api/v1/sources/{id}/removal` | 数据源删除预览：任务/对象引用统计与阻塞原因 | 200 |
| DELETE `/api/v1/sources/{id}?confirm=<名称>` | 删除数据源并**一并清除加密凭据**；不连接、不改动数据库本身；被任务或对象引用时返回 409 | 200/409/422 |
| GET/POST `/api/v1/jobs` | 任务列表/新增；`backup_type` 取 `full`/`differential`/`log`。`backup_method` 选择 MySQL 备份路线：空为默认（逻辑全量 `mysqldump`）、`logical`、`physical`（Percona XtraBackup，需 `DBBAK_MYSQL_PHYSICAL_ENABLED=1` 且执行器为 Linux）。`log` 仅对 SQL Server 事务日志或 MySQL 物理路线的 binlog 归档可用；`differential` 仅 SQL Server。`subdir` 为任务级输出子目录。`location_kind` 区分备份落点：`storage`（默认，`storage_root`/`target_id` 指向执行器可达目录）或 `database_server`（仅 SQL Server，`server_dir` 为数据库服务器上的本地绝对路径，不使用存储位置；产物标 `uploaded` 未校验，以 msdb LSN 与 backup_size 为凭证，删除/清理只清记录，远程文件需人工删除） | 200/201 |
| PATCH `/api/v1/jobs/{id}` | 调度、启停、保留配置与 `subdir`，需 revision | 200 |
| GET `/api/v1/jobs/{id}/removal?force=0\|1` | 评估删除任务的范围（配置 + 备份文件 + 运行记录）与阻塞项，只读 | 200 |
| DELETE `/api/v1/jobs/{id}[?force=1]` | 删除任务：文件逐个删除成功后移除记录；任务必须已暂停 | 200/409/422 |
| GET `/api/v1/jobs/{id}/artifacts/removal?force=0\|1` | 只评估任务下备份文件的删除范围，只读 | 200 |
| DELETE `/api/v1/jobs/{id}/artifacts[?force=1]` | 只删除任务的备份文件，保留任务配置 | 200/409/422 |
| POST `/api/v1/jobs/{id}/runs` | 手动触发，支持 Idempotency-Key | 202/200 |
| GET `/api/v1/runs?job_id=` | 运行列表 | 200 |
| GET `/api/v1/runs/{id}` | 运行状态与三个验证等级 | 200 |
| GET `/api/v1/runs/{id}/logs` | 已脱敏运行日志 | 200 |
| POST `/api/v1/runs/{id}/restore-verification` | 登记隔离恢复实测结果 | 200 |
| GET `/api/v1/artifacts?job_id=` | 备份文件、摘要与恢复依赖字段 | 200 |
| PATCH `/api/v1/artifacts/{id}/hold` | 人工锁定/解除 | 200 |
| GET `/api/v1/artifacts/{id}/removal?force=0\|1` | 评估单个备份对象的删除范围与阻塞项，只读 | 200 |
| DELETE `/api/v1/artifacts/{id}[?force=1]` | 删除单个备份对象；已校验对象需强制确认 | 200/409/422 |
| POST `/api/v1/jobs/{id}/cleanup-previews` | 生成十分钟服务端清理预览 | 201 |
| POST `/api/v1/cleanup-runs` | 按 preview_id 复查并清理 | 200 |

## 数据源

新增：

```json
{
  "name": "隔离 MySQL",
  "engine": "mysql",
  "host": "127.0.0.1",
  "port": 33306,
  "database": "dbbak_source",
  "username": "dbbak_test",
  "password": "只在请求体中提交"
}
```

编辑时 `password` 可省略以保留原凭据，`revision` 必填。列表和响应不包含密码或密文。`engine` 当前支持 `mysql` 与 `postgresql`，已有数据源不能修改引擎。数据库名或维护库名允许字母、数字、下划线、`$` 和连字符，最长 64 字符。PostgreSQL 的 `database` 是连接维护库（通常为 postgres），备份范围为整个实例。

SQL Server 数据源额外支持 `trust_server_certificate`（布尔，默认 `false`）。为 `true` 时 sqlcmd 追加 `-C`，跳过 TLS 证书链校验，仍保留加密通道——自建实例普遍使用自签名证书，ODBC Driver 18 默认强制校验，此时不勾选会返回 `TLS_VERIFY_FAILED`。该字段对 `mysql`/`postgresql` 无意义，提交也会被强制落为 `false`。库中已有数据源由迁移 10 回填为 `1`（保持升级前行为），需要严格校验的部署应逐条改回。

连接检测真实执行 mysql 或 psql，分别返回 TOOL_MISSING、NETWORK_ERROR、AUTH_FAILED、DATABASE_NOT_FOUND、PREREQUISITE_MISSING 等错误，不会把 TCP 连通当作备份可用。SQL Server 连接另有 TLS_VERIFY_FAILED（证书不受信）。PostgreSQL 还检查 REPLICATION/超级用户权限和 max_wal_senders，返回 backup_type=physical_cluster_full。

## 任务与调度

```json
{
  "name": "每分钟隔离全量",
  "source_id": "src_...",
  "storage_root": "D:/backups/mysql",
  "target_id": "repo_...",
  "subdir": "mysql-prod",
  "timezone": "Asia/Shanghai",
  "schedule_type": "interval",
  "time_of_day": "02:00",
  "weekday": 0,
  "interval_minutes": 15,
  "enabled": true,
  "retention_days": 30,
  "min_full": 2,
  "auto_cleanup": false
}
```

`schedule_type` 支持 `daily`、`weekly`、`interval`。每日/每周使用时区和 `time_of_day`；周计划另用 0~6 的 `weekday`；间隔计划使用 1~1440 分钟。`auto_cleanup: true` 启用独立全量自动清理，调度器每天任务本地时间 05:00 后使用与人工清理相同的预览、指纹、路径和哈希复查；日志链感知自动清理仍未实现。

### 本地存储目录（`storage_root`，推荐）

- 本地备份目录直接在任务中填写：保存时服务端规范化路径并执行真实写检查，失败返回 422。
- 相同目录的多个任务自动复用同一条存储目标登记；`target_id` 仍会返回，仅供兼容旧客户端，填写了 `storage_root` 时可省略。
- `kind=oss`（S3 兼容 / 阿里云 OSS）不使用根目录：`root` 是对象键前缀（可空），连接参数为 `endpoint`（主机名，可带端口）、`bucket`、`region`、`use_ssl`、`path_style`，`username` 为 AccessKey ID、`password` 为 AccessKey Secret（加密入库、只写不回显）。保存时执行「写探测对象 → 读回长度 → 删除」的真实写检查。

### 输出子目录（`subdir`，可选）

同一存储根目录往往被多个实例共用，因此具体落点属于任务配置，而不是仓库配置：

- `subdir` 是相对存储根目录的路径，存储统一用正斜杠，例如 `mysql-prod`、`sqlserver-prod/华东`；留空表示直接写入根目录（历史任务行为）。
- 实际落点为 `<根目录>/<subdir>/<数据库名>/<年>/<月>/<文件>`。数据库段与年月段由服务端固定生成，不能由客户端指定。
- 备份文件名为 `<数据库名>-<YYYYMMDDHHmmss><扩展名>`，时间取本次运行开始时刻的 **UTC**（与 `runs.started_at` 同源，界面按任务时区显示）；扩展名由路线决定（`.sql`、`.bak`、`.trn` 等）。数据库名中的路径分隔符与 `* ? " < > | :` 会替换为下划线，因此可以直接在服务器目录里按库名和时间辨认文件；同一数据库在同一秒内触发两次会命名相同，落盘侧检查目标已存在即拒绝覆盖（`BACKUP_ALREADY_EXISTS`）。
- 保存时服务端规范化并拒绝绝对路径、盘符、UNC、控制字符、`< > : " | ? *`，以及 Clean 后越出根目录的 `..`；单层不超过 60 字符，总长不超过 120。执行器每次运行前再次复核。
- 修改 `subdir` 只影响之后产生的对象，已登记对象按 `relative_path` 定位，清理不看当前任务配置。`subdir` 变化会让任务 revision 递增，旧清理预览随之失效。

```json
{"name":"生产 MySQL 全量","source_id":"src_...","target_id":"repo_...","subdir":"mysql-prod","schedule_type":"daily","time_of_day":"02:00"}
```

`subdir` 与其它任务字段一起提交；`PATCH` 省略该字段时会被写成空字符串，请始终回传完整任务配置。

运行状态与任务启用状态分离。运行包含：

- `command_status`：`not_run`、`running`、`succeeded`、`failed`。
- `file_status`：`pending`、`verifying`、`verified`、`quarantined` 等。
- `restore_status`：初始 `not_run`，隔离恢复登记后为 `verified` 或 `failed`。

重复 Idempotency-Key 返回原运行；不同键在活动运行期间返回 409。服务重启后无法确认的活动运行标记 `interrupted`，不会伪造成功。

## 备份与恢复验证

MySQL 使用 `--single-transaction --skip-lock-tables --quick --routines --events --triggers --hex-blob --set-gtid-purged=OFF --no-tablespaces --result-file=<隔离临时文件>`。密码仅通过子进程 MYSQL_PWD 传递。

PostgreSQL 使用 `--format=tar --wal-method=fetch --checkpoint=fast` 生成 base.tar，密码仅通过 PGPASSWORD 传递。engine_metadata 记录整实例物理范围、system_identifier 和 timeline。tar 恢复前校验路径与链接，并启动独立集群核对。

MySQL 单库转储不包含 CREATE DATABASE 和 USE；PostgreSQL base.tar 是整集群物理文件。两类文件均通过大小和 SHA-256 后才标记 verified。

恢复结果登记：

```json
{
  "status": "verified",
  "target": "dbbak_restore",
  "details": "已核对 4 个字段、3 行、金额合计 601.25"
}
```

该接口只记录已经执行的隔离演练，本身不会执行恢复，也不接受将未校验备份标记为已恢复。

## 清理预览与执行

预览返回 `preview_id`、`expires_at` 和 `result`。每个对象包含 `candidate` 和具体 `reason`。候选必须已过保留期、不属于最低份数、已校验、未锁定、无活动运行且符合独立全量模型。

执行请求：

```json
{"preview_id": "preview_..."}
```

服务端重新检查任务 revision、对象指纹、活动运行、真实路径、文件大小和 SHA-256。任何变化都要求重新预览。部分失败按对象返回，不会把部分删除报告为全部成功。

## 手动删除任务与备份对象

清理（cleanup）按保留策略自动筛选候选；删除（removal）按用户显式点击的目标执行，两者共用路径约束与摘要复核，但语义不同。删除一律分两步，浏览器与脚本走同一套校验：

1. **评估**：`GET .../removal`（任务、任务文件、单对象三个入口）返回 `file_count`、`total_size`、`run_count` 与 `blockers`。该请求只读，不删任何内容。
2. **执行**：`DELETE ...?force=1&confirm=<目标名称>`，确认名称按 URL 编码传递。任务与任务文件的确认名称是任务名，单个对象的确认名称是 `relative_path`。确认名走查询参数而不是请求头，因为浏览器 `fetch` 的请求头值只允许 ISO-8859-1，中文任务名会被直接拒绝（`String contains non ISO-8859-1 code point`）；`X-DBBak-Confirm` 仍然接受，供外部脚本使用，值含百分号转义时服务端会解码。

阻塞项（`code`）含义：

| 代码 | 原因 | 解除方式 |
| --- | --- | --- |
| `JOB_ENABLED` | 任务仍启用，删除途中可能再次入队 | 先暂停任务 |
| `ACTIVE_RUN` | 存在 queued/running/verifying/reconciling 运行 | 等待结束后再删除 |
| `CONFIRMATION_REQUIRED` | 范围包含已校验或人工锁定对象 | 带 `force=1` 并提交正确确认名称 |
| `CHAIN_DEPENDENT` | 仍有差异或日志依赖该恢复点 | 先删除依赖对象 |
| `ALREADY_DELETED` | 对象已删除 | 无需重复操作 |

执行规则：

- `force=1` 只放宽「已校验备份需确认」这一条，不会绕过活动运行与恢复链依赖。
- 已校验对象删除前复核大小与 SHA-256；不一致、符号链接或越出授权根目录一律拒绝。
- 备份中断会留下临时目录（如 PostgreSQL 的 `.run_<id>.attempt-1`）。这类**由 runner 命名的工作目录**（现行 `<数据库>/<年>/<月>/run_<hex>`，历史记录中的 `.run_<hex>.attempt-<n>` 同样识别；且对象未处于 `verified`）会在整棵校验后递归清理，其中出现符号链接、设备/管道文件或条目超限则整体放弃、不做部分删除；其它目录只删除空目录，**非空目录一律拒绝、不做递归删除**，需人工确认后清理；磁盘上已不存在的对象直接登记为 `deleted`，不会让记录永久悬挂。
- 部分失败时 `message` 会附带前几条真实原因（相对路径 + 具体错误），便于直接定位。
- 文件删除成功后才把对象标记 `deleted`；任一文件失败则整体返回 `409 PARTIAL_FAILURE`，记录与文件保持原状，可重试。
- 删除任务在所有文件删除成功后于一个事务内移除任务的已删对象记录、运行日志、运行记录、清理预览和任务本身，并写入 `job.delete` 审计（含任务名、备份类型、保留策略）。运行历史随任务消失，因此删除前请确认不再需要这些记录。

响应示例：

```json
{
  "delete_run_id": "run_...",
  "files": [{"artifact_id": "art_...", "path": "mysql-prod/db/2026/11/run_....sql", "deleted": true, "message": "已删除并登记"}],
  "deleted_files": 2,
  "failed_files": 0,
  "runs_removed": 3,
  "job_deleted": true,
  "message": "已删除 2 个备份文件"
}
```

## 未实现接口

SQL Server 原生全量已完成真实备份与异名恢复验收；任务创建受 `DBBAK_SQLSERVER_ENABLED` 控制，默认关闭。差异/日志产品 API、远程 Agent 控制端、日志链自动清理和自动恢复执行没有生产 API。对象存储（`kind=oss`）已有 API 与实现，真实 OSS 桶验收未执行。MySQL binlog 与 PostgreSQL WAL 已通过独立脚本验证，但尚未接入产品调度。访问未完成能力不得以前端模拟代替。
