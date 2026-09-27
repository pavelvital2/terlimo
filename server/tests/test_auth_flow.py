"""Installation enrollment and installation-scoped sessions over real HTTP + PostgreSQL.

The client side signs with the accepted contract module (terlimo-s1-contracts @ 336bd6d)
while the server verifies with the ported implementation, so the flow is not a
self-signed check of one implementation.
"""

from __future__ import annotations

import asyncio
import hashlib
import importlib.util
import json
import os
import secrets
from datetime import UTC, datetime, timedelta
from pathlib import Path

import asyncpg
import pytest
from aiohttp.test_utils import TestClient, TestServer
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ec

from terlimo_backend import pop
from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.maintenance import sweep_once
from terlimo_backend.migrations import runner

CONTRACT_DIR = Path(os.environ.get("TERLIMO_CONTRACT_DIR", "/home/pavel/projects/terlimo-s1-contracts"))
CONTRACT_POP_PATH = CONTRACT_DIR / "auth" / "pop_canonical.py"

if not CONTRACT_POP_PATH.exists():
    pytest.skip("accepted contract module not available (set TERLIMO_CONTRACT_DIR)", allow_module_level=True)


def _load_contract_pop():
    spec = importlib.util.spec_from_file_location("contract_pop_canonical_flow", CONTRACT_POP_PATH)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


contract_pop = _load_contract_pop()
MOBILE = "/api/mobile/v1"


class KeyMaterial:
    def __init__(self) -> None:
        self.private = ec.generate_private_key(ec.SECP256R1())
        self.der = self.private.public_key().public_bytes(
            serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo
        )
        self.spki_b64 = contract_pop.b64url_encode(self.der)
        self.fingerprint = hashlib.sha256(self.der).hexdigest()


class PoPClient:
    def __init__(self, key: KeyMaterial, environment: str = "test") -> None:
        self.key = key
        self.environment = environment

    def proof(
        self,
        challenge: dict,
        *,
        op: str,
        scope: str,
        extra: dict | None = None,
        idempotency_key: str | None = None,
        ts: str | None = None,
        request_id: str | None = None,
        nonce: str | None = None,
    ) -> tuple[dict, dict]:
        request_id = request_id or secrets.token_hex(16)
        nonce = nonce or challenge["nonce_b64"]
        ts = ts or datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")
        payload = {
            "env": self.environment,
            "scope": scope,
            "op": op,
            "installation_id": self.key.fingerprint,
            "ts": ts,
            "nonce": nonce,
            "request_id": request_id,
        }
        if idempotency_key is not None:
            payload["idempotency_key"] = idempotency_key
        payload.update(extra or {})
        payload_bytes = contract_pop.canonical_json(payload)
        message = contract_pop.pop_message(
            request_id, challenge["challenge_id"], nonce, payload_bytes
        )
        proof = {
            "algorithm": "ES256",
            "signature_b64": contract_pop.sign(self.key.private, message),
            "request_id": request_id,
            "challenge_id": challenge["challenge_id"],
            "nonce_b64": nonce,
            "payload_hash": hashlib.sha256(payload_bytes).hexdigest(),
            "signed_payload_b64": contract_pop.b64url_encode(payload_bytes),
            "environment": self.environment,
        }
        return proof, payload


async def _start_client(settings) -> TestClient:
    database = Database(settings)
    app = create_app(settings, database)
    client = TestClient(TestServer(app))
    await client.start_server()
    return client


async def _challenge(client: TestClient, key: KeyMaterial, purpose: str, environment: str = "test") -> dict:
    response = await client.post(
        f"{MOBILE}/auth/challenge",
        json={
            "installation_fingerprint": key.fingerprint,
            "purpose": purpose,
            "environment": environment,
        },
    )
    assert response.status == 200, await response.text()
    return await response.json()


async def _enroll(client: TestClient, pop_client: PoPClient, challenge: dict, name: str = "Pixel TEST"):
    proof, _ = pop_client.proof(
        challenge,
        op="installations.create",
        scope="enrollment",
        extra={
            "public_key_spki_b64": pop_client.key.spki_b64,
            "platform": "android",
            "name": name,
        },
    )
    return await client.post(
        f"{MOBILE}/installations",
        json={"public_key_spki_b64": pop_client.key.spki_b64, "proof": proof},
    )


async def _session(client: TestClient, pop_client: PoPClient, challenge: dict, scopes: list[str], idem: str, headers=None):
    proof, _ = pop_client.proof(
        challenge,
        op="auth.session",
        scope="session",
        extra={"requested_scopes": scopes},
        idempotency_key=idem,
    )
    return await client.post(f"{MOBILE}/auth/session", json={"proof": proof}, headers=headers)


async def _scalar(database_url: str, query: str, *args):
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        return await connection.fetchval(query, *args)
    finally:
        await connection.close()


async def _execute(database_url: str, query: str, *args) -> None:
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await connection.execute(query, *args)
    finally:
        await connection.close()


@pytest.fixture
async def auth_client(migrated_url, settings_factory):
    settings = settings_factory(migrated_url)
    client = await _start_client(settings)
    try:
        yield client, migrated_url, settings
    finally:
        await client.close()


