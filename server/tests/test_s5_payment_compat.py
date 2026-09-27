"""S5 frozen Android wire contract while TEST merchant creation stays disabled."""

from __future__ import annotations

import uuid

import asyncpg

from test_auth_flow import _challenge, _session
from test_s4_payments import _app, _auth, _bind, _installation_id, _session_token
from terlimo_backend.s5_payments import PAYMENTS_PATH, PLANS_PATH, QUOTES_PATH


def _key() -> str:
    return f"s5-{uuid.uuid4().hex}"


async def test_public_plans_and_owner_bound_immutable_quote(migrated_url, settings_factory):
    client, _settings, database = await _app(settings_factory, migrated_url)
    try:
        plans_response = await client.get(PLANS_PATH)
        assert plans_response.status == 200
        plans = await plans_response.json()
        assert plans["schema_version"] == "1.0" and plans["status"] == "ok"
        assert plans["plans_revision"].isdigit()
        assert [(p["duration_code"], p["amount"]["amount_minor"]) for p in plans["plans"]] == [
            ("days:30", 20000), ("months:3", 48000), ("months:6", 84000)
        ]
        assert [p["title"] for p in plans["plans"]] == ["30 дней", "3 месяца", "6 месяцев"]
        assert all(p["base_device_limit"] == 2 and p["methods"] == ["sbp", "crypto"] for p in plans["plans"])
        assert all("card" not in p["methods"] for p in plans["plans"])

        first, token = await _session_token(client)
        _second, other_token = await _session_token(client)
        plan = plans["plans"][0]
        body = {"plan_id": plan["plan_id"], "duration_code": "days:30", "method": "sbp"}
        key = _key()
        headers = {**_auth(token), "Idempotency-Key": key}
        created = await client.post(QUOTES_PATH, headers=headers, json=body)
        assert created.status == 200, await created.text()
        quote = await created.json()
        assert quote["amount"] == {"amount_minor": 20000, "currency": "RUB"}
        assert quote["duration_code"] == "days:30" and quote["method"] == "sbp"
        assert quote["device_limit"] == 2 and quote["expires_at"].endswith("Z")
        replay = await client.post(QUOTES_PATH, headers=headers, json=body)
        assert (await replay.json())["quote_id"] == quote["quote_id"]
        changed = await client.post(QUOTES_PATH, headers=headers, json={**body, "method": "crypto"})
        assert changed.status == 409 and (await changed.json())["code"] == "IDEMPOTENCY_CONFLICT"
        unavailable = await client.post(QUOTES_PATH, headers={**_auth(token), "Idempotency-Key": _key()}, json={**body, "method": "card"})
        assert unavailable.status == 403 and (await unavailable.json())["code"] == "METHOD_UNAVAILABLE"
        other = await client.post(QUOTES_PATH, headers={**_auth(other_token), "Idempotency-Key": key}, json=body)
        assert other.status == 200 and (await other.json())["quote_id"] != quote["quote_id"]
        assert first.key.fingerprint
    finally:
        await database.close()
        await client.close()


async def test_payment_create_off_never_mints_order_and_status_is_owner_only(migrated_url, settings_factory):
    client, _settings, database = await _app(settings_factory, migrated_url)
    try:
        pop, token = await _session_token(client)
        _other_pop, other_token = await _session_token(client)
        plan = (await (await client.get(PLANS_PATH)).json())["plans"][0]
        quote = await client.post(
            QUOTES_PATH, headers={**_auth(token), "Idempotency-Key": _key()},
            json={"plan_id": plan["plan_id"], "duration_code": "days:30", "method": "sbp"},
        )
        quote_id = (await quote.json())["quote_id"]
        create = await client.post(
            PAYMENTS_PATH, headers={**_auth(token), "Idempotency-Key": _key()}, json={"quote_id": quote_id}
        )
        assert create.status == 503
        error = await create.json()
        assert error["code"] == "SERVICE_UNAVAILABLE" and error["retryable"] is True
        foreign = await client.post(
            PAYMENTS_PATH, headers={**_auth(other_token), "Idempotency-Key": _key()}, json={"quote_id": quote_id}
        )
        assert foreign.status == 404 and (await foreign.json())["code"] == "NOT_FOUND"
        connection = await asyncpg.connect(migrated_url)
        try:
            assert await connection.fetchval("SELECT count(*) FROM payment_orders") == 0
            installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
            order_id = await connection.fetchval(
                """INSERT INTO payment_orders
                   (installation_id,idempotency_key,quote,amount,currency,months,tariff_key,status)
                   VALUES ($1,$2,'{}'::jsonb,200,'RUB',1,'terlimo-200-30d-v1','pending') RETURNING id""",
                installation_id, _key(),
            )
        finally:
            await connection.close()
        own = await client.get(f"{PAYMENTS_PATH}/{order_id}", headers=_auth(token))
        assert own.status == 200
        view = await own.json()
        assert view["payment_id"] == str(order_id)
        assert view["payment_status"] == "pending"
        assert view["credited_entitlement_revision"] is None
        assert view["access_application_state"] == "not_requested"
        hidden = await client.get(f"{PAYMENTS_PATH}/{order_id}", headers=_auth(other_token))
        assert hidden.status == 404 and (await hidden.json())["code"] == "PAYMENT_NOT_FOUND"
        connection = await asyncpg.connect(migrated_url)
        try:
            await connection.execute("UPDATE payment_orders SET status='succeeded' WHERE id=$1", order_id)
        finally:
            await connection.close()
        await _bind(migrated_url, installation_id, 555000222)
        connection = await asyncpg.connect(migrated_url)
        try:
            account_id = await connection.fetchval(
                "SELECT account_id FROM account_bindings WHERE installation_id=$1 AND status='active'",
                installation_id,
            )
            await connection.execute(
                """INSERT INTO entitlements(account_id,kind,status,starts_at,ends_at,device_limit,revision)
                   VALUES ($1,'paid','active',now()-interval '1 day',now()+interval '20 days',2,7)""",
                account_id,
            )
        finally:
            await connection.close()
        refreshed = await _session(
            client, pop, await _challenge(client, pop.key, "session"),
            ["session:read", "session:write"], _key(),
        )
        assert refreshed.status == 200
        token = (await refreshed.json())["session"]["session_id"]
        me = await client.get("/api/mobile/v1/me", headers=_auth(token))
        assert me.status == 200 and (await me.json())["entitlement"]["revision"] == "7"
        # A paid provider state without this order's entitlement credit cannot borrow an
        # existing active trial/paid right's revision.
        uncredited = await client.get(f"{PAYMENTS_PATH}/{order_id}", headers=_auth(token))
        assert uncredited.status == 200
        uncredited_view = await uncredited.json()
        assert uncredited_view["payment_status"] == "paid"
        assert uncredited_view["credited_entitlement_revision"] is None
        assert uncredited_view["access_application_state"] == "pending"
    finally:
        await database.close()
        await client.close()
