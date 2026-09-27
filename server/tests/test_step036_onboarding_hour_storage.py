"""Slice A tests: durable onboarding-hour storage/admission (disposable DB only)."""
from __future__ import annotations

import asyncio
import base64
import json
import shutil
import tempfile
import uuid
from pathlib import Path

import asyncpg
import pytest

from terlimo_backend.migrations import runner
from terlimo_backend.onboarding_hour import (
    BOOTSTRAP_REVOKE_OPERATION,
    GatewayContext,
    OnboardingError,
    OnboardingHourHandlers,
    admit_evidence,
    create_intent,
    expire_intents,
    intent_response,
    revoke_intent,
)

PIN = base64.urlsafe_b64encode(b"p" * 32).rstrip(b"=").decode()


class FakeCipher:
    def encrypt(self, plaintext: str) -> bytes:
        return base64.b64encode(plaintext.encode("utf-8"))

    def decrypt(self, ciphertext: bytes) -> str:
        return base64.b64decode(bytes(ciphertext)).decode("utf-8")


class FakeBootstrapClient:
    def __init__(self) -> None:
        self.provisions: list[dict] = []
        self.revokes: list[str] = []

    async def bootstrap_provision(self, *, credential_id, secret, expires_at, node_id):
        self.provisions.append(
            {"credential_id": credential_id, "secret": secret, "node_id": node_id}
        )
        return {"ok": True}

    async def bootstrap_revoke(self, *, credential_id):
        self.revokes.append(credential_id)


async def _connect(url: str) -> asyncpg.Connection:
    connection = await asyncpg.connect(url, timeout=10)
    await connection.set_type_codec(
        "jsonb", schema="pg_catalog", encoder=json.dumps, decoder=json.loads
    )
    return connection


async def _seed_installation(database_url: str) -> uuid.UUID:
    connection = await _connect(database_url)
    try:
        return await connection.fetchval(
            """
            INSERT INTO installations
                (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test', 'android', $1, 'spki', 'technical') RETURNING id
            """,
            uuid.uuid4().hex,
        )
    finally:
        await connection.close()


