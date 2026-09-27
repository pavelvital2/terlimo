"""Targeted STEP036 hour data-subject source tests (no HTTP, no runtime).

Covers the installation-scoped hour grant ownership added by 0020: exact ownership/isolation,
revoke scoping, the stable numeric hour fence, and the lossless-refusal down migration.
"""
from __future__ import annotations

import json
from datetime import UTC, datetime, timedelta
from types import SimpleNamespace

import asyncpg
import pytest


class _R(dict):
    __getattr__ = dict.__getitem__

from terlimo_backend import gateway_control, onboarding_hour
from terlimo_backend.migrations import runner
from terlimo_backend.mobile_catalog import (
    SUBJECT_BINDING,
    SUBJECT_HOUR,
    _admission_fence,
    _data_subject,
)


async def _connect(url: str) -> asyncpg.Connection:
    connection = await asyncpg.connect(url, timeout=10)
    await connection.set_type_codec(
        "jsonb", encoder=json.dumps, decoder=json.loads, schema="pg_catalog"
    )
    return connection


async def _installation(connection: asyncpg.Connection) -> object:
    return await connection.fetchval(
        """
        INSERT INTO installations
            (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
        VALUES ('test', 'android', $1, 'spki', 'technical')
        RETURNING id
        """,
        __import__("secrets").token_hex(32),
    )


async def _hour(connection: asyncpg.Connection, installation_id: object) -> object:
    return await connection.fetchval(
        """
        INSERT INTO entitlements (account_id, installation_id, kind, status, starts_at, ends_at,
                                  device_limit, revision)
        VALUES (NULL, $1, 'onboarding_hour', 'active', now() - interval '1 minute',
                now() + interval '1 hour', 1, 1)
        RETURNING id
        """,
        installation_id,
    )


async def _gateway(connection: asyncpg.Connection) -> object:
    return await connection.fetchval(
        """
        INSERT INTO gateways (gateway_key, environment, endpoints, capabilities, registry_state,
                              confirmed_max_workers)
        VALUES ($1, 'test', '{}'::jsonb, '[]'::jsonb, 'registered', 36)
        RETURNING id
        """,
        f"gw-{__import__('uuid').uuid4().hex[:8]}",
    )


async def test_ensure_hour_grant_ownership_isolation_and_revoke(migrated_url):
    connection = await _connect(migrated_url)
    try:
        i1, i2 = await _installation(connection), await _installation(connection)
        e1, e2 = await _hour(connection, i1), await _hour(connection, i2)
        g1 = await _gateway(connection)
        async with connection.transaction():
            outcome = await gateway_control.ensure_hour_grant(
                connection,
                installation_id=i1,
                hour_entitlement_id=e1,
                gateway_id=g1,
                max_lease_seconds=900,
            )
            assert outcome == "enqueued"
        grant = await connection.fetchrow(
            "SELECT * FROM grants WHERE hour_entitlement_id = $1 AND gateway_id = $2", e1, g1
        )
        assert grant["binding_id"] is None
        assert grant["installation_id"] == i1
        assert grant["state"] == "pending"
        assert grant["not_after"] is not None
        apply_op = await connection.fetchrow(
            "SELECT * FROM outbox_operations WHERE payload->>'grant_id' = $1", str(grant["opaque_id"])
        )
        assert apply_op is not None
        assert apply_op["payload"]["hour_entitlement_id"] == str(e1)
        # Isolation: the other installation's hour has no grant anywhere.
        assert await connection.fetchval(
            "SELECT count(*) FROM grants WHERE hour_entitlement_id = $1", e2
        ) == 0

        async with connection.transaction():
            revoked = await gateway_control.revoke_hour_grants(
                connection, installation_id=i1, hour_entitlement_id=e1
            )
        assert revoked == 1
        after = await connection.fetchrow("SELECT * FROM grants WHERE id = $1", grant["id"])
        assert after["state"] == "revoked"
        revoke_op = await connection.fetchrow(
            "SELECT * FROM outbox_operations WHERE operation_type = 'gateway.revoke_grant'"
        )
        assert revoke_op is not None
        assert await connection.fetchval(
            "SELECT revision FROM entitlements WHERE id = $1", e1
        ) == 2  # hour fence changed on revoke, not current time
    finally:
        await connection.close()


