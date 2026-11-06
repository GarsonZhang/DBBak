# DBBak 数据库备份管理开发文档

跨平台服务架构与工程实现规范

版本：1.0 · 日期：2026-10-30 · 状态：设计评审稿

配套文件：[产品需求](DBBak_产品需求文档.md) · [原型设计说明](DBBak_原型设计说明.md) · [HTML 交互原型](../web/index.html)

## 1 系统架构与实现边界

本方案定义 Windows 原生服务、Linux 服务与 Linux Docker 容器的共同架构。正式服务负责调度、备份、校验、保留和审计。当前浏览器原型仅演示配置与交互，真实后端和三类发行包尚未实现。

### 建议架构

建议首版采用 Go 模块化单体并嵌入 Web 资源，内部区分 API、调度、分派和清理模块。Go 支持 Windows 与 Linux 构建，依赖与目标架构仍须验证。[S1] 技术选型为工程建议，数据库兼容清单待实测确定。

| 组件 | 职责与数据流 |
| --- | --- |
| 控制服务 | 保存配置并计算计划，将任务交给本机或远程执行器。 |
| 执行器 Agent | 主动拉取任务，调用工具，上报进度、清单和结果。 |
| 备份仓库 | 保存备份对象，支持代理本地目录、网络共享或 S3。 |
| 元数据库 | 保存任务、租约、恢复依赖和审计，不存放备份正文。 |

单控制节点使用本地 SQLite WAL 和持久任务表，纯 Go 驱动待构建及事务验证；不得将元数据库放在 NAS。多节点与 PostgreSQL 元数据库后续扩展，首版不依赖 Redis。管理端跨平台与具体备份工具兼容性分别验收。

## 2 模块划分与执行拓扑

控制服务提供统一业务 API，本机执行器可与服务同机安装；远程 Agent 部署到数据库所在网络，使用双向 TLS 建立身份并主动连接控制端。初次注册使用短期且只能使用一次的注册令牌，换取绑定执行器身份的证书；后续请求仅使用该身份访问分配给自己的任务。

| 模块 | 建议代码边界 | 主要责任 |
| --- | --- | --- |
| 入口与界面 | cmd/server 与 web | 启动服务、嵌入页面、健康检查。 |
| 领域与接口 | internal/domain 与 api | 校验配置、权限判断、版本并发控制。 |
| 调度与队列 | scheduler 与 dispatcher | 计算到期时间、事务入队、租约分配。 |
| 执行与适配 | agent 与 adapters | 进程生命周期、引擎检查、结构化结果。 |
| 存储与保留 | storage 与 retention | 对象写入、依赖闭包、清理预览和执行。 |
| 基础设施 | metadata 与 secrets | 数据库迁移、加密凭据、审计及指标。 |

### 执行约束

- 单机模式走同一任务协议和权限检查，不另外实现一套本地备份逻辑。关闭浏览器不影响服务；宿主机关机或 Agent 离线会中断或延迟执行。
- 数据库互斥资源由适配器声明，不能按仓库划分；同实例向不同仓库备份仍可能冲突。首版默认同实例仅运行一项数据备份；长期日志采集使用独立冲突规则，不长期占用数据备份独占锁。
- 仓库使用可配置并发和带宽配额；日志归档独立管理，但须与全量基线、日志位置和工具能力协调。禁止以固定同时任务数承诺所有机器吞吐量。

## 3 备份适配器与能力发现

能力由数据库类型与版本、数据库主机系统、工具版本、恢复模式、文件权限及许可共同决定。探测结果输出 supported、unsupported、prerequisite_missing 或 unverified；仅 supported 可直接启用，前提缺失须显示修复说明，未验证组合不能标记为支持。

