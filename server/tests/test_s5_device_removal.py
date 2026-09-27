"""S5 §05.4–05.5 devices: list/delete-one, idempotency, isolation, fencing, durable revoke."""
from __future__ import annotations

import asyncio
import uuid
from datetime import UTC, datetime, timedelta

import pytest
from aiohttp.test_utils import TestClient, TestServer
from test_auth_flow import MOBILE, KeyMaterial, PoPClient, _challenge, _enroll
from test_auth_flow import _session as _auth_session
from test_step036_onboarding_hour_storage import _connect

from terlimo_backend.api import create_app
from terlimo_backend.auth_api import ApiError
from terlimo_backend.db import Database
from terlimo_backend.gateway_control import (
    APPLY_OPERATION,
    REVOKE_OPERATION,
    GatewayControlHandlers,
)
from terlimo_backend.session_auth import sha256_hex_text
from terlimo_backend.telegram_binding import LINK_PATH, _token_hash, confirm_registration
from terlimo_backend.worker import OutboxWorker

DEVICES = f"{MOBILE}/devices"
LINK = LINK_PATH


async def _app(settings_factory, migrated_url, **kwargs):
    settings = settings_factory(migrated_url, **kwargs)
    database = Database(settings)
    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    return client, settings, database


async def _bound_session(client, migrated_url, *, telegram_id):
    key = KeyMaterial()
    pop = PoPClient(key)
    assert (await _enroll(client, pop, await _challenge(client, key, "enrollment"))).status == 200
    c = await _connect(migrated_url)
    try:
        iid = await c.fetchval(
            "SELECT id FROM installations WHERE public_key_fingerprint=$1", pop.key.fingerprint
        )
        acc = await c.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified',$1) RETURNING id", telegram_id
        )
        bid = await c.fetchval(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active') RETURNING id",
            acc, iid,
        )
    finally:
        await c.close()
    resp = await _auth_session(
        client, pop, await _challenge(client, pop.key, "session"),
        ["session:read", "session:write"], f"idem-{uuid.uuid4().hex}",
    )
    assert resp.status == 200, await resp.text()
    token = (await resp.json())["session"]["session_id"]
    return token, acc, iid, bid


async def _add_device(connection, account_id, *, telegram_suffix=0):
    iid = await connection.fetchval(
        "INSERT INTO installations (environment, public_key_fingerprint, name, platform) VALUES ('test',$1,$2,'android') RETURNING id",
        f"fp-{uuid.uuid4().hex}", f"device-{telegram_suffix}",
    )
    bid = await connection.fetchval(
        "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active') RETURNING id",
        account_id, iid,
    )
    return bid


async def _grant(connection, binding_id):
    gateway_id = await connection.fetchval(
        "SELECT id FROM gateways LIMIT 1"
    )
    if gateway_id is None:
        gateway_id = await connection.fetchval(
            "INSERT INTO gateways (gateway_key, environment, registry_state) VALUES ($1,'test','registered') RETURNING id",
            f"gw-{uuid.uuid4().hex}",
        )
    await connection.execute(
        """
        INSERT INTO grants (binding_id, gateway_id, desired_generation, applied_generation, not_after, state)
        VALUES ($1, $2, 1, 1, now() + interval '1 hour', 'applied')
        """,
        binding_id, gateway_id,
    )
    return gateway_id


