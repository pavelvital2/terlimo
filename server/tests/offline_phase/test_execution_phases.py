"""Only artificial clocks, in-memory DB/HTTP/session doubles; no sockets or backend."""
import asyncio
import json
import logging
from contextlib import asynccontextmanager
from datetime import UTC, datetime, timedelta
from types import SimpleNamespace

import pytest
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ec

from terlimo_backend import auth_api, auth_api_phase as diagnostic, pop, service_relay


class Sink:
    def __init__(self, fail=False):
        self.lines = []
        self.fail = fail
    def info(self, template, *values):
        if self.fail:
            raise OSError("synthetic sink failure")
        self.lines.append(template % values)


def records(sink):
    return [json.loads(line.split(" records=", 1)[1]) for line in sink.lines]


@pytest.mark.parametrize("cpu,queue,available", [
    (0, 0, True), (2_000_000_000, 0, True),
    (0, 1_500_000_000, True), (0, 0, False),
])
def test_fake_two_second_states_are_distinct(monkeypatch, cpu, queue, available):
    clock = {"wall": 100, "cpu": 10, "queue": 20}
    monkeypatch.setattr(diagnostic.time, "monotonic_ns", lambda: clock["wall"])
    monkeypatch.setattr(diagnostic.time, "thread_time_ns", lambda: clock["cpu"])
    monkeypatch.setattr(diagnostic.threading, "get_native_id", lambda: 12)
    monkeypatch.setattr(diagnostic, "_schedstat", lambda: (100, clock["queue"], 1) if available else None)
    sink = Sink();phase = diagnostic.AuthApiPhase("session", sink=sink)
    phase.activate("a" * 32);phase.mark("keyload_begin")
    clock["wall"] += 2_000_000_000;clock["cpu"] += cpu;clock["queue"] += queue
    phase.mark("keyload_end");phase.finish()
    start, end = records(sink)[0]
    assert end["wall_ns"] - start["wall_ns"] == 2_000_000_000
    assert end["thread_cpu_ns"] - start["thread_cpu_ns"] == cpu
    assert end["sched_state"] == ("AVAILABLE" if available else "UNKNOWN")
    if available:
        assert end["sched_runqueue_wait_ns"] - start["sched_runqueue_wait_ns"] == queue
    else:
        assert end["sched_runqueue_wait_ns"] is None
    assert not any("io_wait" in k for k in end)  # Never invent wait = wall-CPU-runqueue.


def test_changed_tid_is_unknown(monkeypatch):
    tid = [1];monkeypatch.setattr(diagnostic.threading, "get_native_id", lambda: tid[0])
    phase = diagnostic.AuthApiPhase("session");phase.activate("a" * 32)
    tid[0] = 2;phase.mark("keyload_begin")
    assert phase.records[0]["tid_state"] == "UNKNOWN_CHANGED_TID"
    assert phase.records[0]["thread_cpu_ns"] is None
    assert phase.records[0]["sched_runtime_ns"] is None


@pytest.mark.parametrize("data", ["0 0 0", "broken", "1 2", "9 " * 70])
def test_missing_or_disabled_schedstat_unknown(monkeypatch, data):
    import io
    monkeypatch.setattr(diagnostic, "open", lambda *a, **k: io.StringIO(data), raising=False)
    assert diagnostic._schedstat() is None


def test_unreadable_schedstat_unknown(monkeypatch):
    def fail(*a, **k):raise PermissionError
    monkeypatch.setattr(diagnostic, "open", fail, raising=False)
    assert diagnostic._schedstat() is None


