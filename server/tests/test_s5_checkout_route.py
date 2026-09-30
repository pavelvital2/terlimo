"""POST /payments/{PathId}/checkout-session: bounded route over the durable receipt helper with
injected synthetic policy. Isolated migrated PostgreSQL, fake provider, no live effects."""
from __future__ import annotations

import asyncio
import uuid

import pytest
from aiohttp.test_utils import TestClient, TestServer

from test_auth_flow import MOBILE
from test_step036_onboarding_hour_storage import _connect
from test_s5_checkout_owner import StubProvider, _bind_session, _hdr, _quote, _settings
from test_s5_checkout_receipts import POLICY
from terlimo_backend.s5_checkout_receipts import CheckoutPolicy
from datetime import timedelta

from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.payments import PAYMENT_PROVIDER_KEY
from terlimo_backend.s5_payments import CHECKOUT_POLICY_KEY


async def _stack(settings_factory, migrated_url, provider, policy=POLICY):
    settings = _settings(settings_factory, migrated_url)
    database = Database(settings)
    app = create_app(settings, database)
    app[PAYMENT_PROVIDER_KEY] = provider
    if policy is not None:
        app[CHECKOUT_POLICY_KEY] = policy
    client = TestClient(TestServer(app))
    await client.start_server()
    return client, settings, database


async def _bound_session(client, migrated_url, tg, scopes=("session:read", "session:write", "payment:write")):
    # Reuse the owner-stage helper but with the payment:write scope for linked sessions.
    import test_s5_checkout_owner as owner
    key_material = None
    from test_auth_flow import KeyMaterial, PoPClient, _challenge, _enroll, _session
    key = KeyMaterial(); pop = PoPClient(key)
    assert (await _enroll(client, pop, await _challenge(client, key, "enrollment"))).status == 200
    c = await _connect(migrated_url)
    try:
        iid = await c.fetchval("SELECT id FROM installations WHERE public_key_fingerprint=$1", pop.key.fingerprint)
        acc = await c.fetchval("INSERT INTO accounts (status, telegram_id) VALUES ('verified',$1) RETURNING id", tg)
        binding = await c.fetchval(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active') RETURNING id",
            acc, iid,
        )
    finally:
        await c.close()
    s = await _session(client, pop, await _challenge(client, pop.key, "session"), list(scopes), f"idem-{uuid.uuid4().hex}")
    assert s.status == 200, await s.text()
    return iid, acc, binding, (await s.json())["session"]["session_id"]


async def _pending_order(client, token, key="route-order-key-0001"):
    qid = await _quote(client, token)
    r = await client.post(f"{MOBILE}/payments", headers=_hdr(token, key), json={"quote_id": qid})
    assert r.status == 200, await r.text()
    return uuid.UUID((await r.json())["payment_id"])


async def _post_session(client, token, order_id, key="route-session-key-01"):
    return await client.post(
        f"{MOBILE}/payments/{order_id}/checkout-session",
        headers=_hdr(token, key),
    )


