"""Gateway-control chain: desired grant -> outbox -> real adapter wire -> readback -> applied.

The gateway side in these tests is a wire-faithful fake of the reviewed WDTT admin surface
(tests/fake_gateway_admin.py), clearly labelled as not the real gateway; the adapter itself is
the production GatewayAdminClient and speaks the exact admin socket protocol. Real
applied-grant evidence against the actual gateway binary is produced separately.
"""

from __future__ import annotations

import json

import asyncpg
import pytest
from fake_gateway_admin import FakeGatewayAdmin

from terlimo_backend.config import Settings
from terlimo_backend.db import Database
from terlimo_backend.gateway_adapter import GatewayAdminClient
from terlimo_backend.gateway_control import (
    APPLY_OPERATION,
    GatewayControlHandlers,
    ensure_grant,
    revoke_binding_grants,
)
from terlimo_backend.pop import installation_fingerprint
from terlimo_backend.worker import OutboxWorker

FINGERPRINT = installation_fingerprint(
    "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEf2GWLvpp78Vox1cAEyfhgUKaywhAfUgWYaf5JldPvQZWP_mwVeNQNWK6hBTYmLpeci1h7TWFg-mkxysKZevnHA"
)


def _settings(database_url: str, settings_factory, **overrides) -> Settings:
    return settings_factory(
        database_url,
        gateway_admin_main_password="fixture-main",
        gateway_admin_timeout_seconds=5,
        gateway_local_admin_enabled=True,
        **overrides,
    )


def _handlers(settings: Settings) -> dict:
    def factory(_key, endpoints):
        return GatewayAdminClient(
            socket_path=str(endpoints["admin_socket"]),
            main_password=settings.gateway_admin_main_password,
            timeout_seconds=settings.gateway_admin_timeout_seconds,
        )

    return GatewayControlHandlers(settings, client_factory=factory).as_handlers()


