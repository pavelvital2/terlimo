"""S5 07.1 correction2: plan snapshot only from a durable, proven installation-bound quote."""
from __future__ import annotations

import json
import os
import subprocess
import uuid

import pytest
from aiohttp.test_utils import TestClient, TestServer

from test_auth_flow import MOBILE, KeyMaterial, PoPClient, _challenge, _enroll, _session
from test_step036_onboarding_hour_storage import _connect

from terlimo_backend.auth_api import ApiError
from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.mobile_account import _entitlement_plan
from terlimo_backend.payments import PAYMENT_PROVIDER_KEY, ProviderPayment, create_order, record_webhook
from terlimo_backend.trial_activation import TRIAL_SOURCE_PLAN

PLANS = {
    "terlimo-30d": (1, "days:30", 200),
    "terlimo-3m": (3, "months:3", 480),
    "terlimo-6m": (6, "months:6", 840),
}


class FakeProvider:
    def __init__(self):
        self.calls = 0

    def capabilities(self):
        return {"checkout": True, "qr": False}

    async def create_payment(self, *, amount, currency, months, order_ref, description):
        self.calls += 1
        return ProviderPayment(provider_payment_id=f"tx-{order_ref}", pay_url=f"https://pay/{order_ref}", qr=None, variant="sbp")

    async def get_status(self, provider_payment_id):
        return {"id": provider_payment_id, "status": "CONFIRMED", "amount": 200, "currency": "RUB"}


def _settings(settings_factory, url):
    return settings_factory(
        url,
        payment_currency="RUB",
        payment_tariff_key="terlimo-200-30d-v1",
        payment_price_rub_1=200,
        payment_price_rub_3=480,
        payment_price_rub_6=840,
        platega_methods="sbp",
        platega_merchant_id="merchant-1",
        platega_secret="secret-1",
    )


async def _stack(settings_factory, migrated_url):
    settings = _settings(settings_factory, migrated_url)
    database = Database(settings)
    app = create_app(settings, database)
    app[PAYMENT_PROVIDER_KEY] = FakeProvider()
    client = TestClient(TestServer(app))
    await client.start_server()
    return client, settings, database


async def _keypop(client):
    key = KeyMaterial()
    pop = PoPClient(key)
    assert (await _enroll(client, pop, await _challenge(client, key, "enrollment"))).status == 200
    return key, pop


async def _installation(migrated_url, fingerprint):
    c = await _connect(migrated_url)
    try:
        return await c.fetchval("SELECT id FROM installations WHERE public_key_fingerprint=$1", fingerprint)
    finally:
        await c.close()


async def _bind(migrated_url, installation_id, tg):
    c = await _connect(migrated_url)
    try:
        acc = await c.fetchval("INSERT INTO accounts (status, telegram_id) VALUES ('verified',$1) RETURNING id", tg)
        await c.execute("INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')", acc, installation_id)
    finally:
        await c.close()


async def _session_after_bind(client, pop, tg):
    session = await _session(client, pop, await _challenge(client, pop.key, "session"),
                             ["session:read", "session:write"], f"idem-{uuid.uuid4().hex}")
    assert session.status == 200, await session.text()
    return (await session.json())["session"]["session_id"]


async def _quote(client, token, plan_id):
    r = await client.post(
        f"{MOBILE}/quotes",
        headers={"Authorization": f"Bearer {token}", "Idempotency-Key": f"q-{uuid.uuid4().hex}"},
        json={"plan_id": plan_id, "duration_code": PLANS[plan_id][1], "method": "sbp"},
    )
    assert r.status == 200, await r.text()
    return (await r.json())["quote_id"]


async def _me_plan(client, token):
    me = await client.get(f"{MOBILE}/me", headers={"Authorization": f"Bearer {token}"})
    assert me.status == 200, await me.text()
    return (await me.json())["entitlement"]["plan"]


