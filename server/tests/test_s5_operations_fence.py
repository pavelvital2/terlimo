"""Opt-in observable recovery marker (compatible with the strict accepted decoder)."""

from __future__ import annotations

import uuid

import pytest
from aiohttp.test_utils import TestClient, TestServer
from test_catalog_access_sync import (
    MOBILE,
    _add_gateway,
    _auth,
    _connect,
    _current_revision,
    _seed_subject,
)

from terlimo_backend.api import create_app
from terlimo_backend.db import Database

LEGACY_TOP_LEVEL = {
    "request_id", "server_time", "schema_version", "status",
    "operation_id", "operation_type", "state", "access_application_state", "per_node",
}
LEGACY_NODE = {
    "gateway_id", "state", "applied_generation", "not_after", "last_error", "attempts",
}


@pytest.fixture
async def catalog_env(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    database = Database(settings)
    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    try:
        yield client, migrated_url, settings, database
    finally:
        await client.close()


async def _sync(client, subject, key, catalog_revision):
    return await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": key},
        json={"catalog_revision": str(catalog_revision), "binding_revision": "1"},
    )


async def _dead_expired(client, database_url, subject, gateway, parent_id):
    connection = await _connect(database_url)
    try:
        grant = await connection.fetchrow(
            "SELECT id, opaque_id FROM grants WHERE binding_id=$1 AND gateway_id=$2",
            subject["binding_id"], gateway,
        )
        child = await connection.fetchrow(
            """SELECT id, target_revision FROM outbox_operations
               WHERE correlation_id=$1 AND operation_type='gateway.apply_grant'""",
            uuid.UUID(parent_id),
        )
        await connection.execute(
            "UPDATE grants SET desired_generation=$2, state='pending', not_after=now()-interval '1 second' WHERE id=$1",
            grant["id"], child["target_revision"],
        )
        await connection.execute(
            "UPDATE outbox_operations SET status='dead', attempts=12, max_attempts=12 WHERE id=$1",
            child["id"],
        )
    finally:
        await connection.close()


@pytest.mark.asyncio
async def test_optin_marker_shape_and_legacy_compatibility(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    gateway = await _add_gateway(database_url, "gw-optin")
    revision = await _current_revision(database_url, subject["installation_id"])
    created = await _sync(client, subject, "optin-marker-key-0001", revision)
    assert created.status == 200, await created.text()
    parent = (await created.json())["operation_id"]

    legacy = await client.get(f"{MOBILE}/operations/{parent}", headers=_auth(subject["token"]))
    legacy_body = await legacy.json()
    assert set(legacy_body) == LEGACY_TOP_LEVEL
    assert set(legacy_body["per_node"][0]) == LEGACY_NODE

    await _dead_expired(client, database_url, subject, gateway, parent)

    plain = await client.get(f"{MOBILE}/operations/{parent}", headers=_auth(subject["token"]))
    assert "sync_recovery" not in await plain.json()

    observed = await client.get(
        f"{MOBILE}/operations/{parent}?observe=sync_recovery",
        headers=_auth(subject["token"]),
    )
    observed_body = await observed.json()
    assert set(observed_body) == LEGACY_TOP_LEVEL | {"sync_recovery"}
    assert set(observed_body["per_node"][0]) == LEGACY_NODE
    marker = observed_body["sync_recovery"]
    assert marker["required"] is True
    assert marker["binding_revision"] == "1"
    assert marker["catalog_revision"].isdigit()
    blocked = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert blocked.status == 503
    assert marker["catalog_revision"] == (await blocked.json())["details"]["catalog_revision"]

    unknown = await client.get(
        f"{MOBILE}/operations/{uuid.uuid4()}?observe=sync_recovery",
        headers=_auth(subject["token"]),
    )
    assert unknown.status == 404


@pytest.mark.asyncio
async def test_same_revision_fresh_key_then_second_rotation_epoch(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    gateway = await _add_gateway(database_url, "gw-optin2")
    revision = await _current_revision(database_url, subject["installation_id"])
    created = await _sync(client, subject, "optin-rotate-key-0001", revision)
    parent = (await created.json())["operation_id"]
    await _dead_expired(client, database_url, subject, gateway, parent)

    marker = (
        await (
            await client.get(
                f"{MOBILE}/operations/{parent}?observe=sync_recovery",
                headers=_auth(subject["token"]),
            )
        ).json()
    )["sync_recovery"]
    assert marker["required"] is True

    # Fresh key with exactly the marker revisions (same as client's saved current snapshot).
    rotated = await _sync(client, subject, "optin-rotate-key-0002", marker["catalog_revision"])
    assert rotated.status == 200, await rotated.text()
    rotated_body = await rotated.json()
    assert rotated_body["access_application_state"] == "pending"

    replay = await _sync(client, subject, "optin-rotate-key-0002", marker["catalog_revision"])
    assert replay.status == 200
    assert (await replay.json())["operation_id"] == rotated_body["operation_id"]

    old_key_new_digest = await _sync(
        client, subject, "optin-rotate-key-0001", marker["catalog_revision"]
    )
    assert old_key_new_digest.status == 409
    assert (await old_key_new_digest.json())["code"] == "IDEMPOTENCY_CONFLICT"

    # After the rotation the marker is no longer a rotation hint (new epoch only if it fails).
    after = (
        await (
            await client.get(
                f"{MOBILE}/operations/{rotated_body['operation_id']}?observe=sync_recovery",
                headers=_auth(subject["token"]),
            )
        ).json()
    )["sync_recovery"]
    assert after["required"] is False

    connection = await _connect(database_url)
    try:
        assert await connection.fetchval(
            "SELECT count(*) FROM grants WHERE binding_id=$1 AND gateway_id=$2",
            subject["binding_id"], gateway,
        ) == 1
        assert await connection.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE correlation_id=$1 AND operation_type='gateway.apply_grant'",
            uuid.UUID(rotated_body["operation_id"]),
        ) == 1
    finally:
        await connection.close()
