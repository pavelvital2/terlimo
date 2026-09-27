"""Targeted integration: indefinite (ends_at NULL) TEST entitlement end-to-end.

Proves the real path (HTTP access/sync -> outbox -> real worker/handlers -> catalog served) with
a bounded technical lease, and that finite expired / future / inactive / revoked entitlements
never grant (same eligibility as session_auth, no schema/API changes).
"""
from __future__ import annotations

import json

import pytest
from aiohttp.test_utils import TestClient, TestServer
from fake_gateway_admin import FakeGatewayAdmin
from test_catalog_access_sync import (
    MOBILE,
    _add_gateway,
    _auth,
    _connect,
    _current_revision,
    _seed_subject,
    _validator,
)

from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.gateway_adapter import GatewayAdminClient
from terlimo_backend.gateway_control import GatewayControlHandlers, ensure_grant
from terlimo_backend.worker import OutboxWorker


@pytest.fixture
async def env(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    database = Database(settings)
    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    try:
        yield client, migrated_url, settings, database
    finally:
        await client.close()


async def _set_entitlement(database_url, entitlement_id, *, sql):
    connection = await _connect(database_url)
    try:
        await connection.execute(sql, entitlement_id)
    finally:
        await connection.close()


async def _grant_row(database_url):
    connection = await _connect(database_url)
    try:
        return await connection.fetchrow(
            "SELECT id, desired_generation, applied_generation, gateway_generation, lease_seq, state, not_after FROM grants"
        )
    finally:
        await connection.close()


async def _sync(client, token, key, revision, binding_revision="1"):
    return await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(token), "Idempotency-Key": key},
        json={"catalog_revision": revision, "binding_revision": binding_revision},
    )


async def test_indefinite_active_full_path_and_same_grant_renewal(env):
    client, database_url, settings, database = env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-indef")
    await fake.start()
    try:
        subject = await _seed_subject(database_url)
        await _set_entitlement(
            database_url, subject["entitlement_id"],
            sql="UPDATE entitlements SET starts_at = now() - interval '1 hour', ends_at = NULL WHERE id = $1",
        )
        await _add_gateway(database_url, "gw-indef", admin_socket=fake.socket_path)
        handlers = GatewayControlHandlers(
            settings,
            client_factory=lambda _k, ep: GatewayAdminClient(
                socket_path=str(ep["admin_socket"]), main_password="fixture-main", timeout_seconds=5
            ),
        ).as_handlers()
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="indefinite")

        revision = await _current_revision(database_url, subject["installation_id"])
        sync = await _sync(client, subject["token"], "indef-sync-0000001", revision)
        assert sync.status == 200, await sync.text()
        pending = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert pending.status == 409
        assert "lease_seq" not in json.dumps(await pending.json())

        assert await worker.drain() == 2
        served = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert served.status == 200, await served.text()
        body = await served.json()
        assert not list(_validator("catalog.json", "CatalogResponse").iter_errors(body))
        access = body["gateways"][0]["access"]
        assert access["generation"] == "1" and int(access["lease_seq"]) >= 1
        first = await _grant_row(database_url)
        assert int(first["desired_generation"]) == int(first["applied_generation"]) == 1
        assert int(first["gateway_generation"]) == 1
        import datetime as dt
        now = dt.datetime.now(dt.UTC)
        assert now < first["not_after"] <= now + dt.timedelta(seconds=905)

        # renewal: same grant row, bounded technical lease again (no magic dates)
        connection = await _connect(database_url)
        try:
            await connection.execute("UPDATE grants SET not_after = now() - interval '1 second'")
        finally:
            await connection.close()
        probe = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert probe.status == 409
        revision2 = (await probe.json())["details"]["catalog_revision"]
        sync2 = await _sync(client, subject["token"], "indef-sync-0000002", revision2)
        assert sync2.status == 200, await sync2.text()
        assert await worker.drain() == 2
        renewed = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert renewed.status == 200
        second = await _grant_row(database_url)
        assert second["id"] == first["id"]
        assert int(second["desired_generation"]) == int(second["applied_generation"]) == 2
        assert int(second["lease_seq"]) >= 2
        now2 = dt.datetime.now(dt.UTC)
        assert now2 < second["not_after"] <= now2 + dt.timedelta(seconds=905)
    finally:
        await fake.stop()


@pytest.mark.parametrize(
    "name,sql",
    [
        ("finite_expired", "UPDATE entitlements SET ends_at = now() - interval '1 hour' WHERE id = $1"),
        ("future_start", "UPDATE entitlements SET starts_at = now() + interval '1 hour', ends_at = NULL WHERE id = $1"),
        ("status_pending", "UPDATE entitlements SET status = 'pending' WHERE id = $1"),
        ("status_revoked", "UPDATE entitlements SET status = 'revoked' WHERE id = $1"),
    ],
)
async def test_ineffective_entitlements_never_grant(env, name, sql):
    _client, database_url, _settings, _database = env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id=f"gw-{name}")
    await fake.start()
    try:
        subject = await _seed_subject(database_url)
        gateway = await _add_gateway(database_url, f"gw-{name}", admin_socket=fake.socket_path)
        await _set_entitlement(database_url, subject["entitlement_id"], sql=sql)
        connection = await _connect(database_url)
        try:
            outcome = await ensure_grant(
                connection,
                binding_id=subject["binding_id"],
                gateway_id=gateway,
                entitlement_id=subject["entitlement_id"],
                max_lease_seconds=900,
            )
            ops = await connection.fetchval("SELECT count(*) FROM outbox_operations WHERE binding_id = $1", subject["binding_id"])
        finally:
            await connection.close()
        assert outcome == "no_entitlement", f"{name}: {outcome}"
        assert ops == 0, f"{name} created an outbox operation"
    finally:
        await fake.stop()


async def test_revoked_grant_is_still_refused(env):
    _client, database_url, _settings, _database = env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-revoked-grant")
    await fake.start()
    try:
        subject = await _seed_subject(database_url)
        gateway = await _add_gateway(database_url, "gw-revoked-grant", admin_socket=fake.socket_path)
        connection = await _connect(database_url)
        try:
            await connection.execute(
                """INSERT INTO grants (binding_id, gateway_id, desired_generation, applied_generation,
                                       not_after, state, gateway_credential, lease_seq, gateway_generation)
                   VALUES ($1,$2,1,1, now()+interval '10 minutes','revoked','cred',1,1)""",
                subject["binding_id"], gateway,
            )
            outcome = await ensure_grant(
                connection, binding_id=subject["binding_id"], gateway_id=gateway,
                entitlement_id=subject["entitlement_id"], max_lease_seconds=900,
            )
        finally:
            await connection.close()
        assert outcome == "revoked"
    finally:
        await fake.stop()
