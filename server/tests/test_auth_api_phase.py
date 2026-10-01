"""AUTH-specific probe tests with isolated HTTP and fake DB; no live requests."""

import asyncio
import base64
import json
from contextlib import asynccontextmanager
from types import SimpleNamespace

import pytest
from aiohttp import web
from aiohttp.test_utils import TestClient, TestServer, make_mocked_request

from terlimo_backend import auth_api
from terlimo_backend import service_relay as relay
from terlimo_backend.auth_api_phase import new_auth_api_phase

SECRET = "do-not-log-fingerprint-body-headers-certificate-exception"
RID = "a" * 32


class Connection:
    def __init__(self):
        self.calls = []

    async def fetchval(self, sql, *args):
        self.calls.append("rate")
        return 0

    async def execute(self, sql, *args):
        self.calls.append("insert")


class Database:
    def __init__(self):
        self.connection = Connection()
        self.active = 0

    @asynccontextmanager
    async def acquire(self):
        self.active += 1
        try:
            yield self.connection
        finally:
            self.active -= 1


def settings():
    return SimpleNamespace(
        service_timeout_seconds=2,
        service_max_concurrency=2,
        service_upstream_host="127.0.0.1",
        challenge_ttl_seconds=60,
        challenge_rate_limit_per_minute=20,
        public_challenge_limit_per_minute=100,
        environment="test",
    )


def frame(path="/api/mobile/v1/auth/challenge"):
    body = json.dumps(
        {"installation_fingerprint": "f" * 64, "purpose": "session", "environment": "test"}
    ).encode()
    return json.dumps(
        {
            "v": 1,
            "op": "service.http",
            "request_id": RID,
            "method": "POST",
            "path": path,
            "query": "",
            "headers": {"Content-Type": "application/json"},
            "body_b64": base64.urlsafe_b64encode(body).rstrip(b"=").decode(),
        }
    ).encode()


@pytest.mark.parametrize("enabled", [False, True])
async def test_full_challenge_path_off_on(enabled, monkeypatch, caplog):
    monkeypatch.delenv("TERLIMO_AUTH_API_PHASE_PROBE", raising=False)
    if enabled:
        monkeypatch.setenv("TERLIMO_AUTH_API_PHASE_PROBE", "1")
    caplog.set_level("INFO", logger="terlimo_backend.auth_api_phase")
    db = Database()
    config = settings()
    inner = web.Application()
    inner[auth_api.AUTH_SERVICE_KEY] = auth_api.InstallationSessionService(config, db)
    inner.router.add_post("/api/mobile/v1/auth/challenge", auth_api._handle_challenge)
    async with TestServer(inner) as server:
        config.api_port = server.port
        monkeypatch.setattr(relay, "_peer_identities", lambda ssl: [SECRET])

        async def resolve(connection, identities, environment):
            assert identities == [SECRET]

        monkeypatch.setattr(relay, "resolve_gateway_context", resolve)
        # Mock TLS metadata only; production validation and frame parsing remain intact.
        outer = web.Application()
        outer[relay.SERVICE_ACTIVE_KEY] = [0]
        handler = relay._service_handler(config, db)

        async def wrapped(request):
            request.transport.get_extra_info = lambda name: None
            return await handler(request)

        outer.router.add_post(relay.SERVICE_PATH, wrapped)
        async with TestClient(TestServer(outer)) as client:
            response = await client.post(relay.SERVICE_PATH, data=frame())
            result = await response.json()
            assert result["status"] == 200 and result["request_id"] == RID
            body = json.loads(
                base64.urlsafe_b64decode(
                    result["body_b64"] + "=" * ((-len(result["body_b64"])) % 4)
                )
            )
            assert body["status"] == "ok" and body["purpose"] == "session"
            assert db.connection.calls == ["rate", "rate", "insert"]
            assert db.active == 0 and outer[relay.SERVICE_ACTIVE_KEY] == [0]
    logs = [r.message for r in caplog.records if r.message.startswith("AUTHPHASE")]
    assert bool(logs) is enabled
    if enabled:
        for phase in [
            "body_read_end",
            "parse_end",
            "peer_identity_end",
            "context_pool_acquire_begin",
            "context_pool_acquire_end",
            "resolve_gateway_context_begin",
            "resolve_gateway_context_end",
            "replay_begin",
            "replay_request_begin",
            "replay_response_headers",
            "replay_cappedbody_end",
            "replay_session_close_end",
            "replay_end",
            "json_end",
            "challenge_pool_acquire_begin",
            "challenge_pool_acquire_end",
            "rate_queries_end",
            "challenge_insert_end",
            "response_ready",
        ]:
            assert any("phase=" + phase + " " in line for line in logs), phase
        inner_logs = [l for l in logs if "scope=challenge " in l]
        assert all("request_id=" + body["request_id"] + " " in l for l in inner_logs)
        assert body["request_id"] != RID
        assert all("request_id=" + RID + " " in l for l in logs if "scope=service " in l)
        assert SECRET not in "\n".join(logs) and "f" * 64 not in "\n".join(logs)
        assert body["nonce_b64"] not in "\n".join(logs)
        assert body["challenge_id"] not in "\n".join(logs)