| 适配路线 | 可用模式与关键前提 | 文件与范围 |
| --- | --- | --- |
| SQL Server 原生 | 全量、差异、事务日志；日志需要适当恢复模式与有效链。PITR 默认要求 FULL。 | 由 SQL Server 服务写入可访问路径；记录数据库标识、LSN 与差异基线。[S3] |
| MySQL 逻辑 | mysqldump 全量与 binlog 归档；记录一致性快照的日志坐标或 GTID。 | 导出写到执行器；mysqldump 不提供物理差异。[S4][S6] |
| MySQL 物理 | 差异需经验证的物理适配器，例如 Enterprise Backup；检查许可与版本。 | 需要数据库侧文件与工具权限，不能仅凭数据库端口执行。[S5] |
| PostgreSQL Linux | pgBackRest 全量、差异与 WAL；配置 stanza 和连续归档。 | 数据库侧也需要受支持工具；物理备份按实例或集群处理。[S8] |
| PostgreSQL Windows | 基线采用经验证的 pg_basebackup 与 WAL 路线；差异默认关闭。 | 按实例备份；按引擎及工具版本验证增量能力。pgBackRest 无原生 Windows 支持。[S7][S9] |

统一适配接口包括 Probe、Plan、Execute、CollectManifest、Verify、Cancel 和 Reconcile。每个结果必须返回工具版本、退出状态、备份范围、对象清单、位置元数据和恢复依赖。pg_dump 仅作逻辑导出，不能充当 WAL 物理恢复基线；物理备份不承诺跨操作系统或跨主版本直接恢复。

## 4 元数据模型与一致性

| 实体 | 关键字段 | 约束与索引 |
| --- | --- | --- |
| sources | id、engine、instance_uid、scope、host、secret_ref | 实例身份与连接地址分离；连接可迁移。 |
| executors | id、platform、capabilities、last_seen、certificate_id | 证书指纹唯一；按状态和心跳查询。 |
| storage_targets | id、kind、executor_id、root、secret_ref | 根目录与执行器绑定；凭据引用不回显。 |
| jobs 与 schedules | job_id、source_id、target_id、subdir、adapter、config_version、timezone、next_run_at | 到期索引 enabled 与 next_run_at；`subdir` 是任务在根目录下的相对落点，用于区分共用同一根目录的多个实例。 |
| runs | id、job_id、scheduled_at、state、attempt、lease_owner、lease_until、fence | 计划触发唯一键为任务、计划版本、UTC 触发时间。 |
| artifacts 与 edges | artifact_id、run_id、group_id、type、size、hash、state、start_pos、end_pos、parent_id | 对象键唯一；依赖边唯一；按组及时间范围查询。 |
| retention 与 cleanup | group_id、days、min_full、auto_enabled、preview_hash、revision、state | 策略与清单版本冲突则拒绝旧预览。 |
| audit 与 secret_refs | actor、action、target、result、request_id；provider、key_id、version | 审计追加写；密钥版本引用不含明文。 |

保护组按源实例、备份范围、仓库和适配器汇总保留要求；恢复链另记 SQL Server 数据库身份与 recovery fork、MySQL 源身份及日志坐标、PostgreSQL system identifier 与 timeline。跨谱系关系必须由引擎元数据证明。任务改库或迁移仓库不重写旧文件归属。对象状态为 writing、uploaded、verified、quarantined、deleting、deleted。

所有业务时间戳采用 UTC，另存原始时区。对象上传与元数据库事务无法原子提交，因此先写临时对象，完成后校验并提交清单；后台对账处理已上传未登记、已标删除未删完等中间状态。清单版本递增，更新操作使用 revision 做乐观并发控制。

## 5 调度状态机与重复执行控制

首版提供每日、每周和分钟周期，高级 Cron 后置。默认时区为 Asia/Shanghai；计算器使用任务时区生成 UTC 触发时刻。夏令时不存在的时刻跳过并记事件，重复时刻只执行一次。服务恢复后默认不集中补跑错过的任务，记录 missed 事件并允许人工补跑。