async def _credit(client, migrated_url, settings, iid, token, plan_id, key):
    qid = await _quote(client, token, plan_id) if plan_id else None
    months = PLANS[plan_id][0] if plan_id else 1
    c = await _connect(migrated_url)
    try:
        order = await create_order(c, settings, FakeProvider(), installation_id=iid, months=months,
                                   idempotency_key=key, quote_id=qid)
        oid = order["payment"]["order_id"]
        res = await record_webhook(c, settings, merchant="merchant-1", secret="secret-1",
                                   body={"id": f"tx-{oid}", "amount": PLANS[plan_id][2] if plan_id else 200,
                                         "currency": "RUB", "status": "CONFIRMED"})
        assert res["result"] == "succeeded"
        row = await c.fetchrow("SELECT ends_at, revision FROM entitlements WHERE kind='paid'")
        return oid, row["ends_at"], int(row["revision"])
    finally:
        await c.close()


async def _bind_session(client, migrated_url, tg):
    key, pop = await _keypop(client)
    iid = await _installation(migrated_url, pop.key.fingerprint)
    await _bind(migrated_url, iid, tg)
    token = await _session_after_bind(client, pop, tg)
    return iid, token


async def test_paid_issuance_renewal_replay_from_proven_quotes(migrated_url, settings_factory):
    client, settings, database = await _stack(settings_factory, migrated_url)
    try:
        iid, token = await _bind_session(client, migrated_url, 777101)
        oid1, ends1, rev1 = await _credit(client, migrated_url, settings, iid, token, "terlimo-30d", "ap-1")
        assert await _me_plan(client, token) == {"id": "terlimo-30d", "title": "30 дней", "duration_code": "days:30"}

        oid2, ends2, rev2 = await _credit(client, migrated_url, settings, iid, token, "terlimo-3m", "ap-2")
        assert await _me_plan(client, token) == {"id": "terlimo-3m", "title": "3 месяца", "duration_code": "months:3"}
        assert ends2 > ends1 and rev2 == rev1 + 1

        c = await _connect(migrated_url)
        try:
            res = await record_webhook(c, settings, merchant="merchant-1", secret="secret-1",
                                       body={"id": f"tx-{oid2}", "amount": 480, "currency": "RUB", "status": "CONFIRMED"})
            assert res["result"] == "duplicate"
            row = await c.fetchrow("SELECT ends_at, revision FROM entitlements WHERE kind='paid'")
        finally:
            await c.close()
        assert row["ends_at"] == ends2 and int(row["revision"]) == rev2
        assert await _me_plan(client, token) == {"id": "terlimo-3m", "title": "3 месяца", "duration_code": "months:3"}
    finally:
        await database.close()
        await client.close()


async def test_renewal_without_proven_plan_sets_null_once(migrated_url, settings_factory):
    client, settings, database = await _stack(settings_factory, migrated_url)
    try:
        iid, token = await _bind_session(client, migrated_url, 777102)
        _oid1, _ends1, rev1 = await _credit(client, migrated_url, settings, iid, token, "terlimo-30d", "rn-1")
        assert await _me_plan(client, token) is not None

        # New credited order with NO proven selected plan: the active right's title is not
        # authoritative, so the snapshot becomes NULL (not the old plan).
        c = await _connect(migrated_url)
        try:
            order = await create_order(c, settings, FakeProvider(), installation_id=iid, months=1, idempotency_key="rn-2")
            oid = order["payment"]["order_id"]
            res = await record_webhook(c, settings, merchant="merchant-1", secret="secret-1",
                                       body={"id": f"tx-{oid}", "amount": 200, "currency": "RUB", "status": "CONFIRMED"})
            assert res["result"] == "succeeded"
            row = await c.fetchrow("SELECT ends_at, revision FROM entitlements WHERE kind='paid'")
        finally:
            await c.close()
        assert await _me_plan(client, token) is None
        assert int(row["revision"]) == rev1 + 1

        # replay leaves null/end/revision unchanged
        c = await _connect(migrated_url)
        try:
            res = await record_webhook(c, settings, merchant="merchant-1", secret="secret-1",
                                       body={"id": f"tx-{oid}", "amount": 200, "currency": "RUB", "status": "CONFIRMED"})
            assert res["result"] == "duplicate"
            row2 = await c.fetchrow("SELECT ends_at, revision FROM entitlements WHERE kind='paid'")
        finally:
            await c.close()
        assert row2["ends_at"] == row["ends_at"] and int(row2["revision"]) == int(row["revision"])
        assert await _me_plan(client, token) is None
    finally:
        await database.close()
        await client.close()


