"""Expired failed grant recovery stays fenced and uses normal sync/worker paths."""

from __future__ import annotations

import json
import uuid
from datetime import UTC, datetime, timedelta

import pytest
from fake_gateway_admin import FakeGatewayAdmin
from test_catalog_access_sync import (
    MOBILE, _add_gateway, _auth, _connect, _current_revision, _seed_subject, catalog_env,
)

from terlimo_backend.gateway_adapter import GatewayAdminClient
from terlimo_backend.gateway_control import GatewayControlHandlers
from terlimo_backend.mobile_catalog import _recoverable_expired_apply_failure
from terlimo_backend.worker import OutboxWorker


def test_expired_failure_predicate_boundary_and_identity():
    now = datetime.now(UTC)
    binding, gateway, grant = uuid.uuid4(), uuid.uuid4(), uuid.uuid4()
    row = {
        "gateway_row_id": gateway, "grant_binding_id": binding,
        "grant_state": "pending", "not_after": now, "desired_generation": 5,
        "op_type": "gateway.apply_grant", "op_status": "dead",
        "op_gateway_id": gateway, "op_binding_id": binding, "op_target": 5,
        "op_grant_id": str(grant), "opaque_id": grant,
        "op_generation": "5", "op_action": "apply",
    }
    assert _recoverable_expired_apply_failure(row, ("binding", binding), now)
    assert _recoverable_expired_apply_failure({**row, "op_status": "failed"}, ("binding", binding), now)
    for change in (
        {"not_after": now + timedelta(microseconds=1)},
        {"op_target": 4}, {"op_generation": "4"},
        {"op_gateway_id": uuid.uuid4()}, {"op_binding_id": uuid.uuid4()},
        {"op_grant_id": str(uuid.uuid4())}, {"op_type": "gateway.revoke_grant"},
        {"op_status": "pending"}, {"grant_state": "revoked"},
    ):
        assert not _recoverable_expired_apply_failure({**row, **change}, ("binding", binding), now)
    assert not _recoverable_expired_apply_failure(row, ("binding", uuid.uuid4()), now)
    assert not _recoverable_expired_apply_failure(row, ("hour", binding), now)