| 状态流转 | 进入条件与处理 |
| --- | --- |
| queued → leased | 事务领取且写入租约、执行器及单调递增 fence；取得实例互斥锁。 |
| leased → running | Agent 确认接单并完成前置检查；开始记录进度和工具进程身份。 |
| running → verifying | 工具返回成功且文件上传完成；验证清单、校验和与依赖完整性。 |
| verifying → succeeded | 校验达到配置要求，原子提交可恢复元数据；记录当前验证等级。 |
| 任意执行态 → cancelling | 接收取消请求，通知工具并等待实际停止；停止确认后进入 cancelled。 |
| 租约失效 → reconciling | 先核查旧进程和数据库操作；确认停止后标记 interrupted 或重新排队。 |
| 失败 → failed 或 queued | 仅可重试的失败使用有限次数退避；每次重试保存独立 attempt。 |

调度事务使用唯一触发键防止重复入队；手动运行使用 Idempotency-Key，重复键及相同负载返回原任务，负载不同返回 409。租约建议 60 秒、心跳 15 秒，作为可调初值。fence 阻止过期 Agent 提交结果，但无法自动终止已在数据库内执行的命令，因此不能仅凭租约过期立即重派。

SQL Server 日志备份可按分钟调度；binlog 与 WAL 归档采用持续采集或工具原生归档。页面的日志周期应映射为该适配器支持的采集、切换或延迟检查参数，不能把每 15 分钟轮询直接承诺为 15 分钟 RPO。

## 6 备份管道与恢复验证

执行前冻结任务配置快照，验证网络、凭据权限、工具版本、目标空间及有效基线。差异缺少基线时默认失败并给出提示；仅用户启用允许自动补全量后，才创建独立全量任务。日志缺口必须告警，不能静默重建链并宣称原恢复窗口连续。

| 阶段 | 输出及失败处理 |
| --- | --- |
| 准备 | 以 run_id 与 attempt 建立唯一临时对象前缀；记录数据库身份和备份前位置；检查实例互斥锁。重试不覆盖旧文件。 |
| 执行 | 通过进程参数数组调用工具或数据库驱动发送命令；限时、限速并采集结构化进度。 |
| 归档 | 读取工具生成的清单与位置；压缩、可选加密后写入仓库，记录每个对象的校验和。 |
| 校验 | 确认退出状态、传输完成、对象大小与摘要、基线关系以及日志连续性。 |
| 提交 | 提交清单及依赖边，更新可恢复窗口；只有已确认结果才标记成功。 |
| 对账 | 失败文件隔离，保留诊断记录；定期处理孤立临时对象和无法确认的数据库操作。 |

### 文件路径与验证等级

mysqldump 和 pg_dump 输出到执行器文件系统；SQL Server BACKUP 的路径属于数据库服务可访问的文件系统，可通过共享目录或数据库侧 Agent 转存。[S3][S4] NAS 权限需按服务账号验证。只持久化 Docker 目录不构成一致性备份，禁止直接复制正在运行的数据目录作为通用方案。

验证等级分别记录文件校验、工具校验和恢复演练。文件摘要正确不能证明数据库可恢复。恢复演练在隔离目标上恢复指定恢复点，核对工具结果、数据库可打开及用户定义的只读检查；禁止默认覆盖生产数据库。首版可先提供恢复清单与演练记录，自动恢复执行后置。

## 7 保留计算与安全清理

同一保护组内，保留天数和最低全量份数取全局与关联任务的最大值。任一任务关闭自动清理，只阻止该组的自动删除；人工清理仍需经过预览和保护检查。持有 retention_hold 的文件、运行中正在写入的对象、未验证对象及最近一条有效恢复链均不得进入普通清理候选。

- 确定保护窗口起点 T0，保留覆盖 T0 的有效基线及后续恢复点，即使基线早于保留期。日志缺段、外部全量改变差异基线或 timeline 切换时，重新核验依赖；链不完整则暂停该组自动清理并报告恢复缺口。
- 保留最近 N 份已验证且可独立构成有效恢复链的全量，扩展所有差异或增量祖先；依据 LSN、binlog 坐标或 WAL 范围保留恢复所需连续日志。
- 对上述节点计算依赖闭包。其余对象须同时满足已到期、清单完整、没有引用且不在保护状态，才能成为候选；未知文件和无法解析的链保留待核查。
- 预览返回候选对象、释放容量、保护原因、清单与策略 revision、有效期和服务器签名摘要。执行时重新授权并校验版本、运行状态及闭包；变化后返回 409 要求重新预览。

