"""S5 §3.2B: the selected quote method drives one idempotent provider checkout URL."""
from __future__ import annotations

import json
import uuid

import asyncpg
import pytest
from test_s4_payments import (
    FakePlategaProvider,
    _app,
    _auth,
    _FakeSession,
    _session_token,
)

from terlimo_backend.auth_api import ApiError
from terlimo_backend.payments import PlategaHttpProvider
from terlimo_backend.s5_payments import PAYMENTS_PATH, PLANS_PATH, QUOTES_PATH


def _key() -> str:
    return f"s5-{uuid.uuid4().hex}"


async def _quote(client, token, plan_id, *, method, key=None):
    return await client.post(
        QUOTES_PATH,
        headers={**_auth(token), "Idempotency-Key": key or _key()},
        json={"plan_id": plan_id, "duration_code": "days:30", "method": method},
    )


async def _create(client, token, quote_id, key=None):
    return await client.post(
        PAYMENTS_PATH, headers={**_auth(token), "Idempotency-Key": key or _key()}, json={"quote_id": quote_id}
    )


async def test_selected_method_reaches_provider_with_single_order_and_url(migrated_url, settings_factory):
    provider = FakePlategaProvider()
    client, _settings, database = await _app(settings_factory, migrated_url, provider=provider)
    try:
        pop, token = await _session_token(client)
        plan = (await (await client.get(PLANS_PATH)).json())["plans"][0]
        quote = await (await _quote(client, token, plan["plan_id"], method="crypto")).json()
        key = _key()
        created = await _create(client, token, quote["quote_id"], key=key)
        assert created.status == 200, await created.text()
        body = await created.json()
        # Public PaymentResponse stays the frozen 5-key Android shape; method/amount are not here.
        assert set(body) == {
            "request_id", "server_time", "schema_version", "status",
            "payment_id", "payment_status", "checkout_reference",
            "credited_entitlement_revision", "access_application_state",
        }
        assert body["payment_status"] == "pending"
        assert body["checkout_reference"] == f"https://pay.test/{body['payment_id']}"
        # The selected method/amount are exposed by the QUOTE and kept in the internal order snapshot.
        assert quote["method"] == "crypto"
        assert quote["amount"] == {"amount_minor": 20000, "currency": "RUB"}
        assert provider.create_calls == [{"method": "crypto", "order_ref": body["payment_id"], "amount": 200}]
        connection = await asyncpg.connect(migrated_url)
        try:
            assert await connection.fetchval("SELECT count(*) FROM payment_orders") == 1
            row_quote = await connection.fetchval(
                "SELECT quote FROM payment_orders WHERE id=$1", uuid.UUID(body["payment_id"])
            )
            snapshot = row_quote
            while isinstance(snapshot, str):
                snapshot = json.loads(snapshot)
            assert snapshot["method"] == "crypto"
        finally:
            await connection.close()
        # Exact same-key retry: durable replay, same order, no second provider call.
        replay = await _create(client, token, quote["quote_id"], key=key)
        assert replay.status == 200 and (await replay.json())["payment_id"] == body["payment_id"]
        assert len(provider.create_calls) == 1
        # Status is owner-bound and exposes the same method/url.
        status = await client.get(f"{PAYMENTS_PATH}/{body['payment_id']}", headers=_auth(token))
        status_body = await status.json()
        assert set(status_body) == set(body)
        assert status_body["checkout_reference"] == body["checkout_reference"]
        assert pop.key.fingerprint
    finally:
        await database.close()
        await client.close()


async def test_same_key_with_another_quote_conflicts_without_provider_call(migrated_url, settings_factory):
    provider = FakePlategaProvider()
    client, _settings, database = await _app(settings_factory, migrated_url, provider=provider)
    try:
        _pop, token = await _session_token(client)
        plan = (await (await client.get(PLANS_PATH)).json())["plans"][0]
        first = await (await _quote(client, token, plan["plan_id"], method="sbp")).json()
        key = _key()
        assert (await _create(client, token, first["quote_id"], key=key)).status == 200
        second = await (await _quote(client, token, plan["plan_id"], method="sbp")).json()
        assert second["quote_id"] != first["quote_id"]
        conflict = await _create(client, token, second["quote_id"], key=key)
        assert conflict.status == 409 and (await conflict.json())["code"] == "ORDER_CONFLICT"
        assert len(provider.create_calls) == 1
    finally:
        await database.close()
        await client.close()


async def test_pending_order_without_checkout_url_is_honest_refusal(migrated_url, settings_factory):
    provider = FakePlategaProvider(pay_url=None)
    client, _settings, database = await _app(settings_factory, migrated_url, provider=provider)
    try:
        _pop, token = await _session_token(client)
        plan = (await (await client.get(PLANS_PATH)).json())["plans"][0]
        quote = await (await _quote(client, token, plan["plan_id"], method="sbp")).json()
        key = _key()
        refused = await _create(client, token, quote["quote_id"], key=key)
        assert refused.status == 503
        assert (await refused.json())["code"] == "PAYMENT_CHECKOUT_UNAVAILABLE"
        assert len(provider.create_calls) == 1
        # The durable order keeps the provider identity (no duplicate invoice) and status
        # refuses the same way; replay must not call the provider again.
        connection = await asyncpg.connect(migrated_url)
        try:
            row = await connection.fetchrow("SELECT id, provider_payment_id, provider_create_state FROM payment_orders")
            assert row["provider_payment_id"] == f"tx-{row['id']}" and row["provider_create_state"] == "created"
        finally:
            await connection.close()
        replay = await _create(client, token, quote["quote_id"], key=key)
        assert replay.status == 503 and (await replay.json())["code"] == "PAYMENT_CHECKOUT_UNAVAILABLE"
        assert len(provider.create_calls) == 1
        status = await client.get(f"{PAYMENTS_PATH}/{row['id']}", headers=_auth(token))
        # Status stays readable (owner-only) and reports the pending order without a URL.
        assert status.status == 200 and (await status.json())["checkout_reference"] is None
    finally:
        await database.close()
        await client.close()


async def test_platega_explicit_method_maps_ids_and_refuses_unmapped():
    class Settings:  # minimal duck-typed settings for the adapter body
        platega_methods = "sbp,international,crypto"
        platega_method_ids = "sbp:2,international:12,crypto:13"
        platega_merchant_id = "m"
        platega_secret = "s"
        platega_base_url = "https://app.platega.io"
        platega_return_url = "https://return.test"
        platega_failed_url = "https://failed.test"
        platega_http_timeout_seconds = 5

    provider = PlategaHttpProvider(Settings())
    provider._session = _FakeSession(200, {"transactionId": "tx-1", "redirect": "https://pay/1"})
    paid = await provider.create_payment(amount=200, currency="RUB", months=1, order_ref="o1",
                                         description="d", method="crypto")
    assert paid.pay_url == "https://pay/1" and paid.variant == "crypto"
    body = provider._session.last_json
    assert body["paymentMethod"] == 13

    provider._session = _FakeSession(200, {"transactionId": "tx-2", "redirect": "https://pay/2"})
    await provider.create_payment(amount=200, currency="RUB", months=1, order_ref="o2",
                                  description="d", method="sbp")
    assert provider._session.last_json["paymentMethod"] == 2

    provider._session = _FakeSession(200, {"transactionId": "tx-3", "redirect": "https://pay/3"})
    with pytest.raises(ApiError) as err:
        await provider.create_payment(amount=200, currency="RUB", months=1, order_ref="o3",
                                      description="d", method="card")
    assert err.value.code == "METHOD_UNAVAILABLE" and provider._session.last_json is None
