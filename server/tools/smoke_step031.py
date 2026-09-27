#!/usr/bin/env python3
"""STEP03.1 smoke: migrations, readiness and durable outbox on real PostgreSQL.

Runs an isolated PostgreSQL (bundled pgserver binaries, datadir under /tmp),
applies the versioned migrations through the documented CLI, starts the API and
the worker as real processes, simulates a crashed worker and an unknown job type,
and prints evidence lines. Nothing outside the given datadir/ports is touched.
"""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

from terlimo_backend.migrations import runner as migration_runner

ROOT = Path(__file__).resolve().parents[1]
VENV_BIN = ROOT / ".venv" / "bin"


def say(message: str) -> None:
    print(f"[smoke] {message}", flush=True)


def evidence(name: str, value: object) -> None:
    print(f"[evidence] {name}={json.dumps(value, ensure_ascii=False)}", flush=True)


def run(cmd: list[str], env: dict[str, str]) -> subprocess.CompletedProcess:
    result = subprocess.run(cmd, env=env, capture_output=True, text=True, timeout=120, check=False)
    say(f"$ {' '.join(cmd)} -> rc={result.returncode}")
    if result.stdout.strip():
        say(f"stdout: {result.stdout.strip()}")
    if result.returncode != 0 and result.stderr.strip():
        say(f"stderr: {result.stderr.strip()}")
    return result


def http_post_json(url: str, payload: dict) -> tuple[int, dict]:
    data = json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(
        url, data=data, headers={"Content-Type": "application/json"}, method="POST"
    )
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            return response.status, json.loads(response.read())
    except urllib.error.HTTPError as error:
        return error.code, json.loads(error.read())


def http_json(url: str) -> tuple[int, dict]:
    try:
        with urllib.request.urlopen(url, timeout=5) as response:
            return response.status, json.loads(response.read())
    except urllib.error.HTTPError as error:
        return error.code, json.loads(error.read())


def wait_for(url: str, expected: int, timeout: float = 20.0) -> dict:
    deadline = time.monotonic() + timeout
    last: dict = {}
    while time.monotonic() < deadline:
        try:
            status, body = http_json(url)
            last = body
            if status == expected:
                return body
        except (OSError, urllib.error.URLError, json.JSONDecodeError):
            pass
        time.sleep(0.25)
    raise RuntimeError(f"{url} did not return {expected} in time (last={last})")


async def db_fetch(dsn: str, query: str, *args):
    import asyncpg

    connection = await asyncpg.connect(dsn, timeout=10)
    try:
        return await connection.fetch(query, *args)
    finally:
        await connection.close()


async def db_execute(dsn: str, query: str, *args) -> str:
    import asyncpg

    connection = await asyncpg.connect(dsn, timeout=10)
    try:
        return await connection.execute(query, *args)
    finally:
        await connection.close()


async def db_fetchval(dsn: str, query: str, *args):
    import asyncpg

    connection = await asyncpg.connect(dsn, timeout=10)
    try:
        return await connection.fetchval(query, *args)
    finally:
        await connection.close()


async def db_create_database(admin_dsn: str, name: str) -> None:
    import asyncpg

    connection = await asyncpg.connect(admin_dsn, timeout=10)
    try:
        await connection.execute(f'CREATE DATABASE "{name}"')
    finally:
        await connection.close()


def ensure_port_free(port: int) -> None:
    import socket as socket_module

    probe = socket_module.socket(socket_module.AF_INET, socket_module.SOCK_STREAM)
    try:
        probe.setsockopt(socket_module.SOL_SOCKET, socket_module.SO_REUSEADDR, 1)
        probe.bind(("127.0.0.1", port))
    except OSError as error:
        raise SystemExit(f"port {port} is already in use; choose another --api-port/--bad-api-port") from error
    finally:
        probe.close()


def start_api(env: dict[str, str], port: int) -> subprocess.Popen:
    process = subprocess.Popen(
        [str(VENV_BIN / "terlimo-api")],
        env={**env, "API_PORT": str(port)},
    )
    say(f"started terlimo-api on port {port} (pid={process.pid})")
    return process


def stop_process(process: subprocess.Popen, name: str) -> None:
    if process.poll() is not None:
        say(f"{name} already exited with rc={process.returncode}")
        return
    process.terminate()
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)
    say(f"stopped {name} (rc={process.returncode})")