async def test_happy_path_enrollment_and_session(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)

    challenge = await _challenge(client, key, "enrollment")
    assert challenge["single_use"] is True
    assert challenge["purpose"] == "enrollment"
    enroll = await _enroll(client, pop_client, challenge)
    assert enroll.status == 200, await enroll.text()
    enroll_body = await enroll.json()
    assert enroll_body["schema_version"] == "1.0"
    assert enroll_body["entitlement_created"] is False
    installation = enroll_body["installation"]
    assert installation["installation_id"] == key.fingerprint
    assert installation["fingerprint"] == key.fingerprint
    assert installation["platform"] == "android"
    assert installation["state"] == "technical"
    assert enroll_body["session"]["scopes"] == ["enrollment"]
    assert enroll_body["session"]["account_ref"] is None
    assert len(enroll_body["session"]["session_id"]) >= 32

    session_challenge = await _challenge(client, key, "session")
    session = await _session(
        client,
        pop_client,
        session_challenge,
        ["session:read"],
        "idem-flow-00000001",
    )
    assert session.status == 200, await session.text()
    session_body = await session.json()
    assert session_body["session"]["scopes"] == ["session:read"]
    assert session_body["session"]["installation_ref"] == key.fingerprint
    assert session_body["session"]["generation"] == "1"

    assert await _scalar(database_url, "SELECT count(*) FROM installations") == 1
    assert await _scalar(database_url, "SELECT count(*) FROM sessions") == 2
    for table in ("accounts", "entitlements", "account_bindings", "grants"):
        assert await _scalar(database_url, f"SELECT count(*) FROM {table}") == 0, table


async def test_wrong_key_and_wrong_challenge_binding(auth_client):
    client, _, _ = auth_client
    key_a = KeyMaterial()
    key_b = KeyMaterial()
    pop_a = PoPClient(key_a)

    challenge_a = await _challenge(client, key_a, "enrollment")

    # Signed by B while the request presents A's key and A's challenge.
    proof_b, _ = PoPClient(key_b).proof(
        challenge_a,
        op="installations.create",
        scope="enrollment",
        extra={"public_key_spki_b64": key_b.spki_b64, "platform": "android", "name": None},
    )
    response = await client.post(
        f"{MOBILE}/installations",
        json={"public_key_spki_b64": key_a.spki_b64, "proof": proof_b},
    )
    assert response.status == 401
    assert (await response.json())["code"] == "PROOF_INVALID"

    # A valid proof from A against a challenge issued for B.
    challenge_b = await _challenge(client, key_b, "enrollment")
    proof_a, _ = pop_a.proof(
        challenge_b,
        op="installations.create",
        scope="enrollment",
        extra={"public_key_spki_b64": key_a.spki_b64, "platform": "android", "name": None},
    )
    response = await client.post(
        f"{MOBILE}/installations",
        json={"public_key_spki_b64": key_a.spki_b64, "proof": proof_a},
    )
    assert response.status == 401
    assert (await response.json())["code"] == "PROOF_INVALID"


async def test_wrong_scope_and_wrong_environment(auth_client):
    client, _, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    assert (await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))).status == 200
    challenge = await _challenge(client, key, "session")

    proof, _ = pop_client.proof(
        challenge, op="device.bindings", scope="binding", extra={"device_name": "x"}
    )
    response = await client.post(f"{MOBILE}/auth/session", json={"proof": proof})
    assert response.status == 400
    assert (await response.json())["code"] == "WRONG_SCOPE"

    production = PoPClient(key, environment="production")
    proof, _ = production.proof(
        challenge,
        op="auth.session",
        scope="session",
        extra={"requested_scopes": ["session:read"]},
        idempotency_key="idem-flow-00000002",
    )
    response = await client.post(f"{MOBILE}/auth/session", json={"proof": proof})
    assert response.status == 400
    assert (await response.json())["code"] == "WRONG_ENVIRONMENT"


async def test_expired_challenge_replay_and_ts_window(auth_client, database_url, settings_factory):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)

    expired = await _challenge(client, key, "enrollment")
    await _execute(
        database_url,
        "UPDATE auth_challenges SET expires_at = now() - interval '1 second' WHERE challenge_id = $1",
        expired["challenge_id"],
    )
    response = await _enroll(client, pop_client, expired)
    assert response.status == 401
    assert (await response.json())["code"] == "CHALLENGE_EXPIRED"

    challenge = await _challenge(client, key, "enrollment")
    first = await _enroll(client, pop_client, challenge)
    assert first.status == 200, await first.text()
    proof, _ = pop_client.proof(
        challenge,
        op="installations.create",
        scope="enrollment",
        extra={"public_key_spki_b64": key.spki_b64, "platform": "android", "name": None},
    )
    replay = await client.post(
        f"{MOBILE}/installations",
        json={"public_key_spki_b64": key.spki_b64, "proof": proof},
    )
    assert replay.status == 401
    assert (await replay.json())["code"] == "REPLAY_DETECTED"

    skew_client = await _start_client(settings_factory(database_url, proof_skew_seconds=1))
    try:
        old_ts = (datetime.now(UTC) - timedelta(seconds=120)).strftime("%Y-%m-%dT%H:%M:%SZ")
        challenge = await _challenge(skew_client, key, "enrollment")
        proof, _ = pop_client.proof(
            challenge,
            op="installations.create",
            scope="enrollment",
            extra={"public_key_spki_b64": key.spki_b64, "platform": "android", "name": None},
            ts=old_ts,
        )
        response = await skew_client.post(
            f"{MOBILE}/installations",
            json={"public_key_spki_b64": key.spki_b64, "proof": proof},
        )
        assert response.status == 401
        assert (await response.json())["code"] == "PROOF_INVALID"
    finally:
        await skew_client.close()


async def test_repeat_and_concurrent_enrollment_keep_one_installation(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)

    first = await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))
    second = await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))
    assert first.status == 200 and second.status == 200
    assert (await first.json())["installation"]["installation_id"] == key.fingerprint
    assert (await second.json())["installation"]["installation_id"] == key.fingerprint
    assert await _scalar(database_url, "SELECT count(*) FROM installations") == 1

    challenge_a = await _challenge(client, key, "enrollment")
    challenge_b = await _challenge(client, key, "enrollment")
    responses = await asyncio.gather(
        _enroll(client, pop_client, challenge_a), _enroll(client, pop_client, challenge_b)
    )
    assert all(response.status == 200 for response in responses)
    assert await _scalar(database_url, "SELECT count(*) FROM installations") == 1
    assert await _scalar(database_url, "SELECT count(*) FROM sessions") == 4


