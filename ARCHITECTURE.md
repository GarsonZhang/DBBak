# ARCHITECTURE.md | Vault 数据库备份管理

> 本文件只记录稳定、可长期复用的架构事实：系统边界、模块职责、数据流、核心接口与已知约束。
> 阶段进度、待办和验证结果不写在这里，分别见 `TASKS.md` 和 `Vault_文档/`。
> 与代码冲突时以代码为准，并同步修正本文件。

## 1 系统边界

Vault 是**单实例**的数据库备份控制台：一个 Go 进程同时提供 REST API、中文 Web 界面、后台调度和本地执行器。没有独立的控制面集群，也没有外部消息队列。

三条边界必须保持清晰：

1. **元数据与备份正文分离。** SQLite 存放配置、运行、文件和审计记录；备份文件写在受控仓库目录。元数据库不得放到 NAS 或备份目录内。
2. **控制与执行分离。** API 只做校验和入队，真正的工具进程调用由执行器完成，两者通过持久化的 run 记录衔接，因此关掉浏览器不影响执行。
3. **命令成功 ≠ 文件可用 ≠ 恢复可行。** 运行记录用 `command_status`、`file_status`、`restore_status` 三个字段分别表达。

## 2 运行时拓扑

```text
浏览器 ──HTTP──> internal/api ──> metadata(SQLite WAL)
                      │                 ▲
                      │ 入队            │ 状态/日志/文件
                      ▼                 │
                 backup.Runner ─────────┘
                      │
                      ├─> backup.Adapter（mysql / postgresql / sqlserver）
                      │        └─> 外部工具进程：mysqldump / pg_basebackup / sqlcmd
                      └─> 授权仓库目录（临时目录 → 校验 → 提交为最终文件）

backup.Scheduler ──每分钟──> 计划任务入队 + 每日自动清理
security.KeyStore ──> master.key（AES-GCM，独立于 SQLite）
```

## 3 代码模块

| 路径 | 职责 | 边界 |
| --- | --- | --- |
| `cmd/vault/` | `init-admin`、`serve`、`version` 三个子命令 | 只做装配，不含业务逻辑 |
| `cmd/agent/` | 远程 Agent 客户端入口（当前仅 enroll） | 不承载备份逻辑 |
| `internal/api/` | 路由、认证、CSRF 同源校验、参数校验、静态资源 | 不直接调用数据库工具 |
| `internal/backup/` | 适配器注册表、执行器、调度、路径约束、清理 | 唯一允许启动外部进程的位置 |
| `internal/metadata/` | SQLite 连接、版本化迁移、各实体仓储 | 不感知 HTTP 与工具进程 |
| `internal/security/` | bcrypt、会话令牌、AES-GCM 密钥库 | 不保存明文凭据 |
| `internal/config/` | 环境变量解析与默认值 | 无副作用 |
| `internal/agent/` | mTLS 客户端、一次性注册、心跳与事件上报、fence 校验 | 服务端接口尚未实现 |
| `embed.go` + `web/` | 嵌入式前端资源 | 目录必须与 `embed.go` 同处模块根 |
| `scripts/`、`deploy/`、`packaging/` | 隔离环境、验收脚本、部署与构建资产 | 不属于运行时代码 |

Go 模块根位于仓库根目录，`go build ./...`、`go test ./...` 可直接在根执行，无需切换目录。

## 4 关键数据流

### 4.1 备份执行（`internal/backup/runner.go`）

1. 按 `job → source → storage_target` 校验，检查引擎适配器是否存在；缺失则直接失败，不落到工具调用。
2. 创建 run，状态 `queued`，`attempt=1`，三态字段初始化为 `not_run / pending / not_run`。
3. 读取并解密凭据，登记 `running`，写运行日志。
4. 采集工具版本，建立 artifact 记录（`state=writing`）。
5. 在仓库内创建**唯一工作目录** `{database}/{yyyy}/{mm}/.{run_id}.attempt-1/`，由适配器写入实际产物。
6. 校验产物大小与 SHA-256，`run.state=verifying`。
7. 提交为最终文件 `{database}/{yyyy}/{mm}/{run_id}{ext}`，artifact 置 `verified`，run 置 `succeeded`。
8. 任一步失败：run 置 `failed`，artifact 置 `quarantined`，记录结构化错误码；不删除失败产物，留作对账。

临时目录与最终文件使用不同路径，是为了让中断、重试和审计都能分辨"这条记录对应哪个物理文件"。

### 4.2 调度与幂等（`scheduler.go`）

- 每分钟 `tickWithTime`，按任务时区判断是否到点；默认**不集中补跑**错过的时间点，只写 missed 记录。
- 计划触发依靠唯一键去重，手动触发依靠幂等键，同一任务不允许并发执行。
- 每日在固定窗口执行自动清理，仅对**显式启用**策略的任务生效。

### 4.3 清理（`cleanup.go`）