@pytest.mark.parametrize("kind", ["bad_frame", "other_path", "bad_challenge"])
async def test_unvalidated_or_other_routes_emit_nothing(kind, monkeypatch, caplog):
    monkeypatch.setenv("TERLIMO_AUTH_API_PHASE_PROBE", "1")
    caplog.set_level("INFO", logger="terlimo_backend.auth_api_phase")
    db = Database()
    config = settings()
    if kind == "bad_challenge":
        app = web.Application()
        app[auth_api.AUTH_SERVICE_KEY] = auth_api.InstallationSessionService(config, db)
        app.router.add_post("/api/mobile/v1/auth/challenge", auth_api._handle_challenge)
        async with TestClient(TestServer(app)) as client:
            response = await client.post(
                "/api/mobile/v1/auth/challenge", json={"installation_fingerprint": SECRET}
            )
            assert response.status == 400
    else:
        monkeypatch.setattr(relay, "_peer_identities", lambda ssl: [])

        async def resolve(*args):
            pass

        monkeypatch.setattr(relay, "resolve_gateway_context", resolve)

        async def replay(*args):
            return {"status": 200}

        monkeypatch.setattr(relay, "_replay_service_request", replay)
        request = make_mocked_request("POST", relay.SERVICE_PATH, app=web.Application())
        request.app[relay.SERVICE_ACTIVE_KEY] = [0]

        async def read(*args):
            return SECRET.encode() if kind == "bad_frame" else frame("/api/mobile/v1/auth/session")

        monkeypatch.setattr(relay, "_read_request_capped", read)
        await relay._service_handler(config, db)(request)
        assert request.app[relay.SERVICE_ACTIVE_KEY] == [0]
    assert not any(r.message.startswith("AUTHPHASE") for r in caplog.records)
    assert not db.connection.calls


@pytest.mark.parametrize("enabled", [False, True])
@pytest.mark.parametrize("mode", ["cancel", "timeout", "cap"])
async def test_replay_cleanup_caps_and_cancellation(enabled, mode, monkeypatch, caplog):
    monkeypatch.setenv("TERLIMO_AUTH_API_PHASE_PROBE", "1" if enabled else "0")
    caplog.set_level("INFO", logger="terlimo_backend.auth_api_phase")
    entered = asyncio.Event()
    closed = []
    original = relay.ClientSession

    def session(*args, **kwargs):
        obj = original(*args, **kwargs)
        close = obj.close

        async def closing():
            await close()
            closed.append(obj.closed)

        obj.close = closing
        return obj

    monkeypatch.setattr(relay, "ClientSession", session)

    async def upstream(request):
        entered.set()
        if mode == "cap":
            return web.Response(body=b"x" * (relay.SERVICE_MAX_RESPONSE_BODY + 1))
        await asyncio.Event().wait()

    app = web.Application()
    app.router.add_post("/api/mobile/v1/auth/challenge", upstream)
    async with TestServer(app) as server:
        config = settings()
        config.api_port = server.port
        if mode == "timeout":
            config.service_timeout_seconds = 0.05
        phase = new_auth_api_phase("service")
        if phase is not None:
            phase.activate(RID)
        task = asyncio.create_task(
            relay._replay_service_request(config, relay._parse_service_request(frame()), phase)
        )
        await entered.wait()
        if mode == "cancel":
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task
        else:
            result = await task
            assert result["request_id"] == RID
            assert result["error"]["code"] == (
                "SERVICE_BAD_RESPONSE" if mode == "cap" else "SERVICE_UNAVAILABLE"
            )
        assert closed == [True]
    logs = [r.message for r in caplog.records if r.message.startswith("AUTHPHASE")]
    assert bool(logs) is enabled
    if enabled:
        assert any("phase=replay_session_close_end " in l for l in logs)