async def test_session_idempotent_retry_and_conflict(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    assert (await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))).status == 200

    idem = "idem-session-retry-01"
    scopes = ["session:read"]
    first = await _session(
        client, pop_client, await _challenge(client, key, "session"), scopes, idem
    )
    assert first.status == 200, await first.text()
    first_session = (await first.json())["session"]["session_id"]

    retry = await _session(
        client, pop_client, await _challenge(client, key, "session"), scopes, idem
    )
    assert retry.status == 200, await retry.text()
    assert (await retry.json())["session"]["session_id"] == first_session
    assert await _scalar(database_url, "SELECT count(*) FROM sessions") == 2

    conflict = await _session(
        client,
        pop_client,
        await _challenge(client, key, "session"),
        ["session:read", "session:write"],
        idem,
    )
    assert conflict.status == 409
    assert (await conflict.json())["code"] == "IDEMPOTENCY_CONFLICT"

    header_mismatch = await _session(
        client,
        pop_client,
        await _challenge(client, key, "session"),
        scopes,
        idem,
        headers={"Idempotency-Key": "idem-session-other-01"},
    )
    assert header_mismatch.status == 409
    assert (await header_mismatch.json())["code"] == "IDEMPOTENCY_CONFLICT"
    assert await _scalar(database_url, "SELECT count(*) FROM sessions") == 2


async def test_bad_signed_extension_is_rejected(auth_client):
    client, _, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    assert (await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))).status == 200
    proof, _ = pop_client.proof(
        await _challenge(client, key, "session"),
        op="auth.session",
        scope="session",
        extra={"requested_scopes": ["session:read"], "extensions": {"Bad": 1}},
        idempotency_key="idem-bad-extension-01",
    )
    response = await client.post(f"{MOBILE}/auth/session", json={"proof": proof})
    assert response.status == 400
    assert (await response.json())["code"] == "BAD_MESSAGE"


async def test_unknown_installation_session_and_unsigned_idempotency_header(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    challenge = await _challenge(client, key, "session")
    response = await _session(
        client,
        pop_client,
        challenge,
        ["session:read"],
        "idem-unknown-install-01",
    )
    assert response.status == 401
    assert (await response.json())["code"] == "PROOF_INVALID"
    assert await _scalar(database_url, "SELECT count(*) FROM sessions") == 0

    enroll_challenge = await _challenge(client, key, "enrollment")
    proof, _ = pop_client.proof(
        enroll_challenge,
        op="installations.create",
        scope="enrollment",
        extra={"public_key_spki_b64": key.spki_b64, "platform": "android", "name": None},
    )
    response = await client.post(
        f"{MOBILE}/installations",
        json={"public_key_spki_b64": key.spki_b64, "proof": proof},
        headers={"Idempotency-Key": "idem-unsigned-header-01"},
    )
    assert response.status == 400
    assert (await response.json())["code"] == "BAD_MESSAGE"


async def test_challenge_rate_limit_and_purpose_boundary(auth_client):
    client, _, _ = auth_client
    key = KeyMaterial()
    for _ in range(20):
        assert (await _challenge(client, key, "enrollment"))["single_use"] is True
    limited = await client.post(
        f"{MOBILE}/auth/challenge",
        json={
            "installation_fingerprint": key.fingerprint,
            "purpose": "enrollment",
            "environment": "test",
        },
    )
    assert limited.status == 429
    assert (await limited.json())["code"] == "RATE_LIMITED"

    not_enabled = await client.post(
        f"{MOBILE}/auth/challenge",
        json={
            "installation_fingerprint": key.fingerprint,
            "purpose": "binding",
            "environment": "test",
        },
    )
    assert not_enabled.status == 400
    assert (await not_enabled.json())["code"] == "BAD_MESSAGE"


async def test_rate_limit_is_configurable(migrated_url, settings_factory):
    client = await _start_client(settings_factory(migrated_url, challenge_rate_limit_per_minute=2))
    try:
        key = KeyMaterial()
        assert (await _challenge(client, key, "enrollment"))["single_use"] is True
        assert (await _challenge(client, key, "enrollment"))["single_use"] is True
        limited = await client.post(
            f"{MOBILE}/auth/challenge",
            json={
                "installation_fingerprint": key.fingerprint,
                "purpose": "enrollment",
                "environment": "test",
            },
        )
        assert limited.status == 429
    finally:
        await client.close()


async def test_stored_session_receipt_revalidates_live_state(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    assert (await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))).status == 200
    idem = "idem-revalidate-0001"
    first = await _session(client, pop_client, await _challenge(client, key, "session"), ["session:read"], idem)
    assert first.status == 200
    bearer = (await first.json())["session"]["session_id"]
    bearer_hash = hashlib.sha256(bearer.encode()).hexdigest()
    assert await _scalar(database_url, "SELECT count(*) FROM sessions") == 2

    await _execute(
        database_url,
        "UPDATE sessions SET expires_at = now() - interval '1 day' WHERE token_sha256 = $1",
        bearer_hash,
    )
    retry = await _session(client, pop_client, await _challenge(client, key, "session"), ["session:read"], idem)
    assert retry.status == 401
    body = await retry.json()
    assert body["code"] == "SESSION_EXPIRED"
    assert "session" not in body

    await _execute(
        database_url,
        "UPDATE sessions SET expires_at = now() + interval '1 day', revoked_at = now() WHERE token_sha256 = $1",
        bearer_hash,
    )
    retry = await _session(client, pop_client, await _challenge(client, key, "session"), ["session:read"], idem)
    assert retry.status == 401
    assert (await retry.json())["code"] == "SESSION_INVALID"

    await _execute(
        database_url,
        "UPDATE sessions SET revoked_at = NULL WHERE token_sha256 = $1",
        bearer_hash,
    )
    await _execute(database_url, "UPDATE installations SET state = 'revoked' WHERE public_key_fingerprint = $1", key.fingerprint)
    retry = await _session(client, pop_client, await _challenge(client, key, "session"), ["session:read"], idem)
    assert retry.status == 403
    assert (await retry.json())["code"] == "DEVICE_REVOKED"
    assert await _scalar(database_url, "SELECT count(*) FROM sessions") == 2