async def _seed_gateway(database_url: str, key: str = "terlimo-035-node") -> tuple[uuid.UUID, dict]:
    endpoints = {
        "node_id": key,
        "peer_ip": "127.0.0.1",
        "dtls_port": 57400,
        "wg_port": 57401,
        "dtls_spki_sha256": PIN,
        "target_workers": 36,
    }
    connection = await _connect(database_url)
    try:
        gateway_id = await connection.fetchval(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, capabilities,
                                  registry_state, confirmed_max_workers)
            VALUES ($1, 'test', $2::jsonb, '["managed"]'::jsonb, 'registered', 36)
            RETURNING id
            """,
            key,
            json.dumps(endpoints),
        )
        return gateway_id, endpoints
    finally:
        await connection.close()


async def _counts(database_url: str) -> dict[str, int]:
    connection = await _connect(database_url)
    try:
        return {
            table: await connection.fetchval(f"SELECT count(*) FROM {table}")
            for table in (
                "onboarding_intents",
                "onboarding_evidence",
                "outbox_operations",
                "entitlements",
            )
        }
    finally:
        await connection.close()


def _handlers(client: FakeBootstrapClient) -> OnboardingHourHandlers:
    return OnboardingHourHandlers(
        cipher=FakeCipher(), client_factory=lambda _key, _endpoints: client
    )


async def _provision(database_url: str, intent_id: str, client: FakeBootstrapClient) -> None:
    connection = await _connect(database_url)
    try:
        operation = await connection.fetchrow(
            "SELECT * FROM outbox_operations WHERE payload->>'intent_id' = $1", intent_id
        )
        await _handlers(client).bootstrap_provision(connection, operation)
    finally:
        await connection.close()


async def test_migration_preflight_fails_on_duplicate_hours(database_url):
    real_dir = runner.VERSIONS_DIR
    with tempfile.TemporaryDirectory() as tmp:
        partial = Path(tmp)
        for path in sorted(real_dir.glob("*.sql")):
            if path.name.startswith(("0017", "0019", "0020")):
                continue
            shutil.copy2(path, partial / path.name)
        connection = await _connect(database_url)
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
            for _ in range(2):
                await connection.execute(
                    """
                    INSERT INTO entitlements
                        (account_id, installation_id, kind, status, starts_at, ends_at)
                    VALUES (NULL, $1, 'onboarding_hour', 'active', now(), now() + interval '1 hour')
                    """,
                    installation_id,
                )
            with pytest.raises(asyncpg.RaiseError, match="MIGRATION_UNSAFE_0017"):
                await runner.apply_migrations(connection)
        finally:
            await connection.close()


async def test_intent_idempotency_and_active_conflict(migrated_url):
    installation_id = await _seed_installation(migrated_url)
    await _seed_gateway(migrated_url)
    connection = await _connect(migrated_url)
    try:
        first = await create_intent(
            connection,
            installation_id=installation_id,
            request_key="intent-key-1",
            environment="test",
            cipher=FakeCipher(),
        )
        repeat = await create_intent(
            connection,
            installation_id=installation_id,
            request_key="intent-key-1",
            environment="test",
            cipher=FakeCipher(),
        )
        assert first["id"] == repeat["id"] and first["credential_id"] == repeat["credential_id"]
        assert first["state"] == "pending"
        with pytest.raises(OnboardingError) as conflict:
            await create_intent(
                connection,
                installation_id=installation_id,
                request_key="intent-key-2",
                environment="test",
                cipher=FakeCipher(),
            )
        assert conflict.value.code == "ONBOARDING_UNIT_ACTIVE"
        assert await connection.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE operation_type='gateway.bootstrap_provision'"
        ) == 1
    finally:
        await connection.close()


async def test_intent_creation_is_atomic_with_outbox(migrated_url, monkeypatch):
    installation_id = await _seed_installation(migrated_url)
    await _seed_gateway(migrated_url)
    from terlimo_backend import onboarding_hour as module

    async def broken_enqueue(*args, **kwargs):
        raise RuntimeError("outbox unavailable")

    monkeypatch.setattr(module, "_enqueue", broken_enqueue)
    connection = await _connect(migrated_url)
    try:
        with pytest.raises(RuntimeError):
            await create_intent(
                connection,
                installation_id=installation_id,
                request_key="intent-key-atomic",
                environment="test",
                cipher=FakeCipher(),
            )
        counts = await _counts(migrated_url)
        assert counts["onboarding_intents"] == 0
        assert counts["outbox_operations"] == 0
    finally:
        await connection.close()


async def test_intent_response_states_secret_delivery_and_wipe(migrated_url):
    installation_id = await _seed_installation(migrated_url)
    await _seed_gateway(migrated_url)
    connection = await _connect(migrated_url)
    client = FakeBootstrapClient()
    try:
        created = await create_intent(
            connection,
            installation_id=installation_id,
            request_key="intent-key-3",
            environment="test",
            cipher=FakeCipher(),
        )
        pending = await intent_response(
            connection, installation_id=installation_id, request_key="intent-key-3", cipher=FakeCipher()
        )
        assert pending["state"] == "pending" and "secret" not in pending
        assert pending["retry_after"] >= 1

        await _provision(migrated_url, created["id"], client)
        ready = await intent_response(
            connection, installation_id=installation_id, request_key="intent-key-3", cipher=FakeCipher()
        )
        assert ready["state"] == "ready" and len(ready["secret"]) >= 32
        stored = await connection.fetchrow(
            "SELECT secret_enc, secret_hash FROM onboarding_intents WHERE id = $1", created["id"]
        )
        assert stored["secret_enc"] is not None and stored["secret_hash"]

        evidence = await admit_evidence(
            connection,
            caller=GatewayContext(
                gateway_id=uuid.UUID(created["gateway_id"]),
                gateway_key="terlimo-035-node",
                environment="test",
            ),
            credential_id=created["credential_id"],
            connection_id="conn-1",
            request_id="req-1",
        )
        assert evidence["state"] == "started" and evidence["replay"] is False
        wiped = await connection.fetchrow(
            "SELECT secret_enc, secret_hash, state FROM onboarding_intents WHERE id = $1",
            created["id"],
        )
        assert wiped["state"] == "started" and wiped["secret_enc"] is None and wiped["secret_hash"]
        started = await intent_response(
            connection, installation_id=installation_id, request_key="intent-key-3", cipher=FakeCipher()
        )
        assert started["state"] == "started" and started["started_at"] and "secret" not in started

        # replay after expiry returns the historical start and never a new right
        await connection.execute("UPDATE entitlements SET ends_at = now() - interval '1 hour'")
        replay = await admit_evidence(
            connection,
            caller=GatewayContext(
                gateway_id=uuid.UUID(created["gateway_id"]),
                gateway_key="terlimo-035-node",
                environment="test",
            ),
            credential_id=created["credential_id"],
            connection_id="conn-1",
            request_id="req-1",
        )
        assert replay["replay"] is True and replay["started_at"] == evidence["started_at"]
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 1 and counts["entitlements"] == 1
    finally:
        await connection.close()


async def test_evidence_concurrent_once(migrated_url):
    installation_id = await _seed_installation(migrated_url)
    gateway_id, _ = await _seed_gateway(migrated_url)
    connection = await _connect(migrated_url)
    try:
        created = await create_intent(
            connection,
            installation_id=installation_id,
            request_key="intent-key-4",
            environment="test",
            cipher=FakeCipher(),
        )
    finally:
        await connection.close()
    await _provision(migrated_url, created["id"], FakeBootstrapClient())
    caller = GatewayContext(gateway_id=gateway_id, gateway_key="terlimo-035-node", environment="test")

    async def attempt(request_id: str):
        connection = await _connect(migrated_url)
        try:
            return await admit_evidence(
                connection,
                caller=caller,
                credential_id=created["credential_id"],
                connection_id=f"conn-{request_id}",
                request_id=request_id,
            )
        finally:
            await connection.close()

    results = await asyncio.gather(attempt("a"), attempt("b"), return_exceptions=True)
    assert all(not isinstance(item, Exception) for item in results), results
    counts = await _counts(migrated_url)
    assert counts["onboarding_evidence"] == 1 and counts["entitlements"] == 1


async def test_evidence_rejects_invalid_binding_ttl_and_not_ready(migrated_url):
    installation_id = await _seed_installation(migrated_url)
    gateway_id, _ = await _seed_gateway(migrated_url)
    connection = await _connect(migrated_url)
    try:
        created = await create_intent(
            connection,
            installation_id=installation_id,
            request_key="intent-key-5",
            environment="test",
            cipher=FakeCipher(),
        )
        good = GatewayContext(gateway_id=gateway_id, gateway_key="terlimo-035-node", environment="test")
        with pytest.raises(OnboardingError) as unknown:
            await admit_evidence(
                connection, caller=good, credential_id="nope", connection_id="c", request_id="r"
            )
        assert unknown.value.code == "ONBOARDING_CREDENTIAL_UNKNOWN"
        with pytest.raises(OnboardingError) as mismatch:
            await admit_evidence(
                connection,
                caller=GatewayContext(gateway_id=gateway_id, gateway_key="other", environment="test"),
                credential_id=created["credential_id"],
                connection_id="c",
                request_id="r",
            )
        assert mismatch.value.code == "ONBOARDING_ENV_MISMATCH"
        with pytest.raises(OnboardingError) as not_ready:
            await admit_evidence(
                connection,
                caller=good,
                credential_id=created["credential_id"],
                connection_id="c",
                request_id="r",
            )
        assert not_ready.value.code == "ONBOARDING_GATEWAY_NOT_READY"
        await connection.execute(
            "UPDATE onboarding_intents SET expires_at = now() - interval '1 second' WHERE id = $1",
            created["id"],
        )
        with pytest.raises(OnboardingError) as expired:
            await admit_evidence(
                connection,
                caller=good,
                credential_id=created["credential_id"],
                connection_id="c",
                request_id="r",
            )
        assert expired.value.code == "ONBOARDING_INTENT_EXPIRED"
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0
    finally:
        await connection.close()


async def test_revoke_race_and_after_start_semantics(migrated_url):
    installation_id = await _seed_installation(migrated_url)
    gateway_id, _ = await _seed_gateway(migrated_url)
    connection = await _connect(migrated_url)
    client = FakeBootstrapClient()
    try:
        first = await create_intent(
            connection,
            installation_id=installation_id,
            request_key="intent-key-6",
            environment="test",
            cipher=FakeCipher(),
        )
        revoked = await revoke_intent(connection, intent_id=uuid.UUID(first["id"]))
        assert revoked["revoked"] is True
        with pytest.raises(OnboardingError) as race:
            await admit_evidence(
                connection,
                caller=GatewayContext(gateway_id=gateway_id, gateway_key="terlimo-035-node", environment="test"),
                credential_id=first["credential_id"],
                connection_id="c",
                request_id="r",
            )
        assert race.value.code == "ONBOARDING_INTENT_REVOKED"
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0

        # late provisioning on the revoked intent must not revive it and must request a revoke
        await _provision(migrated_url, first["id"], client)
        state = await connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", uuid.UUID(first["id"])
        )
        assert state == "revoked"
        assert await connection.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE operation_type = $1",
            BOOTSTRAP_REVOKE_OPERATION,
        ) == 1

        # after start: bootstrap revoke does not touch the hour
        second = await create_intent(
            connection,
            installation_id=installation_id,
            request_key="intent-key-7",
            environment="test",
            cipher=FakeCipher(),
        )
        await _provision(migrated_url, second["id"], client)
        await admit_evidence(
            connection,
            caller=GatewayContext(gateway_id=gateway_id, gateway_key="terlimo-035-node", environment="test"),
            credential_id=second["credential_id"],
            connection_id="c2",
            request_id="r2",
        )
        after = await revoke_intent(connection, intent_id=uuid.UUID(second["id"]))
        assert after["revoked"] is False
        assert await connection.fetchval("SELECT status FROM entitlements") == "active"
        assert await connection.fetchval("SELECT state FROM onboarding_intents WHERE id = $1", uuid.UUID(second["id"])) == "started"
    finally:
        await connection.close()


async def test_expire_intents_wipes_secret(migrated_url):
    installation_id = await _seed_installation(migrated_url)
    await _seed_gateway(migrated_url)
    connection = await _connect(migrated_url)
    try:
        created = await create_intent(
            connection,
            installation_id=installation_id,
            request_key="intent-key-8",
            environment="test",
            cipher=FakeCipher(),
        )
        await connection.execute(
            "UPDATE onboarding_intents SET expires_at = now() - interval '1 second' WHERE id = $1",
            created["id"],
        )
        assert await expire_intents(connection) == 1
        row = await connection.fetchrow(
            "SELECT state, secret_enc, secret_hash FROM onboarding_intents WHERE id = $1",
            created["id"],
        )
        assert row["state"] == "expired" and row["secret_enc"] is None and row["secret_hash"]
    finally:
        await connection.close()


async def _seed_commercial(database_url: str, installation_id: uuid.UUID, kind: str = "paid") -> uuid.UUID:
    connection = await _connect(database_url)
    try:
        account = await connection.fetchval(
            "INSERT INTO accounts (status) VALUES ('verified') RETURNING id"
        )
        await connection.execute(
            """
            INSERT INTO account_bindings (account_id, installation_id, status, generation)
            VALUES ($1, $2, 'active', 1)
            """,
            account,
            installation_id,
        )
        entitlement = await connection.fetchval(
            """
            INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at, device_limit, revision)
            VALUES ($1, $2, 'active', now() - interval '1 hour', now() + interval '1 day', 2, 1)
            RETURNING id
            """,
            account,
            kind,
        )
        return entitlement
    finally:
        await connection.close()


async def _revoke_op_count(database_url: str, credential_id: str) -> int:
    connection = await _connect(database_url)
    try:
        return int(
            await connection.fetchval(
                "SELECT count(*) FROM outbox_operations WHERE operation_type = $1 AND payload->>'credential_id' = $2",
                BOOTSTRAP_REVOKE_OPERATION,
                credential_id,
            )
        )
    finally:
        await connection.close()


async def test_commercial_right_blocks_intent_and_first_start(migrated_url):
    installation_id = await _seed_installation(migrated_url)
    await _seed_gateway(migrated_url)
    await _seed_commercial(migrated_url, installation_id)
    connection = await _connect(migrated_url)
    try:
        with pytest.raises(OnboardingError) as blocked:
            await create_intent(
                connection,
                installation_id=installation_id,
                request_key="paid-key-1",
                environment="test",
                cipher=FakeCipher(),
            )
        assert blocked.value.code == "ONBOARDING_SUBSCRIPTION_ACTIVE"
        counts = await _counts(migrated_url)
        assert counts["onboarding_intents"] == 0 and counts["outbox_operations"] == 0
        assert counts["entitlements"] == 1  # only the paid right
    finally:
        await connection.close()

    # paid activated between intent ready and first start: hour is not consumed
    other_installation = await _seed_installation(migrated_url)
    connection = await _connect(migrated_url)
    try:
        created = await create_intent(
            connection,
            installation_id=other_installation,
            request_key="paid-key-2",
            environment="test",
            cipher=FakeCipher(),
        )
    finally:
        await connection.close()
    await _provision(migrated_url, created["id"], FakeBootstrapClient())
    await _seed_commercial(migrated_url, other_installation)
    connection = await _connect(migrated_url)
    try:
        with pytest.raises(OnboardingError) as race:
            await admit_evidence(
                connection,
                caller=GatewayContext(
                    gateway_id=uuid.UUID(created["gateway_id"]),
                    gateway_key="terlimo-035-node",
                    environment="test",
                ),
                credential_id=created["credential_id"],
                connection_id="c",
                request_id="r",
            )
        assert race.value.code == "ONBOARDING_SUBSCRIPTION_ACTIVE"
        assert await connection.fetchval(
            "SELECT count(*) FROM onboarding_evidence WHERE credential_id = $1",
            created["credential_id"],
        ) == 0
        assert await connection.fetchval(
            "SELECT count(*) FROM entitlements WHERE kind = 'onboarding_hour'"
        ) == 0
    finally:
        await connection.close()


async def test_consumed_unit_blocks_new_request_key(migrated_url):
    installation_id = await _seed_installation(migrated_url)
    await _seed_gateway(migrated_url)
    connection = await _connect(migrated_url)
    try:
        await connection.execute(
            """
            INSERT INTO entitlements
                (account_id, installation_id, kind, status, starts_at, ends_at)
            VALUES (NULL, $1, 'onboarding_hour', 'expired', now() - interval '2 hours',
                    now() - interval '1 hour')
            """,
            installation_id,
        )
        with pytest.raises(OnboardingError) as consumed:
            await create_intent(
                connection,
                installation_id=installation_id,
                request_key="unit-key-1",
                environment="test",
                cipher=FakeCipher(),
            )
        assert consumed.value.code == "ONBOARDING_UNIT_STARTED"
        assert await connection.fetchval("SELECT count(*) FROM onboarding_intents") == 0
        assert await connection.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE operation_type = 'gateway.bootstrap_provision'"
        ) == 0
    finally:
        await connection.close()


async def test_expired_intent_targeted_revoke_commits_survives_rejected_start(migrated_url):
    installation_id = await _seed_installation(migrated_url)
    gateway_id, _ = await _seed_gateway(migrated_url)
    connection = await _connect(migrated_url)
    try:
        created = await create_intent(
            connection,
            installation_id=installation_id,
            request_key="exp-key-1",
            environment="test",
            cipher=FakeCipher(),
        )
        await connection.execute(
            "UPDATE onboarding_intents SET expires_at = now() - interval '1 second' WHERE id = $1",
            created["id"],
        )
        caller = GatewayContext(gateway_id=gateway_id, gateway_key="terlimo-035-node", environment="test")
        with pytest.raises(OnboardingError) as expired:
            await admit_evidence(
                connection,
                caller=caller,
                credential_id=created["credential_id"],
                connection_id="c",
                request_id="r",
            )
        assert expired.value.code == "ONBOARDING_INTENT_EXPIRED"
        row = await connection.fetchrow(
            "SELECT state, secret_enc, secret_hash FROM onboarding_intents WHERE id = $1",
            created["id"],
        )
        assert row["state"] == "expired" and row["secret_enc"] is None and row["secret_hash"]
        assert await _revoke_op_count(migrated_url, created["credential_id"]) == 1
        with pytest.raises(OnboardingError) as again:
            await intent_response(
                connection, installation_id=installation_id, request_key="exp-key-1", cipher=FakeCipher()
            )
        assert again.value.code == "ONBOARDING_INTENT_EXPIRED"
        assert await _revoke_op_count(migrated_url, created["credential_id"]) == 1  # idempotent

        # batch sweep path enqueues a targeted revoke as well
        sweep_installation = await _seed_installation(migrated_url)
        connection2 = await _connect(migrated_url)
        try:
            sweep = await create_intent(
                connection2,
                installation_id=sweep_installation,
                request_key="exp-key-2",
                environment="test",
                cipher=FakeCipher(),
            )
            await connection2.execute(
                "UPDATE onboarding_intents SET expires_at = now() - interval '1 second' WHERE id = $1",
                sweep["id"],
            )
            assert await expire_intents(connection2) == 1
        finally:
            await connection2.close()
        assert await _revoke_op_count(migrated_url, sweep["credential_id"]) == 1
    finally:
        await connection.close()


async def test_bad_caller_validated_before_credential_lookup(migrated_url):
    connection = await _connect(migrated_url)
    try:
        with pytest.raises(OnboardingError) as unknown_gateway:
            await admit_evidence(
                connection,
                caller=GatewayContext(gateway_id=uuid.uuid4(), gateway_key="ghost", environment="test"),
                credential_id="unknown-credential",
                connection_id="c",
                request_id="r",
            )
        assert unknown_gateway.value.code == "ONBOARDING_ENV_MISMATCH"
        gateway_id, _ = await _seed_gateway(migrated_url)
        with pytest.raises(OnboardingError) as wrong_key:
            await admit_evidence(
                connection,
                caller=GatewayContext(gateway_id=gateway_id, gateway_key="wrong-key", environment="test"),
                credential_id="unknown-credential",
                connection_id="c",
                request_id="r",
            )
        assert wrong_key.value.code == "ONBOARDING_ENV_MISMATCH"
        counts = await _counts(migrated_url)
        assert counts["onboarding_intents"] == 0 and counts["onboarding_evidence"] == 0
    finally:
        await connection.close()


async def test_intent_response_first_expiry_commits_reconcile(migrated_url):
    """B3: the FIRST intent_response on an active expired-TTL intent must commit wipe+revoke
    and only then raise 410; a repeat stays idempotent."""
    installation_id = await _seed_installation(migrated_url)
    await _seed_gateway(migrated_url)
    connection = await _connect(migrated_url)
    try:
        created = await create_intent(
            connection,
            installation_id=installation_id,
            request_key="resp-exp-1",
            environment="test",
            cipher=FakeCipher(),
        )
        await connection.execute(
            "UPDATE onboarding_intents SET expires_at = now() - interval '1 second' WHERE id = $1",
            created["id"],
        )
        with pytest.raises(OnboardingError) as first:
            await intent_response(
                connection, installation_id=installation_id, request_key="resp-exp-1", cipher=FakeCipher()
            )
        assert first.value.code == "ONBOARDING_INTENT_EXPIRED"
        row = await connection.fetchrow(
            "SELECT state, secret_enc, secret_hash FROM onboarding_intents WHERE id = $1",
            created["id"],
        )
        assert row["state"] == "expired" and row["secret_enc"] is None and row["secret_hash"]
        assert await _revoke_op_count(migrated_url, created["credential_id"]) == 1
        with pytest.raises(OnboardingError) as second:
            await intent_response(
                connection, installation_id=installation_id, request_key="resp-exp-1", cipher=FakeCipher()
            )
        assert second.value.code == "ONBOARDING_INTENT_EXPIRED"
        assert await _revoke_op_count(migrated_url, created["credential_id"]) == 1
    finally:
        await connection.close()


async def test_lock_protocol_serializes_conforming_writer(migrated_url):
    """B1 protocol: an installation-row FOR UPDATE held by a conforming writer serializes our
    start (no partial evidence), then the start succeeds after release."""
    installation_id = await _seed_installation(migrated_url)
    gateway_id, _ = await _seed_gateway(migrated_url)
    setup = await _connect(migrated_url)
    try:
        created = await create_intent(
            setup,
            installation_id=installation_id,
            request_key="lock-key-1",
            environment="test",
            cipher=FakeCipher(),
        )
    finally:
        await setup.close()
    await _provision(migrated_url, created["id"], FakeBootstrapClient())
    caller = GatewayContext(gateway_id=gateway_id, gateway_key="terlimo-035-node", environment="test")

    holder = await _connect(migrated_url)
    waiter = await _connect(migrated_url)
    try:
        transaction = holder.transaction()
        await transaction.start()
        await holder.execute("SELECT id FROM installations WHERE id = $1 FOR UPDATE", installation_id)
        await waiter.execute("SET statement_timeout = 700")
        with pytest.raises(asyncpg.exceptions.QueryCanceledError):
            await admit_evidence(
                waiter,
                caller=caller,
                credential_id=created["credential_id"],
                connection_id="blocked",
                request_id="blocked",
            )
        assert (await _counts(migrated_url))["onboarding_evidence"] == 0
        await transaction.rollback()
        result = await admit_evidence(
            waiter,
            caller=caller,
            credential_id=created["credential_id"],
            connection_id="after-release",
            request_id="after-release",
        )
        assert result["state"] == "started" and result["replay"] is False
    finally:
        await holder.close()
        await waiter.close()


async def test_historical_hour_does_not_block_paid_upgrade(migrated_url):
    """A consumed historical hour must not prevent a paid/trial upgrade for the installation."""
    installation_id = await _seed_installation(migrated_url)
    await _seed_gateway(migrated_url)
    connection = await _connect(migrated_url)
    try:
        await connection.execute(
            """
            INSERT INTO entitlements
                (account_id, installation_id, kind, status, starts_at, ends_at)
            VALUES (NULL, $1, 'onboarding_hour', 'expired', now() - interval '2 hours',
                    now() - interval '1 hour')
            """,
            installation_id,
        )
        with pytest.raises(OnboardingError) as consumed:
            await create_intent(
                connection,
                installation_id=installation_id,
                request_key="upgrade-key-1",
                environment="test",
                cipher=FakeCipher(),
            )
        assert consumed.value.code == "ONBOARDING_UNIT_STARTED"
        await _seed_commercial(migrated_url, installation_id, kind="trial")
        assert await connection.fetchval(
            "SELECT status FROM entitlements WHERE kind = 'trial'"
        ) == "active"
        assert await connection.fetchval(
            "SELECT status FROM entitlements WHERE kind = 'onboarding_hour'"
        ) == "expired"
        assert await connection.fetchval(
            "SELECT count(*) FROM entitlements WHERE kind = 'onboarding_hour'"
        ) == 1
    finally:
        await connection.close()
