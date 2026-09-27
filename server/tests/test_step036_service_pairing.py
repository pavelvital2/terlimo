"""Local pairing harness: accepted node service handler/codec (Go) -> live ServiceRelay
(AF_UNIX) -> per-node mTLS service endpoint -> existing public backend API.

The Go side uses the real node codec and handler (WL_SERVICE_PAIRING_SOCKET); no transport
mocks and no replacement API. A TEST-only middleware barrier proves active-upstream
cancellation, and the spawned Go subprocess is terminated and awaited on timeout/error.
TEST certificates, loopback listeners and the bundled TEST PostgreSQL only.
"""
from __future__ import annotations

import asyncio
import os
from dataclasses import replace

import pytest
from aiohttp import web
from test_step036_onboarding_hour_storage import _seed_gateway
from test_step036_service_relay import (
    GATEWAY_KEY,
    _Material,
    _service_settings,
)

from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.evidence_transport import EvidenceListener
from terlimo_backend.service_relay import ServiceRelay

NODE_DIR = os.environ.get("WL_NODE_DIR", "/tmp/opencode/wdtt-service")


async def _communicate_or_terminate(proc: asyncio.subprocess.Process, timeout: float):
    try:
        return await asyncio.wait_for(proc.communicate(), timeout=timeout)
    except TimeoutError:
        proc.terminate()
        try:
            await asyncio.wait_for(proc.wait(), timeout=5)
        except TimeoutError:
            proc.kill()
            await proc.wait()
        raise


async def test_node_backend_service_pairing(migrated_url, settings_factory, tmp_path):
    if not os.path.isdir(NODE_DIR):
        pytest.skip("node worktree is not available")
    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    app_settings = await _service_settings(settings_factory, migrated_url, material, api_port=0)
    database = Database(app_settings)
    await database.ensure_ready()
    app = create_app(replace(app_settings, service_endpoint_enabled=False), database)

    barrier_entered = asyncio.Event()
    upstream_cancelled = asyncio.Event()
    release_upstream = asyncio.Event()
    barrier_file = tmp_path / "barrier.entered"

    @web.middleware
    async def barrier_middleware(request: web.Request, handler):
        if request.headers.get("Idempotency-Key") == "pairing-barrier":
            barrier_entered.set()
            barrier_file.write_text("entered")
            try:
                await asyncio.wait_for(release_upstream.wait(), timeout=10)
            except TimeoutError:
                pass
            except asyncio.CancelledError:
                upstream_cancelled.set()  # whole chain reached and cancelled the API handler
                raise
        return await handler(request)

    app.middlewares.append(barrier_middleware)

    runner = web.AppRunner(app, handler_cancellation=True)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    api_port = runner.addresses[0][1]
    svc = replace(
        app_settings,
        api_port=api_port,
        service_relay_enabled=True,
        evidence_backend_host="127.0.0.1",
        evidence_backend_port=0,
        evidence_backend_server_name="localhost",
        evidence_node_cert_file=material.nodes[GATEWAY_KEY][0],
        evidence_node_key_file=material.nodes[GATEWAY_KEY][1],
        evidence_backend_ca_file=material.ca_file,
    )
    listener = EvidenceListener(svc, database)
    await listener.start()
    relay = ServiceRelay(replace(svc, evidence_backend_port=listener.port))
    await relay.start()
    env = dict(os.environ)
    env.update(
        {
            "PATH": "/tmp/go/bin:" + env.get("PATH", ""),
            "GOPATH": "/home/pavel/go",
            "GOFLAGS": "-mod=mod",
            "GOPROXY": "off",
            "GOTOOLCHAIN": "local",
            "WL_SERVICE_PAIRING_SOCKET": svc.service_relay_socket,
            "WL_PAIRING_BARRIER_FILE": str(barrier_file),
        }
    )
    proc: asyncio.subprocess.Process | None = None
    try:
        proc = await asyncio.create_subprocess_exec(
            "go", "test", "-run", "TestServicePairingUnixChain", "-count=1", "-v", ".",
            cwd=NODE_DIR,
            env=env,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.STDOUT,
        )
        out, _ = await _communicate_or_terminate(proc, timeout=300)
        text = out.decode("utf-8", "replace")
        print(text)
        assert proc.returncode == 0, text
        assert text.count("PAIRING_OK") == 8, text
        assert "--- PASS: TestServicePairingUnixChain" in text, text
        assert barrier_entered.is_set(), "active-upstream barrier was never entered"
        for _ in range(100):
            if upstream_cancelled.is_set():
                break
            await asyncio.sleep(0.02)
        assert upstream_cancelled.is_set(), "API handler was not cancelled after peer disconnect"
        assert not relay._tasks, "relay handler tasks not released after pairing"
    finally:
        release_upstream.set()
        if proc is not None and proc.returncode is None:
            proc.terminate()
            try:
                await asyncio.wait_for(proc.wait(), timeout=5)
            except TimeoutError:
                proc.kill()
                await proc.wait()
        await relay.stop()
        await listener.stop()
        await runner.cleanup()
        await database.close()