async def test_hour_owner_trigger_rejects_cross_installation(migrated_url):
    connection = await _connect(migrated_url)
    try:
        i1, i2 = await _installation(connection), await _installation(connection)
        e1 = await _hour(connection, i1)
        g1 = await _gateway(connection)
        with pytest.raises(asyncpg.RaiseError):
            await connection.execute(
                """
                INSERT INTO grants
                    (binding_id, installation_id, hour_entitlement_id, gateway_id, desired_generation,
                     not_after, state, gateway_credential)
                VALUES (NULL, $1, $2, $3, 1, now() + interval '1 hour', 'pending', 'cred')
                """,
                i2,
                e1,
                g1,
            )
    finally:
        await connection.close()


async def test_data_subject_and_hour_fence_semantics():
    hour = _R(
        id="hour-1", status="active", revision=3, ends_at=datetime.now(UTC) + timedelta(minutes=30)
    )
    now = datetime.now(UTC)
    ctx = SimpleNamespace(
        management_only=False,
        binding=None,
        binding_status="none",
        account_state="UNLINKED",
        onboarding_hour=hour,
        evaluated_at=now,
    )
    assert _data_subject(ctx) == (SUBJECT_HOUR, "hour-1")
    assert _admission_fence(ctx, (SUBJECT_HOUR, "hour-1")) == "3"
    ctx.management_only = True
    assert _data_subject(ctx) is None  # a management-only control session is never data
    ctx.management_only = False
    binding = _R(id="bind-1", status="active", generation=7)
    ctx.binding = binding
    ctx.account_state = "ACTIVE_PAID"
    assert _data_subject(ctx) == (SUBJECT_BINDING, "bind-1")  # commercial priority
    assert _admission_fence(ctx, (SUBJECT_BINDING, "bind-1")) == "7"  # real binding generation


async def test_expire_hour_units_revokes_only_its_grants_and_is_idempotent(migrated_url):
    connection = await _connect(migrated_url)
    try:
        i1 = await _installation(connection)
        e1 = await _hour(connection, i1)
        g1 = await _gateway(connection)
        async with connection.transaction():
            await gateway_control.ensure_hour_grant(
                connection, installation_id=i1, hour_entitlement_id=e1, gateway_id=g1,
                max_lease_seconds=900,
            )
        # Age the hour past its deadline without extending it.
        await connection.execute(
            "UPDATE entitlements SET ends_at = now() - interval '1 minute' WHERE id = $1", e1
        )
        async with connection.transaction():
            expired = await onboarding_hour.expire_hour_units(connection)
        assert expired == 1
        entitlement = await connection.fetchrow("SELECT status, revision FROM entitlements WHERE id = $1", e1)
        assert entitlement["status"] == "expired"
        assert entitlement["revision"] == 2  # fence changed once, deadline untouched
        grant = await connection.fetchrow("SELECT state FROM grants WHERE hour_entitlement_id = $1", e1)
        assert grant["state"] == "revoked"
        # Idempotent: a repeated sweep enqueues nothing new.
        async with connection.transaction():
            assert await onboarding_hour.expire_hour_units(connection) == 0
        count = await connection.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE operation_type = 'gateway.revoke_grant'"
        )
        assert count == 1
    finally:
        await connection.close()


async def test_0020_down_refuses_with_hour_grant(migrated_url):
    connection = await _connect(migrated_url)
    try:
        i1 = await _installation(connection)
        e1 = await _hour(connection, i1)
        g1 = await _gateway(connection)
        async with connection.transaction():
            await gateway_control.ensure_hour_grant(
                connection, installation_id=i1, hour_entitlement_id=e1, gateway_id=g1,
                max_lease_seconds=900,
            )
        with pytest.raises(asyncpg.PostgresError):
            await runner.rollback_migration(connection, "0020_hour_grant_subject")
    finally:
        await connection.close()
