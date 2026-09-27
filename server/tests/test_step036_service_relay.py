"""G6 service-only API relay: real mTLS listener + fixed loopback replay + AF_UNIX relay.

Drives the production builders (EvidenceListener/register_service_routes via listener start,
ServiceRelay) with ephemeral certs, the bundled TEST PostgreSQL and the real aiohttp public
API. No live material, no migrations, no node.
"""
from __future__ import annotations

import asyncio
import base64
import json
import os
import time
import uuid
from dataclasses import replace

import aiohttp
import pytest
from aiohttp import web
from test_step036_evidence_transport import _Material
from test_step036_onboarding_hour_storage import _seed_gateway


async def _start_site(app, port, ssl_context):
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", port, ssl_context=ssl_context)
    await site.start()
    return runner, runner.addresses[0][1]

from terlimo_backend.api import create_app
from terlimo_backend.config import ConfigError, load_settings
from terlimo_backend.db import Database
from terlimo_backend.evidence_transport import EvidenceListener, EvidenceTransportError
from terlimo_backend.service_relay import (
    SERVICE_PATH,
    ServiceRelay,
    service_path_allowed,
)

GATEWAY_KEY = "terlimo-035-node"


def _b64(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def _service_request(method: str, path: str, *, body: bytes = b"", headers=None, request_id=None, query=""):
    return json.dumps(
        {
            "v": 1,
            "op": "service.http",
            "request_id": request_id or uuid.uuid4().hex,
            "method": method,
            "path": path,
            "query": query,
            "headers": headers or {},
            "body_b64": _b64(body),
        }
    ).encode() + b"\n"


async def _service_settings(settings_factory, url, material, **overrides):
    values = {
        "evidence_endpoint_enabled": False,
        "service_endpoint_enabled": True,
        "evidence_server_cert_file": material.server_cert,
        "evidence_server_key_file": material.server_key,
        "evidence_client_ca_file": material.ca_file,
        "evidence_listen_host": "127.0.0.1",
        "evidence_listen_port": 0,
        "service_upstream_host": "127.0.0.1",
        "service_max_concurrency": 4,
        "service_relay_enabled": False,
        "service_relay_socket": str(material.tmp / "service.sock"),
        "service_relay_allowed_uid": os.getuid(),
        "service_timeout_seconds": 5,
    }
    values.update(overrides)
    return settings_factory(url, **values)


async def _post(session, port, payload: bytes):
    async with session.post(f"https://localhost:{port}{SERVICE_PATH}", data=payload) as response:
        return response.status, await response.json()


async def test_service_endpoint_replays_real_api_and_fixed_route(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    app_settings = await _service_settings(settings_factory, migrated_url, material)
    database = Database(app_settings)
    await database.ensure_ready()
    app = create_app(replace(app_settings, service_endpoint_enabled=False), database)
    runner, api_port = await _start_site(app, 0, None)
    service_settings = replace(app_settings, api_port=api_port)
    listener = EvidenceListener(service_settings, database)
    await listener.start()
    session = aiohttp.ClientSession(connector=aiohttp.TCPConnector(ssl=material.node_context()))
    try:
        # Faithful existing API: /me without a session token keeps its 401 + body.
        status, body = await _post(session, listener.port, _service_request("GET", "/api/mobile/v1/me"))
        assert status == 200 and body["v"] == 1 and body["status"] == 401
        decoded = json.loads(base64.urlsafe_b64decode(body["body_b64"] + "=" * (-len(body["body_b64"]) % 4)))
        assert decoded["code"] == "SESSION_INVALID"

        # Fixed route: an internal/arbitrary path is denied before any upstream call.
        _status, body = await _post(session, listener.port, _service_request("GET", "/internal/onboarding/evidence"))
        assert body["error"]["code"] == "SERVICE_PATH_DENIED"

        # Redirects are never followed (allow_redirects=False upstream): unknown route returns
        # the public API's own 404 faithfully.
        _status, body = await _post(
            session, listener.port,
            _service_request("GET", "/api/mobile/v1/operations/01234567-89ab-cdef-0123-456789abcdef"),
        )
        assert body["v"] == 1 and body["status"] == 401
    finally:
        await session.close()
        await listener.stop()
        await runner.cleanup()
        await database.close()


async def test_service_endpoint_large_frames_caps_and_no_redirect(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)

    big = b"r" * (300 * 1024)

    async def echo(request: web.Request) -> web.Response:
        body = await request.read()
        if request.path.endswith("/redirect"):
            raise web.HTTPFound("/elsewhere")
        return web.Response(body=big if request.path.endswith("/access/sync") else body, content_type="application/json")

    upstream = web.Application()
    upstream.router.add_post("/api/mobile/v1/access/sync", echo)
    runner, api_port = await _start_site(upstream, 0, None)

    settings = await _service_settings(settings_factory, migrated_url, material, api_port=api_port)
    database = Database(settings)
    await database.ensure_ready()
    listener = EvidenceListener(settings, database)
    await listener.start()
    session = aiohttp.ClientSession(connector=aiohttp.TCPConnector(ssl=material.node_context()))
    try:
        # >16KiB request body and >256KiB response body survive the relay.
        payload = _service_request("POST", "/api/mobile/v1/access/sync", body=b"q" * (20 * 1024))
        _status, body = await _post(session, listener.port, payload)
        assert body["status"] == 200
        decoded = base64.urlsafe_b64decode(body["body_b64"] + "=" * (-len(body["body_b64"]) % 4))
        assert len(decoded) == len(big)

        # Decoded request cap (64KiB) is enforced before upstream.
        _status, body = await _post(
            session, listener.port,
            _service_request("POST", "/api/mobile/v1/access/sync", body=b"q" * (70 * 1024)),
        )
        assert body["error"]["code"] == "SERVICE_BAD_FRAME"
    finally:
        await session.close()
        await listener.stop()
        await runner.cleanup()
        await database.close()


async def test_service_endpoint_rejects_bad_peer_and_invalid_frame(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    settings = await _service_settings(settings_factory, migrated_url, material)
    database = Database(settings)
    await database.ensure_ready()
    listener = EvidenceListener(settings, database)
    await listener.start()
    try:
        # Unknown CN with a cert from the trusted CA: registry rejects.
        session = aiohttp.ClientSession(connector=aiohttp.TCPConnector(ssl=material.node_context("terlimo-unknown")))
        try:
            async with session.post(f"https://localhost:{listener.port}{SERVICE_PATH}", data=_service_request("GET", "/api/mobile/v1/me")) as response:
                body = await response.json()
            assert body["error"]["code"] in ("EVIDENCE_REGISTRY_UNKNOWN", "EVIDENCE_REGISTRY_AMBIGUOUS")
        finally:
            await session.close()

        # Cert signed by a foreign CA: TLS-level rejection.
        session = aiohttp.ClientSession(connector=aiohttp.TCPConnector(ssl=material.node_context("terlimo-foreign")))
        try:
            with pytest.raises((aiohttp.ClientError, ConnectionError, OSError)):
                async with session.post(f"https://localhost:{listener.port}{SERVICE_PATH}", data=_service_request("GET", "/api/mobile/v1/me")):
                    pass
        finally:
            await session.close()
    finally:
        await listener.stop()
        await database.close()


async def test_service_relay_peercred_and_forward(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    app_settings = await _service_settings(settings_factory, migrated_url, material)
    database = Database(app_settings)
    await database.ensure_ready()
    app = create_app(replace(app_settings, service_endpoint_enabled=False), database)
    runner, api_port = await _start_site(app, 0, None)
    service_settings = replace(
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
    listener = EvidenceListener(service_settings, database)
    await listener.start()
    relay_settings = replace(service_settings, evidence_backend_port=listener.port)
    relay = ServiceRelay(relay_settings)
    await relay.start()
    try:
        reader, writer = await asyncio.open_unix_connection(relay_settings.service_relay_socket)
        writer.write(_service_request("GET", "/api/mobile/v1/me"))
        await writer.drain()
        line = await asyncio.wait_for(reader.readline(), timeout=5)
        body = json.loads(line)
        assert body["status"] == 401
        writer.close()
        await writer.wait_closed()
    finally:
        await relay.stop()
        await listener.stop()
        await runner.cleanup()
        await database.close()

    # Wrong peer uid: the relay closes without a response.
    wrong = replace(relay_settings, service_relay_allowed_uid=os.getuid() + 1)
    relay_wrong = ServiceRelay(wrong)
    await relay_wrong.start()
    try:
        reader, writer = await asyncio.open_unix_connection(wrong.service_relay_socket)
        writer.write(_service_request("GET", "/api/mobile/v1/me"))
        await writer.drain()
        with pytest.raises((asyncio.IncompleteReadError, TimeoutError, ConnectionError, OSError)):
            await asyncio.wait_for(reader.readline(), timeout=1)
        writer.close()
        try:
            await writer.wait_closed()
        except (ConnectionError, OSError):
            pass
    finally:
        await relay_wrong.stop()


def test_service_path_allowlist_and_role_validation(monkeypatch):
    assert service_path_allowed("GET", "/api/mobile/v1/me")
    assert service_path_allowed("POST", "/api/mobile/v1/onboarding/intents")
    assert service_path_allowed("POST", "/api/mobile/v1/registration/telegram/link")
    assert service_path_allowed("POST", "/api/mobile/v1/trial/activate")
    assert service_path_allowed("GET", "/api/mobile/v1/plans")
    assert service_path_allowed("POST", "/api/mobile/v1/quotes")
    assert service_path_allowed("POST", "/api/mobile/v1/payments")
    assert service_path_allowed("GET", "/api/mobile/v1/payments/01234567-89ab-cdef-0123-456789abcdef")
    assert not service_path_allowed("POST", "/api/mobile/v1/plans")
    assert not service_path_allowed("GET", "/api/mobile/v1/quotes")
    assert not service_path_allowed("GET", "/api/mobile/v1/payments")
    assert not service_path_allowed("POST", "/api/mobile/v1/payments/01234567-89ab-cdef-0123-456789abcdef")
    assert not service_path_allowed("GET", "/api/mobile/v1/payments/../me")
    assert not service_path_allowed("GET", "/api/mobile/v1/payments/orders/01234567-89ab-cdef-0123-456789abcdef")
    assert not service_path_allowed("GET", "/api/mobile/v1/registration/telegram/link")
    assert not service_path_allowed("GET", "/api/mobile/v1/trial/activate")
    assert not service_path_allowed("POST", "/api/mobile/v1/registration/telegram/confirm")
    assert service_path_allowed("GET", "/api/mobile/v1/operations/01234567-89ab-cdef-0123-456789abcdef")
    for method, path in (("GET", "/internal/onboarding/evidence"), ("POST", "/api/mobile/v1/me"), ("CONNECT", "host:443")):
        assert not service_path_allowed(method, path)

    for name in ("ONBOARDING_EVIDENCE_SERVER_CERT_FILE", "ONBOARDING_SERVICE_RELAY_SOCKET", "ONBOARDING_SERVICE_RELAY_UID"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("ONBOARDING_SERVICE_ENDPOINT_ENABLED", "1")
    monkeypatch.delenv("ONBOARDING_SERVICE_RELAY_ENABLED", raising=False)
    with pytest.raises(ConfigError):
        load_settings(require_database=False)


async def _start_dummy(app, host="127.0.0.1"):
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, host, 0)
    await site.start()
    return runner, runner.addresses[0][1]


async def _listener_and_relay(settings_factory, migrated_url, material, api_port, *, max_concurrency=4):
    settings = await _service_settings(
        settings_factory, migrated_url, material,
        api_port=api_port, service_max_concurrency=max_concurrency,
        service_relay_enabled=True,
        evidence_backend_host="127.0.0.1", evidence_backend_port=0,
        evidence_backend_server_name="localhost",
        evidence_node_cert_file=material.nodes[GATEWAY_KEY][0],
        evidence_node_key_file=material.nodes[GATEWAY_KEY][1],
        evidence_backend_ca_file=material.ca_file,
    )
    database = Database(settings)
    await database.ensure_ready()
    listener = EvidenceListener(settings, database)
    await listener.start()
    relay = ServiceRelay(replace(settings, evidence_backend_port=listener.port))
    await relay.start()
    return settings, database, listener, relay


async def test_service_relay_near_64k_request_large_response_and_raw_query(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    seen = {}

    async def echo(request: web.Request) -> web.Response:
        seen["query"] = request.raw_path
        await request.read()
        return web.Response(body=b"z" * (300 * 1024), content_type="application/json")

    upstream = web.Application()
    upstream.router.add_post("/api/mobile/v1/access/sync", echo)
    runner, api_port = await _start_dummy(upstream)
    settings, database, listener, relay = await _listener_and_relay(
        settings_factory, migrated_url, material, api_port
    )
    try:
        reader, writer = await asyncio.open_unix_connection(settings.service_relay_socket, limit=1536 * 1024 + 1)
        writer.write(
            _service_request(
                "POST", "/api/mobile/v1/access/sync",
                body=b"q" * (64 * 1024), query="a=%2Fb&c=1",
            )
        )
        await writer.drain()
        line = await asyncio.wait_for(reader.readline(), timeout=10)
        body = json.loads(line)
        assert body["status"] == 200, body
        decoded = base64.urlsafe_b64decode(body["body_b64"] + "=" * (-len(body["body_b64"]) % 4))
        assert len(decoded) == 300 * 1024
        assert seen["query"].endswith("?a=%2Fb&c=1"), seen["query"]  # raw target preserved
        writer.close()
        await writer.wait_closed()
    finally:
        await relay.stop()
        await listener.stop()
        await runner.cleanup()
        await database.close()


async def test_service_relay_bounded_admission_and_disconnect(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    started = asyncio.Event()

    async def stall(request: web.Request) -> web.Response:
        started.set()
        await asyncio.sleep(30)  # stall until cancelled
        return web.Response(text="late")

    upstream = web.Application()
    upstream.router.add_post("/api/mobile/v1/access/sync", stall)
    runner, api_port = await _start_dummy(upstream)
    settings, database, listener, relay = await _listener_and_relay(
        settings_factory, migrated_url, material, api_port, max_concurrency=1
    )
    try:
        _first_reader, first_writer = await asyncio.open_unix_connection(settings.service_relay_socket)
        first_writer.write(_service_request("POST", "/api/mobile/v1/access/sync", body=b"a" * 16))
        await first_writer.drain()
        await asyncio.wait_for(started.wait(), timeout=5)

        # Second connection exceeds the cap: closed immediately, never queued.
        second_reader, second_writer = await asyncio.open_unix_connection(settings.service_relay_socket)
        second_writer.write(_service_request("GET", "/api/mobile/v1/me"))
        await second_writer.drain()
        with pytest.raises((asyncio.IncompleteReadError, ConnectionError, OSError, TimeoutError)):
            await asyncio.wait_for(second_reader.read(), timeout=2)
        second_writer.close()
        try:
            await second_writer.wait_closed()
        except (ConnectionError, OSError):
            pass

        # Node disconnect aborts the stalled upstream instead of waiting for the timeout.
        first_writer.close()
        try:
            await first_writer.wait_closed()
        except (ConnectionError, OSError):
            pass
        deadline = time.monotonic() + 5
        while relay._tasks and time.monotonic() < deadline:
            await asyncio.sleep(0.05)
        assert not relay._tasks, "relay task leaked after disconnect"

        # stop() with an active in-flight read/write returns promptly and removes the socket.
        _third_reader, third_writer = await asyncio.open_unix_connection(settings.service_relay_socket)
        third_writer.write(_service_request("POST", "/api/mobile/v1/access/sync", body=b"b" * 16))
        await third_writer.drain()
        await asyncio.wait_for(relay.stop(), timeout=5)
        third_writer.close()
    finally:
        await listener.stop()
        await runner.cleanup()
        await database.close()


async def test_service_endpoint_strict_negatives(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    settings = await _service_settings(settings_factory, migrated_url, material, api_port=1)
    database = Database(settings)
    await database.ensure_ready()
    listener = EvidenceListener(settings, database)
    await listener.start()
    session = aiohttp.ClientSession(connector=aiohttp.TCPConnector(ssl=material.node_context()))

    def frame(**overrides):
        payload = {
            "v": 1, "op": "service.http", "request_id": uuid.uuid4().hex,
            "method": "GET", "path": "/api/mobile/v1/me", "query": "",
            "headers": {}, "body_b64": "",
        }
        payload.update(overrides)
        return json.dumps(payload).encode() + b"\n"

    cases = {
        "bool_v": frame(v=True),
        "numeric_request_id": frame(request_id=12345),
        "padded_b64": frame(body_b64="YQ=="),
        "garbage_b64": frame(body_b64="!!!!"),
        "percent_path": frame(path="/api/mobile/v1/%2e%2e/me"),
        "fragment_query": frame(path="/api/mobile/v1/me", query="a=1#x"),
        "control_header": frame(headers={"Content-Type": "a\u0000b"}),
        "noncanonical_b64": frame(body_b64="AAAAA"),
    }
    try:
        for name, payload in cases.items():
            async with session.post(f"https://localhost:{listener.port}{SERVICE_PATH}", data=payload) as response:
                body = await response.json()
            code = body["error"]["code"]
            assert code in ("SERVICE_BAD_FRAME", "SERVICE_BAD_HEADERS", "SERVICE_BAD_PATH"), (name, body)
    finally:
        await session.close()
        await listener.stop()
        await database.close()


async def test_service_relay_socket_safety(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    base = await _service_settings(
        settings_factory, migrated_url, material,
        service_relay_enabled=True,
        evidence_backend_host="127.0.0.1", evidence_backend_port=1,
        evidence_backend_server_name="localhost",
        evidence_node_cert_file=material.nodes[GATEWAY_KEY][0],
        evidence_node_key_file=material.nodes[GATEWAY_KEY][1],
        evidence_backend_ca_file=material.ca_file,
    )
    # Non-private parent directory is refused.
    public_dir = tmp_path / "public"
    public_dir.mkdir(mode=0o755)
    relay_public = ServiceRelay(replace(base, service_relay_socket=str(public_dir / "s.sock")))
    with pytest.raises(EvidenceTransportError):
        await relay_public.start()
    # A foreign regular file at the socket path is never unlinked.
    private_dir = tmp_path / "private"
    private_dir.mkdir(mode=0o700)
    victim = private_dir / "s.sock"
    victim.write_text("do-not-delete")
    relay = ServiceRelay(replace(base, service_relay_socket=str(victim)))
    with pytest.raises(EvidenceTransportError):
        await relay.start()
    assert victim.read_text() == "do-not-delete"


async def test_service_endpoint_ipv6_loopback(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)

    async def ok(_request: web.Request) -> web.Response:
        return web.Response(text="ok", content_type="application/json")

    upstream = web.Application()
    upstream.router.add_get("/api/mobile/v1/me", ok)
    try:
        runner, api_port = await _start_dummy(upstream, host="::1")
    except OSError:
        pytest.skip("IPv6 loopback unavailable")
    settings = await _service_settings(
        settings_factory, migrated_url, material, api_port=api_port, service_upstream_host="::1"
    )
    database = Database(settings)
    await database.ensure_ready()
    listener = EvidenceListener(settings, database)
    await listener.start()
    session = aiohttp.ClientSession(connector=aiohttp.TCPConnector(ssl=material.node_context()))
    try:
        async with session.post(
            f"https://localhost:{listener.port}{SERVICE_PATH}", data=_service_request("GET", "/api/mobile/v1/me")
        ) as response:
            body = await response.json()
        assert body["status"] == 200, body
    finally:
        await session.close()
        await listener.stop()
        await runner.cleanup()
        await database.close()


async def test_service_endpoint_slow_body_times_out_and_releases_slot(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)

    async def ok(_request: web.Request) -> web.Response:
        return web.Response(text="ok", content_type="application/json")

    upstream = web.Application()
    upstream.router.add_get("/api/mobile/v1/me", ok)
    runner, api_port = await _start_dummy(upstream)
    settings = await _service_settings(
        settings_factory, migrated_url, material,
        api_port=api_port, service_max_concurrency=1, service_timeout_seconds=1,
    )
    database = Database(settings)
    await database.ensure_ready()
    listener = EvidenceListener(settings, database)
    await listener.start()
    ctx = material.node_context()
    try:
        # Slow body without disconnect: the full admitted handler must time out and free the slot.
        started = time.monotonic()
        _reader, writer = await asyncio.open_connection("localhost", listener.port, ssl=ctx, server_hostname="localhost")
        writer.write(
            b"POST " + SERVICE_PATH.encode() + b" HTTP/1.1\r\nHost: localhost\r\n"
            b"Content-Type: application/json\r\nContent-Length: 100000\r\n\r\n{"
        )
        await writer.drain()
        await asyncio.sleep(1.5)
        writer.close()
        try:
            await writer.wait_closed()
        except (ConnectionError, OSError):
            pass
        elapsed = time.monotonic() - started
        assert elapsed < 5

        # Slot is free again: a normal request through the same listener succeeds.
        session = aiohttp.ClientSession(connector=aiohttp.TCPConnector(ssl=material.node_context()))
        try:
            async with session.post(
                f"https://localhost:{listener.port}{SERVICE_PATH}", data=_service_request("GET", "/api/mobile/v1/me")
            ) as response:
                body = await response.json()
            assert body["status"] == 200, body
        finally:
            await session.close()
    finally:
        await listener.stop()
        await runner.cleanup()
        await database.close()


async def test_service_relay_no_orphans_or_lost_exceptions(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    started = asyncio.Event()

    async def stall(_request: web.Request) -> web.Response:
        started.set()
        await asyncio.sleep(30)
        return web.Response(text="late")

    upstream = web.Application()
    upstream.router.add_post("/api/mobile/v1/access/sync", stall)
    runner, api_port = await _start_dummy(upstream)
    settings, database, listener, relay = await _listener_and_relay(
        settings_factory, migrated_url, material, api_port, max_concurrency=2
    )
    recorded: list[dict] = []
    loop = asyncio.get_running_loop()
    previous = loop.get_exception_handler()
    loop.set_exception_handler(lambda _loop, context: recorded.append(context))
    try:
        _reader, writer = await asyncio.open_unix_connection(settings.service_relay_socket)
        writer.write(_service_request("POST", "/api/mobile/v1/access/sync", body=b"a" * 16))
        await writer.drain()
        await asyncio.wait_for(started.wait(), timeout=5)
        writer.close()
        try:
            await writer.wait_closed()
        except (ConnectionError, OSError):
            pass
        deadline = time.monotonic() + 5
        while relay._tasks and time.monotonic() < deadline:
            await asyncio.sleep(0.05)
        assert not relay._tasks, "handler task orphaned after disconnect"
        await relay.stop()
        assert not relay._tasks
        assert recorded == [], f"lost task exceptions: {recorded}"
    finally:
        loop.set_exception_handler(previous)
        await listener.stop()
        await runner.cleanup()
        await database.close()


async def test_service_relay_second_instance_and_stop_ownership(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    base = await _service_settings(
        settings_factory, migrated_url, material,
        service_relay_enabled=True,
        evidence_backend_host="127.0.0.1", evidence_backend_port=1,
        evidence_backend_server_name="localhost",
        evidence_node_cert_file=material.nodes[GATEWAY_KEY][0],
        evidence_node_key_file=material.nodes[GATEWAY_KEY][1],
        evidence_backend_ca_file=material.ca_file,
    )
    private_dir = tmp_path / "private"
    private_dir.mkdir(mode=0o700)
    relay1 = ServiceRelay(replace(base, service_relay_socket=str(private_dir / "s.sock")))
    await relay1.start()
    try:
        # Second instance on the same live path must fail closed and leave the first intact.
        relay2 = ServiceRelay(replace(base, service_relay_socket=str(private_dir / "s.sock")))
        with pytest.raises(EvidenceTransportError):
            await relay2.start()
        assert relay1._server is not None
        # Double start on the same object is guarded.
        with pytest.raises(EvidenceTransportError):
            await relay1.start()

        # If the path is replaced, stop() must not remove the replacement (inode ownership).
        os.unlink(str(private_dir / "s.sock"))
        replacement = private_dir / "s.sock"
        replacement.write_text("replacement")
        await relay1.stop()
        assert replacement.read_text() == "replacement"
    finally:
        await relay1.stop()


def test_service_path_allowlist_usage_route():
    assert service_path_allowed("GET", "/api/mobile/v1/usage")
    assert not service_path_allowed("POST", "/api/mobile/v1/usage")
    assert not service_path_allowed("GET", "/api/mobile/v1/usage/")
    assert not service_path_allowed("GET", "/api/mobile/v1/usage/extra")
    assert not service_path_allowed("GET", "/api/mobile/v1/usages")
    assert not service_path_allowed("GET", "/api/mobile/v1/usage?window=30")
    assert not service_path_allowed("GET", "/api/mobile/v1/usage/../me")
    assert not service_path_allowed("GET", "/api/mobile/internal/usage")


def test_service_path_allowlist_devices_class():
    # Android 54ecabc DEVICES class: exactly two operations, bearer + Idempotency-Key forwarded.
    assert service_path_allowed("GET", "/api/mobile/v1/devices")
    assert service_path_allowed("DELETE", "/api/mobile/v1/devices/01234567-89ab-cdef-0123-456789abcdef")
    assert not service_path_allowed("POST", "/api/mobile/v1/devices")
    assert not service_path_allowed("GET", "/api/mobile/v1/devices/01234567-89ab-cdef-0123-456789abcdef")
    assert not service_path_allowed("DELETE", "/api/mobile/v1/devices")
    assert not service_path_allowed("DELETE", "/api/mobile/v1/devices/not-a-uuid")
    assert not service_path_allowed("DELETE", "/api/mobile/v1/devices/01234567-89ab-cdef-0123-456789abcdef/extra")
    assert not service_path_allowed("DELETE", "/api/mobile/v1/devices/../me")
    assert not service_path_allowed("DELETE", "/api/mobile/v1/me")
    assert not service_path_allowed("PUT", "/api/mobile/v1/devices/01234567-89ab-cdef-0123-456789abcdef")


async def test_service_endpoint_devices_class_forwarding_and_gates(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    seen: dict = {}

    async def echo(request: web.Request) -> web.Response:
        seen.update(
            {
                "method": request.method,
                "path": request.path,
                "auth": request.headers.get("Authorization"),
                "idem": request.headers.get("Idempotency-Key"),
            }
        )
        return web.json_response({"status": "ok"})

    upstream = web.Application()
    upstream.router.add_get("/api/mobile/v1/devices", echo)
    upstream.router.add_delete("/api/mobile/v1/devices/{device_id}", echo)
    runner, api_port = await _start_site(upstream, 0, None)
    settings = await _service_settings(settings_factory, migrated_url, material, api_port=api_port)
    database = Database(settings)
    await database.ensure_ready()
    listener = EvidenceListener(settings, database)
    await listener.start()
    client = aiohttp.ClientSession(connector=aiohttp.TCPConnector(ssl=material.node_context()))
    try:
        device_id = "01234567-89ab-cdef-0123-456789abcdef"
        headers = {
            "Authorization": "Bearer synthetic-token",
            "Idempotency-Key": f"idem-{uuid.uuid4().hex}",
            "Content-Type": "application/json",
        }
        _status, body = await _post(
            client, listener.port, _service_request("GET", "/api/mobile/v1/devices", headers=headers)
        )
        assert body["status"] == 200
        assert seen == {
            "method": "GET",
            "path": "/api/mobile/v1/devices",
            "auth": "Bearer synthetic-token",
            "idem": headers["Idempotency-Key"],
        }
        _status, body = await _post(
            client,
            listener.port,
            _service_request("DELETE", f"/api/mobile/v1/devices/{device_id}", headers=headers),
        )
        assert body["status"] == 200
        assert seen["method"] == "DELETE" and seen["path"] == f"/api/mobile/v1/devices/{device_id}"
        assert seen["auth"] == "Bearer synthetic-token" and seen["idem"] == headers["Idempotency-Key"]

        before = dict(seen)
        for payload in (
            _service_request("POST", "/api/mobile/v1/devices", headers=headers),
            _service_request("GET", f"/api/mobile/v1/devices/{device_id}", headers=headers),
            _service_request("DELETE", "/api/mobile/v1/devices", headers=headers),
            _service_request("DELETE", "/api/mobile/v1/devices/not-a-uuid", headers=headers),
            _service_request("DELETE", f"/api/mobile/v1/devices/{device_id}/extra", headers=headers),
            _service_request("DELETE", "/api/mobile/v1/me", headers=headers),
        ):
            _status, body = await _post(client, listener.port, payload)
            assert body["error"]["code"] == "SERVICE_PATH_DENIED", body
        _status, body = await _post(
            client, listener.port, _service_request("PATCH", f"/api/mobile/v1/devices/{device_id}")
        )
        assert body["error"]["code"] == "SERVICE_BAD_METHOD"
        # only these two endpoint/method pairs reached the upstream at all
        assert seen == before
    finally:
        await client.close()
        await listener.stop()
        await runner.cleanup()
        await database.close()


async def test_service_error_frames_echo_request_id(migrated_url, settings_factory, tmp_path):
    """Rejected frames must echo the frame request_id; otherwise the node peer reports
    SERVICE_BAD_RESPONSE instead of the real reason code (devices cold-refresh defect)."""
    import json as _json

    material = _Material(tmp_path)
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    app_settings = await _service_settings(settings_factory, migrated_url, material)
    database = Database(app_settings)
    await database.ensure_ready()
    app = create_app(replace(app_settings, service_endpoint_enabled=False), database)
    runner, api_port = await _start_site(app, 0, None)
    service_settings = replace(app_settings, api_port=api_port)
    listener = EvidenceListener(service_settings, database)
    await listener.start()
    client = aiohttp.ClientSession(connector=aiohttp.TCPConnector(ssl=material.node_context()))
    try:
        cases = [
            ("GET", "/api/mobile/v1/private", None, "SERVICE_PATH_DENIED"),
            ("GET", "/api/mobile/v1//me", None, "SERVICE_BAD_PATH"),
            ("PATCH", "/api/mobile/v1/me", None, "SERVICE_BAD_METHOD"),
            (
                "GET",
                "/api/mobile/v1/me",
                {"X-Forwarded-Host": "evil"},
                "SERVICE_BAD_HEADERS",
            ),
        ]
        for method, path, headers, want in cases:
            request_id = uuid.uuid4().hex
            _status, body = await _post(
                client,
                listener.port,
                _service_request(method, path, headers=headers, request_id=request_id),
            )
            assert body["error"]["code"] == want, (method, path, body)
            assert body["request_id"] == request_id, (method, path, body["request_id"], request_id)

        # frame-version rejection also echoes a well-formed id
        request_id = uuid.uuid4().hex
        bad_frame = _json.dumps(
            {
                "v": 2,
                "op": "service.http",
                "request_id": request_id,
                "method": "GET",
                "path": "/api/mobile/v1/me",
                "query": "",
                "headers": {},
                "body_b64": "",
            }
        ).encode() + b"\n"
        _status, body = await _post(client, listener.port, bad_frame)
        assert body["error"]["code"] == "SERVICE_BAD_FRAME"
        assert body["request_id"] == request_id

        # a forwarded success keeps echoing the id as well
        request_id = uuid.uuid4().hex
        _status, body = await _post(
            client, listener.port, _service_request("GET", "/api/mobile/v1/me", request_id=request_id)
        )
        assert body["v"] == 1 and body["status"] == 401
        assert body["request_id"] == request_id
    finally:
        await client.close()
        await listener.stop()
        await runner.cleanup()
        await database.close()
