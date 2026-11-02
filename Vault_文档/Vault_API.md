# Vault 当前 API 说明

版本：`0.2.0-phase2` · 前缀：`/api/v1` · JSON：UTF-8 · 时间：RFC 3339 UTC

## 认证与请求约束

管理员通过 `vault init-admin` 创建，无默认账号或默认密码。登录成功后服务设置 HttpOnly、SameSite=Strict 会话 Cookie。所有需要认证的接口支持 Cookie；也支持 `Authorization: Bearer <会话令牌>`。

Cookie 会话的写请求必须带 `X-Vault-Request: 1`，否则返回 403。前端不得把数据库密码或主密钥写入 localStorage。

```powershell
curl -c cookies.txt -H "Content-Type: application/json" -H "X-Vault-Request: 1" `
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
| GET/POST `/api/v1/sources` | MySQL/PostgreSQL 数据源；SQL Server 可登记但任务禁止创建 | 200/201 |
| PATCH `/api/v1/sources/{id}` | 编辑数据源，需 revision | 200 |
| POST `/api/v1/sources/{id}/probe` | 真实连接与版本检测 | 200/422 |
| GET/POST `/api/v1/storage-targets` | 本地目录列表/新增与写检查 | 200/201 |
| PATCH `/api/v1/storage-targets/{id}` | 编辑仓库；有备份时禁止改根目录 | 200 |
| POST `/api/v1/storage-targets/{id}/check` | 真实创建并删除写测试文件 | 200/422 |
| GET/POST `/api/v1/jobs` | 全量任务列表/新增 | 200/201 |
| PATCH `/api/v1/jobs/{id}` | 调度、启停和保留配置，需 revision | 200 |
| POST `/api/v1/jobs/{id}/runs` | 手动触发，支持 Idempotency-Key | 202/200 |
| GET `/api/v1/runs?job_id=` | 运行列表 | 200 |
| GET `/api/v1/runs/{id}` | 运行状态与三个验证等级 | 200 |
| GET `/api/v1/runs/{id}/logs` | 已脱敏运行日志 | 200 |
| POST `/api/v1/runs/{id}/restore-verification` | 登记隔离恢复实测结果 | 200 |
| GET `/api/v1/artifacts?job_id=` | 备份文件、摘要与恢复依赖字段 | 200 |
| PATCH `/api/v1/artifacts/{id}/hold` | 人工锁定/解除 | 200 |
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
  "database": "vault_source",
  "username": "vault_test",
  "password": "只在请求体中提交"
}
```

编辑时 `password` 可省略以保留原凭据，`revision` 必填。列表和响应不包含密码或密文。`engine` 当前支持 `mysql` 与 `postgresql`，已有数据源不能修改引擎。数据库名或维护库名允许字母、数字、下划线、`$` 和连字符，最长 64 字符。PostgreSQL 的 `database` 是连接维护库（通常为 postgres），备份范围为整个实例。

连接检测真实执行 mysql 或 psql，分别返回 TOOL_MISSING、NETWORK_ERROR、AUTH_FAILED、DATABASE_NOT_FOUND、PREREQUISITE_MISSING 等错误，不会把 TCP 连通当作备份可用。PostgreSQL 还检查 REPLICATION/超级用户权限和 max_wal_senders，返回 backup_type=physical_cluster_full。

## 任务与调度

```json
{
  "name": "每分钟隔离全量",
  "source_id": "src_...",
  "target_id": "repo_...",
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

`schedule_type` 支持 `daily`、`weekly`、`interval`。每日/每周使用时区和 `time_of_day`；周计划另用 0~6 的 `weekday`；间隔计划使用 1~1440 分钟。`auto_cleanup: true` 启用独立全量自动清理，调度器每天任务本地时间 05:00 后使用与人工清理相同的预览、指纹、路径和哈希复查；日志链感知清理仍未实现。

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
  "target": "vault_restore",
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

## 未实现接口

SQL Server 原生适配器代码已实现但标为 `unverified`，服务端禁止创建任务，真实恢复未运行。差异/日志产品 API、远程 Agent 控制端、NAS/S3、日志链自动清理和自动恢复执行没有生产 API。MySQL binlog 与 PostgreSQL WAL 已通过独立脚本验证，但尚未接入产品调度。访问未完成能力不得以前端模拟代替。