def _assert_conflict(exc_info, reason):
    err = exc_info.value
    assert isinstance(err, ApiError)
    assert err.code == "ORDER_CONFLICT" and err.http == 409
    assert (err.details or {}).get("reason") == reason


async def test_quote_proof_exact_conflicts_and_no_side_effect(migrated_url, settings_factory):
    client, settings, database = await _stack(settings_factory, migrated_url)
    provider = FakeProvider()
    try:
        iid, token = await _bind_session(client, migrated_url, 777103)
        c = await _connect(migrated_url)
        try:
            with pytest.raises(ApiError) as e1:
                await create_order(c, settings, provider, installation_id=iid, months=1,
                                   idempotency_key="qp-1", quote_id=str(uuid.uuid4()))
            _assert_conflict(e1, "quote_mismatch")
            assert provider.calls == 0

            qid = await _quote(client, token, "terlimo-30d")
            await c.execute("UPDATE s5_payment_quotes SET expires_at = now() - interval '1 minute' WHERE id=$1", uuid.UUID(qid))
            with pytest.raises(ApiError) as e2:
                await create_order(c, settings, provider, installation_id=iid, months=1,
                                   idempotency_key="qp-2", quote_id=qid)
            _assert_conflict(e2, "quote_expired")
            assert provider.calls == 0

            await c.execute("UPDATE s5_payment_quotes SET expires_at = now() + interval '5 minutes' WHERE id=$1", uuid.UUID(qid))
            await create_order(c, settings, provider, installation_id=iid, months=1, idempotency_key="qp-3", quote_id=qid)
            assert provider.calls == 1
            with pytest.raises(ApiError) as e3:
                await create_order(c, settings, provider, installation_id=iid, months=1, idempotency_key="qp-4", quote_id=qid)
            _assert_conflict(e3, "quote_already_used")
            assert provider.calls == 1  # no provider side effect on conflict
        finally:
            await c.close()
    finally:
        await database.close()
        await client.close()


async def test_same_key_retry_after_quote_expiry_returns_durable_order(migrated_url, settings_factory):
    client, settings, database = await _stack(settings_factory, migrated_url)
    provider = FakeProvider()
    try:
        iid, token = await _bind_session(client, migrated_url, 777104)
        qid = await _quote(client, token, "terlimo-30d")
        c = await _connect(migrated_url)
        try:
            first = await create_order(c, settings, provider, installation_id=iid, months=1,
                                       idempotency_key="ret-1", quote_id=qid)
            assert provider.calls == 1
            await c.execute("UPDATE s5_payment_quotes SET expires_at = now() - interval '1 minute' WHERE id=$1", uuid.UUID(qid))
            retry = await create_order(c, settings, provider, installation_id=iid, months=1,
                                       idempotency_key="ret-1", quote_id=qid)
            assert retry["payment"]["order_id"] == first["payment"]["order_id"]
            assert retry["payment"]["pay_url"] == first["payment"]["pay_url"]
            assert provider.calls == 1  # durable replay, no second provider create
            assert await c.fetchval("SELECT count(*) FROM entitlements WHERE kind='paid'") == 0
        finally:
            await c.close()
    finally:
        await database.close()
        await client.close()


