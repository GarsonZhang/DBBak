#!/usr/bin/env python3
"""在 Windows 上使用官方便携 MySQL 8.4.13 准备隔离测试实例。

init：只初始化 .test-runtime/mysql-data。
start：以前台进程启动 127.0.0.1:33306，终端关闭即停止。
seed：在另一个终端创建隔离库、样本数据和随机测试账号密码。
"""
from __future__ import annotations

import os
from pathlib import Path
import secrets
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
RUNTIME = ROOT / ".test-runtime"
HOME = Path(os.environ.get("VAULT_TEST_MYSQL_HOME", ROOT / ".tools/mysql-8.4.13-winx64"))
DATA = RUNTIME / "mysql-data"
BASE = ["--protocol=TCP", "--host=127.0.0.1", "--port=33306"]


def run(args: list[str], **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run([str(arg) for arg in args], check=True, **kwargs)


def initialize() -> None:
    if (DATA / "mysql").exists():
        print("隔离数据目录已初始化，未重复执行。")
        return
    RUNTIME.mkdir(exist_ok=True)
    run([HOME / "bin/mysqld.exe", "--initialize-insecure", f"--basedir={HOME}", f"--datadir={DATA}", "--console"])
    print(f"已初始化隔离目录 {DATA}；下一步执行 start。")


def start() -> None:
    run([HOME / "bin/mysqld.exe", "--no-defaults", f"--basedir={HOME}", f"--datadir={DATA}",
         "--port=33306", "--bind-address=127.0.0.1", "--mysqlx=OFF", "--skip-log-bin", "--console"])


def seed() -> None:
    password_file = RUNTIME / "mysql-password"
    password = secrets.token_urlsafe(24)
    password_file.write_text(password, encoding="ascii")
    password_file.chmod(0o600)
    sql = (
        "CREATE DATABASE IF NOT EXISTS vault_source CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;"
        "CREATE DATABASE IF NOT EXISTS vault_restore CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;"
        "CREATE USER IF NOT EXISTS 'vault_test'@'%' IDENTIFIED BY '" + password + "';"
        "ALTER USER 'vault_test'@'%' IDENTIFIED BY '" + password + "';"
        "GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, LOCK TABLES, RELOAD ON *.* TO 'vault_test'@'%';"
        "GRANT ALL PRIVILEGES ON vault_source.* TO 'vault_test'@'%';"
        "GRANT ALL PRIVILEGES ON vault_restore.* TO 'vault_test'@'%';"
        "CREATE TABLE IF NOT EXISTS vault_source.sample_records ("
        "id BIGINT PRIMARY KEY,name VARCHAR(100) NOT NULL,amount DECIMAL(12,2) NOT NULL,"
        "created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP) ENGINE=InnoDB;"
        "TRUNCATE TABLE vault_source.sample_records;"
        "INSERT INTO vault_source.sample_records(id,name,amount) VALUES "
        "(1,'隔离验证-A',100.50),(2,'隔离验证-B',200.75),(3,'隔离验证-C',300.00);FLUSH PRIVILEGES;"
    )
    run([HOME / "bin/mysql.exe", *BASE, "--user=root", "--default-character-set=utf8mb4", "--execute", sql])
    print("已创建隔离源库、恢复库和 3 行样本；随机密码仅写入 .test-runtime/mysql-password。")


def main() -> None:
    if sys.platform != "win32":
        raise SystemExit("该脚本仅用于 Windows 便携 MySQL；Linux 请使用 deploy/test/compose.yaml 或等效官方包。")
    if len(sys.argv) != 2 or sys.argv[1] not in {"init", "start", "seed"}:
        raise SystemExit("用法: python scripts/bootstrap_mysql_windows.py init|start|seed")
    {"init": initialize, "start": start, "seed": seed}[sys.argv[1]]()


if __name__ == "__main__":
    main()
