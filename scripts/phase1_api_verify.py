#!/usr/bin/env python3
"""DBBak 阶段一真实 API、MySQL 备份与隔离恢复验证。

仅使用隔离测试账号和 .test-runtime 下的凭据文件，不打印密码或会话令牌。
运行前需启动隔离 MySQL 与 DBBak；不支持用于生产数据库。
"""
from __future__ import annotations

import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
RUNTIME = ROOT / ".test-runtime"
BASE_URL = os.environ.get("DBBAK_TEST_URL", "http://127.0.0.1:38080")
MYSQL = Path(os.environ.get("DBBAK_TEST_MYSQL", ROOT / ".tools/mysql-8.4.13-winx64/bin/mysql.exe"))
DB_HOST = os.environ.get("DBBAK_TEST_DB_HOST", "127.0.0.1")
DB_PORT = int(os.environ.get("DBBAK_TEST_DB_PORT", "33306"))
DB_USER = os.environ.get("DBBAK_TEST_DB_USER", "dbbak_test")
SOURCE_DB = "dbbak_source"
RESTORE_DB = "dbbak_restore"


def require_file(path: Path) -> str:
    if not path.is_file():
        raise SystemExit(f"缺少隔离测试凭据文件 {path}；请先按部署说明初始化测试环境")
    return path.read_text(encoding="ascii").strip()


class API:
    def __init__(self) -> None:
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar())
        )

    def call(self, method: str, path: str, payload=None, expected=(200,), headers=None):
        body = None if payload is None else json.dumps(payload, ensure_ascii=False).encode("utf-8")
        request = urllib.request.Request(BASE_URL + path, data=body, method=method)
        request.add_header("Accept", "application/json")
        request.add_header("X-DBBak-Request", "1")
        if body is not None:
            request.add_header("Content-Type", "application/json; charset=utf-8")
        for key, value in (headers or {}).items():
            request.add_header(key, value)
        try:
            with self.opener.open(request, timeout=40) as response:
                status = response.status
                raw = response.read().decode("utf-8")
        except urllib.error.HTTPError as error:
            status = error.code
            raw = error.read().decode("utf-8", errors="replace")
        if status not in expected:
            raise AssertionError(f"{method} {path} 返回 {status}，预期 {expected}: {raw}")
        return status, json.loads(raw) if raw else None


def find_or_create(api: API, collection: str, name: str, payload: dict):
    _, items = api.call("GET", f"/api/v1/{collection}")
    for item in items:
        if item["name"] == name:
            return item
    _, created = api.call("POST", f"/api/v1/{collection}", payload, expected=(201,))
    return created


def mysql_args() -> list[str]:
    return [
        str(MYSQL), "--protocol=TCP", f"--host={DB_HOST}", f"--port={DB_PORT}",
        f"--user={DB_USER}", "--default-character-set=utf8mb4", "--batch", "--skip-column-names",
    ]


def run_mysql(sql: str | None = None, stdin_path: Path | None = None, database: str | None = None) -> str:
    env = os.environ.copy()
    env["MYSQL_PWD"] = require_file(RUNTIME / "mysql-password")
    args = mysql_args()
    if sql is not None:
        args += ["--execute", sql]
    if database:
        args.append(database)
    stdin = None
    if stdin_path is not None:
        stdin = stdin_path.open("rb")
    try:
        result = subprocess.run(args, stdin=stdin, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, check=False)
    finally:
        if stdin is not None:
            stdin.close()
    if result.returncode != 0:
        message = result.stderr.decode("utf-8", errors="replace")
        raise AssertionError(f"mysql 隔离恢复/核对失败（退出码 {result.returncode}）: {message}")
    return result.stdout.decode("utf-8").strip()