1. 只把 `state=verified` 且未锁定的全量备份视为恢复点。
2. 保留集合 = 保留天数窗口 + 最低份数，取更保守者。
3. 生成预览并计算**指纹**（artifact 的 ID/状态/锁定/路径/大小/哈希/run_id 加候选标记），预览绑定该指纹与任务 revision。
4. 执行时重新计算指纹，不一致即拒绝，要求重新预览。
5. 删除前再次解析路径，只允许授权根目录内的已登记文件。

当前只有全量，依赖模型退化为"每份全量是独立恢复点"；差异与日志链接入时必须扩展为依赖图，不能复用现有逻辑冒充。

### 4.4 恢复验证

恢复不在产品内自动执行。外部脚本完成真实恢复后，通过 `POST /api/v1/runs/{id}/restore-verification` 登记目标、结果与时间，`restore_status` 独立于前两个状态。

## 5 数据模型

SQLite 12 张表：`users`、`sessions`、`secrets`、`sources`、`storage_targets`、`jobs`、`runs`、`run_logs`、`artifacts`、`cleanup_previews`、`audit_events`、`schema_migrations`。

迁移为递增版本，当前 v3：

| 版本 | 变更 |
| --- | --- |
| 1 | 基础表结构 |
| 2 | `artifacts.engine_metadata`（物理/逻辑范围、system identifier、LSN 等） |
| 3 | `storage_targets.server_root`（数据库服务侧路径，供 SQL Server 区分执行器路径与实例路径） |

时间统一按 UTC 存储，任务另存时区；配置更新使用 `revision` 乐观并发。

## 6 核心接口

- REST：`/api/v1` 下 29 条路由，覆盖认证、概览、能力、设置、数据源、存储、任务、运行、日志、备份文件、清理预览与执行、恢复验证。除登录和健康检查外全部要求会话认证。
- 静态资源：`/` 及 `web/` 内容由同一进程提供，前端需要 `X-Vault-Request` 同源确认头。
- Agent 协议：`/agent/v1/enroll`、`/leases/pull`、`/leases/{id}/heartbeat`、`/runs/{id}/events`、`/runs/{id}/complete`。**客户端已实现并测试，服务端未实现。**

## 7 适配器契约

当前接口只有四个方法：

```go
type Adapter interface {
    Engine() string
    Version(ctx) (string, error)
    Probe(ctx, source, password) (string, error)
    Execute(ctx, source, target, password, workPath) (ExecuteResult, error)
}
```

`ExecuteResult` 返回产物路径、扩展名、方法与 `engine_metadata`。设计文档中的 `Plan`、`CollectManifest`、`Verify`、`Cancel`、`Reconcile` 尚未进入接口——差异、增量、日志链必须新增能力，不得用重复全量伪装。

| 引擎 | 工具 | 语义 |
| --- | --- | --- |
| MySQL | `mysqldump` | 单库逻辑全量，不带 `CREATE DATABASE`/`USE` |
| PostgreSQL | `pg_basebackup --format=tar` | 整实例物理全量，含恢复所需 WAL |
| SQL Server | `sqlcmd` + `BACKUP DATABASE` | 原生全量；能力状态 `unverified`，禁止建任务 |

## 8 关键设计决定

| 决定 | 理由 |
| --- | --- |
| Go 模块放仓库根 | 遵循 Go 惯例：克隆后可直接 `go build ./...`，IDE、CI 与 Docker 构建都不需要额外路径前缀 |
| SQLite 使用纯 Go 驱动 | 免 CGO，Windows/Linux 交叉构建简单；单实例写入量小 |
| 主密钥独立文件 | 元数据备份与密钥备份分离，防止一份备份同时泄露密文与密钥 |
| 三个状态字段 | 备份命令成功、文件校验通过、恢复验证通过是不同事实 |
| 清理预览绑定指纹 | 防止预览后文件被替换或任务被修改导致误删 |
| 路径只在授权根内解析 | 阻断 `..`、盘符、UNC、符号链接越界，删除范围可枚举 |
| 前端不引框架 | 现有静态界面已够用，避免为一张控制台引入构建链 |

## 9 已知约束与技术债

- **单实例**：无分布式调度、无多租户、无高可用；SQLite 与进程生命周期绑定。
- **SQL Server 未验证**：适配器与脚本已写，真实备份与异名恢复未运行，能力保持 `unverified`。
- **日志链未产品化**：MySQL binlog 与 PostgreSQL WAL 已用独立脚本真实验证，但归档管理、恢复链和 PITR 未进入任务模型。
- **Agent 只有客户端**：mTLS、一次性注册与 fence 已实现并测试，服务端 `/agent/v1` 未实现，无法接 Runner。
- **单角色**：只有管理员，操作员与只读审计员未实现，权限边界靠服务端鉴权而非前端隐藏。
- **无 S3/NAS 适配**：仅本地目录，`storage_targets` 已留 `kind` 字段。
- **未做容量治理**：无仓库空间预检与配额，清理释放空间为估算。
