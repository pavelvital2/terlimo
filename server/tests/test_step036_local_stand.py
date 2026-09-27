"""Local stand smoke: start/readiness/stop + mTLS evidence reachability (TEST-only).

Real backend pieces only (create_app + EvidenceListener + local evidence Unix relay) on
loopback with ephemeral ephemeral certificates and the bundled TEST PostgreSQL. No live
hosts, no node/phone, no secrets printed; the readiness artifact is 0600 under tmp_path.
"""
from __future__ import annotations

import asyncio
import base64
import importlib.util
import json
import os
import socket
import ssl

import pytest
from aiohttp import ClientSession, TCPConnector
from test_step036_onboarding_hour_storage import _connect, _seed_gateway

STAND_PATH = os.path.join(
    os.path.dirname(os.path.dirname(__file__)), "tools", "step036", "local_stand.py"
)


def _load_stand():
    spec = importlib.util.spec_from_file_location("step036_local_stand", STAND_PATH)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


async def test_local_stand_smoke_start_readiness_stop(migrated_url, tmp_path):
    stand_module = _load_stand()
    root = tmp_path / "stand"
    settings = stand_module.build_stand_settings(migrated_url, root, service=True, relay=True)
    await _seed_gateway(migrated_url, key=stand_module.GATEWAY_KEY)
    stand = stand_module.LocalStand(settings, root)
    try:
        readiness = await stand.start()
        assert readiness["environment"] == "test"
        assert readiness["gateway_key"] == stand_module.GATEWAY_KEY
        assert readiness["evidence"]["port"] > 0
        assert readiness["relay_socket"] and os.path.exists(readiness["relay_socket"])
        manifest = root / "readiness.json"
        assert manifest.exists() and (os.stat(manifest).st_mode & 0o777) == 0o600
        assert set(json.loads(manifest.read_text())) >= {"api_base", "evidence", "relay_socket"}

        async with (
            ClientSession() as client,
            client.get(f"{readiness['api_base']}/health/live") as response,
        ):
            assert response.status == 200
            assert (await response.json())["status"] == "live"

        context = ssl.create_default_context(cafile=settings.evidence_client_ca_file)
        context.load_cert_chain(settings.evidence_node_cert_file, settings.evidence_node_key_file)
        context.check_hostname = True
        context.verify_mode = ssl.CERT_REQUIRED
        async with (
            ClientSession(connector=TCPConnector(ssl=context)) as session,
            session.post(
                f"https://localhost:{readiness['evidence']['port']}/internal/onboarding/evidence",
                data=b"{}\n",
            ) as response,
        ):
            assert response.status == 400
            assert (await response.json())["code"].startswith("EVIDENCE_")

        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as probe:
            probe.settimeout(2)
            probe.connect(readiness["relay_socket"])

        assert readiness["service_socket"] and os.path.exists(readiness["service_socket"])
        service_reader, service_writer = await asyncio.open_unix_connection(
            readiness["service_socket"]
        )
        frame = json.dumps(
            {
                "v": 1,
                "op": "service.http",
                "request_id": "a" * 32,
                "method": "GET",
                "path": "/api/mobile/v1/me",
                "query": "",
                "headers": {},
                "body_b64": "",
            }
        ).encode() + b"\n"
        service_writer.write(frame)
        await service_writer.drain()
        service_response = json.loads(await asyncio.wait_for(service_reader.readline(), timeout=10))
        service_writer.close()
        await service_writer.wait_closed()
        # Real public API answer through service relay -> mTLS service endpoint -> API replay:
        # /me without a session is a genuine 401 JSON error from the running backend.
        assert service_response["status"] == 401
        body = json.loads(base64.urlsafe_b64decode(service_response["body_b64"] + "=="))
        assert body["status"] == "error" and body["code"] == "SESSION_INVALID"
    finally:
        relay_socket = settings.evidence_relay_socket
        await stand.stop()
    assert not os.path.exists(relay_socket)
    connection = await _connect(migrated_url)
    try:
        assert await connection.fetchval("SELECT count(*) FROM onboarding_evidence") == 0
        assert await connection.fetchval("SELECT count(*) FROM entitlements") == 0
    finally:
        await connection.close()