async def test_revoked_installation_cannot_enroll_again(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    idem = "idem-enroll-revoked-01"
    assert (await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))).status == 200
    await _execute(database_url, "UPDATE installations SET state = 'revoked' WHERE public_key_fingerprint = $1", key.fingerprint)

    proof, _ = pop_client.proof(
        await _challenge(client, key, "enrollment"),
        op="installations.create",
        scope="enrollment",
        extra={"public_key_spki_b64": key.spki_b64, "platform": "android", "name": None},
        idempotency_key=idem,
    )
    response = await client.post(
        f"{MOBILE}/installations",
        json={"public_key_spki_b64": key.spki_b64, "proof": proof},
    )
    assert response.status == 403
    assert (await response.json())["code"] == "DEVICE_REVOKED"

    fresh = await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))
    assert fresh.status == 403
    assert (await fresh.json())["code"] == "DEVICE_REVOKED"
    assert await _scalar(database_url, "SELECT count(*) FROM installations") == 1
    assert await _scalar(database_url, "SELECT count(*) FROM sessions") == 1


async def test_environments_do_not_share_idempotency(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    idem = "idem-cross-environment-01"
    test_proof, _ = pop_client.proof(
        await _challenge(client, key, "enrollment", "test"),
        op="installations.create",
        scope="enrollment",
        extra={"public_key_spki_b64": key.spki_b64, "platform": "android", "name": None},
        idempotency_key=idem,
    )
    response = await client.post(
        f"{MOBILE}/installations",
        json={"public_key_spki_b64": key.spki_b64, "proof": test_proof},
    )
    assert response.status == 200, await response.text()

    production = PoPClient(key, environment="production")
    prod_proof, _ = production.proof(
        await _challenge(client, key, "enrollment", "production"),
        op="installations.create",
        scope="enrollment",
        extra={"public_key_spki_b64": key.spki_b64, "platform": "android", "name": None},
        idempotency_key=idem,
    )
    response = await client.post(
        f"{MOBILE}/installations",
        json={"public_key_spki_b64": key.spki_b64, "proof": prod_proof},
    )
    assert response.status == 200, await response.text()
    assert await _scalar(database_url, "SELECT count(*) FROM installations WHERE environment = 'test'") == 1
    assert await _scalar(database_url, "SELECT count(*) FROM installations WHERE environment = 'production'") == 1
    environments = await _scalar(
        database_url,
        "SELECT count(*) FROM operation_receipts WHERE environment IN ('test','production') AND idempotency_key = $1",
        idem,
    )
    assert environments == 2


async def test_public_challenge_cap_blocks_random_fingerprints(migrated_url, settings_factory):
    settings = settings_factory(
        migrated_url, public_challenge_limit_per_minute=3, challenge_rate_limit_per_minute=100
    )
    client = await _start_client(settings)
    try:
        statuses = []
        for _ in range(5):
            fingerprint = secrets.token_hex(32)
            response = await client.post(
                f"{MOBILE}/auth/challenge",
                json={
                    "installation_fingerprint": fingerprint,
                    "purpose": "enrollment",
                    "environment": "test",
                },
            )
            statuses.append(response.status)
            if response.status == 429:
                body = await response.json()
                assert body["code"] == "RATE_LIMITED"
                assert body["retry_after_ms"] > 0
                assert response.headers.get("Retry-After") is not None
        assert statuses.count(200) == 3
        assert statuses.count(429) == 2
        assert await _scalar(migrated_url, "SELECT count(*) FROM auth_challenges") == 3
    finally:
        await client.close()


async def test_public_challenge_cap_is_atomic_under_concurrency(migrated_url, settings_factory):
    settings = settings_factory(
        migrated_url, public_challenge_limit_per_minute=3, challenge_rate_limit_per_minute=100
    )
    client = await _start_client(settings)
    try:
        async def issue() -> int:
            fingerprint = secrets.token_hex(32)
            response = await client.post(
                f"{MOBILE}/auth/challenge",
                json={
                    "installation_fingerprint": fingerprint,
                    "purpose": "enrollment",
                    "environment": "test",
                },
            )
            return response.status

        statuses = await asyncio.gather(*[issue() for _ in range(6)])
        assert statuses.count(200) == 3
        assert statuses.count(429) == 3
        assert await _scalar(migrated_url, "SELECT count(*) FROM auth_challenges") == 3
    finally:
        await client.close()


async def test_cleanup_purges_secret_result_and_keeps_tombstone_semantics(
    migrated_url, settings_factory
):
    settings = settings_factory(
        migrated_url,
        receipt_result_ttl_seconds=1,
        challenge_retention_seconds=1,
        idempotency_window_seconds=3600,
    )
    client = await _start_client(settings)
    key = KeyMaterial()
    pop_client = PoPClient(key)
    idem = "idem-cleanup-000001"
    try:
        assert (await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))).status == 200
        session = await _session(client, pop_client, await _challenge(client, key, "session"), ["session:read"], idem)
        assert session.status == 200
        assert await _scalar(
            migrated_url,
            "SELECT count(*) FROM operation_receipts WHERE result IS NOT NULL AND idempotency_key = $1",
            idem,
        ) == 1

        await asyncio.sleep(1.1)
        connection = await asyncpg.connect(migrated_url, timeout=10)
        try:
            result = await sweep_once(connection, settings)
        finally:
            await connection.close()
        assert result["purged_receipt_results"] >= 1
        assert await _scalar(
            migrated_url,
            "SELECT count(*) FROM operation_receipts WHERE result IS NULL AND retain_until > now() AND idempotency_key = $1",
            idem,
        ) == 1

        # Tombstone within the idempotency window: stable error, never a new effect.
        retry = await _session(client, pop_client, await _challenge(client, key, "session"), ["session:read"], idem)
        assert retry.status == 401
        assert (await retry.json())["code"] == "SESSION_EXPIRED"
        assert await _scalar(migrated_url, "SELECT count(*) FROM sessions") == 2

        # Expired challenges are removed after the retention grace; counters too.
        await _execute(
            migrated_url,
            """
            INSERT INTO auth_challenges
                (challenge_id, nonce_b64, installation_fingerprint, purpose, op, environment, expires_at)
            VALUES ($1, 'nonce', $2, 'enrollment', 'installations.create', 'test', now() - interval '2 seconds')
            """,
            secrets.token_hex(16),
            key.fingerprint,
        )
        await _execute(
            migrated_url,
            """
            INSERT INTO public_endpoint_counters (scope, window_start, count)
            VALUES ('mobile_auth_challenge', now() - interval '1 hour', 1)
            """,
        )
        connection = await asyncpg.connect(migrated_url, timeout=10)
        try:
            result = await sweep_once(connection, settings)
        finally:
            await connection.close()
        assert result["deleted_challenges"] >= 1
        assert result["deleted_rate_counters"] >= 1
        assert await _scalar(
            migrated_url,
            "SELECT count(*) FROM auth_challenges WHERE expires_at < now() - interval '1 second'",
        ) == 0
    finally:
        await client.close()