@pytest.mark.asyncio
async def test_off_fastpath_has_no_clock_proc_or_logs(monkeypatch):
    monkeypatch.delenv(diagnostic.FLAG, raising=False)
    def forbidden(*a, **k):raise AssertionError("OFF performed diagnostic work")
    monkeypatch.setattr(diagnostic.time, "monotonic_ns", forbidden)
    monkeypatch.setattr(diagnostic.time, "thread_time_ns", forbidden)
    monkeypatch.setattr(diagnostic, "_schedstat", forbidden)
    monkeypatch.setattr(diagnostic.threading, "get_native_id", forbidden)
    monkeypatch.setattr(diagnostic.logger, "info", forbidden)
    assert diagnostic.new_auth_api_phase("session") is None
    relay = object.__new__(service_relay.ServiceRelay)
    relay._phase_probe_enabled = False
    relay._phase("forward_entry")


def test_bounded_truncation_no_reads_after_cap(monkeypatch):
    calls = []
    monkeypatch.setattr(diagnostic, "_schedstat", lambda: calls.append(1) or (1, 2, 3))
    sink = Sink();phase = diagnostic.AuthApiPhase("session", sink=sink);phase.activate("a" * 32)
    for _ in range(100):phase.mark("keyload_begin")
    phase.finish();phase.finish()
    assert len(calls) == 63
    assert len(records(sink)[0]) == 64
    assert records(sink)[0][-1] == {"seq": 63, "phase": "truncated", "omitted_records": 37}
    assert len(sink.lines) == 1


class FakeConnection:
    def __init__(self, challenge=None, spki=None):
        self.challenge = challenge;self.spki = spki;self.events = []
    async def fetchrow(self, sql, *values):
        self.events.append("fetchrow");await asyncio.sleep(0)
        if "auth_challenges" in sql:return self.challenge
        if "public_key_spki_b64" in sql:return {"public_key_spki_b64": self.spki}
        return {"id": "synthetic-installation", "state": "active", "environment": "test"}
    async def fetchval(self, sql, *values):
        self.events.append("fetchval");await asyncio.sleep(0);return 1
    async def execute(self, sql, *values):
        self.events.append("execute");await asyncio.sleep(0)
    @asynccontextmanager
    async def transaction(self):
        self.events.append("transaction_enter");await asyncio.sleep(0)
        try:yield
        finally:self.events.append("transaction_exit")


class FakeDB:
    def __init__(self, connection):self.connection = connection;self.active = 0
    @asynccontextmanager
    async def acquire(self):
        await asyncio.sleep(0);self.active += 1
        try:yield self.connection
        finally:self.active -= 1;await asyncio.sleep(0)


def session_fixture(monkeypatch, rid="a" * 32):
    now = datetime(2026, 10, 2, tzinfo=UTC);monkeypatch.setattr(auth_api, "now_utc", lambda: now)
    private = ec.generate_private_key(ec.SECP256R1())
    spki = pop.b64url_encode(private.public_key().public_bytes(serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo))
    fingerprint = pop.installation_fingerprint(spki);nonce = pop.b64url_encode(b"FAKE_NONCE_16B!!")
    challenge = {"used_at": None, "purpose": "session", "op": "auth.session", "environment": "test", "nonce_b64": nonce, "expires_at": now + timedelta(seconds=30), "installation_fingerprint": fingerprint, "challenge_id": "b" * 32}
    payload = {"env": "test", "scope": "session", "op": "auth.session", "installation_id": fingerprint, "request_id": rid, "nonce": nonce, "ts": now.isoformat().replace("+00:00", "Z"), "requested_scopes": ["session:read"], "idempotency_key": "synthetic-idempotency-only"}
    raw = pop.canonical_json(payload)
    proof = {"algorithm": "ES256", "request_id": rid, "challenge_id": "b" * 32, "nonce_b64": nonce, "environment": "test", "payload_hash": pop.sha256_hex(raw), "signed_payload_b64": pop.b64url_encode(raw), "signature_b64": pop.sign(private, pop.pop_message(rid, "b" * 32, nonce, raw))}
    db = FakeDB(FakeConnection(challenge, spki));service = auth_api.InstallationSessionService(SimpleNamespace(proof_skew_seconds=30), db)
    async def no_link(*a):await asyncio.sleep(0);return None
    async def consume(*a):await asyncio.sleep(0)
    async def apply(*a, **k):await asyncio.sleep(0);return {"session": {"token": "NEVER_LOG_SYNTHETIC_TOKEN"}}, "created"
    monkeypatch.setattr(service, "_resolve_trusted_link", no_link)
    monkeypatch.setattr(service, "_consume_challenge", consume)
    monkeypatch.setattr(service, "_idempotent_apply", apply)
    return service, {"proof": proof}, db, spki