执行以 cleanup_run 记录每个对象的删除状态，先标记 deleting，再删除对象，最后提交 deleted。对象存储删除应使用版本 ID 或条件请求；部分失败只重试仍存在且仍符合条件的对象。使用短期锁或版本隔离阻止新任务在校验与删除之间引用候选。

对于 pgBackRest 等管理自有仓库的工具，应通过其保留或 expire 接口执行，不能绕过工具任意删文件。[S8] 仓库备份删除与数据库原始 binlog、WAL、事务日志截断分开管理；本模块不会删除未经登记的数据库日志。清理失败不能改变备份成功状态，但必须产生独立告警和审计。

## 8 控制接口契约

接口前缀统一为 /api/v1，JSON 使用 UTF-8，时间使用带 Z 的 RFC 3339 UTC 格式。读取支持 cursor 和 limit；变更接口校验 revision 或 If-Match。管理员管理配置与凭据，运维员运行任务及申请清理，审计员只读。删除与持有解除必须单独授权。

| 方法与资源 | 用途 | 成功状态与权限 |
| --- | --- | --- |
| GET /capabilities | 获取按版本及平台区分的能力 | 200 只读 |
| POST /sources | 创建数据源及凭据引用 | 201 管理员 |
| POST /sources/{id}/probes | 提交连通性与能力探测 | 202 运维员 |
| POST /storage-targets | 保存仓库与路径映射 | 201 管理员 |
| POST /jobs | 创建计划及保留配置 | 201 运维员 |
| PATCH /jobs/{id} | 修改或暂停任务 | 200 运维员 |
| POST /jobs/{id}/runs | 幂等触发一次运行 | 202 运维员 |
| GET /runs/{id} | 读取状态和验证等级 | 200 只读 |
| POST /runs/{id}/cancel | 请求取消并等待确认 | 202 运维员 |
| GET /runs/{id}/logs | 分页获取已脱敏日志 | 200 只读 |
| GET /artifacts | 查询清单及恢复依赖 | 200 只读 |
| POST /artifacts/{id}/hold | 保护或解除人工持有 | 200 管理员 |
| POST /retention/previews | 计算候选与保护原因 | 201 运维员 |
| POST /cleanup-runs | 凭预览令牌提交清理 | 202 清理权限 |
| POST /executors/enrollments | 签发一次性注册令牌 | 201 管理员 |

400 表示格式错误，401 未认证，403 无权限，404 不存在，409 版本或状态冲突，422 备份能力或配置前提不满足，429 超出配额。长任务只返回运行资源，不以 HTTP 请求持续连接承载整个备份过程。

## 9 请求样例与执行器协议

下例为拟定的 SQL Server 任务创建请求，需在创建前完成能力探测。source_id 与 target_id 引用服务端资源，不传入密码。接口保存后返回任务 id、revision 和各计划下次执行时间；这些接口尚未接入当前原型。

```text
POST /api/v1/jobs
{
  "name": "finance 连续保护",
  "source_id": "src_finance", "target_id": "repo_primary",
  "adapter": "sqlserver_native",
  "timezone": "Asia/Shanghai",
  "schedules": [
    {"type":"full", "frequency":"weekly",
     "weekday":0, "time":"02:00"},
    {"type":"differential", "frequency":"daily",
     "time":"04:00"},
    {"type":"log", "interval_minutes":15}
  ],
  "retention": {"days":30, "min_full":2,
                "auto_cleanup":true}
}
```

错误体统一为 {code, message, field_errors, retryable, request_id}。例如 code 为 CAPABILITY_UNSUPPORTED、field_errors 指向 schedules[1].type，提示当前适配器不支持差异；不向客户端返回命令行凭据、文件系统内部异常栈或数据库连接串。