@pytest.mark.asyncio
async def test_expired_dead_apply_new_sync_fenced_then_worker_readback(catalog_env):
    client, database_url, settings, database = catalog_env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-recover")
    await fake.start()
    try:
        subject = await _seed_subject(database_url)
        gateway = await _add_gateway(database_url, "gw-recover", admin_socket=fake.socket_path)
        revision = await _current_revision(database_url, subject["installation_id"])
        body = {"catalog_revision": revision, "binding_revision": "1"}
        first = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "recovery-initial-sync-01"}, json=body,
        )
        assert first.status == 200, await first.text()
        handlers = GatewayControlHandlers(
            settings,
            client_factory=lambda _key, endpoints: GatewayAdminClient(
                socket_path=str(endpoints["admin_socket"]),
                main_password="fixture-main", timeout_seconds=5,
            ),
        ).as_handlers()
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="s5-recovery")
        assert await worker.drain() == 2

        connection = await _connect(database_url)
        try:
            grant = await connection.fetchrow(
                "SELECT id, opaque_id, lease_seq FROM grants WHERE binding_id=$1 AND gateway_id=$2",
                subject["binding_id"], gateway,
            )
            assert grant["lease_seq"] == 1
            old_id = uuid.uuid4()
            await connection.execute(
                "UPDATE grants SET desired_generation=2, state='pending', not_after=now()-interval '1 second' WHERE id=$1",
                grant["id"],
            )
            await connection.execute(
                """INSERT INTO outbox_operations
                   (id, operation_type, payload, idempotency_key, gateway_id, binding_id,
                    target_revision, status, attempts, max_attempts)
                   VALUES ($1, 'gateway.apply_grant', $2::jsonb, $3, $4, $5, 2, 'dead', 12, 12)""",
                old_id,
                json.dumps({"grant_id": str(grant["opaque_id"]), "generation": "2", "action": "apply"}),
                f"grant:{grant['opaque_id']}:gen:2:apply", gateway, subject["binding_id"],
            )
        finally:
            await connection.close()

        blocked_get = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert blocked_get.status == 503
        revision = (await blocked_get.json())["details"]["catalog_revision"]
        body["catalog_revision"] = revision
        # The old revision still fails before any new generation is made.
        stale = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "recovery-stale-sync-01"},
            json={**body, "catalog_revision": str(int(revision) - 1)},
        )
        assert stale.status == 409

        recovered = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "recovery-fresh-sync-01"}, json=body,
        )
        assert recovered.status == 200, await recovered.text()
        recovered_body = await recovered.json()
        assert recovered_body["grants"][0]["desired_generation"] == "3"
        assert recovered_body["access_application_state"] == "pending"
        still_pending = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert still_pending.status == 409
        fresh_body = {
            **body,
            "catalog_revision": (await still_pending.json())["details"]["catalog_revision"],
        }
        same = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "recovery-fresh-sync-01"}, json=body,
        )
        assert same.status == 200
        assert (await same.json())["operation_id"] == recovered_body["operation_id"]
        second = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "recovery-fresh-sync-02"}, json=fresh_body,
        )
        assert second.status == 200, await second.text()
        connection = await _connect(database_url)
        try:
            assert await connection.fetchval("SELECT desired_generation FROM grants WHERE id=$1", grant["id"]) == 3
            assert await connection.fetchval(
                "SELECT count(*) FROM grants WHERE binding_id=$1 AND gateway_id=$2",
                subject["binding_id"], gateway,
            ) == 1
            recovered_parent = uuid.UUID(recovered_body["operation_id"])
            assert await connection.fetchval(
                "SELECT status FROM outbox_operations WHERE id=$1", recovered_parent,
            ) == "done"
            assert await connection.fetchval(
                "SELECT count(*) FROM operation_receipts WHERE result->>'operation_id'=$1",
                str(recovered_parent),
            ) == 1
            assert await connection.fetchval("SELECT status FROM outbox_operations WHERE id=$1", old_id) == "dead"
            assert await connection.fetchval("SELECT attempts FROM outbox_operations WHERE id=$1", old_id) == 12
            assert await connection.fetchval(
                "SELECT count(*) FROM outbox_operations WHERE binding_id=$1 AND target_revision=3 AND operation_type='gateway.apply_grant'",
                subject["binding_id"],
            ) == 1
            assert await connection.fetchval(
                "SELECT count(*) FROM outbox_operations WHERE correlation_id=$1 AND operation_type='gateway.apply_grant'",
                recovered_parent,
            ) == 1
        finally:
            await connection.close()
        assert await worker.drain() == 2
        applied = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert applied.status == 200, await applied.text()
        connection = await _connect(database_url)
        try:
            state = await connection.fetchrow(
                "SELECT desired_generation, applied_generation, lease_seq, state FROM grants WHERE id=$1", grant["id"]
            )
            assert tuple(state) == (3, 3, 2, "applied")
            assert await connection.fetchval("SELECT status FROM outbox_operations WHERE id=$1", old_id) == "dead"
        finally:
            await connection.close()
    finally:
        await fake.stop()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "case",
    ["unexpired", "wrong_target", "newer_other_operation", "other_gateway_failure",
     "revoked_binding", "expired_entitlement"],
)
async def test_recovery_gate_fails_closed_for_other_states(catalog_env, case):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    gateway = await _add_gateway(database_url, "gw-recovery-deny")
    if case == "other_gateway_failure":
        await _add_gateway(database_url, "gw-recovery-other")
    revision = await _current_revision(database_url, subject["installation_id"])
    body = {"catalog_revision": revision, "binding_revision": "1"}
    first = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": "recovery-deny-initial-01"}, json=body,
    )
    assert first.status == 200, await first.text()
    connection = await _connect(database_url)
    try:
        grant = await connection.fetchrow(
            "SELECT id FROM grants WHERE binding_id=$1 AND gateway_id=$2", subject["binding_id"], gateway,
        )
        await connection.execute(
            "UPDATE outbox_operations SET status='dead' WHERE gateway_id=$1 AND binding_id=$2",
            gateway, subject["binding_id"],
        )
        if case == "other_gateway_failure":
            await connection.execute(
                "UPDATE outbox_operations SET status='dead' WHERE binding_id=$1 AND gateway_id<>$2",
                subject["binding_id"], gateway,
            )
        if case != "unexpired":
            await connection.execute(
                "UPDATE grants SET not_after=now()-interval '1 second' WHERE id=$1", grant["id"],
            )
        if case == "wrong_target":
            await connection.execute(
                "UPDATE outbox_operations SET target_revision=0 WHERE gateway_id=$1 AND binding_id=$2",
                gateway, subject["binding_id"],
            )
        elif case == "newer_other_operation":
            await connection.execute(
                """INSERT INTO outbox_operations
                   (operation_type, payload, idempotency_key, gateway_id, binding_id,
                    target_revision, status, attempts, max_attempts)
                   VALUES ('gateway.revoke_grant', '{}'::jsonb, $1, $2, $3, 1, 'failed', 1, 1)""",
                f"recovery-newer-{uuid.uuid4()}", gateway, subject["binding_id"],
            )
        elif case == "revoked_binding":
            await connection.execute(
                "UPDATE account_bindings SET status='revoked' WHERE id=$1", subject["binding_id"],
            )
        elif case == "expired_entitlement":
            await connection.execute(
                "UPDATE entitlements SET ends_at=now()-interval '1 second' WHERE id=$1",
                subject["entitlement_id"],
            )
        count_before = await connection.fetchval("SELECT count(*) FROM outbox_operations")
    finally:
        await connection.close()
    retry = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": "recovery-deny-fresh-02"}, json=body,
    )
    assert retry.status >= 400, await retry.text()
    connection = await _connect(database_url)
    try:
        assert await connection.fetchval("SELECT desired_generation FROM grants WHERE id=$1", grant["id"]) == 1
        assert await connection.fetchval("SELECT count(*) FROM outbox_operations") == count_before
    finally:
        await connection.close()
