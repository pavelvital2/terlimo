"""Durable checkout receipts: one receipt per order, replay/concurrency, owner/state/policy
fail-closed. Isolated migrated PostgreSQL only; fake provider; no invoice/credit side effects."""
from __future__ import annotations

import asyncio
import uuid
from datetime import timedelta

import pytest

from test_auth_flow import MOBILE
from test_step036_onboarding_hour_storage import _connect
from test_s5_checkout_owner import StubProvider, _bind_session, _hdr, _quote, _stack

from terlimo_backend.auth_api import ApiError
from terlimo_backend.s5_checkout_receipts import CheckoutPolicy, issue_checkout_receipt


POLICY = CheckoutPolicy(
    version="synthetic-1",
    allowed_origins=("https://checkout.invalid",),
    allowed_redirects=("https://pay.invalid/return",),
    ttl=timedelta(minutes=3),
)


async def _pending_order(client, token):
    qid = await _quote(client, token)
    r = await client.post(f"{MOBILE}/payments", headers=_hdr(token, "rcpt-00000000000001"), json={"quote_id": qid})
    assert r.status == 200, await r.text()
    return uuid.UUID((await r.json())["payment_id"])


async def _order_owner(migrated_url, order_id):
    c = await _connect(migrated_url)
    try:
        row = await c.fetchrow(
            "SELECT installation_id, account_id, checkout_owner_account_id, checkout_owner_binding_id "
            "FROM payment_orders WHERE id=$1",
            order_id,
        )
    finally:
        await c.close()
    return row


async def _issue(database, order_id, acc, iid, key="receipt-key-0001", policy=POLICY):
    async with database.acquire() as connection:
        return await issue_checkout_receipt(
            connection, order_id=order_id, account_id=acc, installation_id=iid,
            idempotency_key=key, policy=policy,
        )


async def _receipt_count(migrated_url):
    c = await _connect(migrated_url)
    try:
        return await c.fetchval("SELECT count(*) FROM checkout_sessions")
    finally:
        await c.close()


