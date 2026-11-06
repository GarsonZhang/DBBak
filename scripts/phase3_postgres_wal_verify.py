#!/usr/bin/env python3
"""对隔离 PostgreSQL 18.6 执行真实全量备份和 WAL 目标 LSN 恢复验证。

源实例固定为 127.0.0.1:35432/dbbak_pg，恢复实例只使用本次唯一目录和端口。
本脚本不会修改源集群配置，也不会删除本次以外的任何恢复目录。

前置配置由主代理完成。所需归档目录：
    D:/Projects/DBBak/.test-runtime/pg-wal-archive
源集群必须设置（路径按实际安装方式转义）：
    wal_level = replica
    archive_mode = on
    max_wal_senders >= 2
    archive_command = 'copy /Y "%p" "D:/Projects/DBBak/.test-runtime/pg-wal-archive/%f"'
归档目录必须预先存在，归档文件必须是未压缩的原始 WAL 段文件名。
脚本通过 pg_switch_wal() 和归档文件大小等待确认 WAL 已真实归档。
"""
from __future__ import annotations

from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path, PurePosixPath, PureWindowsPath
import re
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
RUNTIME = ROOT / ".test-runtime"
PG_HOME = Path(os.environ.get("DBBAK_TEST_PG_HOME", ROOT / ".tools/postgresql-18.6/pgsql"))
SOURCE_DATA = RUNTIME / "pg-source-data"
ARCHIVE = RUNTIME / "pg-wal-archive"
VERIFY_ROOT = RUNTIME / "phase3-wal-verify"
SOURCE_HOST = "127.0.0.1"
SOURCE_PORT = 35432
SOURCE_USER = "dbbak_pg"
DATABASE = "dbbak_app"
REQUIRED_VERSION = "18.6"
SNAPSHOT_SQL = (
    "SELECT json_agg(row_to_json(t) ORDER BY t.id) FROM "
    "(SELECT id,name,amount,created_at FROM public.sample_records ORDER BY id) t"
)


class VerifyError(RuntimeError):
    pass


def tool(name: str) -> Path:
    suffix = ".exe" if sys.platform == "win32" else ""
    path = PG_HOME / "bin" / f"{name}{suffix}"
    if not path.is_file():
        raise VerifyError(f"缺少 PostgreSQL 工具: {path}")
    return path


def pg_env() -> dict[str, str]:
    password_file = RUNTIME / "pg-password"
    if not password_file.is_file() or password_file.is_symlink():
        raise VerifyError(f"缺少有效隔离密码文件: {password_file}")
    password = password_file.read_text(encoding="ascii").strip()
    if not password:
        raise VerifyError("PostgreSQL 隔离密码文件为空")
    env = os.environ.copy()
    env["PGPASSWORD"] = password
    env["PGCLIENTENCODING"] = "UTF8"
    return env


def execute(
    args: list[object],
    *,
    env: dict[str, str] | None = None,
    log_path: Path | None = None,
) -> subprocess.CompletedProcess[str]:
    argv = [str(arg) for arg in args]
    try:
        if log_path is not None:
            with log_path.open("ab") as log:
                result = subprocess.run(
                    argv, env=env, stdout=log, stderr=subprocess.STDOUT, check=False, timeout=180
                )
        else:
            result = subprocess.run(
                argv,
                env=env,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                encoding="utf-8",
                errors="replace",
                check=False,
                timeout=60,
            )
    except subprocess.TimeoutExpired as exc:
        raise VerifyError(f"命令在明确上限内未结束: {argv[0]}") from exc
    except OSError as exc:
        raise VerifyError(f"无法执行 {argv[0]}: {exc}") from exc
    if result.returncode != 0:
        detail = f"；日志: {log_path}" if log_path else f"；输出: {(result.stderr or result.stdout or '').strip()}"
        raise VerifyError(f"命令失败（退出码 {result.returncode}）: {argv}{detail}")
    return result


def psql_args(sql: str, port: int = SOURCE_PORT) -> list[object]:
    return [
        tool("psql"),
        f"--host={SOURCE_HOST}",
        f"--port={port}",
        f"--username={SOURCE_USER}",
        f"--dbname={DATABASE}",
        "--tuples-only",
        "--no-align",
        "--no-psqlrc",
        "--no-password",
        "--set=ON_ERROR_STOP=1",
        "--command=" + sql,
    ]