async def test_unlinked_session_scope_gate(auth_client):
    client, _, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    assert (await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))).status == 200

    denied = await _session(
        client,
        pop_client,
        await _challenge(client, key, "session"),
        ["account:read", "payment:write", "management-only", "devices:write", "access:sync"],
        "idem-scope-gate-0001",
    )
    assert denied.status == 403
    assert (await denied.json())["code"] == "ACCESS_DENIED"

    unknown = await _session(
        client,
        pop_client,
        await _challenge(client, key, "session"),
        ["bogus:scope"],
        "idem-scope-gate-0002",
    )
    assert unknown.status == 400
    assert (await unknown.json())["code"] == "WRONG_SCOPE"

    allowed = await _session(
        client,
        pop_client,
        await _challenge(client, key, "session"),
        ["session:read", "management-only"],
        "idem-scope-gate-0003",
    )
    assert allowed.status == 200
    assert (await allowed.json())["session"]["scopes"] == ["session:read", "management-only"]


async def test_legacy_receipt_refuses_new_effect_inside_window(migrated_url, settings_factory):
    client = await _start_client(settings_factory(migrated_url))
    key = KeyMaterial()
    pop_client = PoPClient(key)
    try:
        assert (await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))).status == 200
        idem = "idem-legacy-guard-01"
        scopes = ["session:read"]
        challenge = await _challenge(client, key, "session")
        _, payload = pop_client.proof(
            challenge,
            op="auth.session",
            scope="session",
            extra={"requested_scopes": scopes},
            idempotency_key=idem,
        )
        digest = pop.business_digest(payload)
        connection = await asyncpg.connect(migrated_url, timeout=10)
        try:
            await connection.execute(
                """
                INSERT INTO operation_receipts
                    (installation_ref, op, idempotency_key, business_digest, result,
                     environment, retain_until)
                VALUES ($1, 'auth.session', $2, $3, NULL, NULL, now() + interval '7 days')
                """,
                key.fingerprint,
                idem,
                digest,
            )
        finally:
            await connection.close()
        sessions_before = await _scalar(migrated_url, "SELECT count(*) FROM sessions")

        async def retry() -> tuple[int, dict]:
            fresh_challenge = await _challenge(client, key, "session")
            proof, _ = pop_client.proof(
                fresh_challenge,
                op="auth.session",
                scope="session",
                extra={"requested_scopes": scopes},
                idempotency_key=idem,
            )
            response = await client.post(f"{MOBILE}/auth/session", json={"proof": proof})
            return response.status, await response.json()

        status, body = await retry()
        assert status == 409, body
        assert body["code"] == "IDEMPOTENCY_CONFLICT"
        assert body["details"]["reason"] == "legacy_receipt_environment_unknown"
        assert "session" not in body
        assert await _scalar(migrated_url, "SELECT count(*) FROM sessions") == sessions_before
        assert await _scalar(
            migrated_url,
            "SELECT count(*) FROM operation_receipts WHERE environment = 'test' AND idempotency_key = $1",
            idem,
        ) == 0

        # Any digest under the same key is refused, not only the matching one.
        await _execute(
            migrated_url,
            "UPDATE operation_receipts SET business_digest = $2 WHERE environment IS NULL AND idempotency_key = $1",
            idem,
            "0" * 64,
        )
        status, body = await retry()
        assert status == 409
        assert body["details"]["reason"] == "legacy_receipt_environment_unknown"
        assert await _scalar(migrated_url, "SELECT count(*) FROM sessions") == sessions_before

        # After the explicitly verified legacy window has passed, the documented policy allows
        # a new effect (the legacy tombstone no longer blocks).
        await _execute(
            migrated_url,
            "UPDATE operation_receipts SET retain_until = now() - interval '1 second' WHERE environment IS NULL AND idempotency_key = $1",
            idem,
        )
        status, body = await retry()
        assert status == 200, body
        assert "session" in body
        assert await _scalar(migrated_url, "SELECT count(*) FROM sessions") == sessions_before + 1
    finally:
        await client.close()


