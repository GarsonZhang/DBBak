# SQL Server 隔离验证脚本说明

状态：**已通过真实验收（2026-11-02）**。

| 项 | 结果 |
| --- | --- |
| 实例 | SQL Server 2025 (RTM) 17.0.1000.7 Enterprise，默认实例，SQL 登录 |
| 客户端 | `sqlcmd`，`SQLCMDPASSWORD` 传递口令，未使用 `-P` |
| 备份 | 3,788,800 字节，SHA-256 校验通过，`WITH CHECKSUM` |
| 恢复 | `RESTORE FILELISTONLY` + `WITH MOVE` 到独立库，未使用 `REPLACE` |
| 数据核对 | 3 行、金额合计 601.25、4 字段、中文内容与源库一致 |
| 产品路径 | `run_b66efbfff38f5076c178fc7b`：command/file/restore 三态均为通过 |

本轮真实验证暴露并修复了两个缺陷：`msdb.dbo.backupset` 不存在 `backup_lsn` 列；`sqlcmd -Q` 的“(N rows affected)”污染最后一个 LSN 字段。

用户已选择不连接现有默认实例、不安装新实例。本脚本供后续在明确隔离的 SQL Server 环境执行。

## 验收通过后如何启用

1. 本脚本输出 `passed`，且备份文件、目标库、行数与中文内容核对一致。
2. 在 `DBBak_文档/DBBak_兼容性矩阵.md` 记录本次组合：管理端系统、SQL Server 版本、`sqlcmd` 版本、备份方式、验证日期。
3. 设置 `DBBAK_SQLSERVER_ENABLED=1` 并重启服务；此后能力状态变为 `supported`，允许创建任务。
4. Windows 服务通过 `sc qc DBBak` 里的 ImagePath 参数或服务环境变量传递 `DBBAK_*`，Linux 写入 `/etc/dbbak/dbbak.env`，Docker 写入 compose 的 `environment`。
5. 未完成上述验证时**不要**设置该变量：备份可能成功但恢复不可用，而这正是本项目禁止的“命令成功即视为可用”。

## 安全边界

脚本 `scripts/phase2_sqlserver_verify.py` 具有以下约束：

- 必须显式设置 `DBBAK_TEST_SQLSERVER_CONFIRM_ISOLATED=YES`，否则拒绝运行。
- 备份、恢复和临时 SQL 文件必须位于同一个独立 SANDBOX_ROOT 下，不能是磁盘根目录。
- 源库固定为 `dbbak_test_source`，只重建其中的 sample_records 样本表数据。
- 每次生成唯一 `.bak` 文件和 `dbbak_test_restore_<时间>` 数据库，不覆盖旧文件或旧数据库。
- 使用 `RESTORE FILELISTONLY` 获取逻辑文件名，通过 `WITH MOVE` 指定独立恢复文件路径。
- 不使用 `REPLACE`，不删除旧恢复证据。
- SQL 登录密码只通过 `SQLCMDPASSWORD` 环境变量传递，不在命令行使用 `-P`；默认使用 `-E` 受信任连接。
- 包含中文的 SQL 使用 UTF-8 文件和 sqlcmd `-f 65001`，不经过 shell 拼接。
- 只有 `RESTORE VERIFYONLY` 和实际异名恢复均成功，并核对表结构、行数、金额和中文内容后，才输出 passed。
- 压缩验证通过 `DBBAK_TEST_SQLSERVER_COMPRESSION` 指定（空、`off`、`on`、`zstd_low`、`zstd_medium`、`zstd_high`），未知取值直接失败而不降级。输出中带 `compression` 段，包含请求的选项、`compressed_backup_size`、`backup_size` 和有效压缩比：只有 `compressed_backup_size` 小于 `backup_size` 才能证明压缩真正生效，语句执行成功不能作为证据。

## 前置条件

1. 已隔离的 SQL Server 实例及可用 sqlcmd。
2. 当前账号具有创建数据库、备份和恢复权限；建议按测试环境最小权限分配。
3. **SQL Server 服务启动账号**对 SANDBOX_ROOT 下 backups、restore 和 work 具有读写权限。运行脚本的当前用户能看到目录，不代表 SQL Server 服务账号能访问。
4. 当前脚本和 SQL Server 位于同一主机，便于对 `.bak` 计算 SHA-256。远程服务器路径需要后续 Agent 或共享目录设计，不能直接套用。
5. 备份文件版本不能高于恢复目标 SQL Server 的可恢复版本。

官方依据：

- BACKUP DATABASE、TO DISK、CHECKSUM：https://learn.microsoft.com/en-us/sql/t-sql/statements/backup-transact-sql
- BACKUP COMPRESSION、ALGORITHM = ZSTD（自 SQL Server 2025 17.x 起）与 LEVEL：同上页面 `<general_WITH_options>` 的 COMPRESSION 小节
- 备份压缩的版本许可（Express/Web 不支持）：https://learn.microsoft.com/en-us/sql/sql-server/editions-and-components-of-sql-server-2022
- RESTORE、WITH MOVE 与 REPLACE 风险：https://learn.microsoft.com/en-us/sql/t-sql/statements/restore-statements-transact-sql
- sqlcmd、SQLCMDPASSWORD、-E/-U/-f：https://learn.microsoft.com/en-us/sql/tools/sqlcmd/sqlcmd-utility

## 示例（未运行）

Windows 受信任连接：

```powershell
$env:DBBAK_TEST_SQLCMD = "C:\Program Files\Microsoft SQL Server\Client SDK\ODBC\170\Tools\Binn\SQLCMD.EXE"
$env:DBBAK_TEST_SQLSERVER_SERVER = "localhost\MSSQLSERVER"
$env:DBBAK_TEST_SQLSERVER_SANDBOX_ROOT = "D:\dbbak-sqlserver-isolated"
$env:DBBAK_TEST_SQLSERVER_CONFIRM_ISOLATED = "YES"
python scripts/phase2_sqlserver_verify.py
```

带压缩的全量验证（SQL Server 2025 上验证 ZSTD，需先确认实例 Edition 带备份压缩许可）：

```powershell
$env:DBBAK_TEST_SQLSERVER_COMPRESSION = "zstd_medium"
python scripts/phase2_sqlserver_verify.py
```

SQL 登录方式：

```powershell
$env:DBBAK_TEST_SQLSERVER_USER = "dbbak_test"
$env:SQLCMDPASSWORD = Read-Host "隔离测试密码"
python scripts/phase2_sqlserver_verify.py
Remove-Item Env:\SQLCMDPASSWORD
```

脚本只创建/重建 `dbbak_test_source.dbo.sample_records` 的三行样本。不得把该脚本指向包含业务数据的同名数据库。

## 输出与后续接入

成功输出包含备份方法、文件路径、大小、SHA-256、恢复数据库名、3 行、金额 601.25 和 4 个字段。脚本保留恢复证据，不自动清理。

后续在隔离环境跑通后，才能实现并注册 SQL Server 适配器。届时还需在 DBBak 数据源模型中明确区分：

- 管理服务路径。
- 执行器路径。
- SQL Server 服务可访问的备份/恢复路径。

不得直接把当前执行器本地目录默认当成 SQL Server 原生 BACKUP 的目标路径。
