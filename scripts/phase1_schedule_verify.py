#!/usr/bin/env python3
"""验证服务重启后的持久化，以及无浏览器连接时的后台调度。"""
from __future__ import annotations

import json
import sys
import time
from datetime import datetime

from phase1_api_verify import API, require_file, RUNTIME


def parse(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def main() -> None:
    if sys.platform == "win32":
        sys.stdout.reconfigure(encoding="utf-8")
    api = API()
    api.call("POST", "/api/v1/auth/login", {"username": "admin", "password": require_file(RUNTIME / "admin-password")})
    _, settings = api.call("GET", "/api/v1/settings")
    started = parse(settings["started_at"])
    _, sources = api.call("GET", "/api/v1/sources")
    _, jobs = api.call("GET", "/api/v1/jobs")
    _, runs = api.call("GET", "/api/v1/runs")
    _, artifacts = api.call("GET", "/api/v1/artifacts")
    if not sources or not jobs or not runs or not artifacts:
        raise AssertionError("服务重启后，数据源、任务、运行或备份记录丢失")
    restored = [run for run in runs if run["restore_status"] == "verified"]
    if not restored:
        raise AssertionError("服务重启后，恢复验证记录丢失")

    deadline = time.time() + 90
    while time.time() < deadline:
        _, runs = api.call("GET", "/api/v1/runs")
        scheduled = [run for run in runs if run["trigger_type"] == "scheduled" and run["state"] == "succeeded" and parse(run["created_at"]) > started]
        if scheduled:
            print(json.dumps({
                "status": "passed", "service_started_at": settings["started_at"],
                "persistent_sources": len(sources), "persistent_jobs": len(jobs),
                "persistent_restore_verifications": len(restored),
                "scheduled_run_id": scheduled[0]["id"],
                "browser_connection": "验证期间没有打开浏览器；调度由服务进程执行",
            }, ensure_ascii=False, indent=2))
            return
        time.sleep(1)
    raise AssertionError("90 秒内未观察到本次服务启动后的成功计划运行")


if __name__ == "__main__":
    main()
