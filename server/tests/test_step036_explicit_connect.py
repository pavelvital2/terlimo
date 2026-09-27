"""Explicit connect/start contract: public PoP intent + strict start RPC + atomic replay.

Real HTTP (TestClient) for the public route, real mTLS listener for the bootstrap start RPC,
ephemeral certs and the bundled TEST PostgreSQL. The client signs with the accepted contract
module (test_auth_flow helper); the backend verifies with the ported implementation.
"""
from __future__ import annotations

import asyncio
import contextlib
import contextvars
import hashlib
import json
import secrets
import shutil
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path

import asyncpg
import pytest
from aiohttp import ClientSession, TCPConnector
from aiohttp.test_utils import TestClient, TestServer
from cryptography.fernet import Fernet
from test_auth_flow import (
    MOBILE,
    KeyMaterial,
    PoPClient,
    _challenge,
    _enroll,
    _session,
    contract_pop,
)
from test_step036_evidence_transport import _Material, _settings
from test_step036_onboarding_hour_storage import (
    FakeBootstrapClient,
    FakeCipher,
    _connect,
    _counts,
    _seed_gateway,
    _seed_installation,
)

from terlimo_backend import pop as backend_pop
from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.evidence_transport import EVIDENCE_LISTENER_KEY, EVIDENCE_PATH
from terlimo_backend.migrations import runner
from terlimo_backend.onboarding_hour import (
    FernetSecretCipher,
    GatewayContext,
    OnboardingError,
    OnboardingHourHandlers,
    admit_evidence,
    create_intent,
    public_intent,
)

GATEWAY_KEY = "terlimo-035-node"
BACKGROUND_BODY = {"v": 1, "op": "background.keepalive"}


async def _start_stack(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    settings = _settings(
        settings_factory,
        migrated_url,
        material,
        onboarding_secret_key=Fernet.generate_key().decode(),
    )
    database = Database(settings)
    app = create_app(settings, database)
    client = TestClient(TestServer(app))
    await client.start_server()
    return client, settings, material


async def _enrolled_session(client, scopes=("session:read", "session:write")):
    key = KeyMaterial()
    pop_client = PoPClient(key)
    enrollment = await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))
    assert enrollment.status == 200, await enrollment.text()
    session = await _session(
        client,
        pop_client,
        await _challenge(client, key, "session"),
        list(scopes),
        f"idem-{secrets.token_hex(8)}",
    )
    assert session.status == 200, await session.text()
    token = (await session.json())["session"]["session_id"]
    return key, pop_client, token


async def _post_intent(client, pop_client, token, request_key, gateway_key=None):
    challenge = await _challenge(client, pop_client.key, "onboarding-start-intent")
    extra = {"request_key": request_key}
    if gateway_key is not None:
        extra["gateway_key"] = gateway_key
    proof, _ = pop_client.proof(
        challenge,
        op="onboarding.intent",
        scope="onboarding:start",
        extra=extra,
    )
    return await client.post(
        f"{MOBILE}/onboarding/intents",
        json={"proof": proof},
        headers={"Authorization": f"Bearer {token}"},
    )


async def _provision(url, intent_id, secret_key):
    connection = await _connect(url)
    try:
        operation = await connection.fetchrow(
            "SELECT * FROM outbox_operations WHERE payload->>'intent_id' = $1", intent_id
        )
        handlers = OnboardingHourHandlers(
            cipher=FernetSecretCipher(secret_key),
            client_factory=lambda _key, _endpoints: FakeBootstrapClient(),
        )
        await handlers.bootstrap_provision(connection, operation)
    finally:
        await connection.close()


def _start_envelope(
    credential_id,
    pop_client,
    *,
    intent_id,
    request_key,
    challenge,
    connection_id="c" * 32,
    ts=None,
):
    request_id = secrets.token_hex(16)
    payload = {
        "env": pop_client.environment,
        "scope": "onboarding:start",
        "op": "onboarding.start",
        "installation_id": pop_client.key.fingerprint,
        "intent_id": intent_id,
        "request_key": request_key,
        "ts": ts or datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "nonce": challenge["nonce_b64"],
        "request_id": request_id,
    }
    payload_bytes = contract_pop.canonical_json(payload)
    message = contract_pop.pop_message(
        request_id, challenge["challenge_id"], challenge["nonce_b64"], payload_bytes
    )
    proof = {
        "algorithm": "ES256",
        "environment": pop_client.environment,
        "request_id": request_id,
        "challenge_id": challenge["challenge_id"],
        "nonce_b64": challenge["nonce_b64"],
        "payload_hash": hashlib.sha256(payload_bytes).hexdigest(),
        "signed_payload_b64": contract_pop.b64url_encode(payload_bytes),
        "signature_b64": contract_pop.sign(pop_client.key.private, message),
    }
    return {
        "credential_id": credential_id,
        "connection_id": connection_id,
        "request_id": "e" * 32,
        "body": {
            "v": 1,
            "op": "onboarding.start",
            "intent_id": intent_id,
            "request_key": request_key,
            "proof": proof,
        },
    }


async def _post_start(session, port, envelope):
    async with session.post(
        f"https://localhost:{port}{EVIDENCE_PATH}",
        data=json.dumps(envelope).encode() + b"\n",
    ) as response:
        return response.status, await response.json()