@pytest.mark.asyncio
async def test_session_path_phase_pairs_and_off_parity_payload_free(monkeypatch):
    service, body, db, spki = session_fixture(monkeypatch)
    off = await service.create_session(body)
    sink = Sink();phase = diagnostic.AuthApiPhase("session", sink=sink)
    on = await service.create_session(body, phase=phase);phase.finish()
    assert on == off and db.active == 0
    names = [r["phase"] for r in records(sink)[0]]
    for a, b in [("verify_challenge_fetch_begin", "verify_challenge_fetch_end"),
                 ("payload_preview_begin", "payload_preview_end"),
                 ("key_fetch_begin", "key_fetch_end"), ("keyload_begin", "keyload_end"),
                 ("proof_verify_begin", "proof_verify_end"),
                 ("session_transaction_begin", "session_transaction_end"),
                 ("session_apply_begin", "session_apply_end")]:
        assert names.index(a) < names.index(b)
    assert names.index("key_fetch_end") < names.index("key_pool_release_end") < names.index("keyload_begin")
    text = "".join(sink.lines)
    for forbidden in [spki, body["proof"]["signature_b64"], body["proof"]["signed_payload_b64"], "NEVER_LOG_SYNTHETIC_TOKEN", "SELECT", "synthetic-installation"]:
        assert forbidden not in text


@pytest.mark.asyncio
async def test_challenge_pairs(monkeypatch):
    sink = Sink();phase = diagnostic.AuthApiPhase("challenge", sink=sink);db = FakeDB(FakeConnection())
    settings = SimpleNamespace(challenge_ttl_seconds=30, challenge_rate_limit_per_minute=9, public_challenge_limit_per_minute=9)
    service = auth_api.InstallationSessionService(settings, db)
    result = await service.create_challenge({"installation_fingerprint": "a" * 64, "purpose": "session", "environment": "test"}, phase=phase)
    phase.finish();assert result["status"] == "ok" and db.active == 0
    names = [r["phase"] for r in records(sink)[0]]
    assert names.index("challenge_pool_acquire_begin") < names.index("challenge_pool_acquire_end")
    assert names.index("challenge_insert_begin") < names.index("challenge_insert_end") < names.index("challenge_pool_release_end")


@pytest.mark.asyncio
async def test_concurrent_rids_and_relay_task_context_do_not_mix(monkeypatch):
    sink = Sink()
    async def request(rid, task_id):
        p = diagnostic.AuthApiPhase("session", task_id=task_id, sink=sink);p.activate(rid)
        local = diagnostic.AuthApiPhase("relay", task_id=task_id, sink=sink)
        token = service_relay._relay_phase_context.set(local)
        relay = object.__new__(service_relay.ServiceRelay);relay._phase_probe_enabled = True
        try:
            p.mark("keyload_begin");relay._phase("frame_complete")
            await asyncio.sleep(0)
            p.mark("keyload_end");relay._phase("forward_entry")
            local.finish();p.finish()
        finally:service_relay._relay_phase_context.reset(token)
    await asyncio.gather(request("a" * 32, 11), request("b" * 32, 22))
    assert len(sink.lines) == 4
    for rid in ["a" * 32, "b" * 32]:
        line = next(s for s in sink.lines if "request_id=" + rid in s)
        assert len(json.loads(line.split(" records=", 1)[1])) == 2
        assert ("b" * 32 if rid == "a" * 32 else "a" * 32) not in line
    assert all("request_id=UNKNOWN" in s for s in sink.lines if s.startswith("RELAYPHASE"))
    assert service_relay._relay_phase_context.get() is None


