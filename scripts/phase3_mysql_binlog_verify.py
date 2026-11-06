#!/usr/bin/env python3
"""MySQL 8.4.13 隔离实例的真实全量与 binlog 时间点恢复验证。

只连接 127.0.0.1:33306，只重建 dbbak_restore，并在 dbbak_source.sample_records
新增一条带本次运行标识的记录。每次运行使用独立证据目录，不清理 binlog、
不删除历史证据。运行前必须由主流程将实例改为 log-bin 模式。
"""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import time
from datetime import datetime, timezone

ROOT = Path(__file__).resolve().parents[1]
RUNTIME = ROOT / ".test-runtime"
DATA = RUNTIME / "mysql-data"
EVIDENCE_ROOT = RUNTIME / "mysql-binlog-verify"
HOME = Path(os.environ.get("DBBAK_TEST_MYSQL_HOME", ROOT / ".tools/mysql-8.4.13-winx64"))
MYSQL = HOME / "bin/mysql.exe"
MYSQLDUMP = HOME / "bin/mysqldump.exe"
MYSQLBINLOG = HOME / "bin/mysqlbinlog.exe"
BASE = ["--protocol=TCP", "--host=127.0.0.1", "--port=33306"]
USER = "dbbak_test"
SOURCE_DB = "dbbak_source"
RESTORE_DB = "dbbak_restore"
TABLE = "sample_records"
VERSION = "8.4.13"
BINLOG_NAME = re.compile(r"^(?P<prefix>.+\.)(?P<number>\d+)$")
DUMP_COORDINATE = re.compile(
    r"CHANGE\s+(?:REPLICATION\s+SOURCE|MASTER)\s+TO.*?"
    r"(?:SOURCE|MASTER)_LOG_FILE='([^']+)'.*?"
    r"(?:SOURCE|MASTER)_LOG_POS=(\d+)",
    re.IGNORECASE,
)


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def environment() -> dict[str, str]:
    password_file = RUNTIME / "mysql-password"
    password = password_file.read_text(encoding="ascii").strip()
    if not password:
        raise AssertionError(f"密码文件为空: {password_file}")
    env = os.environ.copy()
    env["MYSQL_PWD"] = password
    return env


def execute(args: list[object], *, stdin=None, stdout=subprocess.PIPE) -> subprocess.CompletedProcess:
    command = [str(arg) for arg in args]
    result = subprocess.run(
        command,
        env=environment(),
        stdin=stdin,
        stdout=stdout,
        stderr=subprocess.PIPE,
        check=False,
    )
    if result.returncode != 0:
        stderr = (result.stderr or b"").decode("utf-8", errors="replace").strip()
        captured_stdout = result.stdout if isinstance(result.stdout, bytes) else b""
        detail = stderr or captured_stdout.decode("utf-8", errors="replace").strip()
        raise RuntimeError(
            f"{Path(command[0]).name} 执行失败，退出码 {result.returncode}: {detail[:4000]}"
        )
    return result


def mysql_args(database: str | None = None) -> list[object]:
    args: list[object] = [
        MYSQL,
        "--no-defaults",
        *BASE,
        f"--user={USER}",
        "--default-character-set=utf8mb4",
        "--connect-timeout=10",
        "--batch",
        "--raw",
        "--skip-column-names",
        "--show-warnings",
    ]
    if database is not None:
        args.append(f"--database={database}")
    return args


def mysql_sql(sql: str, database: str | None = None) -> str:
    result = execute([*mysql_args(database), "--execute", sql])
    return result.stdout.decode("utf-8", errors="strict").strip()


def mysql_import(path: Path) -> None:
    # 防止全量导入和重放自身产生新 binlog，污染后续恢复区间。
    args = [
        *mysql_args(RESTORE_DB),
        "--init-command=SET SESSION sql_log_bin=0",
    ]
    with path.open("rb") as source:
        execute(args, stdin=source)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_manifest(run_dir: Path, manifest: dict) -> None:
    (run_dir / "result.json").write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )


def require_inside(path: Path, root: Path, label: str) -> None:
    try:
        path.relative_to(root)
    except ValueError as exc:
        raise AssertionError(f"{label}越过隔离目录: {path}（边界 {root}）") from exc


def require_regular_file(path: Path, label: str) -> Path:
    resolved = path.resolve(strict=True)
    info = path.lstat()
    reparse = getattr(info, "st_file_attributes", 0) & getattr(
        stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0x400
    )
    if path.is_symlink() or reparse or not stat.S_ISREG(info.st_mode):
        raise AssertionError(f"{label}不是普通文件或包含重解析点: {path}")
    return resolved


