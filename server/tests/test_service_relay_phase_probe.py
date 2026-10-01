"""Bounded relay probe: fixed metadata, original IO and cancellation behavior."""

import asyncio
import time
from types import SimpleNamespace

import pytest
from aiohttp import ClientError, ClientSession, TCPConnector, web

from terlimo_backend import service_relay as module

SECRET = "never-log-payload-url-header-exception"


@pytest.fixture
def relay(monkeypatch, caplog):
    monkeypatch.setenv("TERLIMO_SERVICE_RELAY_PHASE_PROBE", "1")
    monkeypatch.setattr(module, "require_service_relay_material", lambda settings: None)
    monkeypatch.setattr(module, "_peer_credentials", lambda writer: (123, 456))
    caplog.set_level("INFO", logger=module.__name__)
    return module.ServiceRelay(
        SimpleNamespace(
            service_timeout_seconds=2,
            service_relay_allowed_uid=123,
            service_relay_allowed_gid=456,
            service_max_concurrency=1,
            evidence_backend_host=SECRET,
            evidence_backend_port=443,
            evidence_backend_server_name=SECRET,
        )
    )


class Writer:
    def __init__(self):
        self.data = b""
        self.closed = False

    def write(self, data):
        self.data += data

    async def drain(self):
        pass

    def close(self):
        self.closed = True

    async def wait_closed(self):
        pass


class Session:
    def __init__(self, error=None, block=False):
        self.error = error
        self.block = block
        self.entered = asyncio.Event()
        self.cancelled = False
        self.calls = []
        self.closed = False

    def post(self, url, **kwargs):
        self.calls.append((url, kwargs))
        owner = self

        class Response:
            content_length = None

            @property
            def content(self):
                return self

            async def __aenter__(self):
                if owner.error:
                    raise owner.error
                return self

            async def __aexit__(self, *args):
                return False

            async def iter_chunked(self, size):
                owner.entered.set()
                try:
                    if owner.block:
                        await asyncio.Event().wait()
                    yield SECRET.encode()
                except asyncio.CancelledError:
                    owner.cancelled = True
                    raise

        return Response()

    async def close(self):
        self.closed = True


def messages(caplog):
    return [r.getMessage() for r in caplog.records if "RELAYPHASE " in r.getMessage()]


async def test_success_unix_chain_keeps_request_response_and_local_identity(relay, caplog):
    session = Session()
    relay._session = session
    reader = asyncio.StreamReader()
    reader.feed_data(SECRET.encode() + b"\n")
    writer = Writer()
    await relay._handle(reader, writer)
    assert writer.data == SECRET.encode() + b"\n"
    assert writer.closed and not relay._tasks
    assert session.calls[0][1]["data"] == SECRET.encode() + b"\n"
    assert session.calls[0][1]["headers"] == {"Content-Type": "application/json"}
    assert session.calls[0][1]["allow_redirects"] is False
    assert session.calls[0][1]["timeout"].total == 2
    rows = messages(caplog)
    assert [r.split()[1] for r in rows] == [
        "phase=unix_callback",
        "phase=frame_complete",
        "phase=forward_entry",
        "phase=body_begin",
        "phase=body_complete",
        "phase=drain_complete",
    ]
    assert len({r.split()[2] for r in rows}) == 1
    assert "wire" not in "\n".join(rows)
    assert SECRET not in caplog.text
    assert module._relay_phase_context.get() is None


@pytest.mark.parametrize("enabled", [None, "0", "true"])
async def test_default_and_non_one_flags_disable_all_phase_logging(
    relay, caplog, monkeypatch, enabled
):
    if enabled is None:
        monkeypatch.delenv("TERLIMO_SERVICE_RELAY_PHASE_PROBE")
    else:
        monkeypatch.setenv("TERLIMO_SERVICE_RELAY_PHASE_PROBE", enabled)
    off = module.ServiceRelay(relay._settings)
    off._session = Session()
    reader = asyncio.StreamReader()
    reader.feed_data(b"bounded\n")
    await off._handle(reader, Writer())
    assert not messages(caplog)
    assert module._relay_phase_context.get() is None


@pytest.mark.parametrize("error", [ClientError(SECRET), TimeoutError(SECRET), RuntimeError(SECRET)])
async def test_forward_preserves_original_error_contract(relay, caplog, error):
    relay._session = Session(error=error)
    token = module._relay_phase_context.set((42, time.monotonic()))
    try:
        if isinstance(error, RuntimeError):
            with pytest.raises(RuntimeError) as caught:
                await relay._forward(SECRET.encode())
            assert caught.value is error
        else:
            assert await relay._forward(SECRET.encode()) is None
            assert any("phase=forward_error" in r for r in messages(caplog))
    finally:
        module._relay_phase_context.reset(token)
    assert SECRET not in caplog.text


async def test_handle_cancellation_still_cancels_body_and_closes_socket(relay, caplog):
    session = Session(block=True)
    relay._session = session
    reader = asyncio.StreamReader()
    reader.feed_data(b"bounded\n")
    writer = Writer()
    handle = asyncio.create_task(relay._handle(reader, writer))
    await asyncio.wait_for(session.entered.wait(), 1)
    handle.cancel()
    with pytest.raises(asyncio.CancelledError):
        await handle
    assert session.cancelled and writer.closed and not relay._tasks
    assert any("outcome=cancelled" in r for r in messages(caplog))
    assert not any("phase=drain_complete" in r for r in messages(caplog))
    assert SECRET not in caplog.text
    assert module._relay_phase_context.get() is None