@pytest.mark.asyncio
@pytest.mark.parametrize("failure", ["cancel", "reject", "unavailable"])
async def test_route_exceptions_cancel_and_diagnostic_sink_failures_unchanged(monkeypatch, failure):
    service, body, db, _ = session_fixture(monkeypatch)
    class Request:
        headers = {"Content-Type": "application/json"}
        async def read(self):return json.dumps(body).encode()
        app = {auth_api.AUTH_SERVICE_KEY: service}
        async def json(self):return body
    monkeypatch.setattr(auth_api, "random_hex", lambda n: "c" * 32)
    async def fail(*a, **k):
        if failure == "cancel":raise asyncio.CancelledError
        if failure == "reject":raise auth_api.ApiError("PROOF_INVALID")
        raise OSError("synthetic failure")
    monkeypatch.setattr(service, "create_session", fail)
    monkeypatch.setenv(diagnostic.FLAG, "1")
    monkeypatch.setattr(diagnostic.logger, "info", Sink(fail=True).info)
    if failure == "cancel":
        with pytest.raises(asyncio.CancelledError):await auth_api._handle_session(Request())
        monkeypatch.delenv(diagnostic.FLAG)
        with pytest.raises(asyncio.CancelledError):await auth_api._handle_session(Request())
    else:
        on = await auth_api._handle_session(Request());monkeypatch.delenv(diagnostic.FLAG)
        off = await auth_api._handle_session(Request())
        assert (on.status, on.body) == (off.status, off.body)
    assert db.active == 0


@pytest.mark.asyncio
async def test_actual_verification_failure_keeps_error_and_releases_pool(monkeypatch):
    service, body, db, _ = session_fixture(monkeypatch)
    body["proof"]["signature_b64"] = pop.b64url_encode(b"BAD_SYNTHETIC_SIGNATURE")
    for phase in [None, diagnostic.AuthApiPhase("session", sink=Sink(fail=True))]:
        with pytest.raises(auth_api.ApiError) as exc:
            await service.create_session(body, phase=phase)
        assert exc.value.code == "PROOF_INVALID"
        assert db.active == 0
        if phase:phase.finish()

@pytest.mark.asyncio
@pytest.mark.parametrize("route", ["challenge", "session"])
@pytest.mark.parametrize("failure", [None, "cancel", "unexpected"])
async def test_outer_service_phase_pairs_and_cleanup(monkeypatch, route, failure):
    sink = Sink();monkeypatch.setattr(diagnostic.logger, "info", sink.info)
    db = FakeDB(FakeConnection({}, ""))
    settings = SimpleNamespace(service_max_concurrency=2, service_timeout_seconds=5, environment="test")
    parsed = {"request_id": "a" * 32, "path": "/api/mobile/v1/auth/" + route}
    async def read(*a):return b"SYNTHETIC_BODY_MUST_NOT_LOG"
    async def resolve(*a):await asyncio.sleep(0)
    async def replay(*a):
        await asyncio.sleep(0)
        if failure == "cancel":raise asyncio.CancelledError
        if failure == "unexpected":raise RuntimeError("synthetic failure")
        return {"status": "ok"}
    monkeypatch.setattr(service_relay, "_read_request_capped", read)
    monkeypatch.setattr(service_relay, "_parse_service_request", lambda raw: parsed)
    monkeypatch.setattr(service_relay, "_peer_identities", lambda obj: [])
    monkeypatch.setattr(service_relay, "resolve_gateway_context", resolve)
    monkeypatch.setattr(service_relay, "_replay_service_request", replay)
    class Request:
        transport = SimpleNamespace(get_extra_info=lambda key: None)
        app = {service_relay.SERVICE_ACTIVE_KEY: [0]}
    for enabled in [False, True]:
        monkeypatch.setenv(diagnostic.FLAG, "1" if enabled else "0")
        if failure:
            with pytest.raises(asyncio.CancelledError if failure == "cancel" else RuntimeError):
                await service_relay._service_handler(settings, db)(Request())
        else:
            response = await service_relay._service_handler(settings, db)(Request())
            assert response.status == 200 and json.loads(response.body) == {"status": "ok"}
        assert db.active == 0 and Request.app[service_relay.SERVICE_ACTIVE_KEY] == [0]
    assert len(sink.lines) == 1 and "SYNTHETIC_BODY" not in sink.lines[0]
    names = [r["phase"] for r in records(sink)[0]]
    for name in ["context_pool_acquire", "resolve_gateway_context"]:
        assert names.index(name + "_begin") < names.index(name + "_end")
    assert "context_pool_release_end" in names
    if failure is None:assert names.index("replay_begin") < names.index("replay_end") < names.index("response_ready")


