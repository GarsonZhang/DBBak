# MySQL 物理备份（Percona XtraBackup + binlog）验证说明

状态：**未运行**。代码已实现，但从未在真实 MySQL 上执行过一次物理备份与恢复。

本文说明为什么未运行、需要满足哪些前提、按什么步骤验证，以及验证通过后才能启用。

## 一、为什么当前未运行

| 事实 | 影响 |
| --- | --- |
| Percona XtraBackup 只发布 Linux 二进制（deb / rpm / 二进制 tarball / 容器），官方 Downloads 页面没有 Windows 包 | 开发机是 Windows，`xtrabackup` 无法安装，`Version()` 必然返回 `TOOL_MISSING` |
| XtraBackup 直接读取 MySQL 数据目录 | 执行器必须与 MySQL 实例**同机**；远程 Agent（阶段四）尚未实现，跨主机无法使用 |
| 工具主版本必须与 MySQL 主版本严格对齐（8.0 用 8.0，8.4 用 8.4） | 版本不一致时探测直接拒绝，不做“版本差不多就试”的放行 |

因此能力矩阵中该路线固定为 `unverified`，`DBBAK_MYSQL_PHYSICAL_ENABLED` 默认关闭，任务创建会被服务端拒绝。这是有意的：物理备份“命令成功但恢复不可用”的风险比逻辑备份更高。

## 二、已实现范围与未实现范围

已实现：

- `backup_method=physical` 的整实例物理全量：`xtrabackup --backup --stream=xbstream` 输出**单个** `.xbstream` 文件，复用现有哈希校验、FTP/OSS 上传与清理。
- `--extra-lsndir` 留存 `xtrabackup_checkpoints` 与 `xtrabackup_binlog_info`，把 `to_lsn`、`binlog_file`、`binlog_pos`、GTID 写入 `engine_metadata`。
- `backup_type=log` 的 binlog 归档：按基线位点到当前位点复制 `mysqlbinlog --read-from-remote-server --raw`，打包为单个 `.tar`。
- 恢复链按 `artifacts.backup_method` 隔离：逻辑备份与物理备份不能互为基线。
- 探测项：连接、`datadir` 是否本机可见、主版本对齐、`log_bin` 与 `binlog_format`。

明确未实现：

- **增量/差异**（`backup_type=differential`）：XtraBackup 增量需要逐级保留 checkpoints 并逐级 `--prepare`，当前元数据模型无法承载这条链，接口返回 `BACKUP_TYPE_UNSUPPORTED`，不重复全量冒充。
- **常驻 binlog 归档**：一次运行只做一次点到点归档，不启动 `--stop-never`；活动文件只覆盖到取值时刻，元数据以 `active_partial=true` 标明。
- **备份内 `--prepare`**：产物是热备原始数据页，恢复前必须自行 `--prepare`，元数据固定 `prepared=false`。

## 三、前置条件

1. Linux 执行器，且与 MySQL 实例同机（`datadir` 对执行器进程可见）。
2. MySQL Community 版本与 XtraBackup 主版本一致，例如均为 8.4。
3. 备份账号具备：`BACKUP_ADMIN`、`RELOAD`、`LOCK TABLES`、`PROCESS`、`REPLICATION CLIENT`。
4. `log_bin=ON`（仅全量不强制，但 binlog 归档必须）；建议 `binlog_format=ROW`。
5. 备份目标磁盘剩余空间 ≥ 数据目录大小（未启用压缩），并预留一份恢复副本空间。

## 四、手工验证步骤

以下命令均为手工验证用途，用来证明产品代码里的命令序列成立。**产品本身不执行 `--prepare` 与恢复**。

### 1. 物理全量

```bash
xtrabackup --backup --stream=xbstream \
  --target-dir=/tmp/xbt --extra-lsndir=/tmp/lsn \
  --datadir=/var/lib/mysql --host=127.0.0.1 --port=3306 --user=bkp \
  > /tmp/full.xbstream
```

检查 `/tmp/lsn/xtrabackup_checkpoints` 的 `to_lsn`，以及 `/tmp/lsn/xtrabackup_binlog_info` 的 `file` + `pos`。

### 2. 解包与 prepare

```bash
mkdir -p /tmp/restore && xbstream -x -C /tmp/restore < /tmp/full.xbstream
xtrabackup --prepare --target-dir=/tmp/restore
```

`--prepare` 必须用**与备份时相同版本**的 XtraBackup。

### 3. 恢复到隔离实例

准备一个空 `datadir` 的独立实例（独立端口、独立 socket），停止后：

```bash
xtrabackup --copy-back --target-dir=/tmp/restore --datadir=/tmp/restore-datadir
chown -R mysql:mysql /tmp/restore-datadir
```

启动隔离实例，核对表结构与样本数据（行数、金额合计、中文内容）。

### 4. binlog 归档与重放

```bash
mysqlbinlog --read-from-remote-server --raw --host=127.0.0.1 --port=3306 --user=bkp \
  --result-file=/tmp/binlog/ mysql-bin.000003

mysqlbinlog --start-position=<from_pos> /tmp/binlog/mysql-bin.000003 \
  | mysql -h 127.0.0.1 -P <隔离端口> dbbak_restore
```

重放边界必须可核对：基线位点之前的数据已在全量里，位点之后到 `to_pos` 的事务应恰好出现一次。

## 五、验证通过后如何启用

1. 上述四步全部通过，且备份文件 SHA-256、`--prepare` 无告警、隔离实例数据核对一致。
2. 在 `DBBak_文档/DBBak_兼容性矩阵.md` 记录：管理端系统、数据库系统、执行器位置、MySQL 与 XtraBackup 版本、备份方式、验证日期、恢复结果。
3. 设置 `DBBAK_MYSQL_PHYSICAL_ENABLED=1` 并重启服务；能力状态从 `unverified` 变为 `supported`，任务才允许选择物理路线。
4. Windows 服务通过 `sc qc DBBak` 的 ImagePath 参数或服务环境变量传递，Linux 写入 `/etc/dbbak/dbbak.env`，Docker 写入 compose 的 `environment`。

未完成验证前**不要**设置该变量。

## 六、相关配置

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DBBAK_XTRABACKUP` | `xtrabackup` | XtraBackup 可执行文件路径 |
| `DBBAK_MYSQLBINLOG` | `mysqlbinlog` | binlog 读取工具路径 |
| `DBBAK_MYSQL_PHYSICAL_ENABLED` | 空（关闭） | 物理路线启用开关 |
| `DBBAK_BACKUP_TIMEOUT` | `6h` | 单次运行硬超时；大库必须按实际耗时上调 |

## 七、官方依据

- Percona XtraBackup 选项参考（`--stream`、`--extra-lsndir`、`--parallel` 在 stream 模式无效）：https://docs.percona.com/percona-xtrabackup/8.4/xtrabackup-option-reference.html
- 服务端与备份版本对照（主版本必须一致）：https://docs.percona.com/percona-xtrabackup/9.7/server-backup-version-comparison.html
- 支持平台与存储引擎：https://docs.percona.com/percona-xtrabackup/LATEST/index.html
- mysqlbinlog `--raw`、`--read-from-remote-server`：https://dev.mysql.com/doc/refman/8.4/en/mysqlbinlog.html
- MySQL 备份方式总览：https://dev.mysql.com/doc/refman/8.4/en/backup-methods.html