async def test_stand_settings_disable_roles_fail_closed(migrated_url, tmp_path):
    stand_module = _load_stand()
    settings = stand_module.build_stand_settings(migrated_url, tmp_path / "stand2", relay=False)
    shutdown = stand_module.LocalStand(settings, tmp_path / "stand2")
    try:
        readiness = await shutdown.start()
        assert readiness["relay_socket"] is None
    finally:
        await shutdown.stop()
    assert settings.evidence_endpoint_enabled is True


async def test_run_bounded_cancels_startup_on_signal(tmp_path, capsys):
    stand_module = _load_stand()

    class StubStand:
        def __init__(self):
            self.release = asyncio.Event()
            self.stopped = 0
            self.start_cancelled = False

        async def start(self):
            try:
                await self.release.wait()
            except asyncio.CancelledError:
                self.start_cancelled = True
                raise
            return {"api_base": "http://127.0.0.1:1", "evidence": {"port": 1}}

        async def stop(self):
            self.stopped += 1

    stub = StubStand()
    stop_event = asyncio.Event()
    task = asyncio.create_task(stand_module.run_bounded(stub, stop_event))
    await asyncio.sleep(0.05)
    stop_event.set()
    result = await asyncio.wait_for(task, timeout=5)
    assert result is None
    assert stub.start_cancelled is True
    assert stub.stopped == 1
    assert '"status": "ready"' not in capsys.readouterr().out


async def test_stop_runs_every_cleanup_despite_one_failure(tmp_path):
    stand_module = _load_stand()
    root = tmp_path / "stopfail"
    settings = stand_module.build_stand_settings("postgresql://unused", root, service=True, relay=False)
    stand = stand_module.LocalStand(settings, root)
    calls: list[str] = []

    class Failing:
        async def stop(self):
            calls.append("service")
            raise OSError("injected")

    class Recording:
        async def stop(self):
            calls.append("relay")

    class Closing:
        async def close(self):
            calls.append("db")

    stand._service_relay = Failing()
    stand._relay = Recording()
    stand._database = Closing()
    errors = await stand.stop()
    assert calls == ["service", "relay", "db"]
    assert errors and "service" in errors[0]
    assert json.loads((root / "readiness.json").read_text())["status"] == "stopped"


async def test_stop_cleanup_budget_is_parallel_and_complete(tmp_path):
    stand_module = _load_stand()
    root = tmp_path / "budget"
    settings = stand_module.build_stand_settings("postgresql://unused", root, service=True, relay=True)
    stand = stand_module.LocalStand(settings, root)
    calls: list[str] = []

    class Slow:
        def __init__(self, name):
            self.name = name

        async def stop(self):
            calls.append(self.name)
            await asyncio.sleep(0.3)

    class SlowRunner:
        async def cleanup(self):
            calls.append("runner")
            await asyncio.sleep(0.3)

    class SlowDb:
        async def close(self):
            calls.append("db")
            await asyncio.sleep(0.3)

    stand._service_relay = Slow("service")
    stand._relay = Slow("relay")
    stand._runner = SlowRunner()
    stand._database = SlowDb()
    started = asyncio.get_event_loop().time()
    errors = await stand.stop(per_resource_timeout=0.05)
    elapsed = asyncio.get_event_loop().time() - started
    assert calls == ["service", "relay", "runner", "db"]
    assert len(errors) == 4 and all("timeout" in item for item in errors)
    assert elapsed < 0.25, elapsed


async def test_run_bounded_settles_tasks_when_startup_raises(tmp_path):
    stand_module = _load_stand()

    class ExplodingStand:
        def __init__(self):
            self.stopped = 0

        async def start(self):
            await asyncio.sleep(0.01)
            raise RuntimeError("injected startup failure")

        async def stop(self):
            self.stopped += 1

    stub = ExplodingStand()
    stop_event = asyncio.Event()
    before = {task for task in asyncio.all_tasks() if task is not asyncio.current_task()}
    with pytest.raises(RuntimeError):
        await stand_module.run_bounded(stub, stop_event)
    assert stub.stopped == 1
    remaining = {
        task for task in asyncio.all_tasks() if task is not asyncio.current_task()
    } - before
    assert not remaining, remaining