def verify_tools() -> dict[str, str]:
    versions = {}
    for name, tool in (("mysql", MYSQL), ("mysqldump", MYSQLDUMP), ("mysqlbinlog", MYSQLBINLOG)):
        require_regular_file(tool, f"{name} 客户端")
        output = execute([tool, "--version"]).stdout.decode("utf-8", errors="replace").strip()
        if not re.search(rf"(?<![0-9.]){re.escape(VERSION)}(?![0-9.])", output):
            raise AssertionError(f"{name} 必须为 {VERSION}，实际为: {output}")
        versions[name] = output
    server_version = mysql_sql("SELECT VERSION()")
    if server_version != VERSION:
        raise AssertionError(f"服务器必须为 {VERSION}，实际为: {server_version}")
    versions["server"] = server_version
    return versions


def preflight() -> tuple[Path, dict]:
    if sys.platform != "win32":
        raise AssertionError("该脚本仅用于 Windows 便携 MySQL 8.4.13 隔离环境")
    if SOURCE_DB == RESTORE_DB or not all(
        re.fullmatch(r"[a-z][a-z0-9_]+", value) for value in (SOURCE_DB, RESTORE_DB, TABLE)
    ):
        raise AssertionError("数据库或表标识符不符合固定安全约束")
    versions = verify_tools()
    values = mysql_sql(
        "SELECT @@GLOBAL.log_bin,@@GLOBAL.binlog_format,"
        "@@GLOBAL.binlog_row_image,@@GLOBAL.server_id,@@GLOBAL.datadir,"
        "@@GLOBAL.log_bin_basename"
    ).split("\t")
    if len(values) != 6:
        raise AssertionError(f"无法读取完整 binlog 配置: {values!r}")
    log_bin, row_format, row_image, server_id, data_dir_text, basename_text = values
    if log_bin not in ("1", "ON"):
        raise AssertionError("服务器仍禁用 log_bin；请先由主流程重启为 log-bin 模式")
    if row_format.upper() != "ROW" or row_image.upper() != "FULL":
        raise AssertionError("要求 binlog_format=ROW 且 binlog_row_image=FULL")
    if int(server_id) <= 0:
        raise AssertionError("启用 binlog 时 server_id 必须大于 0")
    data_dir = Path(data_dir_text).resolve(strict=True)
    if data_dir != DATA.resolve():
        raise AssertionError(f"数据目录不是固定隔离目录 {DATA}: {data_dir}")
    basename = Path(basename_text)
    if not basename.is_absolute():
        basename = data_dir / basename
    basename = basename.resolve()
    binlog_dir = basename.parent
    allowed_roots = (DATA.resolve(), (RUNTIME / "mysql-binlog").resolve())
    if not any(binlog_dir == root or root in binlog_dir.parents for root in allowed_roots):
        raise AssertionError(f"binlog 目录不在批准的隔离目录内: {binlog_dir}")
    table_count = mysql_sql(
        "SELECT COUNT(*) FROM information_schema.tables "
        "WHERE table_schema='dbbak_source' AND table_name='sample_records' "
        "AND table_type='BASE TABLE'"
    )
    if table_count != "1":
        raise AssertionError("缺少源样本基表 dbbak_source.sample_records")
    # 恢复会话必须能关闭 sql_log_bin，否则全量导入会污染待重放区间。
    mysql_sql("SET SESSION sql_log_bin=0;SET SESSION sql_log_bin=1")
    return binlog_dir, {
        "host": "127.0.0.1",
        "port": 33306,
        "user": USER,
        "versions": versions,
        "data_dir": str(data_dir),
        "binlog_dir": str(binlog_dir),
        "binlog_format": row_format,
        "binlog_row_image": row_image,
        "server_id": int(server_id),
    }


def parse_dump_coordinate(dump_file: Path) -> tuple[str, int]:
    with dump_file.open("r", encoding="utf-8", errors="strict") as stream:
        for line in stream:
            match = DUMP_COORDINATE.search(line)
            if match:
                if not line.lstrip().startswith("--"):
                    raise AssertionError("binlog 坐标不是 --source-data=2 生成的注释")
                return match.group(1), int(match.group(2))
    raise AssertionError("全量文件中未找到真实 binlog 文件和位置")


def split_binlog_name(name: str) -> tuple[str, int, int]:
    if not name or Path(name).name != name:
        raise AssertionError(f"binlog 文件名不安全: {name!r}")
    match = BINLOG_NAME.fullmatch(name)
    if not match:
        raise AssertionError(f"无法解析 binlog 文件序号: {name}")
    number = match.group("number")
    return match.group("prefix"), int(number), len(number)


