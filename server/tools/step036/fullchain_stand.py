#!/usr/bin/env python3
"""Bounded isolated STEP036 backend/node stand for one client fullchain run.

The node receives bootstrap credentials only through the real backend outbox
worker and its admin socket. Its service and evidence relays use separate Unix
sockets. This tool binds only loopback, starts no persistent host service, and
removes its own temporary PostgreSQL instance during cleanup.
"""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import json
import os
import secrets
import signal
import shutil
import tempfile
from dataclasses import replace
from datetime import UTC, datetime
from pathlib import Path

import asyncpg

from terlimo_backend.gateway_adapter import build_bootstrap_client_factory
from terlimo_backend.migrations import runner
from terlimo_backend.onboarding_hour import OnboardingHourHandlers, build_secret_cipher
from terlimo_backend.worker import OutboxWorker

from local_stand import GATEWAY_KEY, LocalStand, _write_private, build_stand_settings

DEFAULT_NODE_DIR = "/tmp/opencode/step036-codex-fullchain-node"
SERVICE_CLASSIFIER = "step036-public-service-classifier"
LOCAL_TURN_HASH = "step036-local-turn-hash"


def _utc() -> str:
    return datetime.now(UTC).isoformat()


async def _wait_node_ready(path: Path, process: asyncio.subprocess.Process) -> dict:
    deadline = asyncio.get_running_loop().time() + 60
    while asyncio.get_running_loop().time() < deadline:
        if path.exists():
            return json.loads(path.read_text())
        if process.returncode is not None:
            raise RuntimeError(f"node exited before readiness: {process.returncode}")
        await asyncio.sleep(0.05)
    raise TimeoutError("node readiness timed out")


async def _stop_node(process: asyncio.subprocess.Process | None, node_root: Path) -> None:
    if process is None:
        return
    _write_private(node_root / "stop", b"1")
    try:
        await asyncio.wait_for(process.wait(), timeout=10)
    except TimeoutError:
        os.killpg(process.pid, signal.SIGTERM)
        try:
            await asyncio.wait_for(process.wait(), timeout=5)
        except TimeoutError:
            os.killpg(process.pid, signal.SIGKILL)
            await asyncio.wait_for(process.wait(), timeout=5)
    finally:
        # go test launches a separate test binary. Bound its whole process group,
        # including a child that might survive the go parent after a failed run.
        with contextlib.suppress(ProcessLookupError):
            os.killpg(process.pid, signal.SIGTERM)


async def _summary(database) -> dict:
    async with database.acquire() as connection:
        counts = {}
        for name in ("onboarding_intents", "onboarding_evidence", "entitlements"):
            counts[name] = await connection.fetchval(f"SELECT count(*) FROM {name}")
        hours = await connection.fetch(
            """
            SELECT starts_at, ends_at FROM entitlements
            WHERE kind = 'onboarding_hour' ORDER BY starts_at
            """
        )
        counts["onboarding_hour_durations_seconds"] = [
            int((row["ends_at"] - row["starts_at"]).total_seconds())
            for row in hours
        ]
        return counts


async def _phase(database) -> dict:
    async with database.acquire() as connection:
        return {
            "ready_intents": await connection.fetchval(
                "SELECT count(*) FROM onboarding_intents WHERE state = 'ready'"
            ),
            "evidence": await connection.fetchval("SELECT count(*) FROM onboarding_evidence"),
            "hours": await connection.fetchval(
                "SELECT count(*) FROM entitlements WHERE kind = 'onboarding_hour'"
            ),
        }