async def test_body_cap_keeps_original_rejection(relay, monkeypatch, caplog):
    relay._session = Session()
    monkeypatch.setattr(module, "SERVICE_MAX_FRAME", 1)
    token = module._relay_phase_context.set((42, time.monotonic()))
    try:
        assert await relay._forward(SECRET.encode()) is None
    finally:
        module._relay_phase_context.reset(token)
    assert any("phase=body_complete" in r and "outcome=error" in r for r in messages(caplog))
    assert SECRET not in caplog.text


async def test_installed_aiohttp_callbacks_ignore_params_and_preserve_cancellation(relay, caplog):
    trace = relay._phase_trace()
    signals = [
        trace.on_connection_queued_start,
        trace.on_connection_queued_end,
        trace.on_dns_resolvehost_start,
        trace.on_dns_resolvehost_end,
        trace.on_connection_create_start,
        trace.on_connection_create_end,
        trace.on_connection_reuseconn,
        trace.on_request_headers_sent,
        trace.on_request_end,
        trace.on_request_exception,
    ]

    class Params:
        def __str__(self):
            raise AssertionError("trace params must not be formatted")

    token = module._relay_phase_context.set((42, time.monotonic()))
    try:
        for signal in signals:
            assert len(signal) == 1
            await signal[0](SECRET, SECRET, Params())
        pending = asyncio.create_task(trace.on_request_end[0](SECRET, SECRET, Params()))
        pending.cancel()
        with pytest.raises(asyncio.CancelledError):
            await pending
    finally:
        module._relay_phase_context.reset(token)
    assert len(messages(caplog)) == 10
    assert SECRET not in caplog.text


@pytest.mark.parametrize("enabled", [False, True])
async def test_start_registers_trace_only_when_enabled_and_keeps_session(
    relay, monkeypatch, tmp_path, enabled
):
    relay._phase_probe_enabled = enabled
    directory = tmp_path / "private"
    directory.mkdir(mode=0o700)
    relay._settings.service_relay_socket = str(directory / "relay.sock")
    context_calls = []

    def context():
        context_calls.append(True)
        return False

    monkeypatch.setattr(relay, "_backend_context", context)
    await relay.start()
    session = relay._session
    try:
        assert len(session.trace_configs) == int(enabled)
        if enabled:
            assert len(session.trace_configs[0].on_request_headers_sent) == 1
        assert relay._session is session and len(context_calls) == 1
    finally:
        await relay.stop()
    assert session.closed
    assert not (directory / "relay.sock").exists()


async def test_probe_does_not_bypass_peer_or_request_cap(relay, caplog, monkeypatch):
    session = Session()
    relay._session = session
    reader = asyncio.StreamReader()
    reader.feed_data(b"bounded\n")
    monkeypatch.setattr(module, "_peer_credentials", lambda writer: (999, 456))
    await relay._handle(reader, Writer())
    assert not session.calls
    assert not any("phase=forward_entry" in r for r in messages(caplog))
    monkeypatch.setattr(module, "_peer_credentials", lambda writer: (123, 456))
    monkeypatch.setattr(module, "SERVICE_MAX_FRAME", 1)
    await relay._handle(reader, Writer())
    assert not session.calls
    assert module._relay_phase_context.get() is None


async def test_real_local_session_emits_connect_queue_headers_body_and_reuse(relay, caplog):
    entered = asyncio.Event()
    release = asyncio.Event()
    queued = asyncio.Event()

    async def serve(request):
        await request.read()
        entered.set()
        await release.wait()
        return web.Response(body=SECRET.encode())

    app = web.Application()
    app.router.add_post("/" + SECRET, serve)
    runner = web.AppRunner(app, access_log=None)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    port = site._server.sockets[0].getsockname()[1]
    trace = relay._phase_trace()

    async def notice_queue(*args):
        queued.set()

    trace.on_connection_queued_start.append(notice_queue)
    token = module._relay_phase_context.set((42, time.monotonic()))
    tasks = []
    try:
        async with ClientSession(connector=TCPConnector(limit=1), trace_configs=[trace]) as session:

            async def call():
                async with session.post(
                    f"http://localhost:{port}/{SECRET}",
                    data=SECRET.encode(),
                    headers={"X-Sentinel": SECRET},
                ) as response:
                    assert await response.read() == SECRET.encode()

            tasks.append(asyncio.create_task(call()))
            await asyncio.wait_for(entered.wait(), 2)
            tasks.append(asyncio.create_task(call()))
            await asyncio.wait_for(queued.wait(), 2)
            release.set()
            await asyncio.gather(*tasks)
        phases = {r.split()[1] for r in messages(caplog)}
        assert {
            "phase=connect_begin",
            "phase=connect_end",
            "phase=dns_begin",
            "phase=dns_end",
            "phase=queue_begin",
            "phase=queue_end",
            "phase=connection_reuse",
            "phase=headers_sent",
            "phase=response_headers",
        } <= phases
        assert SECRET not in caplog.text
    finally:
        for task in tasks:
            if not task.done():
                task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        module._relay_phase_context.reset(token)
        await runner.cleanup()