### Agent 任务交互

Agent 经 POST /agent/v1/leases/pull 拉取任务，收到 run_id、attempt、fence、配置摘要及短期访问引用。通过 /leases/{id}/heartbeat 延长租约并上报进度，通过 /runs/{id}/events 发送有序事件，通过 /runs/{id}/complete 提交结果与清单。所有上报带 fence 与递增序号；重复事件去重，旧租约结果拒绝。取消指令随心跳响应或后续拉取返回，Agent 必须确认实际停止。

## 10 权限凭据与运行安全

正式版使用服务端认证会话与角色权限。首次启动只生成短期初始化令牌，设置管理员后立即作废；不发布默认密码。远程访问通过 HTTPS，默认绑定回环地址，开放到其他网络必须由管理员明确配置。前端不保存数据库密码或云密钥，配置导出也只包含 secret_ref。

| 控制项 | 实现要求 |
| --- | --- |
| 凭据存储 | 通过 SecretProvider 接口接入操作系统保护或外部密钥服务；密钥与密文分离，记录版本及轮换时间。 |
| 授权与审计 | 配置、运行、清理、持有解除、凭据变更独立授权；审计记录操作者、目标、差异摘要和结果。 |
| 进程调用 | 使用参数数组，禁止拼接 shell 命令；工具二进制来自配置白名单，严格限制用户可提供的附加参数。 |
| 路径隔离 | 规范化子路径并验证位于授权根目录；处理符号链接、Windows junction、UNC、盘符及路径穿越。 |
| 网络访问 | 数据源地址和 Agent 目的地可设允许列表；阻止访问云元数据等不属于备份目标的敏感地址。 |
| 日志与配置 | 密码、令牌、连接串等在源头脱敏；审计与运行日志分存，导出遵循相同权限。 |
| 文件保护 | 设置最小读写权限；备份加密密钥有独立恢复流程，不能仅保存在待备份容器中。 |

本机与容器均按最小权限运行，默认不挂载 Docker socket、不要求 privileged。数据库账号按适配器授予必要权限；能查询数据并不等于能执行完整物理备份。证书吊销、Agent 丢失、密钥不可用、仓库只读等状态须产生可追踪错误，禁止降级为未加密或跳过校验后继续宣称成功。

## 11 打包部署与升级运维

| 发行形态 | 安装与持久化设计 |
| --- | --- |
| Windows | 提供签名安装包及 Windows 服务；配置、元数据和日志默认放置 ProgramData 下独立目录，服务账号按权限配置。 |
| Linux | 提供按架构构建的二进制、配置样例和 systemd 单元；配置与状态目录分离，使用专用服务用户。 |
| Docker | 提供 Linux OCI 镜像及 Compose 文件；通过挂载保存状态、备份和凭据引用，固定工具版本并执行非 root 运行验证。 |

Docker 镜像属于 Linux 容器版本。Windows 原生包无需 Docker；在 Windows 上运行该镜像需要适合该宿主系统的 Linux 容器环境。以下仅为执行器的部署配置片段，build 目录和启动程序待工程实现，不是现成可运行的发行包。

```yaml
services:
  dbbak:
    build: ./deploy/docker
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - ./state:/var/lib/dbbak
      - /srv/db-backups:/backups
    stop_grace_period: 120s
```

挂载使文件脱离容器生命周期保存；不会自动提供数据库一致性。[S2] 启动时校验目录权限及工具版本。/health/live 只反映进程存活，/health/ready 检查元数据库和队列可用性，单个备份失败不应使服务反复重启。停止时暂停领取新任务，等待或协调当前任务，超时进入待对账状态。

升级前暂停分派，以一致性备份机制保存元数据库、配置及密钥恢复材料；SQLite 不能只复制主文件而遗漏 WAL。运行幂等迁移后再启动分派。仅结构兼容时允许降级，否则恢复升级前快照并对账仓库。监控队列滞后、失败率、归档延迟、仓库容量、恢复窗口缺口和清理部分失败。

