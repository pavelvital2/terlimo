"""S5 quote->order wiring: minimal create path with C4 invariants (focused, provider stub/OFF)."""
from __future__ import annotations

import uuid

import pytest
from aiohttp.test_utils import TestClient, TestServer

from test_auth_flow import MOBILE, KeyMaterial, PoPClient, _challenge, _enroll, _session
from test_step036_onboarding_hour_storage import _connect

from terlimo_backend.api import create_app
from terlimo_backend.auth_api import ApiError
from terlimo_backend.db import Database
from terlimo_backend.payments import PAYMENT_PROVIDER_KEY, ProviderPayment


class StubProvider:
    def __init__(self):
        self.calls = 0

    def capabilities(self):
        return {"checkout": True, "qr": False}

    async def create_payment(self, *, amount, currency, months, order_ref, description):
        self.calls += 1
        return ProviderPayment(provider_payment_id=f"tx-{order_ref}", pay_url=f"https://pay/{order_ref}", qr=None, variant="sbp")

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
        await c.execute("INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')", acc, iid)
    finally:
        await c.close()
    s = await _session(client, pop, await _challenge(client, pop.key, "session"), ["session:read", "session:write"], f"idem-{uuid.uuid4().hex}")
    assert s.status == 200, await s.text()
    return iid, (await s.json())["session"]["session_id"]


async def _quote(client, token, plan_id="terlimo-30d", duration="days:30"):
    r = await client.post(f"{MOBILE}/quotes", headers={"Authorization": f"Bearer {token}", "Idempotency-Key": f"q-{uuid.uuid4().hex}"},
                          json={"plan_id": plan_id, "duration_code": duration, "method": "sbp"})
    assert r.status == 200, await r.text()
    return (await r.json())["quote_id"]


def _hdr(token, key):
    return {"Authorization": f"Bearer {token}", "Idempotency-Key": key}


async def test_provider_off_fails_closed_without_ledger_write(migrated_url, settings_factory):
    client, settings, database = await _stack(settings_factory, migrated_url, provider=None)
    try:
        iid, token = await _bind_session(client, migrated_url, 888001)
        qid = await _quote(client, token)
        r = await client.post(f"{MOBILE}/payments", headers=_hdr(token, "pay-off-000000000001"), json={"quote_id": qid})
        assert r.status == 503, await r.text()
        assert (await r.json())["code"] == "PAYMENT_PROVIDER_UNAVAILABLE"
        c = await _connect(migrated_url)
        try:
            assert await c.fetchval("SELECT count(*) FROM payment_orders") == 0
        finally:
            await c.close()
    finally:
        await database.close()
        await client.close()


async def test_stub_quote_to_order_and_c4_invariants(migrated_url, settings_factory):
    provider = StubProvider()
    client, settings, database = await _stack(settings_factory, migrated_url, provider=provider)
    try:
        iid, token = await _bind_session(client, migrated_url, 888002)
        qid = await _quote(client, token)
        first = await client.post(f"{MOBILE}/payments", headers=_hdr(token, "pay-000000000000001"), json={"quote_id": qid})
        assert first.status == 200, await first.text()
        body1 = (await first.json())
        assert body1["payment_status"] == "pending" and provider.calls == 1
        payment_id = body1["payment_id"]
        c = await _connect(migrated_url)
        try:
            row = await c.fetchrow("SELECT source_quote_id FROM payment_orders WHERE id=$1", uuid.UUID(payment_id))
            assert str(row["source_quote_id"]) == qid
            await c.execute("UPDATE s5_payment_quotes SET expires_at = now() - interval '1 minute' WHERE id=$1", uuid.UUID(qid))
        finally:
            await c.close()
        # same-key retry after quote expiry returns the durable same order, no second provider create
        retry = await client.post(f"{MOBILE}/payments", headers=_hdr(token, "pay-000000000000001"), json={"quote_id": qid})
        assert retry.status == 200, await retry.text()
        assert (await retry.json())["payment_id"] == payment_id and provider.calls == 1
        # a genuinely new key against the now-expired quote fails with the documented code
        expired = await client.post(f"{MOBILE}/payments", headers=_hdr(token, "pay-000000000000002"), json={"quote_id": qid})
        assert expired.status == 409 and (await expired.json())["code"] == "QUOTE_EXPIRED"
        assert provider.calls == 1
        # different key reusing a still-valid quote conflicts exactly, no provider side effect
        qid2 = await _quote(client, token)
        ok = await client.post(f"{MOBILE}/payments", headers=_hdr(token, "pay-000000000000003"), json={"quote_id": qid2})
        assert ok.status == 200 and provider.calls == 2
        conflict = await client.post(f"{MOBILE}/payments", headers=_hdr(token, "pay-000000000000004"), json={"quote_id": qid2})
        assert conflict.status == 409
        err = await conflict.json()
        assert err["code"] == "ORDER_CONFLICT" and err["details"]["reason"] == "quote_already_used"
        assert provider.calls == 2
    finally:
        await database.close()
        await client.close()


async def test_foreign_quote_is_not_found(migrated_url, settings_factory):
    provider = StubProvider()
    client, settings, database = await _stack(settings_factory, migrated_url, provider=provider)
    try:
        _iid_a, token_a = await _bind_session(client, migrated_url, 888003)
        _iid_b, token_b = await _bind_session(client, migrated_url, 888004)
        qid = await _quote(client, token_a)
        r = await client.post(f"{MOBILE}/payments", headers=_hdr(token_b, "pay-x00000000000001"), json={"quote_id": qid})
        assert r.status == 404, await r.text()
        assert provider.calls == 0
    finally:
        await database.close()
        await client.close()
