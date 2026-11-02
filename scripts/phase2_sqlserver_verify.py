#!/usr/bin/env python3
"""SQL Server 隔离全量备份与异名异路径恢复验证脚本。

当前未在开发环境执行。运行前必须显式确认隔离环境，并保证 SQL Server 服务账号
对 SANDBOX_ROOT 下的备份和恢复目录具有读写权限。脚本不使用 REPLACE，不删除旧证据。
"""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

SQLCMD = os.environ.get("VAULT_TEST_SQLCMD", "sqlcmd")
SERVER = os.environ.get("VAULT_TEST_SQLSERVER_SERVER", "")
USER = os.environ.get("VAULT_TEST_SQLSERVER_USER", "")
SOURCE_DB = "vault_test_source"
RESTORE_PREFIX = "vault_test_restore_"
IDENTIFIER = re.compile(r"^[A-Za-z0-9_]+$")


def fail(message: str) -> None:
    raise SystemExit(message)


def settings() -> tuple[Path, Path, Path, Path]:
    if os.environ.get("VAULT_TEST_SQLSERVER_CONFIRM_ISOLATED") != "YES":
        fail("拒绝运行：必须设置 VAULT_TEST_SQLSERVER_CONFIRM_ISOLATED=YES，确认实例和目录均为隔离测试用途")
    if not SERVER:
        fail("请设置 VAULT_TEST_SQLSERVER_SERVER，例如 localhost\\MSSQLSERVER")
    if not IDENTIFIER.fullmatch(SOURCE_DB) or not IDENTIFIER.fullmatch(RESTORE_PREFIX):
        fail("测试数据库标识符不安全")
    root_text = os.environ.get("VAULT_TEST_SQLSERVER_SANDBOX_ROOT", "")
    if not root_text:
        fail("请设置 VAULT_TEST_SQLSERVER_SANDBOX_ROOT；SQL Server 服务账号必须可读写该目录")
    root = Path(root_text).resolve()
    if not root.is_absolute() or len(root.parts) < 2:
        fail("SANDBOX_ROOT 必须是独立的绝对目录，不能是磁盘根目录")
    backup = (root / "backups").resolve()
    restore = (root / "restore").resolve()
    work = (root / "work").resolve()
    for path in (backup, restore, work):
        if root not in path.parents or "'" in str(path) or "\n" in str(path) or "\r" in str(path):
            fail(f"目录越过隔离边界或包含不安全字符: {path}")
        path.mkdir(parents=True, exist_ok=True)
    return root, backup, restore, work