def copy_bounded_binlogs(
    start: tuple[str, int],
    end: tuple[str, int],
    binlog_dir: Path,
    run_dir: Path,
) -> tuple[list[Path], list[dict]]:
    start_prefix, start_number, start_width = split_binlog_name(start[0])
    end_prefix, end_number, end_width = split_binlog_name(end[0])
    if (start_prefix, start_width) != (end_prefix, end_width) or end_number < start_number:
        raise AssertionError(f"binlog 起止文件不属于同一连续序列: {start} -> {end}")
    if start[1] < 4 or end[1] < 4:
        raise AssertionError(f"binlog 坐标无效: {start} -> {end}")
    if start_number == end_number and end[1] < start[1]:
        raise AssertionError("同一 binlog 文件的结束位置早于开始位置")
    count = end_number - start_number + 1
    if count > 10000:
        raise AssertionError(f"binlog 文件集合异常大，拒绝无界处理: {count}")
    evidence_dir = run_dir / "binlogs"
    evidence_dir.mkdir()
    copies = []
    metadata = []
    for number in range(start_number, end_number + 1):
        name = f"{start_prefix}{number:0{start_width}d}"
        original = require_regular_file(binlog_dir / name, "源 binlog")
        require_inside(original, binlog_dir, "源 binlog")
        if original.parent != binlog_dir:
            raise AssertionError(f"binlog 不是隔离目录的直接子文件: {original}")
        size = original.stat().st_size
        if number == start_number and start[1] > size:
            raise AssertionError(
                f"binlog 开始坐标超过文件大小: {name} position={start[1]} size={size}"
            )
        if number == end_number and end[1] > size:
            raise AssertionError(
                f"binlog 结束坐标超过文件大小: {name} position={end[1]} size={size}"
            )
        original_hash = sha256_file(original)
        destination = evidence_dir / name
        shutil.copyfile(original, destination)
        copied_hash = sha256_file(destination)
        if copied_hash != original_hash:
            raise AssertionError(f"binlog 复制前后哈希不一致: {name}")
        copies.append(destination)
        metadata.append({
            "file": name,
            "source_path": str(original),
            "evidence_path": str(destination),
            "bytes": size,
            "sha256": copied_hash,
        })
    return copies, metadata


def table_snapshot(database: str) -> dict[str, str]:
    return {
        "structure": mysql_sql(f"SHOW CREATE TABLE {database}.{TABLE}"),
        "data": mysql_sql(
            f"SELECT id,name,amount,created_at FROM {database}.{TABLE} ORDER BY id"
        ),
    }


def assert_same_snapshot(source: dict[str, str], restored: dict[str, str], stage: str) -> None:
    if source["structure"] != restored["structure"]:
        raise AssertionError(f"{stage}表结构不一致")
    if source["data"] != restored["data"]:
        raise AssertionError(f"{stage}表数据不一致")