@pytest.mark.asyncio
@pytest.mark.parametrize("failure", [None, "cancel", "forward_error"])
async def test_real_relay_handle_trace_and_cleanup_parity(monkeypatch, failure):
    sink = Sink();monkeypatch.setattr(service_relay.logger, "info", sink.info)
    monkeypatch.setattr(service_relay, "_peer_credentials", lambda writer: (1, 1))
    class Reader:
        async def readuntil(self, separator):return b"SYNTHETIC_FRAME_MUST_NOT_LOG\n"
        async def read(self, n):await asyncio.Event().wait()
    class Writer:
        def __init__(self):self.closed=False;self.data=[]
        def write(self, data):self.data.append(data)
        async def drain(self):await asyncio.sleep(0)
        def close(self):self.closed=True
        async def wait_closed(self):await asyncio.sleep(0)
    outputs=[]
    for enabled in [False, True]:
        relay = object.__new__(service_relay.ServiceRelay)
        relay._phase_probe_enabled=enabled;relay._tasks=set()
        relay._settings=SimpleNamespace(service_timeout_seconds=5, service_relay_allowed_uid=1, service_relay_allowed_gid=1, service_max_concurrency=2)
        async def forward(data):
            relay._phase("forward_entry")
            trace=relay._phase_trace()
            for signal in [trace.on_connection_create_start, trace.on_connection_create_end, trace.on_request_headers_sent, trace.on_request_end]:
                for callback in signal:await callback(None, None, None)
            if failure == "cancel":raise asyncio.CancelledError
            if failure == "forward_error":raise OSError("synthetic")
            return b'{"status":"ok"}'
        relay._forward=forward;writer=Writer()
        await relay._handle(Reader(), writer)
        assert writer.closed and not relay._tasks and service_relay._relay_phase_context.get() is None
        outputs.append(writer.data)
    assert outputs[0] == outputs[1]
    assert len(sink.lines) == 1 and "SYNTHETIC_FRAME" not in sink.lines[0]
    names=[r["phase"] for r in records(sink)[0]]
    assert names[:3] == ["unix_callback", "frame_complete", "forward_entry"]
    assert names.index("connect_begin") < names.index("connect_end") < names.index("headers_sent") < names.index("response_headers")