def sqlcmd(work: Path, sql: str, database: str = "master", capture: bool = False) -> str:
    script = work / ("statement-" + str(time.time_ns()) + ".sql")
    script.write_text(sql, encoding="utf-8")
    args = [SQLCMD, "-S", SERVER, "-d", database, "-b", "-r", "1", "-W", "-s", "|", "-h", "-1", "-f", "65001", "-i", str(script)]
    env = os.environ.copy()
    if USER:
        args += ["-U", USER]
        if not env.get("SQLCMDPASSWORD"):
            fail("使用 SQL 登录时必须通过 SQLCMDPASSWORD 环境变量提供密码；禁止在命令行使用 -P")
    else:
        args += ["-E"]
    try:
        result = subprocess.run(args, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
    finally:
        script.unlink(missing_ok=True)
    stdout = result.stdout.decode("utf-8", errors="replace").strip()
    stderr = result.stderr.decode("utf-8", errors="replace").strip()
    if result.returncode != 0:
        fail(f"sqlcmd 执行失败（退出码 {result.returncode}）: {stderr or stdout}")
    return stdout if capture else ""


def rows(output: str) -> list[list[str]]:
    return [[column.strip() for column in line.split("|")] for line in output.splitlines() if line.strip()]


def main() -> None:
    if sys.platform == "win32":
        sys.stdout.reconfigure(encoding="utf-8")
    root, backup_dir, restore_dir, work = settings()
    run_id = time.strftime("%Y%m%d%H%M%S") + "-" + str(os.getpid())
    restore_db = RESTORE_PREFIX + re.sub(r"[^A-Za-z0-9_]", "", run_id)
    if not IDENTIFIER.fullmatch(restore_db):
        fail("恢复数据库标识符不安全")
    backup_file = backup_dir / ("vault-full-" + run_id + ".bak")
    if backup_file.exists():
        fail(f"拒绝覆盖已有备份: {backup_file}")

    seed = f"""
IF DB_ID(N'{SOURCE_DB}') IS NULL CREATE DATABASE [{SOURCE_DB}];
GO
USE [{SOURCE_DB}];
GO
IF OBJECT_ID(N'dbo.sample_records', N'U') IS NULL
BEGIN
    CREATE TABLE dbo.sample_records(
        id BIGINT PRIMARY KEY,
        name NVARCHAR(100) NOT NULL,
        amount DECIMAL(12,2) NOT NULL,
        created_at DATETIME2 NOT NULL DEFAULT SYSDATETIME()
    );
END;
GO
DELETE FROM [{SOURCE_DB}].dbo.sample_records;
INSERT INTO [{SOURCE_DB}].dbo.sample_records(id,name,amount) VALUES
    (1,N'隔离验证-A',100.50),(2,N'隔离验证-B',200.75),(3,N'隔离验证-C',300.00);
GO
"""
    sqlcmd(work, seed)
    sqlcmd(work, f"BACKUP DATABASE [{SOURCE_DB}] TO DISK = N'{backup_file}' WITH CHECKSUM;")
    if not backup_file.is_file():
        fail("BACKUP 返回成功但未在隔离目录找到备份文件；检查 SQL Server 服务账号路径权限")
    digest = hashlib.sha256(backup_file.read_bytes()).hexdigest()
    size = backup_file.stat().st_size
    sqlcmd(work, f"RESTORE VERIFYONLY FROM DISK = N'{backup_file}' WITH CHECKSUM;")

    listing = sqlcmd(work, f"RESTORE FILELISTONLY FROM DISK = N'{backup_file}';", capture=True)
    files = rows(listing)
    if not files:
        fail("RESTORE FILELISTONLY 未返回逻辑文件")
    moves = []
    for index, file in enumerate(files):
        if len(file) < 3:
            fail(f"无法解析备份逻辑文件行: {file}")
        logical, file_type = file[0], file[2].upper()
        suffix = ".ldf" if file_type == "L" else f"-{index}.mdf"
        target = restore_dir / (restore_db + suffix)
        if root not in target.resolve().parents:
            fail("恢复文件路径越过隔离目录")
        moves.append(f"MOVE N'{logical}' TO N'{target}'")
    restore_sql = f"RESTORE DATABASE [{restore_db}] FROM DISK = N'{backup_file}' WITH " + ",\n".join(moves) + ", RECOVERY;"
    sqlcmd(work, restore_sql)

    count_sum = sqlcmd(work, f"SELECT COUNT(*),SUM(amount) FROM [{restore_db}].dbo.sample_records;", capture=True)
    columns = sqlcmd(work, f"SELECT COUNT(*) FROM [{restore_db}].INFORMATION_SCHEMA.COLUMNS WHERE TABLE_NAME = 'sample_records';", capture=True)
    names = sqlcmd(work, f"SELECT STRING_AGG(name, ',') WITHIN GROUP (ORDER BY id) FROM [{restore_db}].dbo.sample_records;", capture=True)
    normalize = lambda value: value.replace(" | ", "|").replace(" ", "")
    if normalize(count_sum) != "3|601.25" or normalize(columns) != "4" or names != "隔离验证-A,隔离验证-B,隔离验证-C":
        fail(f"隔离恢复数据不一致: rows={count_sum!r}, columns={columns!r}, names={names!r}")
    version = sqlcmd(work, "SELECT @@VERSION;", capture=True)
    print(json.dumps({
        "status": "passed", "server_version": version, "backup_method": "sqlserver_native_full",
        "backup_file": str(backup_file), "bytes": size, "sha256": digest,
        "restore_database": restore_db, "restore": {"rows": 3, "amount_sum": "601.25", "columns": 4},
        "replace_used": False, "source_database_unchanged_except_sample": True,
    }, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