async def test_issue_and_same_key_replay_after_reconnect(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, acc, _b, token = await _bind_session(client, migrated_url, 890001)
        order_id = await _pending_order(client, token)
        row = await _order_owner(migrated_url, order_id)
        first = await _issue(database, order_id, row["checkout_owner_account_id"], row["installation_id"])
        assert first["order_id"] == order_id
        assert first["account_id"] == acc
        assert first["installation_id"] == row["installation_id"]
        assert first["policy_version"] == POLICY.version
        assert first["idempotency_key"] == "receipt-key-0001"
        # replay through an explicitly new connection: durable, same receipt, no second row
        fresh = await _connect(migrated_url)
        try:
            again = await issue_checkout_receipt(
                fresh, order_id=order_id, account_id=row["checkout_owner_account_id"],
                installation_id=row["installation_id"], idempotency_key="receipt-key-0001",
                policy=POLICY,
            )
        finally:
            await fresh.close()
        assert again["id"] == first["id"]
        assert again["expires_at"] == first["expires_at"]
        assert again["policy_version"] == first["policy_version"]
        assert await _receipt_count(migrated_url) == 1
    finally:
        await database.close()
        await client.close()


async def test_competing_requests_create_one_receipt(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bind_session(client, migrated_url, 890002)
        order_id = await _pending_order(client, token)
        row = await _order_owner(migrated_url, order_id)

        async def call():
            async with database.acquire() as connection:
                return await issue_checkout_receipt(
                    connection, order_id=order_id, account_id=row["checkout_owner_account_id"],
                    installation_id=row["installation_id"], idempotency_key="receipt-key-0002",
                    policy=POLICY,
                )

        results = await asyncio.gather(call(), call())
        assert results[0]["id"] == results[1]["id"]
        assert await _receipt_count(migrated_url) == 1
    finally:
        await database.close()
        await client.close()


async def test_different_key_conflicts(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bind_session(client, migrated_url, 890003)
        order_id = await _pending_order(client, token)
        row = await _order_owner(migrated_url, order_id)
        await _issue(database, order_id, row["checkout_owner_account_id"], row["installation_id"])
        with pytest.raises(ApiError) as excinfo:
            await _issue(database, order_id, row["checkout_owner_account_id"], row["installation_id"], key="receipt-key-other")
        assert excinfo.value.code == "IDEMPOTENCY_CONFLICT" and excinfo.value.http == 409
        assert await _receipt_count(migrated_url) == 1
    finally:
        await database.close()
        await client.close()


async def test_foreign_and_null_owner_are_neutral_404(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _ia, _aa, _ba, token_a = await _bind_session(client, migrated_url, 890004)
        _ib, acc_b, _bb, _tb = await _bind_session(client, migrated_url, 890005)
        order_id = await _pending_order(client, token_a)
        row = await _order_owner(migrated_url, order_id)
        with pytest.raises(ApiError) as foreign:
            await _issue(database, order_id, acc_b, row["installation_id"])
        assert foreign.value.code == "PAYMENT_NOT_FOUND" and foreign.value.http == 404
        with pytest.raises(ApiError) as foreign_install:
            await _issue(database, order_id, row["checkout_owner_account_id"], _ib)
        assert foreign_install.value.code == "PAYMENT_NOT_FOUND"
        c = await _connect(migrated_url)
        try:
            await c.execute("UPDATE payment_orders SET checkout_owner_account_id=NULL WHERE id=$1", order_id)
        finally:
            await c.close()
        with pytest.raises(ApiError) as null_owner:
            await _issue(database, order_id, row["checkout_owner_account_id"], row["installation_id"])
        assert null_owner.value.code == "PAYMENT_NOT_FOUND"
        # NULL on both sides (no order owner and no context account) must be a neutral 404,
        # never a NOT NULL database exception.
        with pytest.raises(ApiError) as null_both:
            await _issue(database, order_id, None, row["installation_id"])
        assert null_both.value.code == "PAYMENT_NOT_FOUND" and null_both.value.http == 404
        with pytest.raises(ApiError) as null_installation:
            await _issue(database, order_id, row["checkout_owner_account_id"], None)
        assert null_installation.value.code == "PAYMENT_NOT_FOUND"
        assert await _receipt_count(migrated_url) == 0
    finally:
        await database.close()
        await client.close()


@pytest.mark.parametrize("setter", ["status", "provider_state", "pay_url"])
async def test_invalid_order_state_refused(migrated_url, settings_factory, setter):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bind_session(client, migrated_url, 890006)
        order_id = await _pending_order(client, token)
        row = await _order_owner(migrated_url, order_id)
        c = await _connect(migrated_url)
        try:
            if setter == "status":
                await c.execute("UPDATE payment_orders SET status='succeeded' WHERE id=$1", order_id)
            elif setter == "provider_state":
                await c.execute("UPDATE payment_orders SET provider_create_state='in_flight' WHERE id=$1", order_id)
            else:
                await c.execute("UPDATE payment_orders SET provider_payment_url=NULL WHERE id=$1", order_id)
        finally:
            await c.close()
        with pytest.raises(ApiError) as excinfo:
            await _issue(database, order_id, row["checkout_owner_account_id"], row["installation_id"], key=f"receipt-bad-{setter}")
        assert excinfo.value.code == "PAYMENT_STATE_INVALID" and excinfo.value.http == 409
        assert await _receipt_count(migrated_url) == 0
    finally:
        await database.close()
        await client.close()


async def test_policy_disabled_or_unusable_refused(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bind_session(client, migrated_url, 890007)
        order_id = await _pending_order(client, token)
        row = await _order_owner(migrated_url, order_id)
        bad_policies = [
            None,
            CheckoutPolicy(version="", allowed_origins=("https://x.invalid",), allowed_redirects=("https://y.invalid",), ttl=timedelta(minutes=1)),
            CheckoutPolicy(version="v", allowed_origins=(), allowed_redirects=("https://y.invalid",), ttl=timedelta(minutes=1)),
            CheckoutPolicy(version="v", allowed_origins=("https://x.invalid",), allowed_redirects=("https://y.invalid",), ttl=timedelta(0)),
        ]
        for index, policy in enumerate(bad_policies):
            with pytest.raises(ApiError) as excinfo:
                await _issue(database, order_id, row["checkout_owner_account_id"], row["installation_id"], key=f"receipt-off-{index}", policy=policy)
            assert excinfo.value.code == "CHECKOUT_POLICY_DENIED" and excinfo.value.http == 403
        assert await _receipt_count(migrated_url) == 0
    finally:
        await database.close()
        await client.close()


@pytest.mark.parametrize(
    "setter",
    ["status_succeeded", "status_canceled", "provider_state", "pay_url"],
)
async def test_state_change_refuses_same_key_replay(migrated_url, settings_factory, setter):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bind_session(client, migrated_url, 890010)
        order_id = await _pending_order(client, token)
        row = await _order_owner(migrated_url, order_id)
        first = await _issue(database, order_id, row["checkout_owner_account_id"], row["installation_id"], key="receipt-state-key")
        c = await _connect(migrated_url)
        try:
            if setter == "status_succeeded":
                await c.execute("UPDATE payment_orders SET status='succeeded' WHERE id=$1", order_id)
            elif setter == "status_canceled":
                await c.execute("UPDATE payment_orders SET status='canceled' WHERE id=$1", order_id)
            elif setter == "provider_state":
                await c.execute("UPDATE payment_orders SET provider_create_state='in_flight' WHERE id=$1", order_id)
            else:
                await c.execute("UPDATE payment_orders SET provider_payment_url=NULL WHERE id=$1", order_id)
        finally:
            await c.close()
        before = provider.calls
        with pytest.raises(ApiError) as excinfo:
            await _issue(database, order_id, row["checkout_owner_account_id"], row["installation_id"], key="receipt-state-key")
        assert excinfo.value.code == "PAYMENT_STATE_INVALID" and excinfo.value.http == 409
        assert provider.calls == before
        c = await _connect(migrated_url)
        try:
            receipt = await c.fetchrow("SELECT id, policy_version, expires_at FROM checkout_sessions WHERE order_id=$1", order_id)
            entitlements = await c.fetchval("SELECT count(*) FROM entitlements")
        finally:
            await c.close()
        assert receipt["id"] == first["id"]
        assert receipt["policy_version"] == first["policy_version"]
        assert receipt["expires_at"] == first["expires_at"]
        assert entitlements == 0
        assert await _receipt_count(migrated_url) == 1
    finally:
        await database.close()
        await client.close()


async def test_expired_and_version_changed_refused(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bind_session(client, migrated_url, 890008)
        order_id = await _pending_order(client, token)
        row = await _order_owner(migrated_url, order_id)
        await _issue(database, order_id, row["checkout_owner_account_id"], row["installation_id"])
        c = await _connect(migrated_url)
        try:
            await c.execute("UPDATE checkout_sessions SET expires_at = now() - interval '1 minute'")
        finally:
            await c.close()
        with pytest.raises(ApiError) as expired:
            await _issue(database, order_id, row["checkout_owner_account_id"], row["installation_id"])
        assert expired.value.code == "CHECKOUT_POLICY_DENIED"
        assert await _receipt_count(migrated_url) == 1

        # Version guard is proven on a still-valid receipt of a separate order.
        qid = await _quote(client, token)
        r = await client.post(f"{MOBILE}/payments", headers=_hdr(token, "rcpt-00000000000002"), json={"quote_id": qid})
        assert r.status == 200, await r.text()
        order2 = uuid.UUID((await r.json())["payment_id"])
        row2 = await _order_owner(migrated_url, order2)
        valid = await _issue(database, order2, row2["checkout_owner_account_id"], row2["installation_id"], key="receipt-key-0003")
        changed = CheckoutPolicy(
            version="synthetic-2", allowed_origins=POLICY.allowed_origins,
            allowed_redirects=POLICY.allowed_redirects, ttl=POLICY.ttl,
        )
        with pytest.raises(ApiError) as versioned:
            await _issue(database, order2, row2["checkout_owner_account_id"], row2["installation_id"], key="receipt-key-0003", policy=changed)
        assert versioned.value.code == "CHECKOUT_POLICY_DENIED"
        c = await _connect(migrated_url)
        try:
            kept = await c.fetchrow("SELECT id, policy_version FROM checkout_sessions WHERE order_id=$1", order2)
        finally:
            await c.close()
        assert kept["id"] == valid["id"] and kept["policy_version"] == POLICY.version
        assert await _receipt_count(migrated_url) == 2
    finally:
        await database.close()
        await client.close()


async def test_no_provider_or_credit_side_effects(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bind_session(client, migrated_url, 890009)
        order_id = await _pending_order(client, token)
        row = await _order_owner(migrated_url, order_id)
        calls_after_order = provider.calls
        await _issue(database, order_id, row["checkout_owner_account_id"], row["installation_id"])
        assert provider.calls == calls_after_order == 1
        c = await _connect(migrated_url)
        try:
            status = await c.fetchval("SELECT status FROM payment_orders WHERE id=$1", order_id)
            credit_account = await c.fetchval("SELECT account_id FROM payment_orders WHERE id=$1", order_id)
            entitlements = await c.fetchval("SELECT count(*) FROM entitlements")
        finally:
            await c.close()
        assert status == "pending" and credit_account is None and entitlements == 0
    finally:
        await database.close()
        await client.close()