async def _ready_intent(client, pop_client, token, url, secret_key, request_key="connect-1"):
    created = await _post_intent(client, pop_client, token, request_key)
    assert created.status == 200, await created.text()
    pending = await created.json()
    assert pending["state"] == "pending"
    await _provision(url, pending["intent_id"], secret_key)
    ready = await _post_intent(client, pop_client, token, request_key)
    assert ready.status == 200, await ready.text()
    body = await ready.json()
    assert body["state"] == "ready"
    return pending["intent_id"], body


async def _expire_challenge(url, challenge_id):
    connection = await _connect(url)
    try:
        await connection.execute(
            """
            UPDATE auth_challenges SET expires_at = now() - interval '1 second'
            WHERE challenge_id = $1 AND used_at IS NULL AND superseded_at IS NULL
            """,
            challenge_id,
        )
    finally:
        await connection.close()


async def test_public_intent_never_starts_and_ready_challenge_is_single(
    migrated_url, settings_factory, tmp_path
):
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    client, settings, _material = await _start_stack(migrated_url, settings_factory, tmp_path)
    try:
        _key, pop_client, token = await _enrolled_session(client)
        created = await _post_intent(client, pop_client, token, "connect-poll")
        assert created.status == 200, await created.text()
        pending = await created.json()
        assert pending["state"] == "pending" and pending["status"] == "pending"
        assert pending["gateway"]["node_id"] == GATEWAY_KEY
        assert "bootstrap" not in pending and "start_challenge" not in pending
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0

        await _provision(migrated_url, pending["intent_id"], settings.onboarding_secret_key)
        ready = await (await _post_intent(client, pop_client, token, "connect-poll")).json()
        assert ready["status"] == "ready" and ready["state"] == "ready"
        assert ready["bootstrap"]["credential_id"] == ready["credential_id"]
        assert len(ready["bootstrap"]["secret"]) >= 32 and "retry_after" not in ready
        first_challenge = ready["start_challenge"]
        assert first_challenge["expires_at"]

        again = await (await _post_intent(client, pop_client, token, "connect-poll")).json()
        assert again["start_challenge"]["challenge_id"] == first_challenge["challenge_id"]
        assert again["start_challenge"]["nonce_b64"] == first_challenge["nonce_b64"]

        await _expire_challenge(migrated_url, first_challenge["challenge_id"])
        refreshed = await (await _post_intent(client, pop_client, token, "connect-poll")).json()
        assert refreshed["start_challenge"]["challenge_id"] != first_challenge["challenge_id"]
        connection = await _connect(migrated_url)
        try:
            superseded = await connection.fetchval(
                "SELECT superseded_at FROM auth_challenges WHERE challenge_id = $1",
                first_challenge["challenge_id"],
            )
            assert superseded is not None
        finally:
            await connection.close()
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0
    finally:
        await client.close()


async def test_start_once_3600_and_durable_historical_replay(
    migrated_url, settings_factory, tmp_path
):
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    client, settings, material = await _start_stack(migrated_url, settings_factory, tmp_path)
    session = ClientSession(connector=TCPConnector(ssl=material.node_context()))
    try:
        _key, pop_client, token = await _enrolled_session(client)
        intent_id, ready = await _ready_intent(
            client, pop_client, token, migrated_url, settings.onboarding_secret_key
        )
        port = client.server.app[EVIDENCE_LISTENER_KEY].port
        envelope = _start_envelope(
            ready["credential_id"],
            pop_client,
            intent_id=intent_id,
            request_key="connect-1",
            challenge=ready["start_challenge"],
        )
        status, body = await _post_start(session, port, envelope)
        assert status == 200 and body["state"] == "started" and body["replay"] is False
        connection = await _connect(migrated_url)
        try:
            duration = await connection.fetchval(
                "SELECT extract(epoch FROM (ends_at - starts_at)) FROM entitlements WHERE kind='onboarding_hour'"
            )
            digest = await connection.fetchval(
                "SELECT start_digest FROM onboarding_evidence WHERE credential_id = $1",
                ready["credential_id"],
            )
        finally:
            await connection.close()
        assert int(duration) == 3600
        assert digest and len(digest) == 64

        status, replay = await _post_start(session, port, envelope)
        assert status == 200 and replay["replay"] is True and replay["started_at"] == body["started_at"]

        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                "DELETE FROM auth_challenges WHERE challenge_id = $1",
                ready["start_challenge"]["challenge_id"],
            )
        finally:
            await connection.close()
        status, after_cleanup = await _post_start(session, port, envelope)
        assert status == 200 and after_cleanup["replay"] is True

        polled = await (await _post_intent(client, pop_client, token, "connect-1")).json()
        assert polled["status"] == "started" and polled["state"] == "started"
        assert "bootstrap" not in polled and "start_challenge" not in polled
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 1 and counts["entitlements"] == 1
    finally:
        await session.close()
        await client.close()


