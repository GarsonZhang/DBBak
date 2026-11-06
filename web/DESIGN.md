# DBBak 数据库备份管理

中文数据库备份管理软件交互设计。`SQL` 在此按 Microsoft SQL Server 理解。

## 当前交付

本文保留 v1.1 原型设计作为后续路线参考，设计稿与实现同处 `web/`。当前代码已进入 v0.2.0-phase2：现有中文页面已接入真实 Go API、SQLite、MySQL 逻辑全量、PostgreSQL 整实例物理全量、后台调度、文件校验和清理预览，不再保存演示配置或模拟执行。已验证范围以阶段一及阶段二 PostgreSQL 验证报告为准；SQL Server、差异与日志链、远程 Agent、NAS 和正式发行包仍未实现；对象存储（阿里云 OSS / S3 兼容）已实现，独立全量自动清理已实现并接入调度。

## 引擎适配设计

| 数据库 | 适配器 | 全量 | 差异 | 日志 |
| --- | --- | --- | --- | --- |
| SQL Server | 原生 BACKUP | 支持 | 支持，依赖最近的非 COPY_ONLY 全量基线 | FULL 恢复模式下事务日志链 |
| MySQL | mysqldump + binlog 归档 | 逻辑全量 | 不支持；需更换 MySQL Enterprise Backup 等物理适配器 | 基线记录 binlog 位置或 GTID 后连续归档 |
| PostgreSQL | pgBackRest | 实例/集群物理全量 | 支持 | 连续 WAL 归档 |
| PostgreSQL Windows | pg_basebackup + WAL | 实例/集群物理全量 | 默认禁用，需单独验证工具与版本 | 连续 WAL 归档 |

pgBackRest 路线要求数据库主机与执行工具在兼容环境中。Windows 数据库主机不能仅通过 Linux 仓库端获得 pgBackRest 支持。数据库主机系统、执行器系统、在线状态及工具清单共同决定原型选项；MySQL Enterprise Backup 默认标为待验证，不启用执行。当前所有组合均为演示能力，没有生产兼容验收结果。

差异与增量是不同概念。PostgreSQL 的 pg_dump 不构成物理 WAL 恢复基线；pgBackRest 的物理恢复作用于实例/集群。真实产品需要根据服务器版本、恢复模式、工具版本及授权检查实际能力，不能只按引擎名放开选项。

## 调度与存储

每个任务关联一个数据源、一个备份适配器及一个目标存储；全量、差异与日志分别设置周期。默认业务时区为 Asia/Shanghai。全量支持每日或每周计划，差异通常每日，日志通常按分钟间隔。生产系统统一以 UTC 存时间戳并保留任务时区，在用户界面显示当地时间。相同实例的互斥备份不能并行运行；使用租约锁、幂等执行 ID、超时和有限次数重试。差异/日志执行前验证有效基线和链连续性。

目标存储可配置代理本地目录、网络共享或 S3 兼容对象存储。目录含义是执行代理可访问的服务器路径，不是访问控制台的用户电脑目录。上传采用临时对象，完成后校验大小、校验和及可用元数据，再将其标记为可恢复。

## 生产接入架构

浏览器控制台经 HTTPS 调用控制 API；API 管理任务与权限，持久化到元数据库。调度服务将到期任务投递给持久队列；部署在内网的 Windows/Linux Agent 执行引擎工具，读取秘钥引用，上传备份与结构化日志。调度在后台服务运行，不依赖控制台保持打开。

建议元数据实体：`data_sources`、`storage_targets`、`backup_jobs`、`backup_schedules`、`job_runs`、`backup_artifacts`、`restore_chains`、`retention_policies`、`cleanup_runs` 和 `audit_events`。备份元数据至少记录引擎/工具版本、类型、链 ID、父基线 ID、开始与完成时间、大小、校验和、存储对象 ID、完成状态、保护标记以及 SQL Server LSN / MySQL binlog 位置或 GTID / PostgreSQL WAL 区间。

数据库及仓库凭据由服务端密钥系统加密管理，仅在 UI 展示密钥引用。生产服务采用最小权限账号、按工作空间隔离、操作审计、连接 TLS、可选备份加密以及失败告警。真实连接测试和恢复演练必须由 Agent 进行。

## 保留与清理

按“保留 N 天 + 至少 N 份完整备份链”决定保留的恢复点，并扩展其全量基线、差异/增量祖先及连续日志依赖。超过天数的基线若仍被依赖，仍须保留。手动锁定、正在执行、尚未校验及最近有效恢复链不能自动删除。关闭自动清理只停止自动路径，手动清理仍必须通过依赖保护与预览确认。原型按数据库保守合并样例链；正式版按实例、仓库、适配器组成的保护组汇总策略，再按真实恢复谱系和窗口起点所需日志计算依赖。

流程：确认新备份完成并校验 → 计算依赖闭包 → 生成清理预览 → 执行前重新检查任务状态与依赖 → 删除到期无依赖对象 → 记录审计并支持部分失败重试。预览需显示文件、大小、可释放容量及保护原因。pgBackRest 优先使用其自身 expire 机制。仓库文件清理与数据库原始日志截断是两项独立操作。

## 官方参考

- [SQL Server 差异备份](https://learn.microsoft.com/en-us/sql/relational-databases/backup-restore/differential-backups-sql-server?view=sql-server-ver17)
- [SQL Server 恢复模式](https://learn.microsoft.com/en-us/sql/relational-databases/backup-restore/recovery-models-sql-server?view=sql-server-ver17)
- [MySQL Enterprise Backup 差异与增量](https://dev.mysql.com/doc/mysql-enterprise-backup/8.0/en/mysqlbackup.incremental.html)
- [MySQL 备份方法](https://dev.mysql.com/doc/refman/8.4/en/backup-methods.html)
- [PostgreSQL 连续归档与 PITR](https://www.postgresql.org/docs/17/continuous-archiving.html)
- [pgBackRest 使用指南](https://pgbackrest.org/user-guide.html)

## 开发

`index.html` 为静态入口，`style.css` 提供响应式样式，`app.js` 实现本地交互，无构建步骤。
