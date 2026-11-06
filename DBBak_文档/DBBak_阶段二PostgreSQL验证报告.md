# DBBak 阶段二 PostgreSQL 验证报告

日期：2026-10-30 · 版本：0.2.0-phase2 · 范围：PostgreSQL Windows + pg_basebackup 整实例物理全量

## 结论

在当前 **Windows x86-64 + 本地执行器 + 隔离 PostgreSQL 18.6** 组合下，PostgreSQL 物理全量与独立集群恢复已验证通过。阶段一 MySQL 真实备份恢复回归也保持通过。

本报告只覆盖 PostgreSQL 全量。SQL Server、PostgreSQL 差异/增量、WAL 持续归档和时间点恢复仍未实现，不属于阶段三能力声明。

## 实际实现

- 新增引擎适配器注册表，统一 Version、Probe、Execute 和单文件执行结果。
- MySQL 保持 mysqldump 逻辑全量；PostgreSQL 使用 psql + pg_basebackup 实现整实例物理全量。
- PostgreSQL 探测检查真实版本、REPLICATION/超级用户权限及 max_wal_senders。
- 备份采用 `--format=tar --wal-method=fetch --checkpoint=fast`，生成包含必要 WAL 的 base.tar。
- 元数据记录 logical=false、scope=cluster、format=tar、wal_method=fetch、system_identifier 和 timeline。
- SQLite 第二版迁移为 artifacts 增加 engine_metadata，已有阶段一记录保留。
- 多引擎共用调度、临时目录、SHA-256、运行状态、清理预览和恢复验证记录。
- 中文数据源与任务页面支持 PostgreSQL，明确维护库只用于连接，备份范围为整个实例。

## 真实环境

- 管理服务：Windows x86-64，Go 1.27.3，DBBak 0.2.0-phase2。
- 源集群：PostgreSQL 18.6，127.0.0.1:35432，独立 `.test-runtime/pg-source-data`。
- 工具：psql、pg_basebackup 均 18.6。
- 备份账号：隔离 dbbak_pg，具有 SUPERUSER 与 REPLICATION；只用于本测试。
- 样本：dbbak_app.sample_records，4 个字段、3 行，中文 A/B/C，amount 合计 601.25。
- 恢复：独立运行 ID 对应数据目录，127.0.0.1:35433；每次验证不删除旧恢复证据。

## 真实备份与恢复结果

执行：

```powershell
python scripts/phase2_postgres_verify.py
```

结果：通过。

- 最终验证运行：`run_d2c4f015428651a9f9e3fe7d`。
- 工具版本：`pg_basebackup (PostgreSQL) 18.6`。
- 文件：`.test-runtime/pg-backup-root/postgres/2026/10/run_d2c4f015428651a9f9e3fe7d.tar`。
- 大小：50,284,032 字节。
- SHA-256：`71f66990f7a133636d8164a0aaf293baaea4801c54afd38833b43bcac965e6f1`。
- system_identifier：`7702385004936704968`；timeline：1。
- 解包前检查绝对路径、`..`、符号链接、硬链接和设备对象，随后启动独立恢复集群。
- 恢复后 sample_records 为 4 字段、3 行、amount 合计 601.25，中文内容一致。
- pg_controldata 的 system identifier 与备份元数据一致。
- restore_status 已登记为 verified；恢复集群在验证后停止。
- PostgreSQL 错误密码真实探测返回 AUTH_FAILED。

## 阶段一回归

重新执行 `python scripts/phase1_api_verify.py`，MySQL 8.4.13 真实备份与隔离恢复通过：

- 回归运行：`run_de88b5f884e30496c6977aaf`。
- SQL 文件 2296 字节，SHA-256 与 API 一致。
- dbbak_restore 中 4 字段、3 行、金额 601.25 和中文内容一致。
- 错误密码与非法目录仍分别返回 AUTH_FAILED、STORAGE_NOT_WRITABLE。

## 自动化测试与构建

`go test -cover ./...`、`go vet -p=1 ./...` 和 Linux amd64 交叉构建全部通过。覆盖率为 API 28.9%、backup 52.8%、metadata 27.1%、security 71.1%；入口和 config 无直接覆盖率。Linux 仅完成构建，未运行。

## 中文页面

使用真实 Chrome 登录 0.2.0-phase2 服务，验证概览、任务、数据库、部署与清理相关数据：

- 同时显示 MySQL 与 PostgreSQL 数据源。
- PostgreSQL 显示“整实例物理全量”和维护库语义。
- 能力矩阵显示 mysqldump 与 pg_basebackup 的真实工具版本。
- 未捕获 JavaScript 异常。

## 验证中修正的问题

- psql tuples-only 是一行多列，修正能力探测解析。
- pg_basebackup 的 dbname 按连接信息解析，改为 `dbname=<维护库>`。
- 补齐备份 method 与 engine_metadata 的持久化。
- PostgreSQL 源实例改为受控前台进程，避免命令结束后被回收。
- Windows 中文种子 SQL 改为 UTF-8 标准输入，避免系统代码页破坏内容。
- 恢复输出按 psql 对齐格式规范化，避免把空格当成数据差异。
- 恢复目录改为每次运行独立目录，不批量删除历史恢复集群。

这些问题均通过真实工具与数据库暴露并修正，没有以 mock 结果替代真实验收。

## 未运行与未实现

- SQL Server 真实全量尚未实现，是阶段二下一项任务。
- PostgreSQL 差异/增量、WAL 持续归档、恢复链时间点恢复未实现。
- pgBackRest 未实现；本结果只适用于 Windows 原生 pg_basebackup 路线。
- Linux、Docker、远程 Agent、生产环境、大数据量和性能未验证。
- 自动清理、自动恢复执行、正式 Windows 服务与 systemd 未实现。

## 下一步

固定一个 SQL Server 最小版本、主机系统与备份工具组合，完成真实全量、LSN/基线元数据和隔离目标恢复验证，再结束阶段二。