async def _seed(database_url: str, gateway: FakeGatewayAdmin, *, ends_in_seconds: int = 7200):
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        account = await connection.fetchval(
            "INSERT INTO accounts (status) VALUES ('verified') RETURNING id"
        )
        installation = await connection.fetchval(
            """
            INSERT INTO installations
                (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test', 'android', $1, $2, 'technical')
            RETURNING id
            """,
            FINGERPRINT,
            "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEf2GWLvpp78Vox1cAEyfhgUKaywhAfUgWYaf5JldPvQZWP_mwVeNQNWK6hBTYmLpeci1h7TWFg-mkxysKZevnHA",
        )
        binding = await connection.fetchval(
            """
            INSERT INTO account_bindings (account_id, installation_id, status)
            VALUES ($1, $2, 'active') RETURNING id
            """,
            account,
            installation,
        )
        entitlement = await connection.fetchval(
            """
            INSERT INTO entitlements (account_id, kind, status, ends_at, device_limit)
            VALUES ($1, 'imported', 'active', now() + make_interval(secs => $2), 2)
            RETURNING id
            """,
            account,
            float(ends_in_seconds),
        )
        gateway_id = await connection.fetchval(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, registry_state)
            VALUES ('fake-gw', 'test', $1::jsonb, 'registered')
            RETURNING id
            """,
            json.dumps(
                {
                    "node_id": gateway.node_id,
                    "admin_socket": gateway.socket_path,
                    "target_workers": 36,
                }
            ),
        )
        return {
            "account": account,
            "installation": installation,
            "binding": binding,
            "entitlement": entitlement,
            "gateway": gateway_id,
        }
    finally:
        await connection.close()


async def _grant_row(database_url: str) -> dict:
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        row = await connection.fetchrow("SELECT * FROM grants ORDER BY created_at LIMIT 1")
    finally:
        await connection.close()
    record = dict(row)
    if isinstance(record.get("last_readback"), str):
        record["last_readback"] = json.loads(record["last_readback"])
    if isinstance(record.get("target_route"), str):
        record["target_route"] = json.loads(record["target_route"])
    return record


async def _grant_row_or_none(database_url: str):
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        return await connection.fetchrow("SELECT * FROM grants LIMIT 1")
    finally:
        await connection.close()


async def _count(database_url: str, table: str) -> int:
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        return await connection.fetchval(f"SELECT count(*) FROM {table}")
    finally:
        await connection.close()


async def _outbox(database_url: str, operation_type: str | None = None):
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        if operation_type:
            return await connection.fetch(
                "SELECT * FROM outbox_operations WHERE operation_type = $1 ORDER BY created_at",
                operation_type,
            )
        return await connection.fetch("SELECT * FROM outbox_operations ORDER BY created_at")
    finally:
        await connection.close()


@pytest.fixture
async def gateway_env(migrated_url, settings_factory):
    # Single node identity: the registry key must equal endpoints.node_id.
    gateway = FakeGatewayAdmin(node_id="fake-gw")
    await gateway.start()
    settings = _settings(migrated_url, settings_factory)
    database = Database(settings)
    await database.connect()
    ids = await _seed(migrated_url, gateway)
    try:
        yield gateway, settings, database, ids, migrated_url
    finally:
        await database.close()
        await gateway.stop()


async def _run_once(database, settings, migrated_url) -> int:
    worker = OutboxWorker(
        database, settings, handlers=_handlers(settings), worker_id="gateway-test-worker"
    )
    processed = await worker.drain()
    return processed


async def test_ensure_grant_is_finite_and_technical_only(gateway_env):
    _gateway, settings, database, ids, database_url = gateway_env
    async with database.acquire() as pool_connection:
        assert await ensure_grant(
            pool_connection,
            binding_id=ids["binding"],
            gateway_id=ids["gateway"],
            entitlement_id=ids["entitlement"],
            max_lease_seconds=settings.gateway_max_lease_seconds,
        ) == "enqueued"
        # The first operation is still pending; a repeat must not enqueue a second one.
        assert await ensure_grant(
            pool_connection,
            binding_id=ids["binding"],
            gateway_id=ids["gateway"],
            entitlement_id=ids["entitlement"],
            max_lease_seconds=settings.gateway_max_lease_seconds,
        ) in ("pending", "unchanged")
    grant = await _grant_row(database_url)
    assert grant["desired_generation"] == 1
    assert grant["state"] == "pending"
    assert grant["gateway_credential"]
    operations = await _outbox(database_url, APPLY_OPERATION)
    assert len(operations) == 1
    payload = operations[0]["payload"]
    if isinstance(payload, str):
        payload = json.loads(payload)
    assert set(payload) == {"grant_id", "generation", "action", "not_after"}
    assert payload["action"] == "apply"
    assert operations[0]["idempotency_key"] == f"grant:{payload['grant_id']}:gen:1:apply"


async def test_worker_applies_and_marks_applied_only_after_readback(gateway_env):
    gateway, settings, database, ids, database_url = gateway_env
    async with database.acquire() as connection:
        await ensure_grant(
            connection,
            binding_id=ids["binding"],
            gateway_id=ids["gateway"],
            entitlement_id=ids["entitlement"],
            max_lease_seconds=settings.gateway_max_lease_seconds,
        )
    assert await _run_once(database, settings, database_url) >= 1
    grant = await _grant_row(database_url)
    assert grant["state"] == "applied"
    assert grant["applied_generation"] == 1
    assert grant["lease_seq"] == 1
    assert grant["gateway_generation"] == 1
    assert grant["last_readback"]["grant_id"] == str(grant["opaque_id"])
    assert int(grant["applied_not_after"].timestamp()) == int(grant["not_after"].timestamp())
    assert len(gateway.grants) == 1
    entry = next(iter(gateway.grants.values()))
    assert entry["expires_at"] == int(grant["not_after"].timestamp())
    assert entry["revoked"] is False

    commands = [json.loads(command) for command in gateway.commands]
    for command in commands:
        assert set(command) <= {"operation", "password", "expires_at", "expected_seq", "grant"}
        if "grant" in command:
            assert set(command["grant"]) <= {
                "grant_id",
                "registration_id",
                "node_id",
                "public_key_spki",
                "generation",
                "lease_seq",
                "revoked",
                "operation_id",
            }
    assert "account" not in " ".join(gateway.commands).lower()
    assert "telegram" not in " ".join(gateway.commands).lower()
    assert "payment" not in " ".join(gateway.commands).lower()
    assert "trial" not in " ".join(gateway.commands).lower()


async def test_queued_accepted_is_not_applied_on_readback_mismatch(gateway_env):
    gateway, settings, database, ids, database_url = gateway_env
    async with database.acquire() as connection:
        await ensure_grant(
            connection,
            binding_id=ids["binding"],
            gateway_id=ids["gateway"],
            entitlement_id=ids["entitlement"],
            max_lease_seconds=settings.gateway_max_lease_seconds,
        )
    # Provision succeeds on the fake, but the readback no longer matches (accepted != applied).
    original_dispatch = gateway._provision

    def tampered(raw, command):
        response = original_dispatch(raw, command)
        if response.get("ok"):
            response["client_test"]["expires_at"] -= 30
        return response

    gateway._provision = tampered
    assert await _run_once(database, settings, database_url) >= 1
    grant = await _grant_row(database_url)
    assert grant["state"] == "pending"
    assert grant["applied_generation"] == 0 or grant["applied_generation"] is None
    operation = (await _outbox(database_url, APPLY_OPERATION))[0]
    assert operation["status"] == "pending"
    assert operation["attempts"] == 1


async def test_lost_response_retries_with_same_operation_identity(gateway_env):
    gateway, settings, database, ids, database_url = gateway_env
    async with database.acquire() as connection:
        # Keep the first lease below the configured ceiling so an extension changes not_after.
        await connection.execute(
            "UPDATE entitlements SET ends_at = now() + interval '600 seconds' WHERE id = $1",
            ids["entitlement"],
        )
        await ensure_grant(
            connection,
            binding_id=ids["binding"],
            gateway_id=ids["gateway"],
            entitlement_id=ids["entitlement"],
            max_lease_seconds=settings.gateway_max_lease_seconds,
        )
    assert await _run_once(database, settings, database_url) >= 1
    first = await _grant_row(database_url)
    first_count = len(gateway.grants)

    # Age the lease into the renewal margin, then extend the entitlement: this creates a
    # refresh operation whose response is lost once.
    async with database.acquire() as connection:
        await connection.execute(
            "UPDATE grants SET not_after = now() + interval '100 seconds' WHERE id = $1",
            first["id"],
        )
        await connection.execute(
            "UPDATE entitlements SET ends_at = now() + interval '4 hours' WHERE id = $1",
            ids["entitlement"],
        )
        assert await ensure_grant(
            connection,
            binding_id=ids["binding"],
            gateway_id=ids["gateway"],
            entitlement_id=ids["entitlement"],
            max_lease_seconds=settings.gateway_max_lease_seconds,
        ) == "enqueued"
    gateway.drop_next_response = True
    assert await _run_once(database, settings, database_url) >= 1
    pending = (await _outbox(database_url, APPLY_OPERATION))[-1]
    assert pending["status"] == "pending"
    assert len(gateway.grants) == first_count
    assert gateway.grants[next(iter(gateway.grants))]["lease_seq"] == 1

    # Retry: identical command bytes -> gateway recognizes the operation digest; one effect.
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await connection.execute(
            "UPDATE outbox_operations SET available_at = now() WHERE id = $1", pending["id"]
        )
    finally:
        await connection.close()
    assert await _run_once(database, settings, database_url) >= 1
    refreshed = await _grant_row(database_url)
    assert refreshed["state"] == "applied"
    assert refreshed["lease_seq"] == 2
    assert refreshed["desired_generation"] == 2
    assert len(gateway.grants) == first_count
    assert first["not_after"] < refreshed["not_after"]


async def test_gateway_unavailable_is_retried_with_backoff(gateway_env):
    gateway, settings, database, ids, database_url = gateway_env
    async with database.acquire() as connection:
        await ensure_grant(
            connection,
            binding_id=ids["binding"],
            gateway_id=ids["gateway"],
            entitlement_id=ids["entitlement"],
            max_lease_seconds=settings.gateway_max_lease_seconds,
        )
    await gateway.stop()
    assert await _run_once(database, settings, database_url) >= 1
    operation = (await _outbox(database_url, APPLY_OPERATION))[0]
    assert operation["status"] == "pending"
    assert operation["attempts"] == 1
    assert operation["available_at"] is not None
    grant = await _grant_row(database_url)
    assert grant["state"] == "pending"

    await gateway.start()
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await connection.execute(
            "UPDATE outbox_operations SET available_at = now() WHERE id = $1", operation["id"]
        )
    finally:
        await connection.close()
    assert await _run_once(database, settings, database_url) >= 1
    assert (await _grant_row(database_url))["state"] == "applied"


async def test_revoke_fences_delayed_old_apply(gateway_env):
    gateway, settings, database, ids, database_url = gateway_env
    async with database.acquire() as connection:
        await ensure_grant(
            connection,
            binding_id=ids["binding"],
            gateway_id=ids["gateway"],
            entitlement_id=ids["entitlement"],
            max_lease_seconds=settings.gateway_max_lease_seconds,
        )
    assert await _run_once(database, settings, database_url) >= 1
    applied = await _grant_row(database_url)
    assert applied["state"] == "applied"

    async with database.acquire() as connection:
        assert await revoke_binding_grants(connection, binding_id=ids["binding"]) == 1
    assert await _run_once(database, settings, database_url) >= 1
    revoked = await _grant_row(database_url)
    assert revoked["state"] == "revoked"
    entry = next(iter(gateway.grants.values()))
    assert entry["revoked"] is True and entry["generation"] == 2

    # A delayed stale apply for the pre-revoke generation must not run or revive access.
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await connection.execute(
            """
            INSERT INTO outbox_operations
                (operation_type, payload, idempotency_key, target_revision, gateway_id, max_attempts)
            VALUES ($1, $2::jsonb, 'stale-apply-probe', 1, $3, 12)
            """,
            APPLY_OPERATION,
            json.dumps(
                {
                    "grant_id": str(applied["opaque_id"]),
                    "generation": "1",
                    "action": "apply",
                    "not_after": "x",
                }
            ),
            ids["gateway"],
        )
    finally:
        await connection.close()
    before_commands = len(gateway.commands)
    assert await _run_once(database, settings, database_url) >= 1
    operation = await _outbox(database_url, APPLY_OPERATION)
    stale = next(row for row in operation if row["idempotency_key"] == "stale-apply-probe")
    assert stale["status"] == "failed"
    assert stale["last_error"] == "superseded_by_newer_generation"
    assert len(gateway.commands) == before_commands
    assert (await _grant_row(database_url))["state"] == "revoked"
    assert next(iter(gateway.grants.values()))["revoked"] is True


async def test_ensure_grant_requires_ownership_and_matching_environment(gateway_env):
    _gateway, settings, database, ids, database_url = gateway_env
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        other_account = await connection.fetchval(
            "INSERT INTO accounts (status) VALUES ('verified') RETURNING id"
        )
        foreign_entitlement = await connection.fetchval(
            """
            INSERT INTO entitlements (account_id, kind, status, ends_at, device_limit)
            VALUES ($1, 'imported', 'active', now() + interval '1 hour', 2) RETURNING id
            """,
            other_account,
        )
    finally:
        await connection.close()

    async with database.acquire() as pool_connection:
        assert (
            await ensure_grant(
                pool_connection,
                binding_id=ids["binding"],
                gateway_id=ids["gateway"],
                entitlement_id=foreign_entitlement,
                max_lease_seconds=settings.gateway_max_lease_seconds,
            )
            == "ownership_mismatch"
        )
    assert await _grant_row_or_none(database_url) is None
    assert await _count(database_url, "outbox_operations") == 0

    async with database.acquire() as pool_connection:
        await pool_connection.execute(
            "UPDATE gateways SET environment = 'production' WHERE id = $1", ids["gateway"]
        )
        assert (
            await ensure_grant(
                pool_connection,
                binding_id=ids["binding"],
                gateway_id=ids["gateway"],
                entitlement_id=ids["entitlement"],
                max_lease_seconds=settings.gateway_max_lease_seconds,
            )
            == "environment_mismatch"
        )
    assert await _grant_row_or_none(database_url) is None
    assert await _count(database_url, "outbox_operations") == 0


async def test_apply_readback_generation_mismatch_is_not_applied(gateway_env):
    gateway, settings, database, ids, database_url = gateway_env
    async with database.acquire() as connection:
        await ensure_grant(
            connection,
            binding_id=ids["binding"],
            gateway_id=ids["gateway"],
            entitlement_id=ids["entitlement"],
            max_lease_seconds=settings.gateway_max_lease_seconds,
        )
    original_dispatch = gateway._provision

    def tampered(raw, command):
        response = original_dispatch(raw, command)
        if response.get("ok"):
            response["client_test"]["generation"] = "3"
        return response

    gateway._provision = tampered
    assert await _run_once(database, settings, database_url) >= 1
    grant = await _grant_row(database_url)
    assert grant["state"] == "pending"
    operation = (await _outbox(database_url, APPLY_OPERATION))[0]
    assert operation["status"] == "pending"
    assert operation["attempts"] == 1


async def test_local_admin_transport_requires_test_optin(settings_factory):
    from terlimo_backend.config import Settings
    from terlimo_backend.gateway_adapter import build_gateway_client

    base = {
        "database_url": "postgresql://x@127.0.0.1:1/x",
        "environment": "test",
        "log_level": "WARNING",
        "api_host": "127.0.0.1",
        "api_port": 0,
        "db_pool_min": 1,
        "db_pool_max": 2,
        "db_command_timeout_seconds": 5,
        "worker_poll_interval_seconds": 0.05,
        "worker_lock_timeout_seconds": 30,
        "worker_max_attempts": 8,
    }
    endpoints = {"node_id": "n", "admin_socket": "/tmp/does-not-exist.sock"}
    with pytest.raises(Exception) as error:
        build_gateway_client(Settings(**base), "gw", endpoints)
    assert getattr(error.value, "code", "") == "GATEWAY_LOCAL_ADMIN_DISABLED"

    production = {**base, "environment": "production"}
    with pytest.raises(Exception) as error:
        build_gateway_client(
            Settings(**production, gateway_local_admin_enabled=True), "gw", endpoints
        )
    assert getattr(error.value, "code", "") == "GATEWAY_LOCAL_ADMIN_FORBIDDEN"

    with pytest.raises(Exception) as error:
        build_gateway_client(Settings(**base, gateway_local_admin_enabled=True), "gw", {})
    assert getattr(error.value, "code", "") == "GATEWAY_ENDPOINT_MISSING"


async def test_revoke_follows_proven_target_after_registry_identity_change(gateway_env):
    gateway, settings, database, ids, database_url = gateway_env
    async with database.acquire() as connection:
        await ensure_grant(
            connection,
            binding_id=ids["binding"],
            gateway_id=ids["gateway"],
            entitlement_id=ids["entitlement"],
            max_lease_seconds=settings.gateway_max_lease_seconds,
        )
    assert await _run_once(database, settings, database_url) >= 1
    applied = await _grant_row(database_url)
    assert applied["state"] == "applied"
    assert applied["target_node_id"] == "fake-gw"
    assert applied["target_route"]["admin_socket"] == gateway.socket_path

    # The registry identity changes (key and endpoints.node_id) while the proven management
    # route stays the same: the actual revoke must follow the applied target, not the new id.
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await connection.execute(
            """
            UPDATE gateways
            SET gateway_key = 'fake-gw-renamed',
                endpoints = jsonb_set(endpoints, '{node_id}', '"fake-gw-renamed"')
            WHERE id = $1
            """,
            ids["gateway"],
        )
    finally:
        await connection.close()
    async with database.acquire() as connection:
        assert await revoke_binding_grants(connection, binding_id=ids["binding"]) == 1
    assert await _run_once(database, settings, database_url) >= 1

    revoked = await _grant_row(database_url)
    assert revoked["state"] == "revoked"
    assert revoked["applied_generation"] == revoked["desired_generation"]
    assert revoked["last_readback"]["revoked"] is True
    entry = next(iter(gateway.grants.values()))
    assert entry["revoked"] is True and entry["generation"] == 2

    # Repeat revoke and a stale apply for the old generation must not resurrect the grant.
    async with database.acquire() as connection:
        assert await revoke_binding_grants(connection, binding_id=ids["binding"]) == 0
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await connection.execute(
            """
            INSERT INTO outbox_operations
                (operation_type, payload, idempotency_key, target_revision, gateway_id, max_attempts)
            VALUES ($1, $2::jsonb, 'proven-stale-apply', 1, $3, 12)
            """,
            APPLY_OPERATION,
            json.dumps(
                {
                    "grant_id": str(revoked["opaque_id"]),
                    "generation": "1",
                    "action": "apply",
                    "not_after": "x",
                }
            ),
            ids["gateway"],
        )
    finally:
        await connection.close()
    assert await _run_once(database, settings, database_url) >= 1
    after = await _grant_row(database_url)
    assert after["state"] == "revoked"
    assert after["last_readback"]["revoked"] is True


async def test_revoke_without_proven_target_never_false_succeeds(gateway_env):
    gateway, settings, database, ids, database_url = gateway_env
    async with database.acquire() as connection:
        await ensure_grant(
            connection,
            binding_id=ids["binding"],
            gateway_id=ids["gateway"],
            entitlement_id=ids["entitlement"],
            max_lease_seconds=settings.gateway_max_lease_seconds,
        )
    assert await _run_once(database, settings, database_url) >= 1

    # Legacy row without a stored proven target and a registry that no longer identifies the
    # applied node: no RPC is sent and no actual revoke is claimed.
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await connection.execute(
            "UPDATE grants SET target_node_id = NULL, target_route = NULL, last_readback = NULL"
        )
        await connection.execute(
            """
            UPDATE gateways
            SET gateway_key = 'fake-gw-other',
                endpoints = jsonb_set(endpoints, '{node_id}', '"fake-gw-other"')
            WHERE id = $1
            """,
            ids["gateway"],
        )
    finally:
        await connection.close()
    async with database.acquire() as connection:
        assert await revoke_binding_grants(connection, binding_id=ids["binding"]) == 1
    await _run_once(database, settings, database_url)

    unproven = await _grant_row(database_url)
    assert unproven["state"] == "revoked"
    assert unproven["last_readback"] is None
    assert unproven["applied_generation"] != unproven["desired_generation"]
    assert all(not entry["revoked"] for entry in gateway.grants.values())
    revoke_ops = await _outbox(database_url, "gateway.revoke_grant")
    assert any("proven_target_unavailable" in (op["last_error"] or "") for op in revoke_ops)

    # Target route lost (node unreachable): the retry stays explicit, no false success.
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await connection.execute(
            "UPDATE grants SET target_node_id = 'fake-gw', target_route = $2::jsonb WHERE id = $1",
            unproven["id"],
            json.dumps({"admin_socket": "/tmp/proven-target-gone.sock"}),
        )
    finally:
        await connection.close()
    async with database.acquire() as connection:
        await ensure_grant(
            connection,
            binding_id=ids["binding"],
            gateway_id=ids["gateway"],
            entitlement_id=ids["entitlement"],
            max_lease_seconds=settings.gateway_max_lease_seconds,
        )
    await _run_once(database, settings, database_url)
    lost = await _grant_row(database_url)
    assert lost["last_readback"] is None
    assert all(not entry["revoked"] for entry in gateway.grants.values())


def test_proven_revoke_target_never_falls_back_by_name():
    """Pure repro: a legacy row cannot reconstruct a replaced route from the node name."""
    from terlimo_backend.gateway_control import _proven_revoke_target

    record = {
        "target_node_id": None,
        "target_route": None,
        "last_readback": {"node_id": "synthetic-node"},
        "gateway_key": "synthetic-node",
        "endpoints": {
            "node_id": "synthetic-node",
            "admin_socket": "/tmp/synthetic-replacement.sock",
        },
    }
    assert _proven_revoke_target(record) == (None, None)
    # Malformed persisted routes fail closed too.
    malformed = dict(record, target_route={"admin_socket": 123})
    assert _proven_revoke_target(malformed) == (None, None)
    # A genuinely persisted route (dict or jsonb string) is honored.
    from terlimo_backend.gateway_control import _decode_jsonb

    assert _decode_jsonb('{"admin_socket":"/tmp/original.sock"}') == {
        "admin_socket": "/tmp/original.sock"
    }
    captured = dict(record, target_route={"admin_socket": "/tmp/original.sock"})
    assert _proven_revoke_target(captured) == (
        "synthetic-node",
        {"admin_socket": "/tmp/original.sock"},
    )


async def test_proven_target_jsonb_roundtrip_and_replacement_route_refused(gateway_env):
    gateway, settings, database, ids, database_url = gateway_env
    async with database.acquire() as connection:
        await ensure_grant(
            connection,
            binding_id=ids["binding"],
            gateway_id=ids["gateway"],
            entitlement_id=ids["entitlement"],
            max_lease_seconds=settings.gateway_max_lease_seconds,
        )
    assert await _run_once(database, settings, database_url) >= 1

    # Pool codec returns dict; a raw connection returns the jsonb text: both are the same proof.
    async with database.acquire() as connection:
        routed = await connection.fetchval("SELECT target_route FROM grants LIMIT 1")
        assert isinstance(routed, dict)
        assert routed["admin_socket"] == gateway.socket_path
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        raw = await connection.fetchval("SELECT target_route FROM grants LIMIT 1")
        assert isinstance(raw, str) and gateway.socket_path in raw
    finally:
        await connection.close()

    # Legacy row (no persisted route) with the same node name but a replacement route: no RPC,
    # desired revoke kept, actual revoke never claimed.
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await connection.execute(
            """
            UPDATE grants SET target_node_id = NULL, target_route = NULL,
                              last_readback = '{"node_id": "fake-gw"}'::jsonb
            WHERE binding_id = $1
            """,
            ids["binding"],
        )
        await connection.execute(
            """
            UPDATE gateways SET endpoints = jsonb_set(endpoints, '{admin_socket}',
                                                      '"/tmp/synthetic-replacement.sock"'::jsonb)
            WHERE id = $1
            """,
            ids["gateway"],
        )
    finally:
        await connection.close()
    before_commands = len(gateway.commands)
    async with database.acquire() as connection:
        assert await revoke_binding_grants(connection, binding_id=ids["binding"]) == 1
    await _run_once(database, settings, database_url)
    row = await _grant_row(database_url)
    assert row["state"] == "revoked"
    assert not (row["last_readback"] or {}).get("revoked")
    assert row["applied_generation"] != row["desired_generation"]
    assert all(not entry["revoked"] for entry in gateway.grants.values())
    assert len(gateway.commands) == before_commands
    revoke_ops = await _outbox(database_url, "gateway.revoke_grant")
    assert any("proven_target_unavailable" in (op["last_error"] or "") for op in revoke_ops)