def psql(sql: str, port: int = SOURCE_PORT) -> str:
    return execute(psql_args(sql, port), env=pg_env()).stdout.strip()


def sql_literal(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def source_metadata() -> dict[str, object]:
    sql = (
        "SELECT json_build_object("
        "'version',current_setting('server_version'),"
        "'archive_mode',current_setting('archive_mode'),"
        "'archive_command',current_setting('archive_command'),"
        "'wal_level',current_setting('wal_level'),"
        "'max_wal_senders',current_setting('max_wal_senders'),"
        "'wal_segment_bytes',pg_size_bytes(current_setting('wal_segment_size')),"
        "'system_identifier',(SELECT system_identifier FROM pg_control_system()),"
        "'timeline',(SELECT timeline_id FROM pg_control_checkpoint()))"
    )
    try:
        return json.loads(psql(sql))
    except (json.JSONDecodeError, TypeError) as exc:
        raise VerifyError("无法解析源集群真实元数据") from exc


def preflight() -> dict[str, object]:
    if sys.platform != "win32":
        raise VerifyError("该脚本当前仅用于现有 Windows 便携 PostgreSQL 18.6 环境")
    if not RUNTIME.is_dir():
        raise VerifyError(f"隔离运行目录不存在: {RUNTIME}")
    for name in ("pg_basebackup", "psql", "pg_ctl", "pg_controldata"):
        output = execute([tool(name), "--version"]).stdout.strip()
        if not re.search(rf"\b{re.escape(REQUIRED_VERSION)}\b", output):
            raise VerifyError(f"{name} 版本不是 {REQUIRED_VERSION}: {output}")
    if not ARCHIVE.is_dir() or ARCHIVE.is_symlink():
        raise VerifyError(f"WAL 归档目录不存在或不是普通目录: {ARCHIVE}")
    ensure_within_runtime(ARCHIVE)

    metadata = source_metadata()
    if metadata["version"] != REQUIRED_VERSION:
        raise VerifyError(f"源实例版本不是 {REQUIRED_VERSION}: {metadata['version']}")
    if metadata["archive_mode"] != "on":
        raise VerifyError("源实例 archive_mode 必须为 on；本脚本不会代为修改配置")
    if metadata["wal_level"] not in ("replica", "logical"):
        raise VerifyError(f"源实例 wal_level 不符合物理恢复要求: {metadata['wal_level']}")
    if int(metadata["max_wal_senders"]) < 2:
        raise VerifyError("源实例 max_wal_senders 必须至少为 2")
    command = str(metadata["archive_command"]).strip()
    if not command or command in (":", "true"):
        raise VerifyError("源实例 archive_command 未执行真实 WAL 归档")
    return metadata


def ensure_within_runtime(path: Path) -> None:
    runtime = RUNTIME.resolve()
    resolved = path.resolve()
    if resolved != runtime and runtime not in resolved.parents:
        raise VerifyError(f"路径越过 .test-runtime 安全边界: {path}")


def safe_extract(archive: Path, destination: Path) -> None:
    ensure_within_runtime(archive)
    ensure_within_runtime(destination)
    if not archive.is_file() or archive.is_symlink():
        raise VerifyError(f"tar 不是普通文件: {archive}")
    destination.mkdir(parents=True, exist_ok=True)
    with tarfile.open(archive, "r:*") as tar:
        members = tar.getmembers()
        for member in members:
            posix = PurePosixPath(member.name)
            windows = PureWindowsPath(member.name)
            unsafe = (
                not member.name
                or "\\" in member.name
                or posix.is_absolute()
                or windows.drive
                or windows.root
                or ".." in posix.parts
                or ".." in windows.parts
                or member.issym()
                or member.islnk()
                or not (member.isreg() or member.isdir())
            )
            target = destination / Path(*posix.parts)
            ensure_within_runtime(target)
            if unsafe:
                raise VerifyError(f"备份 tar 包含不安全对象: {member.name!r}")
        tar.extractall(destination, members=members, filter="data")


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind((SOURCE_HOST, 0))
        return int(sock.getsockname()[1])


def wait_for_archives(names: set[str], segment_bytes: int) -> None:
    deadline = time.monotonic() + 120
    pending = set(names)
    while pending and time.monotonic() < deadline:
        for name in sorted(pending.copy()):
            if not re.fullmatch(r"[0-9A-F]{24}", name):
                raise VerifyError(f"源实例返回非法 WAL 文件名: {name!r}")
            path = ARCHIVE / name
            ensure_within_runtime(path)
            if path.is_symlink():
                raise VerifyError(f"WAL 归档文件不允许是链接: {path}")
            if path.is_file() and path.stat().st_size == segment_bytes:
                pending.remove(name)
        if pending:
            time.sleep(0.25)
    if pending:
        raise VerifyError(
            f"120 秒内未等到完整 WAL 归档: {sorted(pending)}；"
            f"请检查源 archive_command 是否写入 {ARCHIVE}"
        )


def wait_promoted(port: int, restore_data: Path) -> None:
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        try:
            result = subprocess.run(
                [str(arg) for arg in psql_args("SELECT NOT pg_is_in_recovery()", port)],
                env=pg_env(),
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                encoding="utf-8",
                errors="replace",
                check=False,
                timeout=10,
            )
        except subprocess.TimeoutExpired:
            result = None
        if result is not None and result.returncode == 0 and result.stdout.strip() == "t":
            return
        if not (restore_data / "postmaster.pid").exists():
            raise VerifyError("恢复集群在等待目标 LSN 时提前停止")
        time.sleep(0.25)
    raise VerifyError("恢复集群 90 秒内未完成目标 LSN 重放并提升")


def control_data(restore_data: Path) -> dict[str, str]:
    output = execute([tool("pg_controldata"), f"--pgdata={restore_data}"]).stdout
    values: dict[str, str] = {}
    for line in output.splitlines():
        if ":" in line:
            key, value = line.split(":", 1)
            values[key.strip()] = value.strip()
    try:
        return {
            "system_identifier": values["Database system identifier"],
            "checkpoint_timeline": values["Latest checkpoint's TimeLineID"],
        }
    except KeyError as exc:
        raise VerifyError("pg_controldata 输出缺少 system identifier 或 timeline") from exc


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def configure_recovery(
    restore_data: Path, port: int, target_lsn: str, timeline: int
) -> None:
    if sys.platform == "win32":
        restore_command = f'copy /Y "{ARCHIVE / "%f"}" "%p"'
    else:
        restore_command = f'cp "{ARCHIVE / "%f"}" "%p"'
    settings = f"""
# 仅追加到本次独立恢复实例；最后出现的设置优先。
archive_mode = off
archive_command = ''
listen_addresses = '127.0.0.1'
port = {port}
restore_command = '{restore_command.replace("'", "''")}'
recovery_target_lsn = '{target_lsn}'
recovery_target_timeline = '{timeline}'
recovery_target_action = 'promote'
"""
    with (restore_data / "postgresql.auto.conf").open("a", encoding="utf-8", newline="\n") as config:
        config.write(settings)
    (restore_data / "recovery.signal").touch(exist_ok=False)


def main() -> None:
    if sys.platform == "win32":
        sys.stdout.reconfigure(encoding="utf-8")
        sys.stderr.reconfigure(encoding="utf-8")
    metadata = preflight()
    print("源集群预检通过，开始 pg_basebackup", flush=True)
    source_system_id = str(metadata["system_identifier"])
    source_timeline = int(metadata["timeline"])

    VERIFY_ROOT.mkdir(parents=True, exist_ok=True)
    ensure_within_runtime(VERIFY_ROOT)
    run_dir = Path(tempfile.mkdtemp(prefix="phase3-", dir=VERIFY_ROOT))
    ensure_within_runtime(run_dir)
    run_id = run_dir.name
    backup_dir = run_dir / "basebackup"
    restore_data = run_dir / "restore-data"
    source_resolved = SOURCE_DATA.resolve()
    restore_resolved = restore_data.resolve()
    if (
        source_resolved == restore_resolved
        or source_resolved in restore_resolved.parents
        or restore_resolved in source_resolved.parents
    ):
        raise VerifyError("恢复目录与源数据目录未隔离")

    baseline = json.loads(psql(SNAPSHOT_SQL))
    execute(
        [
            tool("pg_basebackup"),
            f"--pgdata={backup_dir}",
            "--format=tar",
            "--wal-method=stream",
            "--checkpoint=fast",
            f"--host={SOURCE_HOST}",
            f"--port={SOURCE_PORT}",
            f"--username={SOURCE_USER}",
            "--dbname=dbname=postgres",
            "--no-password",
            f"--label=phase3-{run_id}",
        ],
        env=pg_env(),
        log_path=run_dir / "pg_basebackup.log",
    )
    base_tar = backup_dir / "base.tar"
    wal_tar = backup_dir / "pg_wal.tar"
    extra_tars = [
        path.name
        for path in backup_dir.glob("*.tar")
        if path.name not in ("base.tar", "pg_wal.tar")
    ]
    if extra_tars:
        raise VerifyError(f"集群含额外表空间 tar，当前最小验证不伪造其恢复结果: {extra_tars}")
    for archive in (base_tar, wal_tar):
        if not archive.is_file() or archive.is_symlink():
            raise VerifyError(f"pg_basebackup 未生成预期普通文件: {archive}")

    print("pg_basebackup 已完成，开始安全解包", flush=True)
    safe_extract(base_tar, restore_data)
    safe_extract(wal_tar, restore_data / "pg_wal")
    backup_label = (restore_data / "backup_label").read_text(encoding="utf-8")
    timeline_match = re.search(r"^START TIMELINE:\s*(\d+)\s*$", backup_label, re.MULTILINE)
    if not timeline_match or int(timeline_match.group(1)) != source_timeline:
        raise VerifyError("base.tar 的备份起始 timeline 与源集群记录不一致")

    target_marker = f"phase3-target-{run_id}"
    after_marker = f"phase3-after-{run_id}"
    target_id = int(psql("SELECT COALESCE(MAX(id),0)+1 FROM public.sample_records"))
    psql(
        "INSERT INTO public.sample_records(id,name,amount) VALUES "
        f"({target_id},{sql_literal(target_marker)},1.00)"
    )
    target_lsn = psql("SELECT pg_current_wal_insert_lsn()")
    if not re.fullmatch(r"[0-9A-F]+/[0-9A-F]+", target_lsn):
        raise VerifyError(f"源实例返回非法目标 LSN: {target_lsn!r}")
    target_time = datetime.now(timezone.utc).isoformat()
    target_timeline = int(psql("SELECT timeline_id FROM pg_control_checkpoint()"))
    if target_timeline != source_timeline:
        raise VerifyError("备份与目标点之间发生 timeline 变化；拒绝假定恢复链有效")
    target_snapshot = json.loads(psql(SNAPSHOT_SQL))
    target_wal = psql(f"SELECT pg_walfile_name({sql_literal(target_lsn)})")
    print(f"目标 LSN 已确定: {target_lsn} ({target_wal})", flush=True)

    # 目标后的真实事务必须被 PITR 排除，防止把“恢复到最新”误报为成功。
    psql(
        "INSERT INTO public.sample_records(id,name,amount) VALUES "
        f"({target_id + 1},{sql_literal(after_marker)},2.00)"
    )
    after_lsn = psql("SELECT pg_current_wal_insert_lsn()")
    after_wal = psql(f"SELECT pg_walfile_name({sql_literal(after_lsn)})")
    psql("SELECT pg_switch_wal()")
    archive_names = {target_wal, after_wal}
    wait_for_archives(archive_names, int(metadata["wal_segment_bytes"]))
    # Windows 下 avoid restore_command 的路径/命令解释差异，直接把已核实归档段放入隔离恢复目录。
    for segment in archive_names:
        shutil.copy2(ARCHIVE / segment, restore_data / "pg_wal" / segment)

    print("目标 WAL 段归档完成，开始配置恢复", flush=True)
    restore_port = free_port()
    configure_recovery(restore_data, restore_port, target_lsn, target_timeline)
    start_attempted = False
    process: subprocess.Popen[bytes] | None = None
    log_handle = None
    report: dict[str, object] = {}
    try:
        start_attempted = True
        log_handle = (run_dir / "recovery-postgres.log").open("ab")
        process = subprocess.Popen(
            [
                str(tool("postgres")),
                "-D",
                str(restore_data),
                "-p",
                str(restore_port),
                "-h",
                SOURCE_HOST,
            ],
            stdout=log_handle,
            stderr=subprocess.STDOUT,
            env=pg_env(),
        )
        wait_promoted(restore_port, restore_data)
        print("恢复集群已重放到目标 LSN 并完成提升", flush=True)
        restored_snapshot = json.loads(psql(SNAPSHOT_SQL, restore_port))
        if restored_snapshot != target_snapshot:
            raise VerifyError("恢复后的 sample_records 与明确目标 LSN 的源数据快照不一致")
        if baseline != [row for row in target_snapshot if row["name"] != target_marker]:
            raise VerifyError("目标快照并非基线加唯一目标标记，拒绝继续判定")
        target_count = psql(
            f"SELECT COUNT(*) FROM public.sample_records WHERE name={sql_literal(target_marker)}",
            restore_port,
        )
        after_count = psql(
            f"SELECT COUNT(*) FROM public.sample_records WHERE name={sql_literal(after_marker)}",
            restore_port,
        )
        if target_count != "1" or after_count != "0":
            raise VerifyError(
                f"PITR 边界错误: 目标行数={target_count}，目标后行数={after_count}"
            )
        print("PITR 目标行与目标后行边界核对通过", flush=True)
        recovery_state = json.loads(
            psql(
                "SELECT json_build_object('in_recovery',pg_is_in_recovery(),"
                "'last_replay_lsn',pg_last_wal_replay_lsn())",
                restore_port,
            )
        )
        if recovery_state["in_recovery"] is not False:
            raise VerifyError("恢复实例未按 recovery_target_action=promote 完成提升")
        control = control_data(restore_data)
        if control["system_identifier"] != source_system_id:
            raise VerifyError("恢复集群 system identifier 与源集群不一致")
        report = {
            "status": "passed",
            "run_id": run_id,
            "source": {
                "host": SOURCE_HOST,
                "port": SOURCE_PORT,
                "version": metadata["version"],
                "system_identifier": source_system_id,
                "timeline": source_timeline,
            },
            "backup": {
                "base_tar": str(base_tar),
                "base_tar_bytes": base_tar.stat().st_size,
                "base_tar_sha256": sha256(base_tar),
                "pg_wal_tar": str(wal_tar),
            },
            "recovery": {
                "directory": str(restore_data),
                "port": restore_port,
                "target_lsn": target_lsn,
                "target_time_utc": target_time,
                "target_timeline": target_timeline,
                "archive_directory": str(ARCHIVE),
                "archive_files": sorted(archive_names),
                "last_replay_lsn": recovery_state["last_replay_lsn"],
                "restored_system_identifier": control["system_identifier"],
                "restored_checkpoint_timeline": control["checkpoint_timeline"],
            },
            "verification": {
                "baseline_rows": len(baseline),
                "target_rows": len(target_snapshot),
                "target_marker_id": target_id,
                "target_marker_present": True,
                "after_target_marker_absent": True,
                "sample_records_exact_snapshot_match": True,
            },
        }
    finally:
        if start_attempted:
            print("正在停止隔离恢复集群", flush=True)
            stopped = subprocess.run(
                [
                    str(tool("pg_ctl")),
                    f"--pgdata={restore_data}",
                    "--wait",
                    "--timeout=60",
                    "--mode=fast",
                    "stop",
                ],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                encoding="utf-8",
                errors="replace",
                check=False,
                timeout=90,
            )
            if stopped.returncode != 0:
                message = f"恢复集群停止失败: {(stopped.stderr or stopped.stdout).strip()}"
                if sys.exc_info()[0] is None:
                    raise VerifyError(message)
                print(message, file=sys.stderr)
            if process is not None:
                try:
                    process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    process.terminate()
                    process.wait(timeout=15)
            if log_handle is not None:
                log_handle.close()

    report_path = run_dir / "verification.json"
    report_path.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps(report, ensure_ascii=False, indent=2))
    print(f"验证报告: {report_path}")


if __name__ == "__main__":
    try:
        main()
    except VerifyError as exc:
        print(f"阶段三 PostgreSQL WAL PITR 验证失败: {exc}", file=sys.stderr)
        raise SystemExit(1)