def main() -> None:
    if sys.platform == "win32":
        sys.stdout.reconfigure(encoding="utf-8")
    run_id = (
        datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        + f"-{os.getpid()}-{time.time_ns()}"
    )
    run_dir = EVIDENCE_ROOT / f"run_{run_id}"
    EVIDENCE_ROOT.mkdir(parents=True, exist_ok=True)
    run_dir.mkdir(exist_ok=False)
    manifest = {
        "status": "running",
        "run_id": run_id,
        "started_at": utc_now(),
        "source_database": SOURCE_DB,
        "restore_database": RESTORE_DB,
        "evidence_dir": str(run_dir),
    }
    write_manifest(run_dir, manifest)
    try:
        binlog_dir, configuration = preflight()
        manifest["configuration"] = configuration
        write_manifest(run_dir, manifest)

        # 只重建固定隔离恢复库；这些管理语句位于全量坐标之前。
        mysql_sql(
            "DROP DATABASE IF EXISTS dbbak_restore;"
            "CREATE DATABASE dbbak_restore CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"
        )
        dump_file = run_dir / "dbbak_source_full.sql"
        dump_args = [
            MYSQLDUMP,
            "--no-defaults",
            *BASE,
            f"--user={USER}",
            "--default-character-set=utf8mb4",
            "--single-transaction",
            "--quick",
            "--routines",
            "--events",
            "--triggers",
            "--hex-blob",
            "--no-tablespaces",
            "--set-gtid-purged=OFF",
            "--source-data=2",
            SOURCE_DB,
        ]
        with dump_file.open("xb") as output:
            execute(dump_args, stdout=output)
        start = parse_dump_coordinate(dump_file)
        dump_hash = sha256_file(dump_file)
        manifest["full_backup"] = {
            "path": str(dump_file),
            "bytes": dump_file.stat().st_size,
            "sha256": dump_hash,
            "binlog_file": start[0],
            "binlog_position": start[1],
            "recorded_before_restore": True,
        }
        write_manifest(run_dir, manifest)
        mysql_sql("FLUSH LOGS")

        mysql_import(dump_file)
        source_baseline = table_snapshot(SOURCE_DB)
        restored_baseline = table_snapshot(RESTORE_DB)
        assert_same_snapshot(source_baseline, restored_baseline, "全量恢复后")
        manifest["full_restore"] = {
            "structure_verified": True,
            "data_verified": True,
            "rows": mysql_sql(f"SELECT COUNT(*) FROM {RESTORE_DB}.{TABLE}"),
        }
        write_manifest(run_dir, manifest)

        marker_id = time.time_ns()
        marker = f"phase3-pitr-{run_id}"
        if len(marker) > 100:
            raise AssertionError("样本标识超过 name 列长度")
        for database in (SOURCE_DB, RESTORE_DB):
            if mysql_sql(f"SELECT COUNT(*) FROM {database}.{TABLE} WHERE id={marker_id}") != "0":
                raise AssertionError(f"样本 id 在 {database} 中已存在: {marker_id}")
        output = mysql_sql(
            "USE dbbak_source;"
            f"INSERT INTO sample_records(id,name,amount) VALUES({marker_id},'{marker}',904.13);"
            "SHOW BINARY LOG STATUS;"
            "FLUSH LOGS"
        )
        lines = [line for line in output.splitlines() if line.strip()]
        if not lines:
            raise AssertionError("新增样本后未返回 SHOW BINARY LOG STATUS 坐标")
        status_columns = lines[-1].split("\t")
        if len(status_columns) < 2:
            raise AssertionError(f"无法解析目标 binlog 坐标: {output!r}")
        end = (status_columns[0], int(status_columns[1]))
        manifest["point_in_time_target"] = {
            "binlog_file": end[0],
            "binlog_position": end[1],
            "marker_id": marker_id,
            "marker_name": marker,
            "marker_amount": "904.13",
        }
        write_manifest(run_dir, manifest)

        binlogs, binlog_metadata = copy_bounded_binlogs(start, end, binlog_dir, run_dir)
        manifest["binlogs"] = binlog_metadata
        write_manifest(run_dir, manifest)
        replay_file = run_dir / "replay_to_point.sql"
        replay_args = [
            MYSQLBINLOG,
            "--no-defaults",
            f"--start-position={start[1]}",
            f"--stop-position={end[1]}",
            f"--rewrite-db={SOURCE_DB}->{RESTORE_DB}",
            # mysqlbinlog 先重写再过滤，因此这里必须使用目标库名。
            f"--database={RESTORE_DB}",
            *binlogs,
        ]
        with replay_file.open("xb") as output_file:
            execute(replay_args, stdout=output_file)
        replay_hash = sha256_file(replay_file)
        mysql_import(replay_file)

        marker_row = mysql_sql(
            f"SELECT id,name,amount FROM {RESTORE_DB}.{TABLE} WHERE id={marker_id}"
        )
        expected_marker = f"{marker_id}\t{marker}\t904.13"
        if marker_row != expected_marker:
            raise AssertionError(
                f"重放后未找到唯一预期样本: expected={expected_marker!r} actual={marker_row!r}"
            )
        source_final = table_snapshot(SOURCE_DB)
        restored_final = table_snapshot(RESTORE_DB)
        assert_same_snapshot(source_final, restored_final, "时间点重放后")
        if sha256_file(dump_file) != dump_hash or sha256_file(replay_file) != replay_hash:
            raise AssertionError("全量或重放证据文件在验证过程中发生变化")
        for item, path in zip(binlog_metadata, binlogs, strict=True):
            if sha256_file(path) != item["sha256"]:
                raise AssertionError(f"binlog 证据副本哈希发生变化: {path.name}")

        manifest["replay"] = {
            "path": str(replay_file),
            "bytes": replay_file.stat().st_size,
            "sha256": replay_hash,
            "start_position_applies_to": start[0],
            "stop_position_applies_to": end[0],
            "source_rewritten_to_restore": True,
            "marker_verified": True,
            "structure_verified": True,
            "data_verified": True,
        }
        manifest["status"] = "passed"
        manifest["finished_at"] = utc_now()
        write_manifest(run_dir, manifest)
        print(json.dumps(manifest, ensure_ascii=False, indent=2))
    except BaseException as exc:
        manifest["status"] = "failed"
        manifest["finished_at"] = utc_now()
        manifest["error"] = {"type": type(exc).__name__, "message": str(exc)}
        write_manifest(run_dir, manifest)
        raise


if __name__ == "__main__":
    main()
