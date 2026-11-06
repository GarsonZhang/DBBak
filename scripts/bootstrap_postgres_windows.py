#!/usr/bin/env python3
"""准备 PostgreSQL 18.6 Windows 隔离源实例。

init 初始化独立数据目录；start-source 启动 127.0.0.1:35432；
seed 创建带 REPLICATION 权限的测试角色和样本；stop-source 停止该隔离实例。
"""
from __future__ import annotations

import os
from pathlib import Path
import secrets
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
RUNTIME = ROOT / ".test-runtime"
HOME = Path(os.environ.get("DBBAK_TEST_PG_HOME", ROOT / ".tools/postgresql-18.6/pgsql"))
SOURCE = RUNTIME / "pg-source-data"
PORT = 35432


def run(args: list[object], env=None, input_bytes: bytes | None = None) -> None:
    subprocess.run([str(arg) for arg in args], check=True, env=env, input=input_bytes)


def password(path: Path, create: bool = False) -> str:
    if create and not path.exists():
        path.write_text(secrets.token_urlsafe(24), encoding="ascii")
        path.chmod(0o600)
    if not path.is_file():
        raise SystemExit(f"缺少隔离测试密码文件 {path}")
    return path.read_text(encoding="ascii").strip()


def initialize() -> None:
    if (SOURCE / "PG_VERSION").exists():
        print("PostgreSQL 隔离源目录已初始化，未重复执行。")
        return
    RUNTIME.mkdir(exist_ok=True)
    superuser = RUNTIME / "pg-superuser-password"
    password(superuser, create=True)
    password(RUNTIME / "pg-password", create=True)
    run([HOME / "bin/initdb.exe", f"--pgdata={SOURCE}", "--username=postgres", "--auth=scram-sha-256", f"--pwfile={superuser}", "--encoding=UTF8", "--locale=C"])
    with (SOURCE / "postgresql.conf").open("a", encoding="utf-8") as config:
        config.write(f"\nlisten_addresses = '127.0.0.1'\nport = {PORT}\nmax_wal_senders = 10\nwal_keep_size = '64MB'\n")
    print("已初始化 PostgreSQL 隔离源实例；下一步执行 start-source 与 seed。")


def start_source() -> None:
    # 使用前台进程，便于终端和任务管理器准确控制生命周期；后台 pg_ctl 启动不属于本脚本验收方式。
    run([HOME / "bin/postgres.exe", "-D", SOURCE])


def seed() -> None:
    env = os.environ.copy()
    env["PGPASSWORD"] = password(RUNTIME / "pg-superuser-password")
    env["PGCLIENTENCODING"] = "UTF8"
    role_password = password(RUNTIME / "pg-password")
    role_sql = "DROP ROLE IF EXISTS dbbak_pg; CREATE ROLE dbbak_pg LOGIN SUPERUSER REPLICATION PASSWORD '" + role_password + "';"
    run([HOME / "bin/psql.exe", "--host=127.0.0.1", f"--port={PORT}", "--username=postgres", "--dbname=postgres", "--set=ON_ERROR_STOP=1", "--command=" + role_sql], env=env)
    run([HOME / "bin/psql.exe", "--host=127.0.0.1", f"--port={PORT}", "--username=postgres", "--dbname=postgres", "--set=ON_ERROR_STOP=1", "--command=DROP DATABASE IF EXISTS dbbak_app"], env=env)
    run([HOME / "bin/psql.exe", "--host=127.0.0.1", f"--port={PORT}", "--username=postgres", "--dbname=postgres", "--set=ON_ERROR_STOP=1", "--command=CREATE DATABASE dbbak_app ENCODING 'UTF8'"], env=env)
    sample = (
        "DROP TABLE IF EXISTS sample_records;"
        "CREATE TABLE sample_records (id BIGINT PRIMARY KEY, name VARCHAR(100) NOT NULL, amount NUMERIC(12,2) NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now());"
        "INSERT INTO sample_records(id,name,amount) VALUES (1,'隔离验证-A',100.50),(2,'隔离验证-B',200.75),(3,'隔离验证-C',300.00);"
    )
    # Windows 命令行可能使用系统代码页，含中文 SQL 必须通过 UTF-8 标准输入发送。
    run([HOME / "bin/psql.exe", "--host=127.0.0.1", f"--port={PORT}", "--username=postgres", "--dbname=dbbak_app", "--set=ON_ERROR_STOP=1"], env=env, input_bytes=sample.encode("utf-8"))
    print("已创建 dbbak_pg 角色与 dbbak_app.sample_records 三行样本。")


def stop_source() -> None:
    run([HOME / "bin/pg_ctl.exe", f"--pgdata={SOURCE}", "--wait", "--mode=fast", "stop"])


def main() -> None:
    if sys.platform != "win32":
        raise SystemExit("该脚本仅用于 Windows 便携 PostgreSQL。")
    if len(sys.argv) != 2 or sys.argv[1] not in {"init", "start-source", "seed", "stop-source"}:
        raise SystemExit("用法: python scripts/bootstrap_postgres_windows.py init|start-source|seed|stop-source")
    {"init": initialize, "start-source": start_source, "seed": seed, "stop-source": stop_source}[sys.argv[1]]()


if __name__ == "__main__":
    main()
