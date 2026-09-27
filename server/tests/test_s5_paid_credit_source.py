"""Source-only S5 order credit proof; runs with isolated PostgreSQL and fake provider."""

from __future__ import annotations

import asyncio
import uuid

import asyncpg

from test_auth_flow import _challenge, _session
from test_s4_payments import (
    _app, _auth, _bind, _create_order, _installation_id, _session_token, _webhook_headers,
)
from terlimo_backend.payments import apply_paid_entitlement
from terlimo_backend.s5_payments import PAYMENTS_PATH


async def _bound_token(client, migrated_url):
    pop, _old_token = await _session_token(client)
    installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
    await _bind(migrated_url, installation_id, 555100000 + uuid.uuid4().int % 100000)
    refreshed = await _session(
        client, pop, await _challenge(client, pop.key, "session"),
        ["session:read", "session:write"], f"s5-{uuid.uuid4().hex}",
    )
    assert refreshed.status == 200
    return installation_id, (await refreshed.json())["session"]["session_id"]


async def _order(client, token, months=1):
    response = await _create_order(client, token, months)
    assert response.status == 200, await response.text()
    return (await response.json())["payment"]["order_id"]


async def _mark_paid(client, order_id, amount):
    response = await client.post(
        "/public/platega/webhook", headers=_webhook_headers(),
        json={"id": f"tx-{order_id}", "amount": amount, "currency": "RUB", "status": "CONFIRMED"},
    )
    assert response.status == 200, await response.text()


async def _view(client, token, order_id):
    response = await client.get(f"{PAYMENTS_PATH}/{order_id}", headers=_auth(token))
    assert response.status == 200, await response.text()
    return await response.json()


async def _me(client, token):
    response = await client.get("/api/mobile/v1/me", headers=_auth(token))
    assert response.status == 200, await response.text()
    return await response.json()


async def test_first_paid_after_trial_renewal_and_old_paid_uncredited(migrated_url, settings_factory):
    client, settings, database = await _app(settings_factory, migrated_url)
    try:
        installation_id, token = await _bound_token(client, migrated_url)
        connection = await asyncpg.connect(migrated_url)
        try:
            account_id = await connection.fetchval(
                "SELECT account_id FROM account_bindings WHERE installation_id=$1 AND status='active'",
                installation_id,
            )
            await connection.execute(
                """INSERT INTO entitlements(account_id,kind,status,starts_at,ends_at,device_limit,revision)
                   VALUES ($1,'trial','active',now()-interval '1 day',now()+interval '6 days',2,1)""",
                account_id,
            )
        finally:
            await connection.close()
        before = await _me(client, token)
        assert before["entitlement"]["type"] == "trial"
        assert before["entitlement"]["revision"] == "1"

        first_id = await _order(client, token)
        await _mark_paid(client, first_id, 200)
        first = await _view(client, token, first_id)
        first_me = await _me(client, token)
        assert first["payment_status"] == "paid"
        assert first["credited_entitlement_revision"] == "1"
        assert first_me["entitlement"]["type"] == "paid"
        assert first_me["entitlement"]["revision"] == first["credited_entitlement_revision"]
        assert first["access_application_state"] != "applied"  # no gateway readback

        second_id = await _order(client, token)
        await _mark_paid(client, second_id, 200)
        second = await _view(client, token, second_id)
        second_me = await _me(client, token)
        assert second["credited_entitlement_revision"] == "2"
        assert second_me["entitlement"]["type"] == "paid"
        assert second_me["entitlement"]["revision"] == second["credited_entitlement_revision"]
        assert (await _view(client, token, first_id))["credited_entitlement_revision"] == "1"
        connection = await asyncpg.connect(migrated_url)
        try:
            source_invoice_id = await connection.fetchval(
                "SELECT source_invoice_id FROM entitlements WHERE account_id=$1 AND kind='paid'",
                account_id,
            )
        finally:
            await connection.close()
        assert source_invoice_id == f"platega:tx-{first_id}"

        third_id = await _order(client, token)
        connection = await asyncpg.connect(migrated_url)
        try:
            await connection.execute("UPDATE payment_orders SET status='succeeded' WHERE id=$1", uuid.UUID(third_id))
        finally:
            await connection.close()
        uncredited = await _view(client, token, third_id)
        stale_me = await _me(client, token)
        assert uncredited["payment_status"] == "paid"
        assert uncredited["credited_entitlement_revision"] is None
        assert stale_me["entitlement"]["type"] == "paid"
        assert stale_me["entitlement"]["revision"] == "2"
        async with database.acquire() as connection:
            await apply_paid_entitlement(connection, settings, order_id=uuid.UUID(third_id))
        credited = await _view(client, token, third_id)
        fresh_me = await _me(client, token)
        assert credited["credited_entitlement_revision"] == "3"
        assert fresh_me["entitlement"]["revision"] == "3"
    finally:
        await database.close()
        await client.close()


async def test_two_first_payments_serialize_one_paid_right(migrated_url, settings_factory):
    client, settings, database = await _app(settings_factory, migrated_url)
    try:
        installation_id, token = await _bound_token(client, migrated_url)
        order_ids = [await _order(client, token), await _order(client, token)]
        connection = await asyncpg.connect(migrated_url)
        try:
            await connection.execute(
                "UPDATE payment_orders SET status='succeeded' WHERE id=ANY($1::uuid[])",
                [uuid.UUID(value) for value in order_ids],
            )
        finally:
            await connection.close()

        async def credit(order_id):
            async with database.acquire() as connection:
                await apply_paid_entitlement(connection, settings, order_id=uuid.UUID(order_id))

        await asyncio.gather(*(credit(order_id) for order_id in order_ids))
        views = [await _view(client, token, order_id) for order_id in order_ids]
        revisions = sorted(view["credited_entitlement_revision"] for view in views)
        assert revisions == ["1", "2"]
        connection = await asyncpg.connect(migrated_url)
        try:
            account_id = await connection.fetchval(
                "SELECT account_id FROM account_bindings WHERE installation_id=$1 AND status='active'",
                installation_id,
            )
            rows = await connection.fetch(
                "SELECT revision, ends_at, starts_at FROM entitlements WHERE account_id=$1 AND kind='paid'",
                account_id,
            )
        finally:
            await connection.close()
        assert len(rows) == 1 and rows[0]["revision"] == 2
        assert 59 <= (rows[0]["ends_at"] - rows[0]["starts_at"]).days <= 60
        assert (await _me(client, token))["entitlement"]["revision"] == "2"
    finally:
        await database.close()
        await client.close()
