#!/usr/bin/env python3
"""验证 PostgreSQL 18.6 真实物理全量与独立集群恢复。

仅操作 .test-runtime 中的源/恢复目录和测试账号，不连接生产实例。
"""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import subprocess
import sys
import tarfile
import time

from phase1_api_verify import API, require_file, RUNTIME

ROOT = Path(__file__).resolve().parents[1]
HOME = Path(os.environ.get("VAULT_TEST_PG_HOME", ROOT / ".tools/postgresql-18.6/pgsql"))
RESTORE = RUNTIME / "pg-restore-data"
RESTORE_PORT = 35433


def pg_env() -> dict[str, str]:
    env = os.environ.copy()
    env["PGPASSWORD"] = require_file(RUNTIME / "pg-password")
    env["PGCLIENTENCODING"] = "UTF8"
    return env


def run_command(args: list[object], **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run([str(arg) for arg in args], check=True, **kwargs)


def psql(sql: str, database: str = "vault_app", port: int = RESTORE_PORT) -> str:
    env = pg_env()
    result = run_command([HOME / "bin/psql.exe", "--host=127.0.0.1", f"--port={port}", "--username=vault_pg", f"--dbname={database}", "--tuples-only", "--no-psqlrc", "--command=" + sql], env=env, stdout=subprocess.PIPE, text=True, encoding="utf-8")
    return result.stdout.strip().replace(" | ", "|")


def safe_extract(archive: Path, destination: Path) -> None:
    if destination.exists():
        if destination.parent != RUNTIME or not destination.name.startswith("pg-restore-data-"):
            raise AssertionError("恢复目录安全边界校验失败")
        shutil.rmtree(destination)
    destination.mkdir(parents=True)
    with tarfile.open(archive, "r") as tar:
        for member in tar.getmembers():
            path = PurePosixPath(member.name)
            if path.is_absolute() or ".." in path.parts or member.issym() or member.islnk() or member.isdev():
                raise AssertionError(f"备份 tar 包含不安全对象: {member.name}")
        tar.extractall(destination, filter="data")
    (destination / ".vault-isolated-restore").write_text("phase2 isolated restore", encoding="ascii")


def wait_ready(process: subprocess.Popen) -> None:
    deadline = time.time() + 45
    while time.time() < deadline:
        if process.poll() is not None:
            raise AssertionError(f"恢复集群提前退出，退出码 {process.returncode}")
        result = subprocess.run([HOME / "bin/pg_isready.exe", "--host=127.0.0.1", f"--port={RESTORE_PORT}", "--username=vault_pg", "--dbname=vault_app"], env=pg_env(), stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if result.returncode == 0:
            return
        time.sleep(0.5)
    raise AssertionError("恢复集群 45 秒内未就绪")


def main() -> None:
    if sys.platform == "win32":
        sys.stdout.reconfigure(encoding="utf-8")
    api = API()
    api.call("POST", "/api/v1/auth/login", {"username": "admin", "password": require_file(RUNTIME / "admin-password")})
    source = None
    _, sources = api.call("GET", "/api/v1/sources")
    for item in sources:
        if item["name"] == "Vault 隔离 PostgreSQL 18":
            source = item
            break
    if source is None:
        _, source = api.call("POST", "/api/v1/sources", {
            "name": "Vault 隔离 PostgreSQL 18", "engine": "postgresql", "host": "127.0.0.1", "port": 35432,
            "database": "postgres", "username": "vault_pg", "password": require_file(RUNTIME / "pg-password"),
        }, expected=(201,))
    _, probe = api.call("POST", f"/api/v1/sources/{source['id']}/probe", {})
    if "PostgreSQL 18.6" not in probe["server_version"] or probe["backup_type"] != "physical_cluster_full":
        raise AssertionError(f"PostgreSQL 能力探测不符合基线: {probe}")

    wrong = None
    for item in sources:
        if item["name"] == "Vault PostgreSQL 错误密码负向验证":
            wrong = item
            break
    if wrong is None:
        _, wrong = api.call("POST", "/api/v1/sources", {
            "name": "Vault PostgreSQL 错误密码负向验证", "engine": "postgresql", "host": "127.0.0.1", "port": 35432,
            "database": "postgres", "username": "vault_pg", "password": "intentionally-wrong-test-password",
        }, expected=(201,))
    _, error = api.call("POST", f"/api/v1/sources/{wrong['id']}/probe", {}, expected=(422,))
    if error["code"] != "AUTH_FAILED":
        raise AssertionError(f"PostgreSQL 错误密码未明确失败: {error}")

    target = None
    _, targets = api.call("GET", "/api/v1/storage-targets")
    for item in targets:
        if item["name"] == "Vault PostgreSQL 隔离目录":
            target = item
            break
    if target is None:
        _, target = api.call("POST", "/api/v1/storage-targets", {"name": "Vault PostgreSQL 隔离目录", "kind": "local", "root": str(RUNTIME / "pg-backup-root")}, expected=(201,))
    job = None
    _, jobs = api.call("GET", "/api/v1/jobs")
    for item in jobs:
        if item["name"] == "Vault 隔离 PostgreSQL 全实例":
            job = item
            break
    if job is None:
        _, job = api.call("POST", "/api/v1/jobs", {
            "name": "Vault 隔离 PostgreSQL 全实例", "source_id": source["id"], "target_id": target["id"],
            "timezone": "Asia/Shanghai", "schedule_type": "interval", "time_of_day": "02:00", "weekday": 0,
            "interval_minutes": 60, "enabled": False, "retention_days": 30, "min_full": 1, "auto_cleanup": False,
        }, expected=(201,))
    key = "phase2-postgres-" + str(time.time_ns())
    _, run = api.call("POST", f"/api/v1/jobs/{job['id']}/runs", {}, expected=(202,), headers={"Idempotency-Key": key})
    deadline = time.time() + 180
    while time.time() < deadline:
        _, current = api.call("GET", f"/api/v1/runs/{run['id']}")
        if current["state"] == "succeeded":
            break
        if current["state"] in ("failed", "interrupted"):
            raise AssertionError(f"PostgreSQL 备份失败: {current['error_code']} {current['error_message']}")
        time.sleep(0.5)
    else:
        raise AssertionError("PostgreSQL 备份 180 秒内未完成")
    _, artifacts = api.call("GET", f"/api/v1/artifacts?job_id={job['id']}")
    artifact = next(item for item in artifacts if item["run_id"] == run["id"])
    if artifact["method"] != "pg_basebackup" or not artifact["relative_path"].endswith(".tar"):
        raise AssertionError(f"备份方法或文件格式错误: {artifact}")
    metadata = artifact["engine_metadata"]
    if metadata["scope"] != "cluster" or metadata["logical"] is not False or not metadata["system_identifier"]:
        raise AssertionError(f"物理恢复元数据不完整: {metadata}")
    archive = Path(target["root"]) / Path(artifact["relative_path"])
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    if digest != artifact["sha256"] or archive.stat().st_size != artifact["size"]:
        raise AssertionError("base.tar 与 API 校验记录不一致")

    global RESTORE
    RESTORE = RUNTIME / ("pg-restore-data-" + run["id"])
    safe_extract(archive, RESTORE)
    log_path = RUNTIME / "pg-restore.log"
    with log_path.open("wb") as log:
        process = subprocess.Popen([HOME / "bin/postgres.exe", "-D", RESTORE, "-p", str(RESTORE_PORT), "-h", "127.0.0.1"], stdout=log, stderr=subprocess.STDOUT, env=pg_env())
    try:
        wait_ready(process)
        rows = psql("SELECT COUNT(*),SUM(amount) FROM sample_records")
        columns = psql("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='sample_records'")
        names = psql("SELECT string_agg(name, ',' ORDER BY id) FROM sample_records")
        if rows != "3|601.25" or columns != "4" or names != "隔离验证-A,隔离验证-B,隔离验证-C":
            raise AssertionError(f"PostgreSQL 恢复数据不一致: rows={rows!r}, columns={columns!r}, names={names!r}")
        control = run_command([HOME / "bin/pg_controldata.exe", "-D", RESTORE], stdout=subprocess.PIPE, text=True, encoding="utf-8").stdout
        if metadata["system_identifier"] not in control:
            raise AssertionError("恢复集群 system identifier 与备份元数据不一致")
    finally:
        subprocess.run([HOME / "bin/pg_ctl.exe", "-D", RESTORE, "--wait", "--mode=fast", "stop"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        process.wait(timeout=30)
    _, current = api.call("POST", f"/api/v1/runs/{run['id']}/restore-verification", {
        "status": "verified", "target": f"postgresql://127.0.0.1:{RESTORE_PORT}/vault_app",
        "details": "base.tar 独立集群恢复；4 字段、3 行、金额 601.25、中文内容、system identifier 均一致",
    })
    print(json.dumps({
        "status": "passed", "run_id": run["id"], "tool_version": artifact["tool_version"],
        "archive": str(archive), "bytes": artifact["size"], "sha256": digest,
        "system_identifier": metadata["system_identifier"], "timeline": metadata["timeline"],
        "restore": {"port": RESTORE_PORT, "rows": 3, "amount_sum": "601.25", "columns": 4},
        "restore_status": current["restore_status"], "negative_checks": ["AUTH_FAILED"],
    }, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