def main() -> None:
    if sys.platform == "win32":
        sys.stdout.reconfigure(encoding="utf-8")
    api = API()
    api.call("POST", "/api/v1/auth/login", {
        "username": "admin", "password": require_file(RUNTIME / "admin-password")
    })
    _, capabilities = api.call("GET", "/api/v1/capabilities")
    if capabilities["local_executor"]["status"] != "supported":
        raise AssertionError(f"mysqldump 不可用: {capabilities['local_executor']['message']}")

    source = find_or_create(api, "sources", "DBBak 隔离 MySQL 8.4", {
        "name": "DBBak 隔离 MySQL 8.4", "engine": "mysql", "host": DB_HOST, "port": DB_PORT,
        "database": SOURCE_DB, "username": DB_USER, "password": require_file(RUNTIME / "mysql-password"),
    })
    _, probe = api.call("POST", f"/api/v1/sources/{source['id']}/probe", {})
    if not probe["server_version"].startswith("8.4.13"):
        raise AssertionError(f"测试数据库版本不符合固定基线: {probe['server_version']}")

    wrong_source = find_or_create(api, "sources", "DBBak 错误密码负向验证", {
        "name": "DBBak 错误密码负向验证", "engine": "mysql", "host": DB_HOST, "port": DB_PORT,
        "database": SOURCE_DB, "username": DB_USER, "password": "intentionally-wrong-test-password",
    })
    _, error = api.call("POST", f"/api/v1/sources/{wrong_source['id']}/probe", {}, expected=(422,))
    if error["code"] != "AUTH_FAILED":
        raise AssertionError(f"错误密码未分类为 AUTH_FAILED: {error}")

    target = find_or_create(api, "storage-targets", "DBBak 隔离备份目录", {
        "name": "DBBak 隔离备份目录", "kind": "local", "root": str(RUNTIME / "backup-root"),
    })
    api.call("POST", f"/api/v1/storage-targets/{target['id']}/check", {})
    invalid_root = RUNTIME / "not-a-directory"
    invalid_root.write_text("用于验证普通文件不能作为备份目录", encoding="utf-8")
    _, error = api.call("POST", "/api/v1/storage-targets", {
        "name": "非法目录负向验证", "kind": "local", "root": str(invalid_root),
    }, expected=(422,))
    if error["code"] != "STORAGE_NOT_WRITABLE":
        raise AssertionError(f"非法目录未明确失败: {error}")

    job = find_or_create(api, "jobs", "DBBak 隔离 MySQL 每分钟全量", {
        "name": "DBBak 隔离 MySQL 每分钟全量", "source_id": source["id"], "target_id": target["id"],
        "timezone": "Asia/Shanghai", "schedule_type": "interval", "time_of_day": "02:00",
        "weekday": 0, "interval_minutes": 1, "enabled": True,
        "retention_days": 30, "min_full": 1, "auto_cleanup": False,
    })
    idempotency_key = "phase1-" + str(uuid.uuid4())
    _, run = api.call("POST", f"/api/v1/jobs/{job['id']}/runs", {}, expected=(202,), headers={"Idempotency-Key": idempotency_key})
    _, duplicate = api.call("POST", f"/api/v1/jobs/{job['id']}/runs", {}, expected=(200,), headers={"Idempotency-Key": idempotency_key})
    if duplicate["id"] != run["id"]:
        raise AssertionError("重复幂等触发未返回原运行")

    deadline = time.time() + 60
    while time.time() < deadline:
        _, current = api.call("GET", f"/api/v1/runs/{run['id']}")
        if current["state"] == "succeeded":
            break
        if current["state"] in ("failed", "interrupted"):
            raise AssertionError(f"真实备份失败: {current['error_code']} {current['error_message']}")
        time.sleep(0.5)
    else:
        raise AssertionError("真实备份在 60 秒内未完成")
    if current["command_status"] != "succeeded" or current["file_status"] != "verified" or current["restore_status"] != "not_run":
        raise AssertionError(f"备份验证等级不正确: {current}")

    _, artifacts = api.call("GET", f"/api/v1/artifacts?job_id={job['id']}")
    artifact = next(item for item in artifacts if item["run_id"] == run["id"])
    backup_path = Path(target["root"]) / Path(artifact["relative_path"])
    if not backup_path.is_file():
        raise AssertionError(f"真实备份文件不存在: {backup_path}")
    digest = hashlib.sha256(backup_path.read_bytes()).hexdigest()
    if digest != artifact["sha256"] or backup_path.stat().st_size != artifact["size"]:
        raise AssertionError("真实文件大小或 SHA-256 与 API 记录不一致")
    dump_text = backup_path.read_text(encoding="utf-8")
    if "CREATE TABLE `sample_records`" not in dump_text or "隔离验证-C" not in dump_text:
        raise AssertionError("转储文件缺少预期表结构或样本数据")

    run_mysql(f"DROP DATABASE IF EXISTS {RESTORE_DB}; CREATE DATABASE {RESTORE_DB} CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
    run_mysql(stdin_path=backup_path, database=RESTORE_DB)
    result = run_mysql(f"SELECT COUNT(*),SUM(amount) FROM {RESTORE_DB}.sample_records")
    columns = run_mysql(f"SELECT COUNT(*) FROM information_schema.columns WHERE table_schema='{RESTORE_DB}' AND table_name='sample_records'")
    names = run_mysql(f"SELECT GROUP_CONCAT(name ORDER BY id SEPARATOR ',') FROM {RESTORE_DB}.sample_records")
    if result != "3\t601.25" or columns != "4" or names != "隔离验证-A,隔离验证-B,隔离验证-C":
        raise AssertionError(f"隔离恢复数据不一致: rows={result!r}, columns={columns!r}, names={names!r}")
    _, current = api.call("POST", f"/api/v1/runs/{run['id']}/restore-verification", {
        "status": "verified", "target": RESTORE_DB,
        "details": "已核对 sample_records 4 个字段、3 行、amount 合计 601.25 及中文内容",
    })
    if current["restore_status"] != "verified":
        raise AssertionError("恢复验证结果未持久化")

    _, preview = api.call("POST", f"/api/v1/jobs/{job['id']}/cleanup-previews", {}, expected=(201,))
    candidates = sum(1 for item in preview["result"]["items"] if item["candidate"])
    if candidates != 0:
        raise AssertionError("保留窗口内或最低份数内备份被错误列为候选")
    print(json.dumps({
        "status": "passed", "server_version": probe["server_version"], "run_id": run["id"],
        "artifact": str(backup_path), "bytes": artifact["size"], "sha256": digest,
        "restore": {"database": RESTORE_DB, "rows": 3, "amount_sum": "601.25", "columns": 4},
        "negative_checks": ["AUTH_FAILED", "STORAGE_NOT_WRITABLE"],
        "cleanup_protected_candidates": candidates,
    }, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