@pytest.mark.parametrize("enabled", [False, True])
@pytest.mark.parametrize("scope", ["service", "challenge"])
async def test_pool_cancellation_propagates_and_releases_admission(
    enabled, scope, monkeypatch, caplog
):
    monkeypatch.setenv("TERLIMO_AUTH_API_PHASE_PROBE", "1" if enabled else "0")
    caplog.set_level("INFO", logger="terlimo_backend.auth_api_phase")
    entered = asyncio.Event()

    class BlockedDatabase:
        @asynccontextmanager
        async def acquire(self):
            entered.set()
            await asyncio.Event().wait()
            yield None

    db = BlockedDatabase()
    config = settings()
    app = web.Application()
    if scope == "service":
        app[relay.SERVICE_ACTIVE_KEY] = [0]
        request = make_mocked_request("POST", relay.SERVICE_PATH, app=app)

        async def read(*args):
            return frame()

        monkeypatch.setattr(relay, "_read_request_capped", read)
        monkeypatch.setattr(relay, "_peer_identities", lambda ssl: [SECRET])
        coroutine = relay._service_handler(config, db)(request)
    else:
        app[auth_api.AUTH_SERVICE_KEY] = auth_api.InstallationSessionService(config, db)
        request = make_mocked_request("POST", "/api/mobile/v1/auth/challenge", app=app)

        async def body(*args):
            return {
                "installation_fingerprint": "f" * 64,
                "purpose": "session",
                "environment": "test",
            }

        monkeypatch.setattr(auth_api, "_json_body", body)
        coroutine = auth_api._handle_challenge(request)
    task = asyncio.create_task(coroutine)
    await entered.wait()
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    if scope == "service":
        assert app[relay.SERVICE_ACTIVE_KEY] == [0]
    logs = [r.message for r in caplog.records if r.message.startswith("AUTHPHASE")]
    assert bool(logs) is enabled
    if enabled:
        assert any("outcome=cancelled" in l for l in logs)


async def test_real_mtls_challenge_probe_and_rejected_identity(
    migrated_url, settings_factory, tmp_path, monkeypatch, caplog
):
    from dataclasses import replace

    import aiohttp
    from test_step036_onboarding_hour_storage import _seed_gateway
    from test_step036_service_relay import _Material, _post, _service_settings, _start_site

    from terlimo_backend.api import create_app
    from terlimo_backend.db import Database as RealDatabase
    from terlimo_backend.evidence_transport import EvidenceListener

    monkeypatch.setenv("TERLIMO_AUTH_API_PHASE_PROBE", "1")
    caplog.set_level("INFO", logger="terlimo_backend.auth_api_phase")
    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key="terlimo-035-node")
    config = await _service_settings(settings_factory, migrated_url, material)
    db = RealDatabase(config)
    await db.ensure_ready()
    runner, port = await _start_site(
        create_app(replace(config, service_endpoint_enabled=False), db), 0, None
    )
    listener = EvidenceListener(replace(config, api_port=port), db)
    await listener.start()
    try:
        async with aiohttp.ClientSession(
            connector=aiohttp.TCPConnector(ssl=material.node_context())
        ) as client:
            status, result = await _post(client, listener.port, frame())
        assert status == 200 and result["status"] == 200
        records = [r.message for r in caplog.records if r.message.startswith("AUTHPHASE")]
        assert any(
            "scope=service " in l and "phase=resolve_gateway_context_end " in l for l in records
        )
        assert any("scope=challenge " in l and "phase=challenge_insert_end " in l for l in records)
        caplog.clear()
        async with aiohttp.ClientSession(
            connector=aiohttp.TCPConnector(ssl=material.node_context("terlimo-unknown"))
        ) as client:
            status, rejected = await _post(client, listener.port, frame())
        assert rejected["error"]["code"] in (
            "EVIDENCE_REGISTRY_UNKNOWN",
            "EVIDENCE_REGISTRY_AMBIGUOUS",
        )
        records = [r.message for r in caplog.records if r.message.startswith("AUTHPHASE")]
        assert not any("scope=challenge " in l or "phase=replay_begin " in l for l in records)
    finally:
        await listener.stop()
        await runner.cleanup()
        await db.close()