async def test_down_0004_refuses_with_environment_receipts_and_preserves_state(migrated_url):
    connection = await asyncpg.connect(migrated_url, timeout=10)
    key = KeyMaterial()
    try:
        await connection.execute(
            """
            INSERT INTO operation_receipts
                (installation_ref, op, idempotency_key, business_digest, result, environment,
                 result_expires_at, retain_until)
            VALUES ($1, 'auth.session', 'idem-down-probe-01', repeat('a', 64), '{"session":{}}'::jsonb,
                    'test', now() + interval '1 hour', now() + interval '7 days'),
                   ($1, 'auth.session', 'idem-down-probe-01', repeat('b', 64), '{"session":{}}'::jsonb,
                    'production', now() + interval '1 hour', now() + interval '7 days'),
                   ($1, 'auth.session', 'idem-legacy-down-01', repeat('c', 64), NULL,
                    NULL, NULL, now() + interval '7 days')
            """,
            key.fingerprint,
        )
        before = await connection.fetchrow(
            "SELECT count(*) AS total, count(*) FILTER (WHERE environment IS NOT NULL) AS env_rows FROM operation_receipts"
        )
        index_before = await connection.fetchval(
            "SELECT count(*) FROM pg_indexes WHERE indexname = 'operation_receipts_env_key_idx'"
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0004"):
            await runner.rollback_migration(connection, "0004_receipt_environment_and_retention")
        after = await connection.fetchrow(
            "SELECT count(*) AS total, count(*) FILTER (WHERE environment IS NOT NULL) AS env_rows FROM operation_receipts"
        )
        assert dict(after) == dict(before)
        assert dict(after)["env_rows"] == 2
        assert await connection.fetchval(
            "SELECT count(*) FROM pg_indexes WHERE indexname = 'operation_receipts_env_key_idx'"
        ) == index_before == 1
        status = await runner.migration_status(connection)
        assert "0004_receipt_environment_and_retention" in status["applied"]
    finally:
        await connection.close()


async def test_down_0004_safe_when_only_legacy_receipts(migrated_url):
    connection = await asyncpg.connect(migrated_url, timeout=10)
    try:
        await connection.execute(
            """
            INSERT INTO operation_receipts
                (installation_ref, op, idempotency_key, business_digest, result,
                 environment, retain_until)
            VALUES ('fingerprint', 'auth.session', 'idem-legacy-safe-down-01', repeat('d', 64),
                    NULL, NULL, now() + interval '7 days')
            """
        )
        await runner.rollback_migration(connection, "0004_receipt_environment_and_retention")
        assert await connection.fetchval("SELECT count(*) FROM operation_receipts") == 1
        assert await connection.fetchval(
            """
            SELECT count(*) FROM information_schema.columns
            WHERE table_name = 'operation_receipts' AND column_name = 'environment'
            """
        ) == 0
        assert await connection.fetchval(
            """
            SELECT count(*) FROM pg_constraint
            WHERE conrelid = 'operation_receipts'::regclass AND contype = 'u'
            """
        ) == 1
        status = await runner.migration_status(connection)
        assert "0004_receipt_environment_and_retention" in status["pending"]

        assert await runner.apply_migrations(connection) == ["0004_receipt_environment_and_retention"]
        assert await connection.fetchval("SELECT count(*) FROM operation_receipts") == 1
    finally:
        await connection.close()


async def _add_trusted_link(
    database_url: str,
    fingerprint: str,
    *,
    telegram_id: int = 700000001,
    account_status: str = "verified",
    binding_status: str = "active",
) -> str:
    installation_id = await _scalar(
        database_url,
        "SELECT id FROM installations WHERE public_key_fingerprint = $1",
        fingerprint,
    )
    account_id = await _scalar(
        database_url,
        "INSERT INTO accounts (status, telegram_id) VALUES ($1, $2) RETURNING id",
        account_status,
        telegram_id,
    )
    await _execute(
        database_url,
        """
        INSERT INTO account_bindings (account_id, installation_id, status)
        VALUES ($1, $2, $3)
        """,
        account_id,
        installation_id,
        binding_status,
    )
    return str(account_id)


async def test_linked_session_requires_proven_server_side_link(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    challenge = await _challenge(client, key, "enrollment")
    enrollment = await _enroll(client, pop_client, challenge)
    assert enrollment.status == 200, await enrollment.text()

    # Unlinked: the linked scope set stays unavailable (existing behaviour unchanged).
    unlinked_challenge = await _challenge(client, key, "session")
    denied = await _session(
        client,
        pop_client,
        unlinked_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000001",
    )
    assert denied.status == 403
    assert (await denied.json())["details"]["reason"] == "scope_not_available_for_account_state"

    account_id = await _add_trusted_link(database_url, key.fingerprint)
    linked_challenge = await _challenge(client, key, "session")
    linked = await _session(
        client,
        pop_client,
        linked_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000002",
    )
    assert linked.status == 200, await linked.text()
    linked_body = await linked.json()
    assert linked_body["session"]["account_ref"] == account_id
    assert set(linked_body["session"]["scopes"]) == {"session:read", "access:sync"}
    token = linked_body["session"]["session_id"]

    # A session is still not a VPN right: /me resolves the live binding state.
    me = await client.get(f"{MOBILE}/me", headers={"Authorization": f"Bearer {token}"})
    assert me.status == 200, await me.text()
    me_body = await me.json()
    assert me_body["binding_status"] == "active"
    assert me_body["binding_revision"] == "1"
    row = await _scalar(
        database_url,
        "SELECT account_id FROM sessions WHERE token_sha256 = encode(sha256($1::bytea), 'hex')",
        token.encode(),
    )
    assert str(row) == account_id


async def test_untrusted_or_ambiguous_link_never_elevates(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    challenge = await _challenge(client, key, "enrollment")
    assert (await _enroll(client, pop_client, challenge)).status == 200

    # Active binding, but no proven trusted login (no Telegram identity).
    await _add_trusted_link(database_url, key.fingerprint, telegram_id=None)
    untrusted_challenge = await _challenge(client, key, "session")
    untrusted = await _session(
        client,
        pop_client,
        untrusted_challenge,
        ["access:sync"],
        "linked-key-00000003",
    )
    assert untrusted.status == 403

    # Two active bindings are ambiguous: no elevation.
    installation_id = await _scalar(
        database_url,
        "SELECT id FROM installations WHERE public_key_fingerprint = $1",
        key.fingerprint,
    )
    other_account = await _scalar(
        database_url,
        "INSERT INTO accounts (status, telegram_id) VALUES ('verified', 700000009) RETURNING id",
    )
    await _execute(
        database_url,
        "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1, $2, 'active')",
        other_account,
        installation_id,
    )
    ambiguous_challenge = await _challenge(client, key, "session")
    ambiguous = await _session(
        client,
        pop_client,
        ambiguous_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000004",
    )
    assert ambiguous.status == 403

    # Revoked link: no elevation either.
    await _execute(
        database_url,
        "UPDATE account_bindings SET status = 'revoked' WHERE installation_id = $1",
        installation_id,
    )
    revoked_challenge = await _challenge(client, key, "session")
    revoked = await _session(
        client,
        pop_client,
        revoked_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000005",
    )
    assert revoked.status == 403
    unlinked_read = await _session(
        client,
        pop_client,
        revoked_challenge,
        ["session:read"],
        "linked-key-00000006",
    )
    assert unlinked_read.status == 200
    assert (await unlinked_read.json())["session"]["account_ref"] is None


async def test_refreshed_receipt_rejects_changed_link(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    challenge = await _challenge(client, key, "enrollment")
    assert (await _enroll(client, pop_client, challenge)).status == 200
    account_id = await _add_trusted_link(database_url, key.fingerprint, telegram_id=700000021)

    first_challenge = await _challenge(client, key, "session")
    first = await _session(
        client,
        pop_client,
        first_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000007",
    )
    assert first.status == 200, await first.text()
    first_body = await first.json()
    old_token = first_body["session"]["session_id"]
    assert first_body["session"]["account_ref"] == account_id

    # Revoke the binding, then replay the same idempotency key with a fresh challenge.
    installation_id = await _scalar(
        database_url,
        "SELECT id FROM installations WHERE public_key_fingerprint = $1",
        key.fingerprint,
    )
    await _execute(
        database_url,
        "UPDATE account_bindings SET status = 'revoked' WHERE installation_id = $1",
        installation_id,
    )
    replay_challenge = await _challenge(client, key, "session")
    replay = await _session(
        client,
        pop_client,
        replay_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000007",
    )
    # Fresh authorization runs first: the revoked link no longer admits the linked scope set,
    # and the stored receipt is never consulted for an unauthorized request.
    assert replay.status == 403, await replay.text()
    replay_body = await replay.json()
    assert replay_body["details"]["reason"] == "scope_not_available_for_account_state"
    assert old_token not in json.dumps(replay_body)

    # Replacement to a new subject is also rejected for the old key; a NEW key follows the
    # live authoritative link.
    replacement = await _add_trusted_link(database_url, key.fingerprint, telegram_id=700000022)
    replacement_challenge = await _challenge(client, key, "session")
    replacement_replay = await _session(
        client,
        pop_client,
        replacement_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000007",
    )
    # The link is authorized again but belongs to a different subject: the stored receipt is
    # re-authorized against the live link and the old subject's bearer is never returned.
    assert replacement_replay.status == 401
    assert (await replacement_replay.json())["details"]["reason"] == "binding_changed"
    new_challenge = await _challenge(client, key, "session")
    fresh = await _session(
        client,
        pop_client,
        new_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000008",
    )
    assert fresh.status == 200, await fresh.text()
    assert (await fresh.json())["session"]["account_ref"] == replacement

    # Deleting the link never resurrects the old key.
    await _execute(
        database_url,
        "DELETE FROM account_bindings WHERE installation_id = $1",
        installation_id,
    )
    deleted_challenge = await _challenge(client, key, "session")
    deleted_replay = await _session(
        client,
        pop_client,
        deleted_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000008",
    )
    assert deleted_replay.status == 403
    no_link_challenge = await _challenge(client, key, "session")
    no_link = await _session(
        client,
        pop_client,
        no_link_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000009",
    )
    assert no_link.status == 403


async def test_concurrent_linked_sessions_share_one_receipt(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    challenge = await _challenge(client, key, "enrollment")
    assert (await _enroll(client, pop_client, challenge)).status == 200
    account_id = await _add_trusted_link(database_url, key.fingerprint, telegram_id=700000031)

    challenges = [
        await _challenge(client, key, "session"),
        await _challenge(client, key, "session"),
    ]
    responses = await asyncio.gather(
        *[
            _session(
                client,
                pop_client,
                item,
                ["session:read", "access:sync"],
                "linked-key-00000010",
            )
            for item in challenges
        ]
    )
    assert all(response.status in (200, 401) for response in responses)
    bodies = [await response.json() for response in responses]
    tokens = {
        body["session"]["session_id"] for body in bodies if response_ok(body)
    }
    assert len(tokens) == 1
    assert bodies[0]["session"]["account_ref"] == account_id
    assert await _scalar(
        database_url,
        "SELECT count(*) FROM sessions WHERE account_id = $1",
        account_id,
    ) == 1


def response_ok(body: dict) -> bool:
    return body.get("status") == "ok"


async def test_same_account_replacement_rejects_stale_bearer_and_receipt(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    challenge = await _challenge(client, key, "enrollment")
    assert (await _enroll(client, pop_client, challenge)).status == 200
    account_id = await _add_trusted_link(database_url, key.fingerprint, telegram_id=700000051)

    first_challenge = await _challenge(client, key, "session")
    first = await _session(
        client,
        pop_client,
        first_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000011",
    )
    assert first.status == 200, await first.text()
    old_token = (await first.json())["session"]["session_id"]
    installation_id = await _scalar(
        database_url,
        "SELECT id FROM installations WHERE public_key_fingerprint = $1",
        key.fingerprint,
    )
    old_binding = await _scalar(
        database_url,
        "SELECT binding_id FROM sessions WHERE token_sha256 = encode(sha256($1::bytea), 'hex')",
        old_token.encode(),
    )
    assert old_binding is not None
    headers = {"Authorization": f"Bearer {old_token}"}
    assert (await client.get(f"{MOBILE}/me", headers=headers)).status == 200

    # Same account, replaced binding row: delete + insert.
    await _execute(database_url, "DELETE FROM account_bindings WHERE id = $1", old_binding)
    new_binding = await _scalar(
        database_url,
        """
        INSERT INTO account_bindings (account_id, installation_id, status)
        VALUES ($1, $2, 'active') RETURNING id
        """,
        account_id,
        installation_id,
    )
    assert str(new_binding) != str(old_binding)

    # The old bearer is not resurrected by the new binding row (FK sets the fence to NULL).
    stale = await client.get(f"{MOBILE}/me", headers=headers)
    assert stale.status == 401
    assert (await stale.json())["code"] == "SESSION_INVALID"

    replay_challenge = await _challenge(client, key, "session")
    replay = await _session(
        client,
        pop_client,
        replay_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000011",
    )
    assert replay.status == 401
    assert (await replay.json())["details"]["reason"] == "binding_changed"
    assert old_token not in json.dumps(await replay.json())

    fresh_challenge = await _challenge(client, key, "session")
    fresh = await _session(
        client,
        pop_client,
        fresh_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000012",
    )
    assert fresh.status == 200, await fresh.text()
    fresh_body = await fresh.json()
    assert fresh_body["session"]["account_ref"] == account_id
    fresh_token = fresh_body["session"]["session_id"]
    fresh_row = await _scalar(
        database_url,
        "SELECT binding_id FROM sessions WHERE token_sha256 = encode(sha256($1::bytea), 'hex')",
        fresh_token.encode(),
    )
    assert str(fresh_row) == str(new_binding)
    assert (await client.get(f"{MOBILE}/me", headers={"Authorization": f"Bearer {fresh_token}"})).status == 200


async def test_generation_bump_rejects_stale_bearer_and_receipt(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    challenge = await _challenge(client, key, "enrollment")
    assert (await _enroll(client, pop_client, challenge)).status == 200
    account_id = await _add_trusted_link(database_url, key.fingerprint, telegram_id=700000052)

    first_challenge = await _challenge(client, key, "session")
    first = await _session(
        client,
        pop_client,
        first_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000013",
    )
    assert first.status == 200, await first.text()
    old_token = (await first.json())["session"]["session_id"]

    installation_id = await _scalar(
        database_url,
        "SELECT id FROM installations WHERE public_key_fingerprint = $1",
        key.fingerprint,
    )
    await _execute(
        database_url,
        "UPDATE account_bindings SET generation = generation + 1 WHERE installation_id = $1",
        installation_id,
    )
    stale = await client.get(f"{MOBILE}/me", headers={"Authorization": f"Bearer {old_token}"})
    assert stale.status == 401

    replay_challenge = await _challenge(client, key, "session")
    replay = await _session(
        client,
        pop_client,
        replay_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000013",
    )
    assert replay.status == 401
    assert (await replay.json())["details"]["reason"] == "binding_changed"

    fresh_challenge = await _challenge(client, key, "session")
    fresh = await _session(
        client,
        pop_client,
        fresh_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000014",
    )
    assert fresh.status == 200, await fresh.text()
    fresh_body = await fresh.json()
    assert fresh_body["session"]["account_ref"] == account_id
    me = await client.get(
        f"{MOBILE}/me", headers={"Authorization": f"Bearer {fresh_body['session']['session_id']}"}
    )
    assert me.status == 200
    assert (await me.json())["binding_revision"] == "2"
    generation = await _scalar(
        database_url,
        "SELECT binding_generation FROM sessions WHERE token_sha256 = encode(sha256($1::bytea), 'hex')",
        fresh_body["session"]["session_id"].encode(),
    )
    assert generation == 2


async def test_stale_receipt_waits_on_link_lock_before_rejecting(auth_client):
    client, database_url, _ = auth_client
    key = KeyMaterial()
    pop_client = PoPClient(key)
    challenge = await _challenge(client, key, "enrollment")
    assert (await _enroll(client, pop_client, challenge)).status == 200
    await _add_trusted_link(database_url, key.fingerprint, telegram_id=700000053)

    first_challenge = await _challenge(client, key, "session")
    first = await _session(
        client,
        pop_client,
        first_challenge,
        ["session:read", "access:sync"],
        "linked-key-00000015",
    )
    assert first.status == 200, await first.text()
    installation_id = await _scalar(
        database_url,
        "SELECT id FROM installations WHERE public_key_fingerprint = $1",
        key.fingerprint,
    )

    # Hold the binding lock, start an identical replay, bump the generation, then release:
    # the replay must serialize behind the lock and then fail on the changed fence.
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await connection.execute("BEGIN")
        await connection.fetchrow(
            "SELECT id FROM account_bindings WHERE installation_id = $1 FOR UPDATE",
            installation_id,
        )
        replay_challenge = await _challenge(client, key, "session")
        task = asyncio.create_task(
            _session(
                client,
                pop_client,
                replay_challenge,
                ["session:read", "access:sync"],
                "linked-key-00000015",
            )
        )
        await asyncio.sleep(0.3)
        assert not task.done(), "replay must wait on the binding row lock"
        await connection.execute(
            "UPDATE account_bindings SET generation = generation + 1 WHERE installation_id = $1",
            installation_id,
        )
        await connection.execute("COMMIT")
        replay = await task
    finally:
        await connection.close()
    assert replay.status == 401
    assert (await replay.json())["details"]["reason"] == "binding_changed"