async def test_list_delete_one_keeps_other_and_subscription(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        token, account_id, _iid, current_bid = await _bound_session(client, migrated_url, telegram_id=991401)
        other_bid = await _add_device(c, account_id, telegram_suffix=2)
        ends = (datetime.now(UTC) + timedelta(days=30)).replace(microsecond=0)
        ent = await c.fetchval(
            "INSERT INTO entitlements (account_id, kind, status, ends_at, device_limit, revision) VALUES ($1,'paid','active',$2,2,1) RETURNING id",
            account_id, ends,
        )
        await _grant(c, current_bid)
        await _grant(c, other_bid)

        listing = await client.get(DEVICES, headers={"Authorization": f"Bearer {token}"})
        assert listing.status == 200
        body = await listing.json()
        assert body["slots_used"] == 2 and body["device_limit"] == 2
        assert "revision" in body and body["revision"].isdigit()
        assert sum(1 for d in body["devices"] if d["is_current"]) == 1
        assert all("device_id" in d and "id" not in d for d in body["devices"])

        other_iid = await c.fetchval("SELECT installation_id FROM account_bindings WHERE id=$1", other_bid)
        await c.execute(
            """
            INSERT INTO registration_links (token_sha256, installation_id, environment, status, expires_at)
            VALUES ($1,$2,'test','pending', now() + interval '10 minutes')
            """,
            _token_hash("pending-before-delete"),
            other_iid,
        )
        deleted = await client.delete(f"{DEVICES}/{other_bid}", headers={"Authorization": f"Bearer {token}"})
        assert deleted.status == 200, await deleted.text()
        payload = await deleted.json()
        assert payload["slot_released"] is True and payload["access_application_state"] == "pending"
        assert payload["residual_access_lease_seconds"] is None
        # other device + subscription unchanged
        assert await c.fetchval("SELECT status FROM account_bindings WHERE id=$1", current_bid) == "active"
        assert await c.fetchval("SELECT ends_at FROM entitlements WHERE id=$1", ent) == ends
        # removed device fenced: binding revoked, grants revoked+fenced, installation revoked, sessions revoked
        assert await c.fetchval("SELECT status FROM account_bindings WHERE id=$1", other_bid) == "revoked"
        assert await c.fetchval("SELECT state FROM grants WHERE binding_id=$1", other_bid) == "revoked"
        assert await c.fetchval("SELECT desired_generation > applied_generation FROM grants WHERE binding_id=$1", other_bid)
        # ordinary removal never sets a permanent installation state
        assert await c.fetchval("SELECT state FROM installations WHERE id=$1", other_iid) == "technical"
        # every pending/confirmed proof of the removed installation is invalidated
        assert await c.fetchval(
            "SELECT status FROM registration_links WHERE token_sha256=$1", _token_hash("pending-before-delete")
        ) == "expired"
        revoke_ops = await c.fetch("SELECT id,status FROM outbox_operations WHERE binding_id=$1 AND operation_type=$2", other_bid, REVOKE_OPERATION)
        assert len(revoke_ops) == 1 and revoke_ops[0]["status"] == "pending"
        assert payload["operation_id"] == str(revoke_ops[0]["id"])
        assert "status" in payload and "operation_id" in payload
        # the kept device's grant is untouched
        assert await c.fetchval("SELECT state FROM grants WHERE binding_id=$1", current_bid) == "applied"
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_repeat_delete_is_idempotent(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        token, account_id, _iid, _bid = await _bound_session(client, migrated_url, telegram_id=991402)
        other = await _add_device(c, account_id)
        await _grant(c, other)
        first = await client.delete(f"{DEVICES}/{other}", headers={"Authorization": f"Bearer {token}"})
        second = await client.delete(f"{DEVICES}/{other}", headers={"Authorization": f"Bearer {token}"})
        assert first.status == 200 and second.status == 200
        second_body = await second.json()
        assert second_body["status"] == "pending" and second_body["slot_released"] is True
        assert second_body["operation_id"] == (await first.json())["operation_id"]
        count = await c.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE binding_id=$1 AND operation_type=$2", other, REVOKE_OPERATION
        )
        assert count == 1
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_foreign_account_is_neutral_404(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        _tok_a, _acc_a, _iid_a, bid_a = await _bound_session(client, migrated_url, telegram_id=991403)
        token_b, _acc_b, _iid_b, _bid_b = await _bound_session(client, migrated_url, telegram_id=991404)
        resp = await client.delete(f"{DEVICES}/{bid_a}", headers={"Authorization": f"Bearer {token_b}"})
        assert resp.status == 404
        assert await c.fetchval("SELECT status FROM account_bindings WHERE id=$1", bid_a) == "active"
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_revoke_is_durable_and_retried_when_node_unavailable(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        token, account_id, _iid, _bid = await _bound_session(client, migrated_url, telegram_id=991405)
        other = await _add_device(c, account_id)
        await _grant(c, other)
        # proven revoke target so the handler reaches the RPC and fails on the unreachable socket
        await c.execute(
            """
            UPDATE grants SET target_node_id='gw-test',
                target_route='{"admin_socket": "/tmp/does-not-exist.sock"}'::jsonb,
                gateway_credential='fixture', applied_not_after=not_after, gateway_generation=1
            WHERE binding_id=$1
            """,
            other,
        )
        assert (await client.delete(f"{DEVICES}/{other}", headers={"Authorization": f"Bearer {token}"})).status == 200
        # gateway handlers pointing at a non-existent socket: the durable op must fail and retry,
        # never report the revoke as applied.
        gw_settings = settings_factory(
            migrated_url, gateway_local_admin_enabled=True,
            gateway_admin_main_password="fixture", gateway_admin_timeout_seconds=1,
        )

        def factory(_key, endpoints):
            from terlimo_backend.gateway_adapter import GatewayAdminClient

            return GatewayAdminClient(
                socket_path="/tmp/does-not-exist.sock", main_password="fixture", timeout_seconds=1
            )

        handlers = GatewayControlHandlers(gw_settings, client_factory=factory).as_handlers()
        worker = OutboxWorker(database, gw_settings, handlers=handlers, worker_id="revoke-retry")
        assert await worker.drain() == 1
        row = await c.fetchrow(
            "SELECT status, attempts FROM outbox_operations WHERE binding_id=$1 AND operation_type=$2", other, REVOKE_OPERATION
        )
        assert row["status"] == "pending" and row["attempts"] >= 1
        assert await c.fetchval("SELECT state FROM grants WHERE binding_id=$1", other) == "revoked"
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_delayed_apply_after_revoke_does_not_restore_access(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        token, account_id, _iid, _bid = await _bound_session(client, migrated_url, telegram_id=991406)
        other = await _add_device(c, account_id)
        gateway_id = await _grant(c, other)
        assert (await client.delete(f"{DEVICES}/{other}", headers={"Authorization": f"Bearer {token}"})).status == 200
        grant = await c.fetchrow("SELECT * FROM grants WHERE binding_id=$1", other)
        # A delayed apply event for the removed binding must be fenced out (revoked generation).
        await c.execute(
            """
            INSERT INTO outbox_operations (operation_type, payload, idempotency_key, binding_id, gateway_id, target_revision)
            VALUES ($1, $2::jsonb, $3, $4, $5, $6)
            """,
            APPLY_OPERATION,
            f'{{"grant_id": "{grant["opaque_id"]}", "generation": "{grant["desired_generation"]}", "action": "apply"}}',
            f"late-apply:{uuid.uuid4().hex}", other, gateway_id, grant["desired_generation"],
        )
        gw_settings = settings_factory(
            migrated_url, gateway_local_admin_enabled=True,
            gateway_admin_main_password="fixture", gateway_admin_timeout_seconds=1,
        )

        def factory(_key, endpoints):  # must never be called: the op is fenced before RPC
            raise AssertionError("gateway RPC must not run for a revoked grant")

        handlers = GatewayControlHandlers(gw_settings, client_factory=factory).as_handlers()
        worker = OutboxWorker(database, gw_settings, handlers=handlers, worker_id="late-apply")
        await worker.drain()
        after = await c.fetchrow("SELECT * FROM grants WHERE binding_id=$1", other)
        assert after["state"] == "revoked"
        assert int(after["applied_generation"]) == int(grant["applied_generation"])
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_binding_limit_fences_last_slot_race(migrated_url, settings_factory):
    c = await _connect(migrated_url)
    try:
        settings = settings_factory(
            migrated_url, telegram_bot_username="bot", telegram_bot_key="key",
        )
        account_id = await c.fetchval("INSERT INTO accounts (status, telegram_id) VALUES ('verified', 991499) RETURNING id")
        await c.execute(
            "INSERT INTO entitlements (account_id, kind, status, ends_at, device_limit) VALUES ($1,'paid','active', now()+interval '30 days', 2)",
            account_id,
        )
        for _ in range(2):
            iid = await c.fetchval(
                "INSERT INTO installations (environment, platform, public_key_fingerprint) VALUES ('test','android',$1) RETURNING id",
                f"fp-{uuid.uuid4().hex}",
            )
            await c.execute(
                "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')",
                account_id, iid,
            )
        # a third installation tries to confirm registration for the same account at the limit
        iid3 = await c.fetchval(
            "INSERT INTO installations (environment, platform, public_key_fingerprint) VALUES ('test','android',$1) RETURNING id",
            f"fp-{uuid.uuid4().hex}",
        )
        token = "race-token"
        await c.execute(
            """
            INSERT INTO registration_links (token_sha256, installation_id, environment, expires_at)
            VALUES ($1,$2,'test', now() + interval '10 minutes')
            """,
            _token_hash(token), iid3,
        )
        with pytest.raises(Exception) as error:
            await confirm_registration(c, settings, token=token, telegram_id=991499, telegram_username="u")
        assert "DEVICE_LIMIT_REACHED" in str(error.value)
        assert getattr(error.value, "details", None) == {"slots_used": 2, "device_limit": 2}
        # the Telegram proof is committed even though the binding is not created
        assert await c.fetchval(
            "SELECT status FROM registration_links WHERE token_sha256=$1", _token_hash(token)
        ) == "confirmed"
        assert await c.fetchval(
            "SELECT count(*) FROM account_bindings WHERE account_id=$1 AND status='active'", account_id
        ) == 2
        assert await c.fetchval(
            "SELECT count(*) FROM account_bindings WHERE installation_id=$1", iid3
        ) == 0
        # no trial marker / no new entitlement for the limit-window proof
        assert await c.fetchval(
            "SELECT trial_reason FROM registration_links WHERE token_sha256=$1", _token_hash(token)
        ) != "device_limit_reached"
    finally:
        await c.close()


async def _unbound_session(client, migrated_url, key=None, pop=None):
    key = key or KeyMaterial()
    pop = pop or PoPClient(key)
    assert (await _enroll(client, pop, await _challenge(client, key, "enrollment"))).status == 200
    c = await _connect(migrated_url)
    try:
        iid = await c.fetchval(
            "SELECT id FROM installations WHERE public_key_fingerprint=$1", pop.key.fingerprint
        )
    finally:
        await c.close()
    resp = await _auth_session(
        client, pop, await _challenge(client, key, "session"),
        ["session:read", "session:write"], f"unbound-{uuid.uuid4().hex}",
    )
    assert resp.status == 200, await resp.text()
    token = (await resp.json())["session"]["session_id"]
    return token, iid, key, pop


async def test_full_slot_proof_manages_without_data_then_rebinds(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(
        settings_factory, migrated_url, telegram_bot_username="bot", telegram_bot_key="key"
    )
    c = await _connect(migrated_url)
    try:
        settings = settings_factory(
            migrated_url, telegram_bot_username="bot", telegram_bot_key="key",
        )
        telegram_id = 991421
        account_id = await c.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified',$1) RETURNING id", telegram_id
        )
        await c.execute(
            "INSERT INTO entitlements (account_id, kind, status, ends_at, device_limit) VALUES ($1,'trial','active', now()+interval '7 days', 2)",
            account_id,
        )
        tokens = [await _unbound_session(client, migrated_url) for _ in range(2)]
        for _, iid, _, _ in tokens:
            await c.execute(
                "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')",
                account_id, iid,
            )
        tok_c, iid_c, key_c, pop_c = await _unbound_session(client, migrated_url)

        link = await client.post(LINK, headers={"Authorization": f"Bearer {tok_c}"})
        assert link.status == 200, await link.text()
        raw = (await link.json())["registration"]["token"]
        with pytest.raises(Exception) as error:
            await confirm_registration(c, settings, token=raw, telegram_id=telegram_id, telegram_username="u")
        assert getattr(error.value, "code", "") == "DEVICE_LIMIT_REACHED"
        assert await c.fetchval(
            "SELECT status FROM registration_links WHERE token_sha256=$1", _token_hash(raw)
        ) == "confirmed"
        assert await c.fetchval(
            "SELECT count(*) FROM account_bindings WHERE installation_id=$1", iid_c
        ) == 0

        listing = await client.get(DEVICES, headers={"Authorization": f"Bearer {tok_c}"})
        assert listing.status == 200
        body = await listing.json()
        assert body["slots_used"] == 2 and body["device_limit"] == 2
        assert len(body["devices"]) == 2
        me = await client.get(f"{MOBILE}/me", headers={"Authorization": f"Bearer {tok_c}"})
        assert me.status == 200
        me_body = await me.json()
        # limit window is derivable client-side: confirmed registration, no active binding,
        # and the device list already at the limit
        assert me_body["registration"]["state"] == "registered"
        assert me_body["binding_status"] == "none"
        # no data was granted to the limit-window installation
        assert await c.fetchval(
            "SELECT count(*) FROM grants WHERE binding_id IN (SELECT id FROM account_bindings WHERE installation_id=$1)",
            iid_c,
        ) == 0

        victim = body["devices"][0]["device_id"]
        removed = await client.delete(f"{DEVICES}/{victim}", headers={"Authorization": f"Bearer {tok_c}"})
        assert removed.status == 200, await removed.text()
        removed_body = await removed.json()
        assert removed_body["status"] == "ok" and removed_body["slot_released"] is True

        relink = await client.post(LINK, headers={"Authorization": f"Bearer {tok_c}"})
        assert relink.status == 200, await relink.text()
        new_raw = (await relink.json())["registration"]["token"]
        assert new_raw != raw
        with pytest.raises(ApiError) as expired_error:
            await confirm_registration(c, settings, token=raw, telegram_id=telegram_id, telegram_username="u")
        assert expired_error.value.code == "REGISTRATION_EXPIRED"
        confirmed = await confirm_registration(
            c, settings, token=new_raw, telegram_id=telegram_id, telegram_username="u"
        )
        assert confirmed["binding_status"] == "active" and confirmed["slots_used"] == 2
        assert await c.fetchval(
            "SELECT status FROM account_bindings WHERE installation_id=$1", iid_c
        ) == "active"
        # the pre-bind session is not retro-bound; the standard new session sees the binding
        fresh = await _auth_session(
            client, pop_c, await _challenge(client, key_c, "session"),
            ["session:read", "session:write"], f"rebind-{uuid.uuid4().hex}",
        )
        assert fresh.status == 200, await fresh.text()
        fresh_token = (await fresh.json())["session"]["session_id"]
        me = await client.get(f"{MOBILE}/me", headers={"Authorization": f"Bearer {fresh_token}"})
        assert me.status == 200
        assert (await me.json())["binding_status"] == "active"
        listing = await client.get(DEVICES, headers={"Authorization": f"Bearer {fresh_token}"})
        assert listing.status == 200
        assert any(d["is_current"] for d in (await listing.json())["devices"])
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_delete_invalidates_tokens_and_rebind_does_not_revive_old_access(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(
        settings_factory, migrated_url, telegram_bot_username="bot", telegram_bot_key="key"
    )
    c = await _connect(migrated_url)
    try:
        settings = settings_factory(
            migrated_url, telegram_bot_username="bot", telegram_bot_key="key",
        )
        telegram_id = 991422
        key = KeyMaterial()
        pop = PoPClient(key)
        assert (await _enroll(client, pop, await _challenge(client, key, "enrollment"))).status == 200
        iid = await c.fetchval(
            "SELECT id FROM installations WHERE public_key_fingerprint=$1", pop.key.fingerprint
        )
        account_id = await c.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified',$1) RETURNING id", telegram_id
        )
        bid = await c.fetchval(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active') RETURNING id",
            account_id, iid,
        )
        await _grant(c, bid)
        resp = await _auth_session(
            client, pop, await _challenge(client, key, "session"),
            ["session:read", "session:write"], f"del-{uuid.uuid4().hex}",
        )
        assert resp.status == 200, await resp.text()
        token = (await resp.json())["session"]["session_id"]

        confirmed_raw = "confirmed-before-delete"
        pending_raw = "pending-before-delete"
        await c.execute(
            """
            INSERT INTO registration_links (token_sha256, installation_id, environment, status, expires_at)
            VALUES ($1,$2,'test','pending', now() + interval '10 minutes')
            """,
            _token_hash(confirmed_raw), iid,
        )
        await confirm_registration(
            c, settings, token=confirmed_raw, telegram_id=telegram_id, telegram_username="u"
        )
        await c.execute(
            """
            INSERT INTO registration_links (token_sha256, installation_id, environment, status, expires_at)
            VALUES ($1,$2,'test','pending', now() + interval '10 minutes')
            """,
            _token_hash(pending_raw), iid,
        )

        removed = await client.delete(f"{DEVICES}/{bid}", headers={"Authorization": f"Bearer {token}"})
        assert removed.status == 200, await removed.text()
        assert await c.fetchval("SELECT state FROM installations WHERE id=$1", iid) == "technical"
        for raw in (confirmed_raw, pending_raw):
            assert await c.fetchval(
                "SELECT status FROM registration_links WHERE token_sha256=$1", _token_hash(raw)
            ) == "expired"
            with pytest.raises(Exception) as error:
                await confirm_registration(c, settings, token=raw, telegram_id=telegram_id, telegram_username="u")
            assert getattr(error.value, "code", "") == "REGISTRATION_EXPIRED"
        stale = await client.get(DEVICES, headers={"Authorization": f"Bearer {token}"})
        assert stale.status == 401

        fresh = await _auth_session(
            client, pop, await _challenge(client, key, "session"),
            ["session:read", "session:write"], f"del2-{uuid.uuid4().hex}",
        )
        assert fresh.status == 200, await fresh.text()
        fresh_token = (await fresh.json())["session"]["session_id"]
        relink = await client.post(LINK, headers={"Authorization": f"Bearer {fresh_token}"})
        assert relink.status == 200, await relink.text()
        new_raw = (await relink.json())["registration"]["token"]
        confirmed = await confirm_registration(
            c, settings, token=new_raw, telegram_id=telegram_id, telegram_username="u"
        )
        assert confirmed["binding_status"] == "active"
        assert await c.fetchval("SELECT id FROM account_bindings WHERE installation_id=$1 AND status='active'", iid) == bid
        assert await c.fetchval("SELECT generation FROM account_bindings WHERE id=$1", bid) >= 2
        # the pre-removal session and its bearer are dead for good
        assert (await client.get(DEVICES, headers={"Authorization": f"Bearer {token}"})).status == 401
        # the old grant is not revived by the reactivation
        grant = await c.fetchrow("SELECT state, desired_generation, applied_generation FROM grants WHERE binding_id=$1", bid)
        assert grant["state"] == "revoked"
        assert int(grant["desired_generation"]) > int(grant["applied_generation"])
        # replay of the rebind token is idempotent and creates no extra binding
        replay = await confirm_registration(
            c, settings, token=new_raw, telegram_id=telegram_id, telegram_username="u"
        )
        assert replay["binding_status"] == "active"
        assert await c.fetchval(
            "SELECT count(*) FROM account_bindings WHERE installation_id=$1", iid
        ) == 1
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_effective_entitlement_window_limits_slots(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        token, account_id, _iid, _bid = await _bound_session(client, migrated_url, telegram_id=991430)
        headers = {"Authorization": f"Bearer {token}"}

        assert (await (await client.get(DEVICES, headers=headers)).json())["device_limit"] == 2
        # future right is not effective
        await c.execute(
            "INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at, device_limit) VALUES ($1,'paid','active', now() + interval '1 day', now() + interval '30 days', 5)",
            account_id,
        )
        assert (await (await client.get(DEVICES, headers=headers)).json())["device_limit"] == 2
        # expired right is not effective
        await c.execute(
            "INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at, device_limit) VALUES ($1,'paid','active', now() - interval '30 days', now() - interval '1 day', 5)",
            account_id,
        )
        assert (await (await client.get(DEVICES, headers=headers)).json())["device_limit"] == 2
        # current right is effective, most recent effective one wins
        await c.execute(
            "INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at, device_limit) VALUES ($1,'trial','active', now() - interval '1 hour', now() + interval '7 days', 1)",
            account_id,
        )
        body = await (await client.get(DEVICES, headers=headers)).json()
        assert body["device_limit"] == 1 and body["slots_used"] == 1
        await c.execute(
            "INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at, device_limit) VALUES ($1,'paid','active', now() - interval '1 minute', now() + interval '30 days', 3)",
            account_id,
        )
        assert (await (await client.get(DEVICES, headers=headers)).json())["device_limit"] == 3
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_prepared_delete_fails_after_caller_revoked_while_waiting(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    lock_conn = await _connect(migrated_url)
    try:
        token, account_id, _iid, caller_bid = await _bound_session(client, migrated_url, telegram_id=991431)
        victim = await _add_device(c, account_id)
        await c.execute(
            """
            INSERT INTO registration_links (token_sha256, installation_id, environment, status, expires_at)
            VALUES ($1,(SELECT installation_id FROM account_bindings WHERE id=$2),'test','pending', now() + interval '10 minutes')
            """,
            _token_hash("victim-pending"), victim,
        )
        # Hold the account serialization point so the delete authorizes first and then waits.
        await lock_conn.execute("BEGIN")
        await lock_conn.execute(
            "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", f"bind-account:{account_id}"
        )
        task = asyncio.create_task(
            client.delete(f"{DEVICES}/{victim}", headers={"Authorization": f"Bearer {token}"})
        )
        await asyncio.sleep(0.4)
        # The caller's session is revoked while its prepared delete waits on the lock.
        await c.execute(
            "UPDATE sessions SET revoked_at = now(), generation = generation + 1 WHERE binding_id = $1",
            caller_bid,
        )
        await lock_conn.execute("COMMIT")
        response = await asyncio.wait_for(task, timeout=10)
        assert response.status == 401, await response.text()
        assert await c.fetchval("SELECT status FROM account_bindings WHERE id=$1", victim) == "active"
        assert await c.fetchval(
            "SELECT status FROM registration_links WHERE token_sha256=$1", _token_hash("victim-pending")
        ) == "pending"
    finally:
        await database.close()
        await client.close()
        await c.close()
        await lock_conn.close()


async def test_concurrent_confirm_and_delete_do_not_deadlock(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(
        settings_factory, migrated_url, telegram_bot_username="bot", telegram_bot_key="key"
    )
    c = await _connect(migrated_url)
    confirm_conn = await _connect(migrated_url)
    try:
        settings = settings_factory(
            migrated_url, telegram_bot_username="bot", telegram_bot_key="key",
        )
        token, account_id, _iid, caller_bid = await _bound_session(client, migrated_url, telegram_id=991432)
        await _add_device(c, account_id)
        victim = await c.fetchval(
            "SELECT id FROM account_bindings WHERE account_id=$1 AND status='active' AND id <> $2 LIMIT 1",
            account_id, caller_bid,
        )
        await c.execute(
            "INSERT INTO entitlements (account_id, kind, status, ends_at, device_limit) VALUES ($1,'paid','active', now()+interval '30 days', 2)",
            account_id,
        )
        for attempt in range(3):
            iid = await c.fetchval(
                "INSERT INTO installations (environment, platform, public_key_fingerprint) VALUES ('test','android',$1) RETURNING id",
                f"fp-race-{uuid.uuid4().hex}",
            )
            raw = f"race-confirm-{uuid.uuid4().hex}"
            await c.execute(
                """
                INSERT INTO registration_links (token_sha256, installation_id, environment, status, expires_at)
                VALUES ($1,$2,'test','pending', now() + interval '10 minutes')
                """,
                _token_hash(raw), iid,
            )
            delete_task = asyncio.create_task(
                client.delete(f"{DEVICES}/{victim}", headers={"Authorization": f"Bearer {token}"})
            )
            confirm_result: dict = {}

            async def run_confirm(token_value=raw, sink=confirm_result):
                try:
                    sink["result"] = await confirm_registration(
                        confirm_conn, settings, token=token_value, telegram_id=991432, telegram_username="u"
                    )
                except Exception as error:  # noqa: BLE001 - asserted below
                    sink["error"] = getattr(error, "code", str(error))

            confirm_task = asyncio.create_task(run_confirm())
            await asyncio.wait_for(asyncio.gather(delete_task, confirm_task), timeout=15)
            delete_response = delete_task.result()
            assert delete_response.status == 200, (attempt, await delete_response.text())
            active = await c.fetchval(
                "SELECT count(*) FROM account_bindings WHERE account_id=$1 AND status='active'", account_id
            )
            assert active <= 2
            bound_new = await c.fetchval(
                "SELECT count(*) FROM account_bindings WHERE installation_id=$1 AND status='active'", iid
            )
            if "result" in confirm_result:
                assert bound_new == 1
                assert active == 2
            else:
                assert confirm_result.get("error") == "DEVICE_LIMIT_REACHED"
                assert bound_new == 0
                assert await c.fetchval(
                    "SELECT status FROM registration_links WHERE token_sha256=$1", _token_hash(raw)
                ) == "confirmed"
            # reset for next attempt: restore one free slot deterministically
            if bound_new:
                await c.execute(
                    "UPDATE account_bindings SET status='revoked', revoked_at=now(), generation=generation+1 WHERE installation_id=$1 AND status='active'",
                    iid,
                )
            await c.execute(
                "UPDATE account_bindings SET status='active', revoked_at=NULL WHERE id=$1", victim
            )
    finally:
        await database.close()
        await client.close()
        await c.close()
        await confirm_conn.close()


async def test_rebind_same_account_after_other_account_uses_exact_pair(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(
        settings_factory, migrated_url, telegram_bot_username="bot", telegram_bot_key="key"
    )
    c = await _connect(migrated_url)
    try:
        settings = settings_factory(
            migrated_url, telegram_bot_username="bot", telegram_bot_key="key",
        )
        key = KeyMaterial()
        pop = PoPClient(key)
        assert (await _enroll(client, pop, await _challenge(client, key, "enrollment"))).status == 200
        iid = await c.fetchval(
            "SELECT id FROM installations WHERE public_key_fingerprint=$1", pop.key.fingerprint
        )
        account_a = await c.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified',991433) RETURNING id"
        )
        account_b = await c.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified',991434) RETURNING id"
        )
        binding_a = await c.fetchval(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active') RETURNING id",
            account_a, iid,
        )
        session_a = await _auth_session(
            client, pop, await _challenge(client, key, "session"),
            ["session:read", "session:write"], f"a-{uuid.uuid4().hex}",
        )
        token_a = (await session_a.json())["session"]["session_id"]
        assert (await client.delete(f"{DEVICES}/{binding_a}", headers={"Authorization": f"Bearer {token_a}"})).status == 200

        raw_b = "rebind-b"
        await c.execute(
            """
            INSERT INTO registration_links (token_sha256, installation_id, environment, status, expires_at)
            VALUES ($1,$2,'test','pending', now() + interval '10 minutes')
            """,
            _token_hash(raw_b), iid,
        )
        confirmed_b = await confirm_registration(
            c, settings, token=raw_b, telegram_id=991434, telegram_username="u"
        )
        assert confirmed_b["binding_status"] == "active"
        binding_b = await c.fetchval(
            "SELECT id FROM account_bindings WHERE account_id=$1 AND installation_id=$2 AND status='active'",
            account_b, iid,
        )
        token_b = (
            await (
                await _auth_session(
                    client, pop, await _challenge(client, key, "session"),
                    ["session:read", "session:write"], f"b-{uuid.uuid4().hex}",
                )
            ).json()
        )["session"]["session_id"]
        assert (await client.delete(f"{DEVICES}/{binding_b}", headers={"Authorization": f"Bearer {token_b}"})).status == 200

        raw_a = "rebind-a"
        await c.execute(
            """
            INSERT INTO registration_links (token_sha256, installation_id, environment, status, expires_at)
            VALUES ($1,$2,'test','pending', now() + interval '10 minutes')
            """,
            _token_hash(raw_a), iid,
        )
        confirmed_a = await confirm_registration(
            c, settings, token=raw_a, telegram_id=991433, telegram_username="u"
        )
        assert confirmed_a["binding_status"] == "active"
        assert await c.fetchval(
            "SELECT id FROM account_bindings WHERE account_id=$1 AND installation_id=$2 AND status='active'",
            account_a, iid,
        ) == binding_a
        assert int(await c.fetchval("SELECT generation FROM account_bindings WHERE id=$1", binding_a)) >= 2
        assert await c.fetchval("SELECT status FROM account_bindings WHERE id=$1", binding_b) == "revoked"
        assert await c.fetchval(
            "SELECT count(*) FROM account_bindings WHERE installation_id=$1 AND status='active'", iid
        ) == 1
        # all pre-removal bearers stay dead
        assert (await client.get(DEVICES, headers={"Authorization": f"Bearer {token_a}"})).status == 401
        assert (await client.get(DEVICES, headers={"Authorization": f"Bearer {token_b}"})).status == 401
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_already_revoked_replay_does_not_cancel_new_proof(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        token, account_id, _iid, _caller = await _bound_session(client, migrated_url, telegram_id=991435)
        victim = await _add_device(c, account_id)
        await _grant(c, victim)
        first = await client.delete(f"{DEVICES}/{victim}", headers={"Authorization": f"Bearer {token}"})
        assert first.status == 200
        ops_before = await c.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE binding_id=$1 AND operation_type=$2", victim, REVOKE_OPERATION
        )
        await c.execute(
            """
            INSERT INTO registration_links (token_sha256, installation_id, environment, status, expires_at)
            VALUES ($1,(SELECT installation_id FROM account_bindings WHERE id=$2),'test','pending', now() + interval '10 minutes')
            """,
            _token_hash("fresh-after-removal"), victim,
        )
        replay = await client.delete(f"{DEVICES}/{victim}", headers={"Authorization": f"Bearer {token}"})
        replay_body = await replay.json()
        assert replay.status == 200 and replay_body["slot_released"] is True
        assert replay_body["operation_id"] == (await first.json())["operation_id"]
        assert await c.fetchval(
            "SELECT status FROM registration_links WHERE token_sha256=$1", _token_hash("fresh-after-removal")
        ) == "pending"
        assert await c.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE binding_id=$1 AND operation_type=$2", victim, REVOKE_OPERATION
        ) == ops_before
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_removed_installation_reports_device_removed(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        # no binding history at all -> forbidden, not removed
        token_clean, _iid_clean, _key_clean, _pop_clean = await _unbound_session(client, migrated_url)
        response = await client.get(DEVICES, headers={"Authorization": f"Bearer {token_clean}"})
        assert response.status == 403
        assert (await response.json())["code"] == "DEVICE_MANAGEMENT_FORBIDDEN"

        key = KeyMaterial()
        pop = PoPClient(key)
        assert (await _enroll(client, pop, await _challenge(client, key, "enrollment"))).status == 200
        iid = await c.fetchval(
            "SELECT id FROM installations WHERE public_key_fingerprint=$1", pop.key.fingerprint
        )
        account_id = await c.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified',991436) RETURNING id"
        )
        binding = await c.fetchval(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active') RETURNING id",
            account_id, iid,
        )
        token = (
            await (
                await _auth_session(
                    client, pop, await _challenge(client, key, "session"),
                    ["session:read", "session:write"], f"self-{uuid.uuid4().hex}",
                )
            ).json()
        )["session"]["session_id"]
        assert (await client.delete(f"{DEVICES}/{binding}", headers={"Authorization": f"Bearer {token}"})).status == 200
        # old bearer is a plain 401, the fresh technical session gets the explicit code
        assert (await client.get(DEVICES, headers={"Authorization": f"Bearer {token}"})).status == 401
        fresh = await _auth_session(
            client, pop, await _challenge(client, key, "session"),
            ["session:read", "session:write"], f"self2-{uuid.uuid4().hex}",
        )
        fresh_token = (await fresh.json())["session"]["session_id"]
        response = await client.get(DEVICES, headers={"Authorization": f"Bearer {fresh_token}"})
        assert response.status == 403
        assert (await response.json())["code"] == "DEVICE_REMOVED"
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_unbound_caller_revoked_while_waiting_lock_is_rejected(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(
        settings_factory, migrated_url, telegram_bot_username="bot", telegram_bot_key="key"
    )
    c = await _connect(migrated_url)
    lock_conn = await _connect(migrated_url)
    try:
        settings = settings_factory(
            migrated_url, telegram_bot_username="bot", telegram_bot_key="key",
        )
        account_id = await c.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified',991440) RETURNING id"
        )
        await c.execute(
            "INSERT INTO entitlements (account_id, kind, status, ends_at, device_limit) VALUES ($1,'paid','active', now()+interval '30 days', 2)",
            account_id,
        )
        victim = await _add_device(c, account_id)
        await _add_device(c, account_id)
        # unbound installation with a committed confirmed proof (full slots -> no binding)
        tok_c, _iid_c, _key_c, _pop_c = await _unbound_session(client, migrated_url)
        link = await client.post(LINK, headers={"Authorization": f"Bearer {tok_c}"})
        raw = (await link.json())["registration"]["token"]
        with pytest.raises(ApiError) as limit_error:
            await confirm_registration(c, settings, token=raw, telegram_id=991440, telegram_username="u")
        assert limit_error.value.code == "DEVICE_LIMIT_REACHED"
        await lock_conn.execute("BEGIN")
        await lock_conn.execute(
            "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", f"bind-account:{account_id}"
        )
        task = asyncio.create_task(
            client.delete(f"{DEVICES}/{victim}", headers={"Authorization": f"Bearer {tok_c}"})
        )
        await asyncio.sleep(0.4)
        # the unbound caller session is revoked while the delete waits on the lock
        await c.execute(
            "UPDATE sessions SET revoked_at = now(), generation = generation + 1 WHERE token_sha256 = $1",
            sha256_hex_text(tok_c),
        )
        await lock_conn.execute("COMMIT")
        response = await asyncio.wait_for(task, timeout=10)
        assert response.status == 401, await response.text()
        assert await c.fetchval("SELECT status FROM account_bindings WHERE id=$1", victim) == "active"
        assert await c.fetchval(
            "SELECT status FROM registration_links WHERE token_sha256=$1", _token_hash(raw)
        ) == "confirmed"
    finally:
        await database.close()
        await client.close()
        await c.close()
        await lock_conn.close()


async def _wait_for_waiters(connection, expected: int, timeout: float = 8.0) -> None:
    """Wait until `expected` backend locks are queued (tuple + transactionid waiters).

    A second row-lock waiter queues as a transactionid ShareLock, so the relation filter alone
    undercounts; counting all ungranted locks from other backends is the deterministic barrier.
    """
    deadline = asyncio.get_event_loop().time() + timeout
    seen = 0
    while asyncio.get_event_loop().time() < deadline:
        seen = int(await connection.fetchval(
            "SELECT count(*) FROM pg_locks WHERE NOT granted AND pid <> pg_backend_pid()"
        ))
        if seen >= expected:
            return
        await asyncio.sleep(0.05)
    raise AssertionError(f"expected {expected} queued waiters, saw {seen}")


async def test_cross_account_confirm_vs_delete_barrier_on_installation(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(
        settings_factory, migrated_url, telegram_bot_username="bot", telegram_bot_key="key"
    )
    c = await _connect(migrated_url)
    watcher = await _connect(migrated_url)
    confirm_conn = await _connect(migrated_url)
    lock_conn = await _connect(migrated_url)
    try:
        settings = settings_factory(
            migrated_url, telegram_bot_username="bot", telegram_bot_key="key",
        )
        # account A owns installation I; account B will challenge it with a fresh Telegram proof
        token_a, _account_a, _iid_a, binding_a = await _bound_session(client, migrated_url, telegram_id=991441)
        account_b = await c.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified',991442) RETURNING id"
        )
        await c.execute(
            "INSERT INTO entitlements (account_id, kind, status, ends_at, device_limit) VALUES ($1,'paid','active', now()+interval '30 days', 1)",
            account_b,
        )
        raw = "cross-account-proof"
        await c.execute(
            """
            INSERT INTO registration_links (token_sha256, installation_id, environment, status, expires_at)
            VALUES ($1,(SELECT installation_id FROM account_bindings WHERE id=$2),'test','pending', now() + interval '10 minutes')
            """,
            _token_hash(raw), binding_a,
        )
        installation_id = await c.fetchval(
            "SELECT installation_id FROM account_bindings WHERE id=$1", binding_a
        )
        # deterministic barrier: both operations must queue on the same installation row lock
        await lock_conn.execute("BEGIN")
        await lock_conn.execute("SELECT id FROM installations WHERE id = $1 FOR UPDATE", installation_id)
        confirm_result: dict = {}

        async def run_confirm(sink=confirm_result):
            try:
                sink["result"] = await confirm_registration(
                    confirm_conn, settings, token=raw, telegram_id=991442, telegram_username="u"
                )
            except Exception as error:  # noqa: BLE001 - asserted below
                sink["error"] = getattr(error, "code", str(error))

        delete_task = asyncio.create_task(
            client.delete(f"{DEVICES}/{binding_a}", headers={"Authorization": f"Bearer {token_a}"})
        )
        confirm_task = asyncio.create_task(run_confirm())
        await _wait_for_waiters(watcher, 2)
        await lock_conn.execute("COMMIT")
        await asyncio.wait_for(asyncio.gather(delete_task, confirm_task), timeout=15)
        delete_response = delete_task.result()
        assert delete_response.status == 200, await delete_response.text()
        assert await c.fetchval("SELECT status FROM account_bindings WHERE id=$1", binding_a) == "revoked"
        if "result" in confirm_result:
            assert confirm_result["result"]["binding_status"] == "active"
            assert await c.fetchval(
                "SELECT count(*) FROM account_bindings WHERE account_id=$1 AND installation_id=$2 AND status='active'",
                account_b, installation_id,
            ) == 1
        else:
            # delete released the slot first, so a conflict is the only honest alternative lock winner outcome
            assert confirm_result["error"] in {"REGISTRATION_CONFLICT", "DEVICE_LIMIT_REACHED"}
    finally:
        await database.close()
        await client.close()
        await c.close()
        await watcher.close()
        await confirm_conn.close()
        await lock_conn.close()


async def test_devices_revision_stable_across_me_and_changes_on_state(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        token, account_id, _iid, _caller = await _bound_session(client, migrated_url, telegram_id=991460)
        await _add_device(c, account_id)
        headers = {"Authorization": f"Bearer {token}"}
        first = await client.get(DEVICES, headers=headers)
        r0 = (await first.json())["revision"]
        # alternating /me and /devices with no state change must not bump either revision
        for _ in range(4):
            me = await client.get(f"{MOBILE}/me", headers=headers)
            assert me.status == 200
            assert (await me.json())["revision"]
            again = await client.get(DEVICES, headers=headers)
            assert (await again.json())["revision"] == r0
        # a real delete changes the device snapshot exactly one step
        victim = next(
            device["device_id"] for device in (await first.json())["devices"] if not device["is_current"]
        )
        assert (await client.delete(f"{DEVICES}/{victim}", headers=headers)).status == 200
        results = await asyncio.gather(
            *[client.get(DEVICES, headers=headers) for _ in range(5)]
        )
        revisions = {(await item.json())["revision"] for item in results}
        assert revisions == {str(int(r0) + 1)}
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_worker_readback_marks_revoke_applied(migrated_url, settings_factory):
    from test_gateway_control import FakeGatewayAdmin, _run_once, _seed, _settings

    from terlimo_backend.api import create_app
    from terlimo_backend.db import Database
    from terlimo_backend.gateway_control import ensure_grant

    gateway = FakeGatewayAdmin(node_id="fake-gw")
    await gateway.start()
    settings = _settings(migrated_url, settings_factory)
    database = Database(settings)
    await database.connect()
    ids = await _seed(migrated_url, gateway)
    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    c = await _connect(migrated_url)
    try:
        async with database.acquire() as connection:
            await ensure_grant(
                connection,
                binding_id=ids["binding"],
                gateway_id=ids["gateway"],
                entitlement_id=ids["entitlement"],
                max_lease_seconds=settings.gateway_max_lease_seconds,
            )
        assert await _run_once(database, settings, migrated_url) >= 1
        # a separate active device of the same account performs the removal and its replay
        caller_installation = await c.fetchval(
            "INSERT INTO installations (environment, platform, public_key_fingerprint) VALUES ('test','android',$1) RETURNING id",
            f"fp-{uuid.uuid4().hex}",
        )
        caller_binding = await c.fetchval(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active') RETURNING id",
            ids["account"],
            caller_installation,
        )
        raw = "worker-readback-token"
        await c.execute(
            """
            INSERT INTO sessions
                (token_sha256, account_id, installation_id, scopes, binding_id, binding_generation, expires_at)
            VALUES ($1,$2,$3,'{session:read,session:write}',$4,1, now() + interval '1 hour')
            """,
            sha256_hex_text(raw),
            ids["account"],
            caller_installation,
            caller_binding,
        )
        headers = {"Authorization": f"Bearer {raw}"}
        # 1) delete enqueues the real gateway revoke: honest pending state
        first = await client.delete(f"{DEVICES}/{ids['binding']}", headers=headers)
        assert first.status == 200
        first_body = await first.json()
        assert first_body["status"] == "pending"
        assert first_body["access_application_state"] == "pending"
        operation_id = first_body["operation_id"]
        assert operation_id != f"local-removal:{ids['binding']}:g2"
        # 2) the real worker + fake gateway readback finalizes status='done'
        assert await _run_once(database, settings, migrated_url) >= 1
        op = await c.fetchrow(
            "SELECT status FROM outbox_operations WHERE id = $1::uuid", operation_id
        )
        assert op["status"] == "done"
        # 3) replay reports the readback-confirmed applied state with the same operation identity
        replay = await client.delete(f"{DEVICES}/{ids['binding']}", headers=headers)
        assert replay.status == 200
        replay_body = await replay.json()
        assert replay_body["status"] == "ok"
        assert replay_body["access_application_state"] == "applied"
        assert replay_body["operation_id"] == operation_id
    finally:
        await database.close()
        await client.close()
        await c.close()
        await gateway.stop()


async def test_second_delete_after_rebind_has_new_generation_identity(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        token, account_id, _iid, _caller = await _bound_session(client, migrated_url, telegram_id=991461)
        victim = await _add_device(c, account_id)
        headers = {"Authorization": f"Bearer {token}"}
        first = await client.delete(f"{DEVICES}/{victim}", headers=headers)
        first_body = await first.json()
        assert first_body["access_application_state"] == "not_requested"
        first_id = first_body["operation_id"]
        # a failed historical revoke row for the old epoch must not pollute later summaries
        await c.execute(
            """
            INSERT INTO outbox_operations
                (operation_type, payload, idempotency_key, binding_id, target_revision, status, last_error)
            VALUES ($1, '{}'::jsonb, $2, $3, 2, 'failed', 'old_gateway_error')
            """,
            REVOKE_OPERATION, f"old-revoke:{uuid.uuid4().hex}", victim,
        )
        # new Telegram flow reactivates the same binding on the next generation
        await c.execute(
            "UPDATE account_bindings SET status='active', generation=generation+1, revoked_at=NULL WHERE id=$1",
            victim,
        )
        second = await client.delete(f"{DEVICES}/{victim}", headers=headers)
        second_body = await second.json()
        assert second_body["access_application_state"] == "not_requested"
        second_id = second_body["operation_id"]
        assert second_id != first_id
        assert first_id.endswith(":g2") and second_id.endswith(":g4")
        replay = await client.delete(f"{DEVICES}/{victim}", headers=headers)
        assert (await replay.json())["operation_id"] == second_id
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_revoke_summary_matches_operations_per_grant(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        token, account_id, _iid, _caller = await _bound_session(client, migrated_url, telegram_id=991470)
        target = await _add_device(c, account_id)
        await c.execute(
            "UPDATE account_bindings SET status='revoked', generation=2, revoked_at=now() WHERE id=$1",
            target,
        )
        gw1 = await c.fetchval(
            "INSERT INTO gateways (gateway_key, environment, registry_state) VALUES ($1,'test','registered') RETURNING id",
            f"gw-a-{uuid.uuid4().hex}",
        )
        gw2 = await c.fetchval(
            "INSERT INTO gateways (gateway_key, environment, registry_state) VALUES ($1,'test','registered') RETURNING id",
            f"gw-b-{uuid.uuid4().hex}",
        )
        g1 = await c.fetchrow(
            """
            INSERT INTO grants (binding_id, gateway_id, desired_generation, applied_generation, not_after, state)
            VALUES ($1,$2,2,2, now() + interval '1 hour','revoked') RETURNING id, opaque_id
            """,
            target, gw1,
        )
        g2 = await c.fetchval(
            """
            INSERT INTO grants (binding_id, gateway_id, desired_generation, applied_generation, not_after, state)
            VALUES ($1,$2,2,2, now() + interval '1 hour','revoked') RETURNING id
            """,
            target, gw2,
        )
        op1 = await c.fetchval(
            """
            INSERT INTO outbox_operations
                (operation_type, payload, idempotency_key, target_revision, gateway_id, binding_id, status)
            VALUES ($1, $2::jsonb, $3, 2, $4, $5, 'done') RETURNING id
            """,
            REVOKE_OPERATION,
            {"grant_id": str(g1["opaque_id"]), "generation": "2", "action": "revoke"},
            f"revoke-g1-{uuid.uuid4().hex}",
            gw1,
            target,
        )
        headers = {"Authorization": f"Bearer {token}"}
        # grant1 is readback-confirmed, grant2 has no operation of its own: the summary must not
        # attribute grant1's done operation to grant2 (no cross product), so the state is pending.
        response = await client.delete(f"{DEVICES}/{target}", headers=headers)
        body = await response.json()
        assert body["access_application_state"] == "pending", body
        assert body["operation_id"] == str(op1)
        # a failure of grant1's own operation must not be attributed to grant2 either
        await c.execute(
            "UPDATE outbox_operations SET status='failed', last_error='temporary_gateway_error' WHERE id=$1",
            op1,
        )
        body = await (await client.delete(f"{DEVICES}/{target}", headers=headers)).json()
        assert body["access_application_state"] == "retryable_failure"
        assert body["operation_id"] == str(op1)
        await c.execute("UPDATE outbox_operations SET status='dead' WHERE id=$1", op1)
        body = await (await client.delete(f"{DEVICES}/{target}", headers=headers)).json()
        assert body["access_application_state"] == "rejected"
        assert g2 is not None
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_stale_devices_read_observes_new_snapshot_only(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    lock_conn = await _connect(migrated_url)
    try:
        token, account_id, _iid, _caller = await _bound_session(client, migrated_url, telegram_id=991471)
        victim = await _add_device(c, account_id)
        headers = {"Authorization": f"Bearer {token}"}
        r0 = (await (await client.get(DEVICES, headers=headers)).json())["revision"]
        await lock_conn.execute("BEGIN")
        await lock_conn.execute(
            "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", f"bind-account:{account_id}"
        )
        task = asyncio.create_task(client.get(DEVICES, headers=headers))
        await asyncio.sleep(0.4)
        # a committed device removal happens while the stale GET is still queued for the lock
        await c.execute(
            "UPDATE account_bindings SET status='revoked', generation=generation+1, revoked_at=now() WHERE id=$1",
            victim,
        )
        await lock_conn.execute("COMMIT")
        stale = await asyncio.wait_for(task, timeout=10)
        assert stale.status == 200
        stale_body = await stale.json()
        fresh_body = await (await client.get(DEVICES, headers=headers)).json()
        # the stale GET re-snapshots after the lock and cannot write an older fingerprint with a
        # newer revision: both reads agree on exactly one increment
        assert stale_body["revision"] == fresh_body["revision"] == str(int(r0) + 1)
        removed = next(d for d in stale_body["devices"] if d["device_id"] == str(victim))
        assert removed["status"] == "revoked"
    finally:
        await database.close()
        await client.close()
        await c.close()
        await lock_conn.close()
