"""S5 pending-order checkout ownership: immutable owner captured at first insert, same-key
replay compares owner+installation+quote proof, credit rebinding never rewrites checkout owner.
Provider is a fake; isolated migrated test DB only."""
from __future__ import annotations

import asyncio
import uuid

import pytest
from aiohttp.test_utils import TestClient, TestServer

from test_auth_flow import MOBILE, KeyMaterial, PoPClient, _challenge, _enroll, _session
from test_step036_onboarding_hour_storage import _connect

from terlimo_backend.api import create_app
from terlimo_backend.auth_api import ApiError
from terlimo_backend.db import Database
from terlimo_backend.payments import PAYMENT_PROVIDER_KEY, ProviderPayment, create_order


class StubProvider:
    def __init__(self):
        self.calls = 0

    def capabilities(self):
        return {"checkout": True, "qr": False}

    async def create_payment(self, *, amount, currency, months, order_ref, description, method=None):
        self.calls += 1
        return ProviderPayment(
            provider_payment_id=f"tx-{order_ref}", pay_url=f"https://pay/{order_ref}",
            qr=None, variant="sbp",
        )

    async def get_status(self, provider_payment_id):
        return None


def _settings(settings_factory, url):
    return settings_factory(
        url, payment_currency="RUB", payment_tariff_key="terlimo-200-30d-v1",
        payment_price_rub_1=200, payment_price_rub_3=480, payment_price_rub_6=840,
        platega_methods="sbp", platega_merchant_id="merchant-1", platega_secret="secret-1",
    )


async def _stack(settings_factory, migrated_url, provider):
    settings = _settings(settings_factory, migrated_url)
    database = Database(settings)
    app = create_app(settings, database)
    app[PAYMENT_PROVIDER_KEY] = provider
    client = TestClient(TestServer(app))
    await client.start_server()
    return client, settings, database


async def _bind_session(client, migrated_url, tg):
    key = KeyMaterial(); pop = PoPClient(key)
    assert (await _enroll(client, pop, await _challenge(client, key, "enrollment"))).status == 200
    c = await _connect(migrated_url)
    try:
        iid = await c.fetchval("SELECT id FROM installations WHERE public_key_fingerprint=$1", pop.key.fingerprint)
        acc = await c.fetchval("INSERT INTO accounts (status, telegram_id) VALUES ('verified',$1) RETURNING id", tg)
        binding_id = await c.fetchval(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active') RETURNING id",
            acc, iid,
        )
    finally:
        await c.close()
    s = await _session(
        client, pop, await _challenge(client, pop.key, "session"),
        ["session:read", "session:write"], f"idem-{uuid.uuid4().hex}",
    )
    assert s.status == 200, await s.text()
    return iid, acc, binding_id, (await s.json())["session"]["session_id"]


async def _quote(client, token, plan_id="terlimo-30d", duration="days:30"):
    r = await client.post(
        f"{MOBILE}/quotes",
        headers={"Authorization": f"Bearer {token}", "Idempotency-Key": f"q-{uuid.uuid4().hex}"},
        json={"plan_id": plan_id, "duration_code": duration, "method": "sbp"},
    )
    assert r.status == 200, await r.text()
    return (await r.json())["quote_id"]


def _hdr(token, key):
    return {"Authorization": f"Bearer {token}", "Idempotency-Key": key}


async def _row(migrated_url, payment_id):
    c = await _connect(migrated_url)
    try:
        return await c.fetchrow(
            "SELECT account_id, binding_id, checkout_owner_account_id, checkout_owner_binding_id "
            "FROM payment_orders WHERE id=$1",
            uuid.UUID(payment_id),
        )
    finally:
        await c.close()


async def _direct_create(database, settings, provider, iid, qid, acc, binding, key):
    async with database.acquire() as connection:
        return await create_order(
            connection, settings, provider,
            installation_id=iid, months=1, idempotency_key=key, quote_id=qid,
            method="sbp", public_method="sbp",
            checkout_owner_account_id=acc, checkout_owner_binding_id=binding,
        )