async def test_same_key_conflicts_exact_reasons(migrated_url, settings_factory):
    client, settings, database = await _stack(settings_factory, migrated_url)
    provider = FakeProvider()
    try:
        iid, token = await _bind_session(client, migrated_url, 777105)
        q1 = await _quote(client, token, "terlimo-30d")
        q2 = await _quote(client, token, "terlimo-30d")
        q3 = await _quote(client, token, "terlimo-3m")
        c = await _connect(migrated_url)
        try:
            await create_order(c, settings, provider, installation_id=iid, months=1, idempotency_key="sk-1", quote_id=q1)
            assert provider.calls == 1
            # same key, changed parameters
            with pytest.raises(ApiError) as e1:
                await create_order(c, settings, provider, installation_id=iid, months=3, idempotency_key="sk-1", quote_id=q3)
            _assert_conflict(e1, "idempotency_key_reused")
            # same key, same parameters, different quote identity
            with pytest.raises(ApiError) as e2:
                await create_order(c, settings, provider, installation_id=iid, months=1, idempotency_key="sk-1", quote_id=q2)
            _assert_conflict(e2, "quote_changed")
            assert provider.calls == 1
        finally:
            await c.close()
    finally:
        await database.close()
        await client.close()


def test_entitlement_plan_strict_and_no_fabrication():
    assert _entitlement_plan({"source_plan": {"plan_id": "terlimo-3m", "title": "3 месяца", "duration_code": "months:3"}}) == {
        "id": "terlimo-3m", "title": "3 месяца", "duration_code": "months:3"
    }
    assert _entitlement_plan({"source_plan": json.dumps(TRIAL_SOURCE_PLAN)}) == {"id": "trial-7d", "title": "7 дней", "duration_code": "days:7"}
    assert _entitlement_plan({"source_plan": None}) is None
    assert _entitlement_plan({}) is None
    assert _entitlement_plan({"source_plan": {"title": "x"}}) is None
    assert _entitlement_plan({"source_plan": "not-json"}) is None


def test_contract_optional_plan_portable_path_with_revision_check():
    jsonschema = pytest.importorskip("jsonschema")
    referencing = pytest.importorskip("referencing")
    from referencing import Registry, Resource

    repo = os.environ.get("TERLIMO_CONTRACTS_DIR", "/home/pavel/projects/terlimo-s1-contracts")
    sub_path = os.path.join(repo, "schemas", "subscription.json")
    if not os.path.exists(sub_path):
        pytest.skip(f"contracts repo unavailable at {repo}")
    sub = json.load(open(sub_path))
    common = json.load(open(os.path.join(repo, "schemas", "common.json")))
    registry = Registry().with_resources([
        ("common.json", Resource.from_contents(common)),
        ("https://terlimo.local/schemas/common.json", Resource.from_contents(common)),
        ("subscription.json", Resource.from_contents(sub)),
        ("https://terlimo.local/schemas/subscription.json", Resource.from_contents(sub)),
    ])
    validator = jsonschema.Draft202012Validator(sub["$defs"]["EntitlementSnapshot"], registry=registry)
    base = {"type": "paid", "status": "active", "effective_device_limit": 2, "slots_used": 1,
            "revision": "1", "perpetual_commercial": False}
    validator.validate({**base, "plan": None})
    validator.validate({**base, "plan": {"id": "terlimo-30d", "title": "30 дней", "duration_code": "days:30"}})
    with pytest.raises(jsonschema.ValidationError):
        validator.validate({**base, "plan": {"id": "x", "title": "t", "duration_code": "d", "extra": 1}})
    try:
        head = subprocess.check_output(["git", "-C", repo, "rev-parse", "HEAD"], text=True, stderr=subprocess.DEVNULL).strip()
    except (subprocess.CalledProcessError, FileNotFoundError):
        pytest.skip("contracts repo is not a git checkout; revision check skipped")
    assert head == "caf90037a45d1006ae99c3e5cd86245c44464055", f"contract revision mismatch: {head}"