def main() -> int:
    parser = argparse.ArgumentParser(description="STEP03.1 smoke")
    parser.add_argument("--pgdata", default="/tmp/terlimo-backend-step031-pg")
    parser.add_argument("--keep", action="store_true", help="keep PostgreSQL datadir after the run")
    parser.add_argument("--api-port", type=int, default=18081)
    parser.add_argument("--bad-api-port", type=int, default=19082)
    arguments = parser.parse_args()

    ensure_port_free(arguments.api_port)
    ensure_port_free(arguments.bad_api_port)

    import platform

    runtime_dir = Path(tempfile.mkdtemp(prefix="terlimo-step031-runtime-"))
    runtime_dir.chmod(0o700)
    os.environ["XDG_RUNTIME_DIR"] = str(runtime_dir)
    import pgserver

    say(f"python={sys.version.split()[0]} platform={platform.platform()}")
    pgdata = Path(arguments.pgdata)
    if pgdata.exists():
        shutil.rmtree(pgdata, ignore_errors=True)
    say(f"starting isolated PostgreSQL datadir={pgdata}")
    server = pgserver.get_server(pgdata)
    server.ensure_pgdata_inited()
    server.ensure_postgres_running()
    database_url = server.get_uri()
    database_name = "terlimo_step031"
    asyncio.run(db_create_database(database_url, database_name))
    parts = urlsplit(database_url)
    database_url = urlunsplit(
        (parts.scheme, parts.netloc, f"/{database_name}", parts.query, parts.fragment)
    )
    postgres_version = asyncio.run(db_fetchval(database_url, "SHOW server_version"))
    evidence("postgres_version", postgres_version)
    evidence("database", parts.path or "(socket)")
    env = {**os.environ, "DATABASE_URL": database_url, "TERLIMO_ENV": "test", "LOG_LEVEL": "WARNING"}

    processes: list[subprocess.Popen] = []
    failures: list[str] = []
    try:
        say("step 1: migrations up / repeat / down (reverse) / up")
        expected_versions = [migration.version for migration in migration_runner.discover_versions()]
        first = run([str(VENV_BIN / "terlimo-migrate"), "up"], env)
        second = run([str(VENV_BIN / "terlimo-migrate"), "up"], env)
        if json.loads(first.stdout or "{}") != {"applied": expected_versions}:
            failures.append("first migration run did not apply all expected versions")
        if json.loads(second.stdout or "{}") != {"applied": []}:
            failures.append("second migration run was not a no-op")
        for version in reversed(expected_versions):
            down = run(
                [str(VENV_BIN / "terlimo-migrate"), "down", "--version", version], env
            )
            if down.returncode != 0:
                failures.append(f"migration down failed for {version}")
        reapply = run([str(VENV_BIN / "terlimo-migrate"), "up"], env)
        if json.loads(reapply.stdout or "{}") != {"applied": expected_versions}:
            failures.append("migration re-apply after down failed")
        status = run([str(VENV_BIN / "terlimo-migrate"), "status"], env)
        evidence("migration_status", status.stdout.strip())

        say("step 2: API readiness with migrations applied")
        api = start_api(env, arguments.api_port)
        processes.append(api)
        live = wait_for(f"http://127.0.0.1:{arguments.api_port}/health/live", 200)
        evidence("health_live", live.get("status"))
        ready = wait_for(f"http://127.0.0.1:{arguments.api_port}/health/ready", 200)
        evidence("health_ready", {"status": ready.get("status"), "migrations": ready.get("migrations")})

        say("step 2b: mobile v1 challenge endpoint is routed in the real API process")
        probe_fingerprint = hashlib.sha256(os.urandom(32)).hexdigest()
        challenge_status, challenge_body = http_post_json(
            f"http://127.0.0.1:{arguments.api_port}/api/mobile/v1/auth/challenge",
            {
                "installation_fingerprint": probe_fingerprint,
                "purpose": "enrollment",
                "environment": "test",
            },
        )
        evidence(
            "mobile_challenge",
            {
                "http": challenge_status,
                "status": challenge_body.get("status"),
                "single_use": challenge_body.get("single_use"),
                "purpose": challenge_body.get("purpose"),
            },
        )
        if challenge_status != 200 or challenge_body.get("single_use") is not True:
            failures.append("mobile v1 challenge endpoint not routed correctly")

        say("step 3: API readiness reports database absence")
        bad_env = {
            **env,
            "DATABASE_URL": "postgresql://terlimo_backend:placeholder@127.0.0.1:1/terlimo_backend_test",
            "DB_COMMAND_TIMEOUT_SECONDS": "2",
        }
        bad_api = start_api(bad_env, arguments.bad_api_port)
        processes.append(bad_api)
        bad_ready = wait_for(f"http://127.0.0.1:{arguments.bad_api_port}/health/ready", 503)
        evidence("health_ready_no_db", bad_ready.get("reason"))

        say("step 4: durable job survives worker process restart and applies once")
        run(
            [
                str(VENV_BIN / "terlimo-outbox"),
                "enqueue",
                "--type",
                "self_check",
                "--idempotency-key",
                "smoke-main",
            ],
            env,
        )
        worker_first = run([str(VENV_BIN / "terlimo-worker"), "--once"], env)
        if worker_first.returncode != 0:
            failures.append("worker --once failed")
        run([str(VENV_BIN / "terlimo-worker"), "--once"], env)
        main_job = asyncio.run(
            db_fetch(database_url, "SELECT status, attempts FROM outbox_operations WHERE idempotency_key='smoke-main'")
        )
        main_effects = asyncio.run(
            db_fetchval(database_url, "SELECT count(*) FROM outbox_effect_log WHERE idempotency_key='smoke-main'")
        )
        evidence(
            "smoke_main_after_worker_restart",
            {"job": dict(main_job[0]), "effects": main_effects},
        )
        if main_job[0]["status"] != "done" or main_job[0]["attempts"] != 1 or main_effects != 1:
            failures.append("self_check job did not apply exactly once across worker restart")

        say("step 5: duplicate enqueue is a no-op and creates no second effect")
        duplicate = run(
            [
                str(VENV_BIN / "terlimo-outbox"),
                "enqueue",
                "--type",
                "self_check",
                "--idempotency-key",
                "smoke-main",
            ],
            env,
        )
        if '"enqueued": false' not in duplicate.stdout:
            failures.append("duplicate idempotency key was enqueued twice")
        effects_after_duplicate = asyncio.run(
            db_fetchval(database_url, "SELECT count(*) FROM outbox_effect_log WHERE idempotency_key='smoke-main'")
        )
        evidence("effects_after_duplicate", effects_after_duplicate)
        if effects_after_duplicate != 1:
            failures.append("duplicate enqueue produced a second effect")

        say("step 6: crashed worker (stale processing row) is reclaimed after restart")
        asyncio.run(
            db_execute(
                database_url,
                """
                INSERT INTO outbox_operations
                    (operation_type, idempotency_key, status, attempts, locked_by, locked_at)
                VALUES ('self_check', 'smoke-stale', 'processing', 1, 'crashed', now() - interval '1 hour')
                """,
            )
        )
        worker_recover = run([str(VENV_BIN / "terlimo-worker"), "--once"], env)
        if worker_recover.returncode != 0:
            failures.append("recovery worker failed")
        stale_job = asyncio.run(
            db_fetch(database_url, "SELECT status, attempts FROM outbox_operations WHERE idempotency_key='smoke-stale'")
        )
        stale_effects = asyncio.run(
            db_fetchval(database_url, "SELECT count(*) FROM outbox_effect_log WHERE idempotency_key='smoke-stale'")
        )
        evidence("smoke_stale", {"job": dict(stale_job[0]), "effects": stale_effects})
        if stale_job[0]["status"] != "done" or stale_effects != 1:
            failures.append("stale job was not recovered exactly once")

        say("step 7: unknown operation type is a terminal failure, never success")
        run(
            [
                str(VENV_BIN / "terlimo-outbox"),
                "enqueue",
                "--type",
                "apply_grant",
                "--idempotency-key",
                "smoke-unknown",
            ],
            env,
        )
        run([str(VENV_BIN / "terlimo-worker"), "--once"], env)
        unknown_job = asyncio.run(
            db_fetch(database_url, "SELECT status, last_error FROM outbox_operations WHERE idempotency_key='smoke-unknown'")
        )
        evidence("smoke_unknown", dict(unknown_job[0]))
        if unknown_job[0]["status"] != "failed" or unknown_job[0]["last_error"] != "unsupported_operation_type":
            failures.append("unknown operation type was not a terminal failure")

        say("step 8: long-running worker exits gracefully on SIGTERM")
        loop_worker = subprocess.Popen(
            [str(VENV_BIN / "terlimo-worker")],
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        processes.append(loop_worker)
        time.sleep(2)
        loop_worker.send_signal(signal.SIGTERM)
        try:
            loop_worker.wait(timeout=10)
        except subprocess.TimeoutExpired:
            failures.append("worker did not stop on SIGTERM")
        evidence("worker_sigterm_rc", loop_worker.returncode)
        if loop_worker.returncode != 0:
            failures.append(f"graceful worker stop rc={loop_worker.returncode}")

        statuses = asyncio.run(
            db_fetch(database_url, "SELECT status, count(*) AS count FROM outbox_operations GROUP BY status ORDER BY status")
        )
        evidence("outbox_status", {row["status"]: row["count"] for row in statuses})
    finally:
        for process in processes:
            stop_process(process, "process")
        server.cleanup()
        if arguments.keep:
            say(f"kept PostgreSQL datadir {pgdata}")
        else:
            shutil.rmtree(pgdata, ignore_errors=True)

    if failures:
        say(f"SMOKE FAILED: {failures}")
        return 1
    say("SMOKE PASSED")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