async def test_checkout_owner_captured_and_credit_fields_untouched(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, acc, binding_id, token = await _bind_session(client, migrated_url, 889001)
        qid = await _quote(client, token)
        r = await client.post(f"{MOBILE}/payments", headers=_hdr(token, "own-000000000000001"), json={"quote_id": qid})
        assert r.status == 200, await r.text()
        row = await _row(migrated_url, (await r.json())["payment_id"])
        assert row["checkout_owner_account_id"] == acc
        assert row["checkout_owner_binding_id"] == binding_id
        assert row["account_id"] is None and row["binding_id"] is None
    finally:
        await database.close()
        await client.close()


async def test_same_key_same_owner_replays_without_second_create(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, acc, _b, token = await _bind_session(client, migrated_url, 889002)
        qid = await _quote(client, token)
        first = await client.post(f"{MOBILE}/payments", headers=_hdr(token, "own-000000000000002"), json={"quote_id": qid})
        assert first.status == 200
        payment_id = (await first.json())["payment_id"]
        retry = await client.post(f"{MOBILE}/payments", headers=_hdr(token, "own-000000000000002"), json={"quote_id": qid})
        assert retry.status == 200, await retry.text()
        assert (await retry.json())["payment_id"] == payment_id and provider.calls == 1
        assert (await _row(migrated_url, payment_id))["checkout_owner_account_id"] == acc
    finally:
        await database.close()
        await client.close()


async def test_same_key_different_owner_conflicts(migrated_url, settings_factory):
    provider = StubProvider()
    client, settings, database = await _stack(settings_factory, migrated_url, provider)
    try:
        iid, acc_a, binding_a, token = await _bind_session(client, migrated_url, 889003)
        qid = await _quote(client, token)
        c = await _connect(migrated_url)
        try:
            acc_b = await c.fetchval("INSERT INTO accounts (status, telegram_id) VALUES ('verified',$1) RETURNING id", 889004)
        finally:
            await c.close()
        key = "own-direct-00000001"
        await _direct_create(database, settings, provider, iid, qid, acc_a, binding_a, key)
        with pytest.raises(ApiError) as excinfo:
            await _direct_create(database, settings, provider, iid, qid, acc_b, binding_a, key)
        assert excinfo.value.code == "ORDER_CONFLICT"
        assert excinfo.value.details.get("reason") == "checkout_owner_mismatch"
        assert provider.calls == 1
    finally:
        await database.close()
        await client.close()


async def test_foreign_installation_same_key_conflicts(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _ia, _aa, _ba, token_a = await _bind_session(client, migrated_url, 889005)
        _ib, _ab, _bb, token_b = await _bind_session(client, migrated_url, 889006)
        qid_a = await _quote(client, token_a)
        key = "own-000000000000003"
        first = await client.post(f"{MOBILE}/payments", headers=_hdr(token_a, key), json={"quote_id": qid_a})
        assert first.status == 200
        qid_b = await _quote(client, token_b)
        foreign = await client.post(f"{MOBILE}/payments", headers=_hdr(token_b, key), json={"quote_id": qid_b})
        assert foreign.status == 409, await foreign.text()
        assert (await foreign.json())["details"]["reason"] == "installation_mismatch"
        assert provider.calls == 1
    finally:
        await database.close()
        await client.close()


async def test_legacy_null_owner_replay_does_not_capture(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bind_session(client, migrated_url, 889007)
        qid = await _quote(client, token)
        key = "own-000000000000004"
        first = await client.post(f"{MOBILE}/payments", headers=_hdr(token, key), json={"quote_id": qid})
        assert first.status == 200
        payment_id = (await first.json())["payment_id"]
        c = await _connect(migrated_url)
        try:
            await c.execute(
                "UPDATE payment_orders SET checkout_owner_account_id=NULL, checkout_owner_binding_id=NULL WHERE id=$1",
                uuid.UUID(payment_id),
            )
        finally:
            await c.close()
        retry = await client.post(f"{MOBILE}/payments", headers=_hdr(token, key), json={"quote_id": qid})
        assert retry.status == 200, await retry.text()
        assert (await retry.json())["payment_id"] == payment_id and provider.calls == 1
        row = await _row(migrated_url, payment_id)
        assert row["checkout_owner_account_id"] is None and row["checkout_owner_binding_id"] is None
    finally:
        await database.close()
        await client.close()


async def test_changed_quote_same_key_conflicts(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bind_session(client, migrated_url, 889008)
        qid1 = await _quote(client, token)
        key = "own-000000000000005"
        first = await client.post(f"{MOBILE}/payments", headers=_hdr(token, key), json={"quote_id": qid1})
        assert first.status == 200
        qid2 = await _quote(client, token)
        changed = await client.post(f"{MOBILE}/payments", headers=_hdr(token, key), json={"quote_id": qid2})
        assert changed.status == 409, await changed.text()
        assert (await changed.json())["details"]["reason"] == "quote_changed"
        assert provider.calls == 1
    finally:
        await database.close()
        await client.close()


async def test_concurrent_same_key_single_provider_create(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        _iid, _acc, _b, token = await _bind_session(client, migrated_url, 889009)
        qid = await _quote(client, token)
        key = "own-000000000000006"
        responses = await asyncio.gather(
            client.post(f"{MOBILE}/payments", headers=_hdr(token, key), json={"quote_id": qid}),
            client.post(f"{MOBILE}/payments", headers=_hdr(token, key), json={"quote_id": qid}),
        )
        bodies = [await r.json() for r in responses]
        assert [r.status for r in responses] == [200, 200], bodies
        assert bodies[0]["payment_id"] == bodies[1]["payment_id"]
        assert provider.calls == 1
    finally:
        await database.close()
        await client.close()


async def test_webhook_credit_keeps_checkout_owner_after_rebinding(migrated_url, settings_factory):
    provider = StubProvider()
    client, _s, database = await _stack(settings_factory, migrated_url, provider)
    try:
        iid, acc_a, binding_a, token = await _bind_session(client, migrated_url, 889010)
        qid = await _quote(client, token)
        r = await client.post(f"{MOBILE}/payments", headers=_hdr(token, "own-000000000000007"), json={"quote_id": qid})
        assert r.status == 200
        payment_id = (await r.json())["payment_id"]
        c = await _connect(migrated_url)
        try:
            acc_b = await c.fetchval("INSERT INTO accounts (status, telegram_id) VALUES ('verified',$1) RETURNING id", 889011)
            await c.execute("UPDATE account_bindings SET status='revoked' WHERE id=$1", binding_a)
            await c.execute(
                "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')",
                acc_b, iid,
            )
        finally:
            await c.close()
        hook = await client.post(
            "/public/platega/webhook",
            headers={"X-MerchantId": "merchant-1", "X-Secret": "secret-1"},
            json={"id": f"tx-{payment_id}", "amount": 200, "currency": "RUB", "status": "CONFIRMED"},
        )
        assert hook.status == 200, await hook.text()
        assert (await hook.json())["result"] == "succeeded"
        row = await _row(migrated_url, payment_id)
        # credit target follows the active binding at credit time (rebinding)
        assert row["account_id"] == acc_b and row["binding_id"] is not None
        # immutable checkout owner keeps the verified account that created the order
        assert row["checkout_owner_account_id"] == acc_a
        assert row["checkout_owner_binding_id"] == binding_a
    finally:
        await database.close()
        await client.close()