## 12 测试矩阵与交付门槛

发布门槛按可验证的数据库、主机系统和适配器组合建立。Windows、Linux 与 Docker 是控制服务发行形态；数据库可能部署在另一台不同系统的主机，不能以控制台安装成功替代备份能力验收。具体 Windows 与 Linux 发行版本、CPU 架构及数据库版本在首轮试点前冻结，目前均为待验证清单。

| 测试层 | 必测场景 | 对应需求 |
| --- | --- | --- |
| 单元测试 | 周期与夏令时、错过触发、幂等键、状态转换、路径规范化、保留依赖闭包。 | FR05 至 FR08 |
| 平台集成 | 三类安装、服务重启、权限不足、中文及空格路径、挂载与数据持久化。 | FR01 FR12 |
| 引擎集成 | 全量与允许的差异日志；缺基线、工具不匹配、日志缺口、数据库身份变化。 | FR02 FR06 FR07 |
| 故障注入 | 断网、进程终止、磁盘满、上传部分成功、租约过期、过期 Agent 上报。 | FR03 FR04 FR06 |
| 清理测试 | 窗口起点基线、跨任务依赖、人工持有、预览过期、并发引用、部分删除失败。 | FR08 |
| 恢复验收 | 从隔离备份恢复指定时间点，验证范围与数据；记录实际恢复耗时。 | FR10 |
| 安全验收 | 越权、凭据泄漏、命令注入、路径穿越、证书吊销和审计完整性。 | FR09 FR11 |

建议开发顺序为领域与安装骨架、全量端到端、日志与差异适配、保留清理、恢复验证及发布加固。每个适配器只有在备份和恢复均通过后才能进入支持列表；未通过的能力在界面中禁用并说明原因。RPO 与 RTO 记录为配置目标和实测结果，不作为未经测量的产品承诺。

正式交付应包含三类发行包、兼容矩阵、数据库迁移、配置样例、API 说明、故障码、恢复手册、自动化测试报告及升级回退步骤。当前阶段交付设计文档与交互原型；真实备份实现和上述工程验证为后续开发范围。

## 13 官方技术依据

以下资料用于核实底层能力和操作约束。产品接口、状态模型、保留算法和默认参数为本方案的工程设计，需在实现中验证。查阅日期为 2026 年 10 月 30 日。

[S1] Go 构建目标与平台变量
<https://go.dev/doc/install/source>

[S2] Docker 持久化存储与目录挂载
<https://docs.docker.com/engine/storage/>
<https://docs.docker.com/engine/storage/bind-mounts/>

[S3] SQL Server 备份设备 恢复模式与差异备份
<https://learn.microsoft.com/en-us/sql/relational-databases/backup-restore/backup-devices-sql-server>
<https://learn.microsoft.com/en-us/sql/relational-databases/backup-restore/recovery-models-sql-server>
<https://learn.microsoft.com/en-us/sql/relational-databases/backup-restore/differential-backups-sql-server>

[S4] MySQL 8.4 mysqldump
<https://dev.mysql.com/doc/refman/8.4/en/mysqldump.html>

[S5] MySQL Enterprise Backup 差异与增量
<https://dev.mysql.com/doc/mysql-enterprise-backup/8.0/en/mysqlbackup.incremental.html>

[S6] MySQL binlog 与时间点恢复
<https://dev.mysql.com/doc/refman/8.4/en/point-in-time-recovery-binlog.html>

[S7] PostgreSQL 基础备份与连续归档
<https://www.postgresql.org/docs/current/app-pgbasebackup.html>
<https://www.postgresql.org/docs/current/continuous-archiving.html>

[S8] pgBackRest 使用指南与保留策略
<https://pgbackrest.org/user-guide.html>

[S9] pgBackRest 维护者关于 Windows 移植的说明
<https://github.com/pgbackrest/pgbackrest/issues/2431>