@pytest.mark.asyncio
async def test_parent_relay_cancel_releases_children_and_context(monkeypatch):
    monkeypatch.setattr(service_relay.logger, "info", Sink(fail=True).info)
    monkeypatch.setattr(service_relay, "_peer_credentials", lambda writer: (1, 1))
    relay=object.__new__(service_relay.ServiceRelay);relay._phase_probe_enabled=True;relay._tasks=set()
    relay._settings=SimpleNamespace(service_timeout_seconds=5, service_relay_allowed_uid=1, service_relay_allowed_gid=1, service_max_concurrency=2)
    entered=asyncio.Event();released=asyncio.Event()
    async def forward(data):
        entered.set()
        try:await asyncio.Event().wait()
        finally:released.set()
    relay._forward=forward
    class Reader:
        async def readuntil(self, sep):return b"synthetic\n"
        async def read(self, n):await asyncio.Event().wait()
    class Writer:
        closed=False
        def close(self):self.closed=True
        async def wait_closed(self):await asyncio.sleep(0)
    writer=Writer();task=asyncio.create_task(relay._handle(Reader(),writer));await entered.wait();task.cancel()
    with pytest.raises(asyncio.CancelledError):await task
    assert released.is_set() and writer.closed and not relay._tasks
    assert service_relay._relay_phase_context.get() is None

@pytest.mark.asyncio
@pytest.mark.parametrize("cpu_delta,queue_delta,available", [(0,0,True),(2_000_000_000,0,True),(0,1_500_000_000,True),(0,0,False)])
async def test_fake_await_two_seconds_execution_state(monkeypatch, cpu_delta, queue_delta, available):
    clock={"wall":1,"cpu":100,"queue":100}
    monkeypatch.setattr(diagnostic.time,"monotonic_ns",lambda:clock["wall"])
    monkeypatch.setattr(diagnostic.time,"thread_time_ns",lambda:clock["cpu"])
    monkeypatch.setattr(diagnostic.threading,"get_native_id",lambda:123)
    monkeypatch.setattr(diagnostic,"_schedstat",lambda:(100,clock["queue"],1) if available else None)
    sink=Sink();phase=diagnostic.AuthApiPhase("session",sink=sink);phase.activate("a"*32)
    phase.mark("fake_await_begin")
    await asyncio.sleep(0)
    clock["wall"]+=2_000_000_000;clock["cpu"]+=cpu_delta;clock["queue"]+=queue_delta
    phase.mark("fake_await_end");phase.finish()
    begin,end=records(sink)[0]
    assert end["wall_ns"]-begin["wall_ns"]==2_000_000_000
    assert end["thread_cpu_ns"]-begin["thread_cpu_ns"]==cpu_delta
    if available:assert end["sched_runqueue_wait_ns"]-begin["sched_runqueue_wait_ns"]==queue_delta
    else:assert end["sched_state"]=="UNKNOWN" and end["sched_runqueue_wait_ns"] is None


def test_unavailable_thread_cpu_and_unvalidated_rid(monkeypatch):
    sink=Sink();phase=diagnostic.AuthApiPhase("session",sink=sink)
    phase.activate("invalid-untrusted-input")
    phase.mark("handler_entry");phase.finish()
    assert not sink.lines
    phase=diagnostic.AuthApiPhase("session",sink=sink);phase.activate("a"*32)
    def unavailable():raise OSError("synthetic")
    monkeypatch.setattr(diagnostic.time,"thread_time_ns",unavailable)
    phase.mark("keyload_begin");phase.finish()
    assert records(sink)[0][0]["thread_cpu_state"] == "UNKNOWN"
    assert records(sink)[0][0]["thread_cpu_ns"] is None


@pytest.mark.asyncio
async def test_challenge_cancel_cleans_pool_and_diagnostic_sink_failure(monkeypatch):
    class Connection(FakeConnection):
        async def execute(self,*a):raise asyncio.CancelledError
    service=auth_api.InstallationSessionService(SimpleNamespace(challenge_ttl_seconds=30,challenge_rate_limit_per_minute=9,public_challenge_limit_per_minute=9),FakeDB(Connection()))
    # Use the same accepted challenge body as the success fixture.
    body={"purpose":"session", "environment":"test", "installation_fingerprint":"a"*64}
    phase=diagnostic.AuthApiPhase("challenge",sink=Sink(fail=True))
    with pytest.raises(asyncio.CancelledError):
        try:await service.create_challenge(body,phase=phase)
        finally:phase.finish()
    assert service._db.active == 0