async def run(root: Path, node_source: Path, seconds: int) -> dict:
    root.mkdir(parents=True, exist_ok=False, mode=0o700)
    root.chmod(0o700)
    runtime = root / "runtime"
    runtime.mkdir(mode=0o700)
    os.environ["XDG_RUNTIME_DIR"] = str(runtime)
    import pgserver  # after XDG_RUNTIME_DIR is set

    pgdata = root / "pgdata"
    pg = pgserver.get_server(pgdata)
    stand: LocalStand | None = None
    node: asyncio.subprocess.Process | None = None
    worker_task: asyncio.Task | None = None
    worker_stop = asyncio.Event()
    node_root = root / "node"
    node_root.mkdir(mode=0o700)
    start = _utc()
    result: dict = {"status": "failed", "started_at": start}
    try:
        pg.ensure_pgdata_inited()
        pg.ensure_postgres_running()
        database_url = pg.get_uri()
        connection = await asyncpg.connect(database_url, timeout=10)
        try:
            await runner.apply_migrations(connection)
        finally:
            await connection.close()
        main_password = secrets.token_urlsafe(32)
        settings = replace(
            build_stand_settings(database_url, root, service=True, relay=True),
            gateway_local_admin_enabled=True,
            gateway_admin_main_password=main_password,
        )
        stand = LocalStand(settings, root)
        ready = await stand.start()
        if ready["relay_socket"] == ready["service_socket"]:
            raise RuntimeError("service and evidence sockets overlap")
        env = dict(os.environ)
        env.update(
            {
                "STEP036_NODE_DIR": str(node_root),
                "STEP036_NODE_MAIN_PASSWORD": main_password,
                "STEP036_EVIDENCE_SOCKET": ready["relay_socket"],
                "STEP036_SERVICE_SOCKET": ready["service_socket"],
                "STEP036_SERVICE_SEED": SERVICE_CLASSIFIER,
                "PATH": "/tmp/go/bin:" + env.get("PATH", ""),
                "GOPATH": "/home/pavel/go",
                "GOPROXY": "off",
                "GOFLAGS": "-mod=mod",
                "GOTOOLCHAIN": "local",
            }
        )
        node_log = (root / "node.log").open("wb")
        os.chmod(root / "node.log", 0o600)
        try:
            node = await asyncio.create_subprocess_exec(
                "go", "test", "-v", "-run", "^TestStep036FullchainNode$", "-count=1", ".",
                cwd=node_source,
                env=env,
                stdout=node_log,
                stderr=asyncio.subprocess.STDOUT,
                start_new_session=True,
            )
        finally:
            node_log.close()
        node_ready = await _wait_node_ready(node_root / "node-ready.json", node)
        endpoints = {
            "node_id": GATEWAY_KEY,
            "peer_ip": "127.0.0.1",
            "dtls_port": node_ready["dtls_port"],
            "wg_port": 56001,
            "dtls_spki_sha256": node_ready["dtls_spki_sha256"],
            "target_workers": 36,
            "admin_socket": node_ready["admin_socket"],
        }
        async with stand._database.acquire() as connection:
            await connection.execute(
                """
                INSERT INTO gateways (gateway_key, environment, endpoints, capabilities,
                                      registry_state, confirmed_max_workers, vk_hashes)
                VALUES ($1, 'test', $2::jsonb, '["managed"]'::jsonb, 'registered', 36, $3::jsonb)
                """,
                GATEWAY_KEY,
                json.dumps(endpoints),
                json.dumps([LOCAL_TURN_HASH]),
            )
        handlers = OnboardingHourHandlers(
            cipher=build_secret_cipher(settings),
            client_factory=build_bootstrap_client_factory(settings),
        ).as_handlers()
        worker = OutboxWorker(stand._database, settings, handlers=handlers, worker_id="step036-fullchain")
        worker_task = asyncio.create_task(worker.run_forever(worker_stop))
        manifest = {
            "status": "ready",
            "created_at": _utc(),
            "api_base": ready["api_base"],
            "base_url": ready["api_base"],
            "environment": "test",
            "service_seed": {
                "version": 1,
                "revision": "1",
                "environment": "test",
                "peer_ip": "127.0.0.1",
                "dtls_port": node_ready["dtls_port"],
                "dtls_spki_sha256": node_ready["dtls_spki_sha256"],
                "service_classifier": SERVICE_CLASSIFIER,
                "vk_hashes": [LOCAL_TURN_HASH],
                "stream_id": 0,
            },
            "node": {
                "peer_ip": "127.0.0.1",
                "dtls_port": node_ready["dtls_port"],
                "dtls_spki_sha256": node_ready["dtls_spki_sha256"],
                "service_classifier": SERVICE_CLASSIFIER,
            },
            "service_socket": ready["service_socket"],
            "evidence_socket": ready["relay_socket"],
        }
        _write_private(root / "fullchain-ready.json", json.dumps(manifest, indent=2).encode())
        print(json.dumps({"status": "ready", "manifest": str(root / "fullchain-ready.json")}), flush=True)
        stop_file = root / "stop"
        deadline = asyncio.get_running_loop().time() + seconds
        phases: list[dict] = []
        previous_phase: dict | None = None
        while asyncio.get_running_loop().time() < deadline:
            if stop_file.exists():
                break
            if node.returncode is not None:
                raise RuntimeError("node stopped during fullchain stand")
            if worker_task.done():
                worker_task.result()
                raise RuntimeError("worker stopped during fullchain stand")
            phase = await _phase(stand._database)
            if phase != previous_phase:
                phases.append({"observed_at": _utc(), **phase})
                previous_phase = phase
            await asyncio.sleep(0.1)
        result = {"status": "stopped", "started_at": start, "stopped_at": _utc()}
        result.update(await _summary(stand._database))
        result["phase_transitions"] = phases
        _write_private(root / "fullchain-result.json", json.dumps(result, indent=2).encode())
        return result
    finally:
        _write_private(root / "fullchain-ready.json", b'{"status":"stopped"}')
        worker_stop.set()
        if worker_task is not None:
            with contextlib.suppress(BaseException):
                await asyncio.wait_for(worker_task, timeout=5)
        try:
            await _stop_node(node, node_root)
        finally:
            try:
                if stand is not None:
                    await stand.stop()
            finally:
                pg.cleanup()
                shutil.rmtree(pgdata, ignore_errors=True)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--node-source", type=Path, default=Path(DEFAULT_NODE_DIR))
    parser.add_argument("--seconds", type=int, default=240)
    args = parser.parse_args()
    if not 1 <= args.seconds <= 540:
        parser.error("--seconds must be 1..540")
    if args.root.exists():
        parser.error("--root must not exist")
    result = asyncio.run(run(args.root, args.node_source, args.seconds))
    print(json.dumps({"status": result["status"], "result": str(args.root / "fullchain-result.json")}), flush=True)


if __name__ == "__main__":
    main()