async def test_success_with_synthetic_policy(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bound_session(client, migrated_url, 891001)
        order_id = await _pending_order(client, token)
        r = await _post_session(client, token, order_id)
        assert r.status == 200, await r.text()
        body = await r.json()
        assert body["status"] == "ok" and body["schema_version"] == "1.0"
        assert len(body["request_id"]) == 32
        assert body["checkout_session_id"]
        assert body["policy_version"] == POLICY.version
        assert body["allowed_origins"] == list(POLICY.allowed_origins)
        assert body["allowed_redirects"] == list(POLICY.allowed_redirects)
        # replay returns the durable same session, no second receipt, no provider/credit effect
        again = await _post_session(client, token, order_id)
        assert again.status == 200
        assert (await again.json())["checkout_session_id"] == body["checkout_session_id"]
        c = await _connect(migrated_url)
        try:
            assert await c.fetchval("SELECT count(*) FROM checkout_sessions") == 1
            assert await c.fetchval("SELECT count(*) FROM entitlements") == 0
            assert await c.fetchval("SELECT count(*) FROM payment_orders WHERE status <> 'pending'") == 0
        finally:
            await c.close()
        assert provider.calls == 1
    finally:
        await database.close()
        await client.close()


async def test_concurrent_same_key_single_session(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bound_session(client, migrated_url, 891002)
        order_id = await _pending_order(client, token)
        responses = await asyncio.gather(
            _post_session(client, token, order_id, "route-conc-key-01"),
            _post_session(client, token, order_id, "route-conc-key-01"),
        )
        bodies = [await r.json() for r in responses]
        assert [r.status for r in responses] == [200, 200], bodies
        assert bodies[0]["checkout_session_id"] == bodies[1]["checkout_session_id"]
        c = await _connect(migrated_url)
        try:
            assert await c.fetchval("SELECT count(*) FROM checkout_sessions") == 1
        finally:
            await c.close()
    finally:
        await database.close()
        await client.close()


async def test_policy_disabled_denied(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider, policy=None)
    try:
        _iid, _acc, _b, token = await _bound_session(client, migrated_url, 891003)
        order_id = await _pending_order(client, token)
        r = await _post_session(client, token, order_id)
        assert r.status == 403, await r.text()
        assert (await r.json())["code"] == "CHECKOUT_POLICY_DENIED"
        c = await _connect(migrated_url)
        try:
            assert await c.fetchval("SELECT count(*) FROM checkout_sessions") == 0
        finally:
            await c.close()
    finally:
        await database.close()
        await client.close()


async def test_unauthenticated_and_missing_scope(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bound_session(client, migrated_url, 891004)
        order_id = await _pending_order(client, token)
        unauth = await client.post(f"{MOBILE}/payments/{order_id}/checkout-session")
        assert unauth.status == 401
        missing_key = await client.post(
            f"{MOBILE}/payments/{order_id}/checkout-session",
            headers={"Authorization": "Bearer " + token},
        )
        assert missing_key.status == 400, await missing_key.text()
        assert (await missing_key.json())["details"]["reason"] == "idempotency_key_required"
        _i2, _a2, _b2, token_readonly = await _bound_session(
            client, migrated_url, 891005, scopes=("session:read", "session:write"),
        )
        order2 = await _pending_order(client, token_readonly, key="route-order-key-0002")
        denied = await _post_session(client, token_readonly, order2, key="route-scope-key-01")
        assert denied.status == 403, await denied.text()
        assert (await denied.json())["code"] == "ACCESS_DENIED"
    finally:
        await database.close()
        await client.close()


async def test_foreign_null_owner_and_opaque_paths(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _ia, _aa, _ba, token_a = await _bound_session(client, migrated_url, 891006)
        _ib, _ab, _bb, token_b = await _bound_session(client, migrated_url, 891007)
        order_id = await _pending_order(client, token_a)
        foreign = await _post_session(client, token_b, order_id, key="route-foreign-key-01")
        assert foreign.status == 404, await foreign.text()
        assert (await foreign.json())["code"] == "PAYMENT_NOT_FOUND"
        c = await _connect(migrated_url)
        try:
            await c.execute("UPDATE payment_orders SET checkout_owner_account_id=NULL WHERE id=$1", order_id)
        finally:
            await c.close()
        null_owner = await _post_session(client, token_a, order_id, key="route-null-key-0001")
        assert null_owner.status == 404
        # bounded opaque: syntactically valid but non-UUID / oversized / traversal-ish ids
        for raw in ("not-a-uuid", "a" * 129, "..", "has%2Fescape"):
            r = await client.post(
                f"{MOBILE}/payments/{raw}/checkout-session",
                headers=_hdr(token_a, f"route-path-key-{len(raw)}"),
            )
            assert r.status in (404, 400), (raw, r.status, await r.text())
        c = await _connect(migrated_url)
        try:
            assert await c.fetchval("SELECT count(*) FROM checkout_sessions") == 0
        finally:
            await c.close()
    finally:
        await database.close()
        await client.close()


async def test_canonical_uuid_only_alias_404(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bound_session(client, migrated_url, 891009)
        order_id = await _pending_order(client, token)
        alias = order_id.hex  # 32-hex UUID alias: bounded transport id, never storage-resolvable
        alias_response = await _post_session(client, token, alias, key="route-alias-key-0001")
        assert alias_response.status == 404, await alias_response.text()
        assert (await alias_response.json())["code"] == "PAYMENT_NOT_FOUND"
        canonical = await _post_session(client, token, order_id, key="route-canon-key-0001")
        assert canonical.status == 200, await canonical.text()
        c = await _connect(migrated_url)
        try:
            assert await c.fetchval("SELECT count(*) FROM checkout_sessions") == 1
        finally:
            await c.close()
    finally:
        await database.close()
        await client.close()


async def test_same_account_other_installation_404(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        iid_a, acc_a, _ba, token_a = await _bound_session(client, migrated_url, 891010)
        order_id = await _pending_order(client, token_a)
        # a second installation linked to the SAME verified account
        from test_auth_flow import KeyMaterial, PoPClient, _challenge, _enroll, _session
        key = KeyMaterial(); pop = PoPClient(key)
        assert (await _enroll(client, pop, await _challenge(client, key, "enrollment"))).status == 200
        c = await _connect(migrated_url)
        try:
            iid_b = await c.fetchval("SELECT id FROM installations WHERE public_key_fingerprint=$1", pop.key.fingerprint)
            await c.execute(
                "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')",
                acc_a, iid_b,
            )
        finally:
            await c.close()
        s = await _session(
            client, pop, await _challenge(client, pop.key, "session"),
            ["session:read", "session:write", "payment:write"], f"idem-{uuid.uuid4().hex}",
        )
        assert s.status == 200, await s.text()
        token_b = (await s.json())["session"]["session_id"]
        response = await _post_session(client, token_b, order_id, key="route-other-install-01")
        assert response.status == 404, await response.text()
        assert (await response.json())["code"] == "PAYMENT_NOT_FOUND"
        c = await _connect(migrated_url)
        try:
            assert await c.fetchval("SELECT count(*) FROM checkout_sessions") == 0
        finally:
            await c.close()
    finally:
        await database.close()
        await client.close()


@pytest.mark.parametrize("bad_policy", [
    "not-a-policy",
    CheckoutPolicy(version=123, allowed_origins=("https://x.invalid",), allowed_redirects=("https://y.invalid",), ttl=timedelta(minutes=1)),
    CheckoutPolicy(version="v", allowed_origins=("https://x.invalid", 5), allowed_redirects=("https://y.invalid",), ttl=timedelta(minutes=1)),
    CheckoutPolicy(version="v", allowed_origins=("https://x.invalid",), allowed_redirects=(b"bytes",), ttl=timedelta(minutes=1)),
    CheckoutPolicy(version="v", allowed_origins=("https://x.invalid",), allowed_redirects=("https://y.invalid",), ttl="soon"),
])
async def test_malformed_policy_denied_without_500(migrated_url, settings_factory, bad_policy):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider, policy=bad_policy)
    try:
        _iid, _acc, _b, token = await _bound_session(client, migrated_url, 891011)
        order_id = await _pending_order(client, token)
        r = await _post_session(client, token, order_id, key="route-badpolicy-key1")
        assert r.status == 403, await r.text()
        assert (await r.json())["code"] == "CHECKOUT_POLICY_DENIED"
        c = await _connect(migrated_url)
        try:
            assert await c.fetchval("SELECT count(*) FROM checkout_sessions") == 0
        finally:
            await c.close()
    finally:
        await database.close()
        await client.close()


async def test_terminal_replay_refused(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bound_session(client, migrated_url, 891008)
        order_id = await _pending_order(client, token)
        ok = await _post_session(client, token, order_id, key="route-term-key-0001")
        assert ok.status == 200, await ok.text()
        c = await _connect(migrated_url)
        try:
            await c.execute("UPDATE payment_orders SET status='succeeded' WHERE id=$1", order_id)
        finally:
            await c.close()
        replay = await _post_session(client, token, order_id, key="route-term-key-0001")
        assert replay.status == 409, await replay.text()
        assert (await replay.json())["code"] == "PAYMENT_STATE_INVALID"
        assert provider.calls == 1
    finally:
        await database.close()
        await client.close()
