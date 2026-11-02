# Vault 兼容性与依赖记录

核对日期：2026-10-30。只有带真实验证结果的组合可称为已验证支持。

## 固定基线

| 组件 | 固定版本 | 用途与许可证/分发注意事项 |
| --- | --- | --- |
| Go 工具链 | 1.27.3 | 构建服务；Go BSD 风格许可证，便携包按官方 SHA-256 校验 |
| MySQL Server | 8.4.13 LTS | 隔离源库和恢复库；Community 版许可证与再分发要求须在发行时审查 |
| mysqldump / mysql | 8.4.13 | 逻辑全量、连接检测和恢复；当前与服务器同版本 |
| PostgreSQL / psql / pg_basebackup | 18.6 | 整实例物理全量与独立集群恢复；PostgreSQL License，发行时保留许可与 notices |
| modernc.org/sqlite | v1.61.0 | 纯 Go SQLite，BSD-3-Clause；含第三方许可，发布时保留 notices |
| golang.org/x/crypto | v0.58.0 | bcrypt 密码哈希，BSD-3-Clause |
| SQLite 驱动内置 SQLite | 3.54.1（包文档所列） | Public Domain；驱动另含 sqlite-vec MIT 等第三方条款 |

官方依据：

- MySQL LTS 发布模型：https://dev.mysql.com/doc/refman/8.4/en/mysql-releases.html
- MySQL 8.4.13 下载与校验：https://dev.mysql.com/downloads/mysql/8.4.html
- mysqldump 能力与选项：https://dev.mysql.com/doc/refman/8.4/en/mysqldump.html
- Go 1.27.3 下载与 SHA-256：https://go.dev/dl/
- PostgreSQL Windows 下载与平台说明：https://www.postgresql.org/download/windows/
- pg_basebackup 官方文档：https://www.postgresql.org/docs/18/app-pgbasebackup.html
- SQLite 驱动、平台和许可：https://pkg.go.dev/modernc.org/sqlite
- bcrypt 文档：https://pkg.go.dev/golang.org/x/crypto/bcrypt

MySQL 8.4 属于 LTS 系列。mysqldump 是逻辑备份工具，不是物理差异/增量工具；大规模备份恢复性能不在本阶段承诺范围内。

## 当前验证矩阵

| 管理服务系统 | 数据库服务器系统 | 执行器 | 数据库/工具版本 | 备份方式 | 验证结果 |
| --- | --- | --- | --- | --- | --- |
| Windows x86-64，Go 1.27.3 | 同机隔离 MySQL，Windows x86-64 | 本地进程，127.0.0.1:33306 | Server/mysql/mysqldump 均 8.4.13 | mysqldump 单库逻辑全量 | 真实备份、SHA-256、隔离恢复通过；详见验证报告 |
| Windows x86-64，Go 1.27.3 | 同机隔离 PostgreSQL，Windows x86-64 | 本地进程，127.0.0.1:35432 | Server/psql/pg_basebackup 均 18.6 | 整实例 tar 物理全量，fetch WAL | 真实备份、独立集群恢复和 system identifier 核对通过 |
| Linux amd64 | 未指定 | 本地 | 未指定 | 仅完成交叉编译 | 未运行，不构成平台支持结论 |
| Docker/Linux | MySQL 8.4.13 配置已提供 | 容器 | Dockerfile 已提供 | 逻辑全量 | 当前环境无 Docker，未构建、未运行 |
| Windows 原生服务 | 未指定 | Windows 服务 | 未指定 | 未指定 | 阶段五，未实现 |
| Linux systemd | 未指定 | systemd | 未指定 | 未指定 | 阶段五，未实现 |

## 能力状态

| 引擎与路线 | 全量 | 差异/增量 | 日志 | 恢复验证 |
| --- | --- | --- | --- | --- |
| MySQL 8.4.13 + mysqldump 8.4.13 | 已实现并在上述 Windows 隔离组合验证 | 产品内未实现，不用重复全量冒充 | binlog 全量坐标 + 时间点重放脚本已验证；产品调度未接入 | 独立 vault_restore 已验证 |
| SQL Server 原生 | 适配器代码已实现，unverified | 未实现 | 未实现 | 未运行 |
| PostgreSQL 18.6 + pg_basebackup 18.6（Windows） | 已实现并在上述隔离组合验证 | 未实现 | WAL 归档+目标 LSN 脚本已验证；产品调度未接入 | 独立集群已验证 |
| PostgreSQL pgBackRest | 未实现 | 未实现 | 未实现 | 未运行 |
| PostgreSQL pg_dump | 未实现 | 未实现 | 未实现 | 未运行 |

## 依赖与许可维护

新增或升级依赖前必须重新查阅官方文档，记录版本、兼容范围、维护状态、许可证和理由。包含 Go 工具链、SQLite 驱动或 MySQL 工具的发行包必须保留相应许可证和第三方 notices；MySQL 工具随自定义镜像再分发前需要单独许可审查。