async def test_start_rejections_write_nothing_and_expired_proof_recovers(
    migrated_url, settings_factory, tmp_path
):
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    client, settings, material = await _start_stack(migrated_url, settings_factory, tmp_path)
    session = ClientSession(connector=TCPConnector(ssl=material.node_context()))
    try:
        _key, pop_client, token = await _enrolled_session(client)
        intent_id, ready = await _ready_intent(
            client, pop_client, token, migrated_url, settings.onboarding_secret_key, "connect-3"
        )
        port = client.server.app[EVIDENCE_LISTENER_KEY].port
        good = _start_envelope(
            ready["credential_id"],
            pop_client,
            intent_id=intent_id,
            request_key="connect-3",
            challenge=ready["start_challenge"],
        )

        background = dict(good)
        background["body"] = BACKGROUND_BODY
        status, body = await _post_start(session, port, background)
        assert status == 400 and body["code"] == "EVIDENCE_UNKNOWN_OPERATION"

        malformed = json.loads(json.dumps(good))
        del malformed["body"]["proof"]["signature_b64"]
        status, body = await _post_start(session, port, malformed)
        assert status == 400 and body["code"] == "EVIDENCE_UNKNOWN_OPERATION"

        cross_intent = json.loads(json.dumps(good))
        cross_intent["body"]["intent_id"] = str(uuid.uuid4())
        status, body = await _post_start(session, port, cross_intent)
        assert status == 403 and body["code"] == "ONBOARDING_ENV_MISMATCH"

        wrong_key = KeyMaterial()
        forged = _start_envelope(
            ready["credential_id"],
            PoPClient(wrong_key),
            intent_id=intent_id,
            request_key="connect-3",
            challenge=ready["start_challenge"],
        )
        status, body = await _post_start(session, port, forged)
        assert status == 401 and body["code"] == "PROOF_INVALID"

        await _expire_challenge(migrated_url, ready["start_challenge"]["challenge_id"])
        status, body = await _post_start(session, port, good)
        assert status == 401 and body["code"] == "CHALLENGE_EXPIRED"

        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0

        refreshed = await (await _post_intent(client, pop_client, token, "connect-3")).json()
        recovered = _start_envelope(
            ready["credential_id"],
            pop_client,
            intent_id=intent_id,
            request_key="connect-3",
            challenge=refreshed["start_challenge"],
        )
        status, body = await _post_start(session, port, recovered)
        assert status == 200 and body["state"] == "started"
    finally:
        await session.close()
        await client.close()


async def test_concurrent_identical_start_single_hour(migrated_url, settings_factory, tmp_path):
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    client, settings, material = await _start_stack(migrated_url, settings_factory, tmp_path)
    session = ClientSession(connector=TCPConnector(ssl=material.node_context()))
    try:
        _key, pop_client, token = await _enrolled_session(client)
        intent_id, ready = await _ready_intent(
            client, pop_client, token, migrated_url, settings.onboarding_secret_key, "connect-4"
        )
        port = client.server.app[EVIDENCE_LISTENER_KEY].port
        envelope = _start_envelope(
            ready["credential_id"],
            pop_client,
            intent_id=intent_id,
            request_key="connect-4",
            challenge=ready["start_challenge"],
        )
        results = await asyncio.gather(
            _post_start(session, port, envelope), _post_start(session, port, envelope)
        )
        assert sorted(result[0] for result in results) == [200, 200]
        replay_flags = sorted(result[1].get("replay") for result in results)
        assert replay_flags == [False, True], results
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 1 and counts["entitlements"] == 1
    finally:
        await session.close()
        await client.close()


async def test_legacy_evidence_replay_fails_closed(migrated_url, settings_factory, tmp_path):
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    client, settings, material = await _start_stack(migrated_url, settings_factory, tmp_path)
    session = ClientSession(connector=TCPConnector(ssl=material.node_context()))
    try:
        _key, pop_client, token = await _enrolled_session(client)
        intent_id, ready = await _ready_intent(
            client, pop_client, token, migrated_url, settings.onboarding_secret_key, "connect-5"
        )
        port = client.server.app[EVIDENCE_LISTENER_KEY].port
        envelope = _start_envelope(
            ready["credential_id"],
            pop_client,
            intent_id=intent_id,
            request_key="connect-5",
            challenge=ready["start_challenge"],
        )
        assert (await _post_start(session, port, envelope))[0] == 200
        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                "UPDATE onboarding_evidence SET start_digest = NULL WHERE credential_id = $1",
                ready["credential_id"],
            )
        finally:
            await connection.close()
        status, body = await _post_start(session, port, envelope)
        assert status == 409 and body["code"] == "ONBOARDING_REPLAY_UNAVAILABLE"
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 1 and counts["entitlements"] == 1
    finally:
        await session.close()
        await client.close()


async def test_migration_0019_upgrade_on_populated_database(database_url, tmp_path):
    versions = Path(runner.__file__).parent / "versions"
    partial = tmp_path / "versions_partial"
    partial.mkdir()
    for path in sorted(versions.glob("*.sql")):
        if path.name.startswith("0019"):
            continue
        shutil.copy2(path, partial / path.name)
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await runner.apply_migrations(connection, versions_dir=partial)
        installation_id = await connection.fetchval(
            """
            INSERT INTO installations
                (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test', 'android', $1, 'spki', 'technical') RETURNING id
            """,
            uuid.uuid4().hex,
        )
        gateway_id = await connection.fetchval(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, capabilities)
            VALUES ('legacy-node', 'test', '{"node_id":"legacy-node"}'::jsonb, '["managed"]'::jsonb)
            RETURNING id
            """
        )
        intent_id = await connection.fetchval(
            """
            INSERT INTO onboarding_intents
                (id, installation_id, gateway_id, environment, unit_epoch, credential_id,
                 request_key, secret_hash, secret_enc, state, expires_at, started_at, hour_not_after)
            VALUES (gen_random_uuid(), $1, $2, 'test', 1, 'legacy-cred', 'legacy-key', 'hash',
                    NULL, 'started', now() + interval '10 minutes', now(),
                    now() + interval '1 hour')
            RETURNING id
            """,
            installation_id,
            gateway_id,
        )
        await connection.execute(
            """
            INSERT INTO onboarding_evidence
                (intent_id, credential_id, installation_id, gateway_id, environment,
                 connection_id_hash, request_id)
            VALUES ($1, 'legacy-cred', $2, $3, 'test', 'legacy-hash', 'legacy-request')
            """,
            intent_id,
            installation_id,
            gateway_id,
        )
        await runner.apply_migrations(connection)
        legacy = await connection.fetchrow(
            "SELECT start_digest, request_id FROM onboarding_evidence WHERE credential_id = 'legacy-cred'"
        )
        assert legacy["start_digest"] is None and legacy["request_id"] == "legacy-request"
        index = await connection.fetchval(
            "SELECT indexname FROM pg_indexes WHERE indexname = 'auth_challenges_start_outstanding_uniq'"
        )
        assert index is not None
        await connection.execute(
            """
            INSERT INTO auth_challenges
                (challenge_id, nonce_b64, installation_fingerprint, purpose, op, environment, expires_at)
            VALUES (repeat('a', 16), repeat('n', 16), $1, 'onboarding-start-intent',
                    'onboarding.intent', 'test', now() + interval '5 minutes')
            """,
            uuid.uuid4().hex,
        )
    finally:
        await connection.close()


def _envelope_digest(envelope):
    signed_b64 = envelope["body"]["proof"]["signed_payload_b64"]
    _raw, payload = backend_pop.decode_signed_payload(signed_b64)
    return backend_pop.business_digest(payload)


async def _admit_direct(url, settings, envelope, *, request_id="d" * 32, ts_epoch=None):
    credential_id = envelope["credential_id"]
    proof = envelope["body"]["proof"]
    connection = await _connect(url)
    try:
        gateway_id = await connection.fetchval(
            "SELECT id FROM gateways WHERE gateway_key = $1", GATEWAY_KEY
        )
        caller = GatewayContext(gateway_id=gateway_id, gateway_key=GATEWAY_KEY, environment="test")
        return await admit_evidence(
            connection,
            caller=caller,
            credential_id=credential_id,
            connection_id="d" * 32,
            request_id=request_id,
            evidence_digest=_envelope_digest(envelope),
            consume_challenge_id=proof["challenge_id"],
            proof_nonce=proof["nonce_b64"],
            proof_ts_epoch=ts_epoch if ts_epoch is not None else datetime.now(UTC).timestamp(),
            proof_skew_seconds=settings.proof_skew_seconds,
        )
    finally:
        await connection.close()


async def _installation_lock_holder(url):
    connection = await _connect(url)
    installation_id = await connection.fetchval("SELECT id FROM installations LIMIT 1")
    await connection.execute("BEGIN")
    await connection.fetchval(
        "SELECT id FROM installations WHERE id = $1 FOR UPDATE", installation_id
    )
    return connection, installation_id


async def _release_holder(holder):
    try:
        await holder.execute("ROLLBACK")
    except (asyncpg.PostgresError, OSError):
        pass
    try:
        await holder.close()
    except (asyncpg.PostgresError, OSError):
        pass


async def _wait_for_lock_waiter(url, timeout=10.0):
    """Deterministic barrier: the blocked start task must actually wait on the installation row."""
    loop = asyncio.get_event_loop()
    deadline = loop.time() + timeout
    while True:
        connection = await _connect(url)
        try:
            waiting = await connection.fetchval(
                """
                SELECT count(*) FROM pg_stat_activity
                WHERE wait_event_type = 'Lock'
                  AND state = 'active'
                  AND pid <> pg_backend_pid()
                  AND query LIKE '%FROM installations%FOR UPDATE%'
                """
            )
        finally:
            await connection.close()
        if waiting and waiting > 0:
            return
        if loop.time() > deadline:
            raise AssertionError("start task never entered the installation lock wait")
        await asyncio.sleep(0.05)


async def test_replay_recheck_precedes_consume_and_freshness_exempt(
    migrated_url, settings_factory, tmp_path
):
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    client, settings, _material = await _start_stack(migrated_url, settings_factory, tmp_path)
    try:
        _key, pop_client, token = await _enrolled_session(client)
        intent_id, ready = await _ready_intent(
            client, pop_client, token, migrated_url, settings.onboarding_secret_key, "connect-6"
        )
        envelope = _start_envelope(
            ready["credential_id"],
            pop_client,
            intent_id=intent_id,
            request_key="connect-6",
            challenge=ready["start_challenge"],
        )
        assert (await _admit_direct(migrated_url, settings, envelope))["replay"] is False
        replay = await _admit_direct(
            migrated_url,
            settings,
            envelope,
            ts_epoch=datetime.now(UTC).timestamp() - 10_000,
        )
        assert replay["replay"] is True
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 1 and counts["entitlements"] == 1
    finally:
        await client.close()


async def test_concurrent_polls_and_poll_versus_start_or_revoke(
    migrated_url, settings_factory, tmp_path
):
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    client, settings, _material = await _start_stack(migrated_url, settings_factory, tmp_path)
    try:
        _key, pop_client, token = await _enrolled_session(client)
        created = await _post_intent(client, pop_client, token, "connect-7")
        pending = await created.json()
        await _provision(migrated_url, pending["intent_id"], settings.onboarding_secret_key)
        first, second = await asyncio.gather(
            _post_intent(client, pop_client, token, "connect-7"),
            _post_intent(client, pop_client, token, "connect-7"),
        )
        assert first.status == 200 and second.status == 200
        ready_a, ready_b = await first.json(), await second.json()
        assert ready_a["start_challenge"]["challenge_id"] == ready_b["start_challenge"]["challenge_id"]
        connection = await _connect(migrated_url)
        try:
            outstanding = await connection.fetchval(
                """
                SELECT count(*) FROM auth_challenges
                WHERE purpose = 'onboarding-start' AND used_at IS NULL AND superseded_at IS NULL
                """
            )
        finally:
            await connection.close()
        assert outstanding == 1

        holder, _installation_id = await _installation_lock_holder(migrated_url)
        poll_task = asyncio.create_task(_post_intent(client, pop_client, token, "connect-7"))
        await asyncio.sleep(0.2)
        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                """
                UPDATE onboarding_intents
                SET state = 'started', started_at = now(), hour_not_after = now() + interval '1 hour',
                    secret_enc = NULL
                WHERE id = $1
                """,
                uuid.UUID(pending["intent_id"]),
            )
        finally:
            await connection.close()
        await holder.execute("COMMIT")
        await holder.close()
        response = await poll_task
        assert response.status == 200
        body = await response.json()
        assert body["status"] == "started" and "bootstrap" not in body and "start_challenge" not in body

        holder, _installation_id = await _installation_lock_holder(migrated_url)
        revoke_task = asyncio.create_task(_post_intent(client, pop_client, token, "connect-7"))
        await asyncio.sleep(0.2)
        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                "UPDATE onboarding_intents SET state = 'revoked', revoked_at = now() WHERE id = $1",
                uuid.UUID(pending["intent_id"]),
            )
        finally:
            await connection.close()
        await holder.execute("COMMIT")
        await holder.close()
        revoked = await revoke_task
        assert revoked.status == 403, await revoked.text()
        assert (await revoked.json())["code"] == "ONBOARDING_INTENT_REVOKED"
    finally:
        await client.close()


async def test_challenge_expiry_after_wait_blocks_new_start(
    migrated_url, settings_factory, tmp_path
):
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    client, settings, material = await _start_stack(migrated_url, settings_factory, tmp_path)
    session = ClientSession(connector=TCPConnector(ssl=material.node_context()))
    try:
        _key, pop_client, token = await _enrolled_session(client)
        intent_id, ready = await _ready_intent(
            client, pop_client, token, migrated_url, settings.onboarding_secret_key, "connect-9"
        )
        port = client.server.app[EVIDENCE_LISTENER_KEY].port
        envelope = _start_envelope(
            ready["credential_id"],
            pop_client,
            intent_id=intent_id,
            request_key="connect-9",
            challenge=ready["start_challenge"],
        )
        holder, _installation_id = await _installation_lock_holder(migrated_url)
        start_task = asyncio.create_task(_post_start(session, port, envelope))
        await asyncio.sleep(0.2)
        await _expire_challenge(migrated_url, ready["start_challenge"]["challenge_id"])
        await holder.execute("COMMIT")
        await holder.close()
        status, body = await start_task
        assert status == 401 and body["code"] == "CHALLENGE_EXPIRED"
        connection = await _connect(migrated_url)
        try:
            used_at = await connection.fetchval(
                "SELECT used_at FROM auth_challenges WHERE challenge_id = $1",
                ready["start_challenge"]["challenge_id"],
            )
        finally:
            await connection.close()
        assert used_at is None
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0

        stale = _start_envelope(
            ready["credential_id"],
            pop_client,
            intent_id=intent_id,
            request_key="connect-9",
            challenge=(await (await _post_intent(client, pop_client, token, "connect-9")).json())[
                "start_challenge"
            ],
            ts=(datetime.now(UTC) - timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ"),
        )
        status, body = await _post_start(session, port, stale)
        assert status == 401 and body["code"] == "PROOF_INVALID"
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0
    finally:
        await session.close()
        await client.close()


async def test_malformed_proof_and_version_types_are_bounded(
    migrated_url, settings_factory, tmp_path
):
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    client, settings, material = await _start_stack(migrated_url, settings_factory, tmp_path)
    session = ClientSession(connector=TCPConnector(ssl=material.node_context()))
    try:
        _key, pop_client, token = await _enrolled_session(client)
        challenge = await _challenge(client, pop_client.key, "onboarding-start-intent")
        proof, _payload = pop_client.proof(
            challenge, op="onboarding.intent", scope="onboarding:start",
            extra={"request_key": "connect-10"},
        )
        broken = json.loads(json.dumps(proof))
        broken["signature_b64"] = ["not", "a", "string"]
        response = await client.post(
            f"{MOBILE}/onboarding/intents",
            json={"proof": broken},
            headers={"Authorization": f"Bearer {token}"},
        )
        assert response.status == 400, await response.text()
        assert (await response.json())["code"] in ("BAD_MESSAGE", "PROOF_INVALID")

        intent_id, ready = await _ready_intent(
            client, pop_client, token, migrated_url, settings.onboarding_secret_key, "connect-10"
        )
        port = client.server.app[EVIDENCE_LISTENER_KEY].port
        good = _start_envelope(
            ready["credential_id"],
            pop_client,
            intent_id=intent_id,
            request_key="connect-10",
            challenge=ready["start_challenge"],
        )
        for mutate in (
            lambda body: body.update(v=True),
            lambda body: body.update(v=1.0),
            lambda body: body["proof"].update(signature_b64=12345),
            lambda body: body.update(request_key="k" * 129),
        ):
            broken_body = json.loads(json.dumps(good))
            mutate(broken_body["body"])
            status, payload = await _post_start(session, port, broken_body)
            assert status == 400, payload
            assert payload["code"] == "EVIDENCE_UNKNOWN_OPERATION"
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0
    finally:
        await session.close()
        await client.close()


async def test_admission_uses_post_lock_moment_for_new_start(
    migrated_url, settings_factory, tmp_path
):
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    client, settings, material = await _start_stack(migrated_url, settings_factory, tmp_path)
    session = ClientSession(connector=TCPConnector(ssl=material.node_context()))
    holder = None
    start_task = None
    try:
        _key, pop_client, token = await _enrolled_session(client)
        intent_id, ready = await _ready_intent(
            client, pop_client, token, migrated_url, settings.onboarding_secret_key, "connect-11"
        )
        port = client.server.app[EVIDENCE_LISTENER_KEY].port
        envelope = _start_envelope(
            ready["credential_id"],
            pop_client,
            intent_id=intent_id,
            request_key="connect-11",
            challenge=ready["start_challenge"],
        )
        holder, _installation_id = await _installation_lock_holder(migrated_url)
        start_task = asyncio.create_task(_post_start(session, port, envelope))
        await _wait_for_lock_waiter(migrated_url)
        released_at = datetime.now(UTC)
        await holder.execute("COMMIT")
        await holder.close()
        holder = None
        status, body = await asyncio.wait_for(start_task, timeout=15)
        start_task = None
        assert status == 200 and body["replay"] is False
        started_at = datetime.fromisoformat(body["started_at"])
        assert started_at >= released_at
        not_after = datetime.fromisoformat(body["not_after"])
        assert (not_after - started_at).total_seconds() == 3600
        connection = await _connect(migrated_url)
        try:
            duration = await connection.fetchval(
                "SELECT extract(epoch FROM (ends_at - starts_at)) FROM entitlements WHERE kind='onboarding_hour'"
            )
            assert int(duration) == 3600
        finally:
            await connection.close()
    finally:
        if start_task is not None:
            start_task.cancel()
            with contextlib.suppress(BaseException):
                await start_task
        if holder is not None:
            await _release_holder(holder)
        await session.close()
        await client.close()


async def test_expired_while_blocked_gives_410_without_writes(
    migrated_url, settings_factory, tmp_path
):
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    client, settings, material = await _start_stack(migrated_url, settings_factory, tmp_path)
    session = ClientSession(connector=TCPConnector(ssl=material.node_context()))
    holder = None
    start_task = None
    try:
        _key, pop_client, token = await _enrolled_session(client)
        intent_id, ready = await _ready_intent(
            client, pop_client, token, migrated_url, settings.onboarding_secret_key, "connect-12"
        )
        port = client.server.app[EVIDENCE_LISTENER_KEY].port
        envelope = _start_envelope(
            ready["credential_id"],
            pop_client,
            intent_id=intent_id,
            request_key="connect-12",
            challenge=ready["start_challenge"],
        )
        holder, _installation_id = await _installation_lock_holder(migrated_url)
        start_task = asyncio.create_task(_post_start(session, port, envelope))
        await _wait_for_lock_waiter(migrated_url)
        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                "UPDATE onboarding_intents SET expires_at = now() - interval '1 second' WHERE id = $1",
                uuid.UUID(intent_id),
            )
        finally:
            await connection.close()
        await holder.execute("COMMIT")
        await holder.close()
        holder = None
        status, body = await asyncio.wait_for(start_task, timeout=15)
        start_task = None
        assert status == 410 and body["code"] == "ONBOARDING_INTENT_EXPIRED"
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0
        connection = await _connect(migrated_url)
        try:
            used_at = await connection.fetchval(
                "SELECT used_at FROM auth_challenges WHERE challenge_id = $1",
                ready["start_challenge"]["challenge_id"],
            )
            state = await connection.fetchval(
                "SELECT state FROM onboarding_intents WHERE id = $1", uuid.UUID(intent_id)
            )
        finally:
            await connection.close()
        assert used_at is None and state == "expired"
    finally:
        if start_task is not None:
            start_task.cancel()
            with contextlib.suppress(BaseException):
                await start_task
        if holder is not None:
            await _release_holder(holder)
        await session.close()
        await client.close()


async def test_pending_retry_after_is_one_second(migrated_url, settings_factory, tmp_path):
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    client, settings, _material = await _start_stack(migrated_url, settings_factory, tmp_path)
    try:
        _key, pop_client, token = await _enrolled_session(client)
        first = await _post_intent(client, pop_client, token, "retry-after-1")
        assert first.status == 200, await first.text()
        pending = await first.json()
        assert pending["state"] == "pending"
        assert pending["retry_after"] == 1

        repeated = await _post_intent(client, pop_client, token, "retry-after-1")
        repeated_body = await repeated.json()
        assert repeated_body["intent_id"] == pending["intent_id"]
        assert repeated_body["state"] == "pending" and repeated_body["retry_after"] == 1

        await _provision(migrated_url, pending["intent_id"], settings.onboarding_secret_key)
        ready = await _post_intent(client, pop_client, token, "retry-after-1")
        ready_body = await ready.json()
        assert ready_body["state"] == "ready"
        assert ready_body["credential_id"] == ready_body["bootstrap"]["credential_id"]
        assert ready_body["start_challenge"]["challenge_id"]
        assert "retry_after" not in ready_body

        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                "UPDATE onboarding_intents SET expires_at = now() - interval '1 second' WHERE id = $1",
                uuid.UUID(pending["intent_id"]),
            )
        finally:
            await connection.close()
        expired = await _post_intent(client, pop_client, token, "retry-after-1")
        assert expired.status == 410
        assert (await expired.json())["code"] == "ONBOARDING_INTENT_EXPIRED"
    finally:
        await client.close()


async def test_gateway_key_selection_and_immutable_conflict(
    migrated_url, settings_factory, tmp_path
):
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    await _seed_gateway(migrated_url, key="terlimo-other-gw")
    client, settings, _material = await _start_stack(migrated_url, settings_factory, tmp_path)
    try:
        _key, pop_client, token = await _enrolled_session(client)
        first = await _post_intent(
            client, pop_client, token, "select-1", gateway_key="terlimo-other-gw"
        )
        assert first.status == 200, await first.text()
        pending = await first.json()
        assert pending["state"] == "pending"
        assert pending["gateway"]["node_id"] == "terlimo-other-gw"

        repeat = await _post_intent(
            client, pop_client, token, "select-1", gateway_key="terlimo-other-gw"
        )
        assert (await repeat.json())["intent_id"] == pending["intent_id"]

        conflict = await _post_intent(
            client, pop_client, token, "select-1", gateway_key=GATEWAY_KEY
        )
        assert conflict.status == 409, await conflict.text()
        assert (await conflict.json())["code"] == "ONBOARDING_INTENT_CONFLICT"

        await _provision(migrated_url, pending["intent_id"], settings.onboarding_secret_key)
        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                "UPDATE onboarding_intents SET expires_at = now() - interval '1 second' WHERE id = $1",
                uuid.UUID(pending["intent_id"]),
            )
        finally:
            await connection.close()
        expired_conflict = await _post_intent(
            client, pop_client, token, "select-1", gateway_key=GATEWAY_KEY
        )
        assert expired_conflict.status == 409, await expired_conflict.text()
        connection = await _connect(migrated_url)
        try:
            row = await connection.fetchrow(
                "SELECT state, secret_enc IS NULL AS wiped FROM onboarding_intents WHERE id = $1",
                uuid.UUID(pending["intent_id"]),
            )
        finally:
            await connection.close()
        assert row["state"] == "ready" and row["wiped"] is False

        _key2, pop_client2, token2 = await _enrolled_session(client)
        legacy = await _post_intent(client, pop_client2, token2, "select-legacy")
        assert legacy.status == 200, await legacy.text()
        legacy_body = await legacy.json()
        assert legacy_body["state"] == "pending"
        assert legacy_body["gateway"]["node_id"] in (GATEWAY_KEY, "terlimo-other-gw")
    finally:
        await client.close()


async def test_concurrent_gateway_selection_single_winner(migrated_url, settings_factory, tmp_path):
    installation_id = await _seed_installation(migrated_url)
    gateway_a, _ = await _seed_gateway(migrated_url, key="terlimo-race-a")
    gateway_b, _ = await _seed_gateway(migrated_url, key="terlimo-race-b")
    holder = await _connect(migrated_url)
    await holder.execute("BEGIN")
    await holder.fetchval(
        "SELECT id FROM installations WHERE id = $1 FOR UPDATE", installation_id
    )

    async def select(key):
        connection = await _connect(migrated_url)
        try:
            return await public_intent(
                connection,
                installation_id=installation_id,
                request_key="race-1",
                environment="test",
                cipher=FakeCipher(),
                challenge_ttl_seconds=300,
                gateway_key=key,
            )
        finally:
            await connection.close()

    first = asyncio.create_task(select("terlimo-race-a"))
    second = asyncio.create_task(select("terlimo-race-b"))
    try:
        await _wait_for_lock_waiter(migrated_url)
        await holder.execute("COMMIT")
        await holder.close()
        results = await asyncio.gather(first, second, return_exceptions=True)
    finally:
        for task in (first, second):
            if not task.done():
                task.cancel()
        with contextlib.suppress(BaseException):
            await holder.execute("ROLLBACK")
        with contextlib.suppress(BaseException):
            await holder.close()

    winners = [item for item in results if isinstance(item, dict)]
    conflicts = [
        item
        for item in results
        if isinstance(item, OnboardingError) and item.code == "ONBOARDING_INTENT_CONFLICT"
    ]
    assert len(winners) == 1, results
    assert len(conflicts) == 1, results
    connection = await _connect(migrated_url)
    try:
        stored_gateway_id = await connection.fetchval(
            "SELECT gateway_id FROM onboarding_intents WHERE installation_id = $1 AND request_key = 'race-1'",
            installation_id,
        )
    finally:
        await connection.close()
    winner_key = winners[0]["gateway"]["node_id"]
    expected = gateway_a if winner_key == "terlimo-race-a" else gateway_b
    assert stored_gateway_id == expected

    # Pins the under-lock comparison in create_intent itself (return-existing branch).
    connection = await _connect(migrated_url)
    try:
        with pytest.raises(OnboardingError) as conflict:
            await create_intent(
                connection,
                installation_id=installation_id,
                request_key="race-1",
                environment="test",
                cipher=FakeCipher(),
                gateway_id=gateway_a if expected == gateway_b else gateway_b,
            )
    finally:
        await connection.close()
    assert conflict.value.code == "ONBOARDING_INTENT_CONFLICT"


async def test_preferred_gateway_conflict_at_intent_response(monkeypatch, migrated_url):
    from terlimo_backend import onboarding_hour as hour_module

    real_intent_response = hour_module.intent_response
    marker = contextvars.ContextVar("preferred_probe", default=False)
    entered = asyncio.Event()
    allow_b = asyncio.Event()

    async def pausing_intent_response(*args, **kwargs):
        # Test-only deterministic barrier: pause the marked caller immediately before the
        # real intent_response (after any preliminary steps), once.
        if marker.get() and not entered.is_set():
            entered.set()
            await allow_b.wait()
        return await real_intent_response(*args, **kwargs)

    monkeypatch.setattr(hour_module, "intent_response", pausing_intent_response)

    installation_id = await _seed_installation(migrated_url)
    _gateway_a, _ = await _seed_gateway(migrated_url, key="terlimo-probe-a")
    _gateway_b, _ = await _seed_gateway(migrated_url, key="terlimo-probe-b")

    async def select(request_key, key):
        connection = await _connect(migrated_url)
        try:
            return await public_intent(
                connection,
                installation_id=installation_id,
                request_key=request_key,
                environment="test",
                cipher=FakeCipher(),
                challenge_ttl_seconds=300,
                gateway_key=key,
            )
        finally:
            await connection.close()

    async def marked_select(request_key, key):
        marker.set(True)
        return await select(request_key, key)

    task_b = asyncio.create_task(marked_select("probe-1", "terlimo-probe-b"))
    await asyncio.wait_for(entered.wait(), timeout=10)
    result_a = await select("probe-1", "terlimo-probe-a")
    assert result_a["gateway"]["node_id"] == "terlimo-probe-a"
    allow_b.set()
    with pytest.raises(OnboardingError) as conflict:
        await asyncio.wait_for(task_b, timeout=10)
    assert conflict.value.code == "ONBOARDING_INTENT_CONFLICT"

    # Expired/ready variant: conflict must win before any wipe or decrypt side effect.
    connection = await _connect(migrated_url)
    try:
        intent_id = await connection.fetchval(
            "SELECT id FROM onboarding_intents WHERE installation_id = $1 AND request_key = 'probe-1'",
            installation_id,
        )
        await connection.execute(
            "UPDATE onboarding_intents SET state = 'ready', secret_enc = $2, expires_at = now() - interval '1 second' WHERE id = $1",
            intent_id,
            FakeCipher().encrypt("synthetic-secret-value-000000000000"),
        )
    finally:
        await connection.close()

    entered.clear()
    allow_b = asyncio.Event()
    task_b2 = asyncio.create_task(marked_select("probe-1", "terlimo-probe-b"))
    await asyncio.wait_for(entered.wait(), timeout=10)
    allow_b.set()
    with pytest.raises(OnboardingError) as expired_conflict:
        await asyncio.wait_for(task_b2, timeout=10)
    assert expired_conflict.value.code == "ONBOARDING_INTENT_CONFLICT"
    connection = await _connect(migrated_url)
    try:
        row = await connection.fetchrow(
            "SELECT state, secret_enc IS NULL AS wiped FROM onboarding_intents WHERE id = $1",
            intent_id,
        )
    finally:
        await connection.close()
    assert row["state"] == "ready" and row["wiped"] is False
