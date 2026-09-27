"""S4 payment slice: quotes, Platega-verified orders, idempotent paid access, H2H boundary.

Local stub provider only (no real Platega call); the unified backend remains the sole
authority for paid access through the normal entitlement/grant path.
"""
from __future__ import annotations

import json
import uuid
from datetime import UTC, datetime, timedelta

import asyncpg
import pytest
from aiohttp.test_utils import TestClient, TestServer

from fake_gateway_admin import FakeGatewayAdmin
from test_auth_flow import MOBILE, KeyMaterial, PoPClient, _challenge, _enroll, _session
from test_step036_onboarding_hour_storage import _connect, _seed_gateway

from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.gateway_adapter import GatewayAdminClient, GatewayError
from terlimo_backend.gateway_control import GatewayControlHandlers
from terlimo_backend.payments import (
    ORDERS_PATH,
    ORDER_PATH,
    QUOTE_PATH,
    PAYMENT_PROVIDER_KEY,
    ProviderPayment,
    apply_parked_payments,
    register_payment_routes,
)
from terlimo_backend.telegram_binding import BOT_KEY_HEADER, CONFIRM_PATH, LINK_PATH
from terlimo_backend.worker import OutboxWorker

BOT_KEY = "test-bot-key"
BOT_USERNAME = "terlimo_reg_bot"
GATEWAY_KEY = "terlimo-035-node"


class FakePlategaProvider:
    def __init__(self, *, qr: str | None = None, pay_url: str | None = "https://pay.test/"):
        self._qr = qr
        self._pay_url = pay_url
        self.status_view: dict | None = None
        self.create_calls: list[dict] = []

    def capabilities(self) -> dict[str, bool]:
        return {"checkout": True, "qr": self._qr is not None}

    async def create_payment(self, *, amount, currency, months, order_ref, description, method=None):
        self.create_calls.append({"method": method, "order_ref": order_ref, "amount": amount})
        return ProviderPayment(
            provider_payment_id=f"tx-{order_ref}",
            pay_url=(f"{self._pay_url}{order_ref}" if self._pay_url else None),
            qr=self._qr,
            variant=method or "sbp",
        )

    async def get_status(self, provider_payment_id):
        if self.status_view is None:
            return None
        view = dict(self.status_view)
        view.setdefault("id", provider_payment_id)
        return view


def _settings(settings_factory, url, **overrides):
    base = dict(
        telegram_bot_username=BOT_USERNAME,
        telegram_bot_key=BOT_KEY,
        registration_token_ttl_seconds=600,
        payment_currency="RUB",
        payment_tariff_key="terlimo-200-30d-v1",
        payment_price_rub_1=200,
        payment_price_rub_3=480,
        payment_price_rub_6=840,
        platega_merchant_id="merchant-1",
        platega_secret="secret-1",
        platega_methods="sbp,international,crypto",
    )
    base.update(overrides)
    return settings_factory(url, **base)


async def _app(settings_factory, migrated_url, provider=None, disable_provider=False, **overrides):
    settings = _settings(settings_factory, migrated_url, **overrides)
    database = Database(settings)
    app = create_app(settings, database)
    app[PAYMENT_PROVIDER_KEY] = None if disable_provider else (provider if provider is not None else FakePlategaProvider())
    client = TestClient(TestServer(app))
    await client.start_server()
    return client, settings, database


async def _session_token(client, scopes=("session:read", "session:write")) -> tuple[PoPClient, str]:
    key = KeyMaterial()
    pop_client = PoPClient(key)
    enrollment = await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))
    assert enrollment.status == 200, await enrollment.text()
    session = await _session(
        client, pop_client, await _challenge(client, key, "session"), list(scopes),
        f"idem-{uuid.uuid4().hex}",
    )
    assert session.status == 200, await session.text()
    return pop_client, (await session.json())["session"]["session_id"]


async def _installation_id(migrated_url: str, fingerprint: str):
    connection = await _connect(migrated_url)
    try:
        return await connection.fetchval(
            "SELECT id FROM installations WHERE public_key_fingerprint = $1", fingerprint
        )
    finally:
        await connection.close()


async def _bind(migrated_url: str, installation_id, telegram_id: int) -> None:
    connection = await _connect(migrated_url)
    try:
        account_id = await connection.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified', $1) RETURNING id", telegram_id
        )
        await connection.execute(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1, $2, 'active')",
            account_id,
            installation_id,
        )
    finally:
        await connection.close()


def _auth(token: str) -> dict[str, str]:
    return {"Authorization": f"Bearer {token}"}


def _webhook_headers():
    return {"X-MerchantId": "merchant-1", "X-Secret": "secret-1"}


async def _paid_count(migrated_url, installation_id) -> int:
    connection = await _connect(migrated_url)
    try:
        return await connection.fetchval(
            """
            SELECT count(*) FROM entitlements e
            JOIN account_bindings b ON b.account_id = e.account_id AND b.status = 'active'
            WHERE b.installation_id = $1 AND e.kind = 'paid'
            """,
            installation_id,
        )
    finally:
        await connection.close()


async def _create_order(client, token, months, idem=None):
    body = {"months": months, "idempotency_key": idem or f"idem-{uuid.uuid4().hex}"}
    return await client.post(ORDERS_PATH, headers=_auth(token), json=body)


async def test_quote_is_server_authoritative_and_blocks_when_unpriced(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    try:
        _pop, token = await _session_token(client)
        quote = await client.post(QUOTE_PATH, headers=_auth(token), json={"months": 3})
        assert quote.status == 200, await quote.text()
        body = await quote.json()
        assert body["quote"]["amount"] == 480 and body["quote"]["currency"] == "RUB"
        assert body["quote"]["months"] == 3 and "sbp" in body["quote"]["methods"]
        assert body["quote"]["duration"] == {"unit": "months", "value": 3}
        assert "period_days" not in body["quote"]
        first = await client.post(QUOTE_PATH, headers=_auth(token), json={"months": 1})
        assert (await first.json())["quote"]["duration"] == {"unit": "days", "value": 30}
        six = await client.post(QUOTE_PATH, headers=_auth(token), json={"months": 6})
        assert (await six.json())["quote"]["duration"] == {"unit": "months", "value": 6}
        bad = await client.post(QUOTE_PATH, headers=_auth(token), json={"months": 2})
        assert bad.status == 400
    finally:
        await database.close()
        await client.close()

    unpriced, _s, database2 = await _app(settings_factory, migrated_url, payment_price_rub_1=0)
    try:
        _pop, token = await _session_token(unpriced)
        blocked = await unpriced.post(QUOTE_PATH, headers=_auth(token), json={"months": 1})
        assert blocked.status == 503
        assert (await blocked.json())["code"] == "PAYMENT_PRICE_UNAVAILABLE"
        order = await _create_order(unpriced, token, 1)
        assert order.status == 503, await order.text()
    finally:
        await database2.close()
        await unpriced.close()


async def test_success_grants_paid_once_and_duplicate_events_do_not_repeat(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555000111)
        created = await _create_order(client, token, 3)
        assert created.status == 200, await created.text()
        order = (await created.json())["payment"]
        assert order["amount"] == 480 and order["pay_url"].startswith("https://pay.test/")

        first = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order['order_id']}", "amount": 480, "currency": "RUB", "status": "CONFIRMED"},
        )
        assert first.status == 200, await first.text()
        assert (await first.json())["result"] == "succeeded"
        assert await _paid_count(migrated_url, installation_id) == 1

        connection = await _connect(migrated_url)
        try:
            ends = await connection.fetchval("SELECT ends_at FROM entitlements WHERE kind = 'paid'")
        finally:
            await connection.close()

        # Duplicate event id and a second distinct event for the same payment: no second interval.
        again = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order['order_id']}", "amount": 480, "currency": "RUB", "status": "CONFIRMED"},
        )
        assert again.status == 200 and (await again.json())["result"] == "duplicate"
        other_event = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order['order_id']}", "eventId": "evt-2", "amount": 480, "currency": "RUB", "status": "CONFIRMED"},
        )
        assert other_event.status == 200 and (await other_event.json())["result"] == "duplicate"
        assert await _paid_count(migrated_url, installation_id) == 1
        connection = await _connect(migrated_url)
        try:
            ends_after = await connection.fetchval("SELECT ends_at FROM entitlements WHERE kind = 'paid'")
        finally:
            await connection.close()
        assert ends_after == ends
    finally:
        await database.close()
        await client.close()


async def test_amount_and_currency_mismatch_never_grant(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555000222)
        created = await _create_order(client, token, 1)
        order_id = (await created.json())["payment"]["order_id"]
        amount_bad = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "amount": 1, "currency": "RUB", "status": "CONFIRMED"},
        )
        assert amount_bad.status == 200 and (await amount_bad.json())["result"] == "amount_mismatch"
        currency_bad = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "amount": 200, "currency": "USD", "status": "CONFIRMED"},
        )
        assert currency_bad.status == 200 and (await currency_bad.json())["result"] == "currency_mismatch"
        assert await _paid_count(migrated_url, installation_id) == 0
    finally:
        await database.close()
        await client.close()


async def test_pending_then_failed_never_grants(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555000333)
        created = await _create_order(client, token, 1)
        order_id = (await created.json())["payment"]["order_id"]
        pending = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "eventId": "p1", "amount": 200, "currency": "RUB", "status": "PENDING"},
        )
        assert (await pending.json())["result"] == "pending"
        failed = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "eventId": "f1", "amount": 200, "currency": "RUB", "status": "CANCELED"},
        )
        assert (await failed.json())["result"] == "canceled"
        status = await client.get(ORDER_PATH.format(order_id=order_id), headers=_auth(token))
        assert (await status.json())["payment"]["status"] == "canceled"
        assert await _paid_count(migrated_url, installation_id) == 0
    finally:
        await database.close()
        await client.close()


async def test_prebinding_payment_parked_then_applied_on_telegram_confirm(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        # Not bound yet: the paid order must stay parked.
        created = await _create_order(client, token, 6)
        order_id = (await created.json())["payment"]["order_id"]
        paid_event = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "amount": 840, "currency": "RUB", "status": "CONFIRMED"},
        )
        assert (await paid_event.json())["result"] == "succeeded"
        assert await _paid_count(migrated_url, installation_id) == 0

        # Real Telegram confirmation binds the account and the hook applies the parked order once.
        link = await client.post(LINK_PATH, headers=_auth(token), json={})
        assert link.status == 200, await link.text()
        link_registration = (await link.json())["registration"]
        confirmed = await client.post(
            CONFIRM_PATH,
            json={"token": link_registration["token"], "telegram_id": 555000444},
            headers={BOT_KEY_HEADER: BOT_KEY},
        )
        assert confirmed.status == 200, await confirmed.text()
        assert await _paid_count(migrated_url, installation_id) == 1
        connection = await _connect(migrated_url)
        try:
            row = await connection.fetchrow(
                "SELECT e.starts_at, e.ends_at, o.applied_entitlement_id FROM entitlements e JOIN payment_orders o ON o.applied_entitlement_id = e.id WHERE o.id = $1",
                order_id,
            )
            assert row is not None and row["applied_entitlement_id"] is not None
            assert (row["ends_at"] - row["starts_at"]).days in (180, 181)
            # Idempotent: applying again changes nothing.
            async with connection.transaction():
                applied = await apply_parked_payments(connection, _settings_obj, installation_id=installation_id)
            assert applied == 0
        finally:
            await connection.close()
        assert await _paid_count(migrated_url, installation_id) == 1
    finally:
        await database.close()
        await client.close()


async def test_foreign_installation_cannot_read_order(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    try:
        pop_a, token_a = await _session_token(client)
        pop_b, token_b = await _session_token(client)
        installation_a = await _installation_id(migrated_url, pop_a.key.fingerprint)
        await _bind(migrated_url, installation_a, 555000555)
        created = await _create_order(client, token_a, 1)
        order_id = (await created.json())["payment"]["order_id"]
        foreign = await client.get(ORDER_PATH.format(order_id=order_id), headers=_auth(token_b))
        assert foreign.status == 404
        own = await client.get(ORDER_PATH.format(order_id=order_id), headers=_auth(token_a))
        assert own.status == 200
    finally:
        await database.close()
        await client.close()


async def test_h2h_qr_unavailable_reports_capability_not_fabricated(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url, provider=FakePlategaProvider(qr=None))
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555000666)
        created = await _create_order(client, token, 1)
        body = await created.json()
        assert body["capability"] == {"checkout": True, "qr": False}
        assert body["payment"]["qr"] is None and body["payment"]["pay_url"]

        qr_client, _s, database2 = await _app(settings_factory, migrated_url, provider=FakePlategaProvider(qr="SBPQR"))
        try:
            pop2, token2 = await _session_token(qr_client)
            installation2 = await _installation_id(migrated_url, pop2.key.fingerprint)
            await _bind(migrated_url, installation2, 555000777)
            created2 = await _create_order(qr_client, token2, 1)
            body2 = await created2.json()
            assert body2["capability"]["qr"] is True and body2["payment"]["qr"] == "SBPQR"
        finally:
            await database2.close()
            await qr_client.close()
    finally:
        await database.close()
        await client.close()


async def test_paid_webhook_enqueues_grant_and_survives_gateway_unavailable(migrated_url, settings_factory):
    """A paid webhook must enqueue the normal grant/outbox path automatically (no manual
    ensure_grant), and an unavailable gateway must leave the paid grant retriable, not lost."""
    settings = _settings(settings_factory, migrated_url)
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id=GATEWAY_KEY)
    await fake.start()
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    try:
        gateway_id, endpoints = await _seed_gateway(migrated_url, key=GATEWAY_KEY)
        endpoints["admin_socket"] = fake.socket_path
        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                "UPDATE gateways SET endpoints = $2::jsonb WHERE id = $1",
                gateway_id,
                json.dumps(endpoints),
            )
        finally:
            await connection.close()
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555000888)
        created = await _create_order(client, token, 1)
        order_id = (await created.json())["payment"]["order_id"]
        webhook = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "amount": 200, "currency": "RUB", "status": "CONFIRMED"},
        )
        assert (await webhook.json())["result"] == "succeeded"
        connection = await _connect(migrated_url)
        try:
            enqueued = await connection.fetchval(
                "SELECT count(*) FROM outbox_operations WHERE operation_type = 'gateway.apply_grant'"
            )
            applied_before = await connection.fetchval("SELECT count(*) FROM grants WHERE applied_generation = 1")
        finally:
            await connection.close()
        assert enqueued == 1 and applied_before == 0, "paid webhook must enqueue the grant via the normal outbox path"

        down = {"flag": True}

        def factory(_key, route):
            if down["flag"]:
                raise GatewayError("GATEWAY_UNREACHABLE")
            return GatewayAdminClient(socket_path=str(route["admin_socket"]), main_password="fixture-main", timeout_seconds=5)

        handlers = GatewayControlHandlers(settings, client_factory=factory).as_handlers()
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="s4-gw")
        await database.ensure_ready()
        await worker.drain()
        connection = await _connect(migrated_url)
        try:
            pending = await connection.fetchval(
                "SELECT count(*) FROM outbox_operations WHERE operation_type = 'gateway.apply_grant' AND status = 'pending'"
            )
            applied0 = await connection.fetchval("SELECT count(*) FROM grants WHERE applied_generation = 1")
        finally:
            await connection.close()
        assert pending == 1 and applied0 == 0, "unavailable gateway must leave the paid grant retriable, not lost"

        down["flag"] = False
        connection = await _connect(migrated_url)
        try:
            await connection.execute("UPDATE outbox_operations SET available_at = now() WHERE status = 'pending'")
        finally:
            await connection.close()
        await worker.drain()
        connection = await _connect(migrated_url)
        try:
            state = await connection.fetchval("SELECT state FROM grants")
            applied = await connection.fetchval("SELECT applied_generation FROM grants")
        finally:
            await connection.close()
        assert state == "applied" and int(applied) == 1
    finally:
        await database.close()
        await client.close()
        await fake.stop()


class FlakyProvider(FakePlategaProvider):
    def __init__(self):
        super().__init__()
        self.fail = True
        self.calls = 0

    async def create_payment(self, **kwargs):
        self.calls += 1
        if self.fail:
            from terlimo_backend.payments import ProviderUnknown

            raise ProviderUnknown()
        return await super().create_payment(**kwargs)


async def _order_rows(migrated_url, installation_id):
    connection = await _connect(migrated_url)
    try:
        return await connection.fetch(
            "SELECT * FROM payment_orders WHERE installation_id = $1 ORDER BY created_at", installation_id
        )
    finally:
        await connection.close()


async def test_provider_failure_is_unknown_and_retry_never_duplicates(migrated_url, settings_factory):
    provider = FlakyProvider()
    client, _settings_obj, database = await _app(settings_factory, migrated_url, provider=provider)
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555001000)
        failed = await client.post(
            ORDERS_PATH, headers=_auth(token), json={"months": 1, "idempotency_key": "idem-provider-fail"}
        )
        assert failed.status == 503, await failed.text()
        assert (await failed.json())["code"] == "PAYMENT_PROVIDER_UNKNOWN"
        rows = await _order_rows(migrated_url, installation_id)
        assert len(rows) == 1 and rows[0]["provider_payment_id"] is None
        assert rows[0]["provider_create_state"] == "unknown"

        # Same key retry: no second provider create (unknown is never retried blindly).
        retry = await client.post(
            ORDERS_PATH, headers=_auth(token), json={"months": 1, "idempotency_key": "idem-provider-fail"}
        )
        assert retry.status == 503 and provider.calls == 1
        assert len(await _order_rows(migrated_url, installation_id)) == 1

        # Same key with different parameters is still a deterministic conflict.
        conflict = await client.post(
            ORDERS_PATH, headers=_auth(token), json={"months": 3, "idempotency_key": "idem-provider-fail"}
        )
        assert conflict.status == 409, await conflict.text()
        assert (await conflict.json())["code"] == "ORDER_CONFLICT"
        assert provider.calls == 1
    finally:
        await database.close()
        await client.close()


async def test_webhook_requires_exact_amount_and_currency(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555001001)
        created = await _create_order(client, token, 1)
        order_id = (await created.json())["payment"]["order_id"]

        missing_amount = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "currency": "RUB", "status": "CONFIRMED"},
        )
        assert (await missing_amount.json())["result"] == "amount_missing"
        malformed_amount = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "eventId": "m2", "amount": "abc", "currency": "RUB", "status": "CONFIRMED"},
        )
        assert (await malformed_amount.json())["result"] == "amount_missing"
        missing_currency = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "eventId": "m3", "amount": 200, "status": "CONFIRMED"},
        )
        assert (await missing_currency.json())["result"] == "currency_missing"
        assert await _paid_count(migrated_url, installation_id) == 0
        status = await client.get(ORDER_PATH.format(order_id=order_id), headers=_auth(token))
        assert (await status.json())["payment"]["status"] == "pending"
    finally:
        await database.close()
        await client.close()


async def test_reconcile_applies_succeeded_but_unapplied_order(migrated_url, settings_factory):
    client, settings, database = await _app(settings_factory, migrated_url)
    from terlimo_backend.payments import reconcile_payments

    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555001002)
        created = await _create_order(client, token, 3)
        order_id = (await created.json())["payment"]["order_id"]
        # Simulate the crash window: succeeded status persisted, entitlement not applied.
        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                "UPDATE payment_orders SET status = 'succeeded', paid_at = now() WHERE id = $1", order_id
            )
        finally:
            await connection.close()
        assert await _paid_count(migrated_url, installation_id) == 0
        provider = FakePlategaProvider()
        provider.status_view = {"amount": 480, "currency": "RUB", "status": "CONFIRMED"}
        connection = await _connect(migrated_url)
        try:
            result = await reconcile_payments(connection, settings, provider, limit=10)
        finally:
            await connection.close()
        assert result["applied"] == 1
        assert await _paid_count(migrated_url, installation_id) == 1
        # Second reconciliation must not grant again.
        connection = await _connect(migrated_url)
        try:
            second = await reconcile_payments(connection, settings, provider, limit=10)
        finally:
            await connection.close()
        assert second["applied"] == 0
        assert await _paid_count(migrated_url, installation_id) == 1
    finally:
        await database.close()
        await client.close()


class _FakeResp:
    def __init__(self, status, payload):
        self.status = status
        self._payload = payload

    async def json(self):
        return self._payload

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False


class _FakeSession:
    def __init__(self, status, payload):
        self._resp = _FakeResp(status, payload)
        self.last_json = None

    def post(self, *a, **k):
        self.last_json = k.get("json")
        return self._resp

    def get(self, *a, **k):
        self.last_json = k.get("json")
        return self._resp


async def test_platega_http_provider_classifies_rejection_vs_unknown(migrated_url, settings_factory):
    from terlimo_backend.auth_api import ApiError
    from terlimo_backend.payments import PlategaHttpProvider

    settings = _settings(settings_factory, migrated_url)
    provider = PlategaHttpProvider(settings)
    # 5xx may follow a created transaction -> unknown (fail closed), not a safe rejection.
    provider._session = _FakeSession(500, {"ok": False})
    with pytest.raises(ApiError) as server_error:
        await provider.create_payment(amount=200, currency="RUB", months=1, order_ref="o1", description="d")
    assert server_error.value.code == "PAYMENT_PROVIDER_UNKNOWN"

    # No documented proof that 4xx precedes a created transaction -> also unknown (fail closed).
    provider._session = _FakeSession(400, {"error": "bad"})
    with pytest.raises(ApiError) as client_error:
        await provider.create_payment(amount=200, currency="RUB", months=1, order_ref="o2", description="d")
    assert client_error.value.code == "PAYMENT_PROVIDER_UNKNOWN"

    provider._session = _FakeSession(503, {})
    assert await provider.get_status("tx-1") is None


class UnknownProvider(FakePlategaProvider):
    def __init__(self):
        super().__init__()
        self.calls = 0

    async def create_payment(self, **kwargs):
        from terlimo_backend.payments import ProviderUnknown

        self.calls += 1
        raise ProviderUnknown()


async def test_concurrent_same_key_creates_one_provider_invoice(migrated_url, settings_factory):
    import asyncio

    provider = FakePlategaProvider()
    provider.calls = 0
    original = provider.create_payment

    async def counting(**kwargs):
        provider.calls += 1
        await asyncio.sleep(0.05)
        return await original(**kwargs)

    provider.create_payment = counting
    client, _settings_obj, database = await _app(settings_factory, migrated_url, provider=provider)
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555002000)
        body = {"months": 1, "idempotency_key": "idem-concurrent"}
        first, second = await asyncio.gather(
            client.post(ORDERS_PATH, headers=_auth(token), json=body),
            client.post(ORDERS_PATH, headers=_auth(token), json=body),
        )
        assert first.status == 200 and second.status == 200
        assert provider.calls == 1
        assert len(await _order_rows(migrated_url, installation_id)) == 1
    finally:
        await database.close()
        await client.close()


async def test_provider_timeout_is_unknown_and_never_creates_second_invoice(migrated_url, settings_factory):
    provider = UnknownProvider()
    client, _settings_obj, database = await _app(settings_factory, migrated_url, provider=provider)
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555002001)
        body = {"months": 1, "idempotency_key": "idem-unknown"}
        first = await client.post(ORDERS_PATH, headers=_auth(token), json=body)
        assert first.status == 503 and (await first.json())["code"] == "PAYMENT_PROVIDER_UNKNOWN"
        rows = await _order_rows(migrated_url, installation_id)
        assert len(rows) == 1 and rows[0]["provider_create_state"] == "unknown"
        second = await client.post(ORDERS_PATH, headers=_auth(token), json=body)
        assert second.status == 503 and (await second.json())["code"] == "PAYMENT_PROVIDER_UNKNOWN"
        assert provider.calls == 1
        assert len(await _order_rows(migrated_url, installation_id)) == 1
    finally:
        await database.close()
        await client.close()


class _RecordingSession:
    def __init__(self):
        self.posts: list[tuple] = []

    def post(self, url, json=None, headers=None):
        self.posts.append((url, json, headers))
        return _FakeResp(200, {"transactionId": "tx-1", "redirect": "https://pay/1"})

    def get(self, url, headers=None):
        return _FakeResp(200, {"transactionId": "tx-1", "status": "CONFIRMED", "paymentDetails": {"amount": 200, "currency": "RUB"}})


async def test_platega_http_create_contract_matches_donor(migrated_url, settings_factory):
    from terlimo_backend.payments import PlategaHttpProvider

    single = PlategaHttpProvider(_settings(settings_factory, migrated_url, platega_methods="sbp"))
    recorder = _RecordingSession()
    single._session = recorder
    await single.create_payment(amount=200, currency="RUB", months=1, order_ref="o1", description="d")
    url, body, _headers = recorder.posts[-1]
    assert url.endswith("/transaction/process")
    assert body["paymentMethod"] == 2 and type(body["paymentMethod"]) is int
    assert isinstance(body["payload"], str) and "order_ref" in body["payload"]

    configured = PlategaHttpProvider(_settings(
        settings_factory, migrated_url, platega_methods="international",
        platega_method_ids="sbp:2,international:11,crypto:13",
    ))
    recorder3 = _RecordingSession()
    configured._session = recorder3
    await configured.create_payment(amount=200, currency="RUB", months=1, order_ref="o3", description="d")
    _u3, body3, _h3 = recorder3.posts[-1]
    assert body3["paymentMethod"] == 11 and type(body3["paymentMethod"]) is int

    multi = PlategaHttpProvider(_settings(settings_factory, migrated_url, platega_methods="sbp,international,crypto"))
    recorder2 = _RecordingSession()
    multi._session = recorder2
    await multi.create_payment(amount=200, currency="RUB", months=1, order_ref="o2", description="d")
    url2, body2, _h2 = recorder2.posts[-1]
    assert url2.endswith("/v2/transaction/process")
    assert "paymentMethod" not in body2 and isinstance(body2["payload"], str)


async def test_webhook_rejects_fractional_amount(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555002002)
        created = await _create_order(client, token, 1)
        order_id = (await created.json())["payment"]["order_id"]
        fractional = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "amount": 200.5, "currency": "RUB", "status": "CONFIRMED"},
        )
        assert (await fractional.json())["result"] == "amount_mismatch"
        assert await _paid_count(migrated_url, installation_id) == 0
    finally:
        await database.close()
        await client.close()


async def test_reconcile_requires_provider_identity_amount_currency(migrated_url, settings_factory):
    client, settings, database = await _app(settings_factory, migrated_url)
    from terlimo_backend.payments import reconcile_payments

    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555002003)
        created = await _create_order(client, token, 1)
        order_id = (await created.json())["payment"]["order_id"]
        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                "UPDATE payment_orders SET status='succeeded', paid_at=now() WHERE id=$1", order_id
            )
        finally:
            await connection.close()
        provider = FakePlategaProvider()
        provider.status_view = {"amount": 999, "currency": "RUB", "status": "CONFIRMED"}
        connection = await _connect(migrated_url)
        try:
            result = await reconcile_payments(connection, settings, provider, limit=10)
        finally:
            await connection.close()
        assert result["applied"] == 0 and await _paid_count(migrated_url, installation_id) == 0
        provider.status_view = {"amount": 200, "currency": "RUB", "status": "CONFIRMED"}
        connection = await _connect(migrated_url)
        try:
            result = await reconcile_payments(connection, settings, provider, limit=10)
        finally:
            await connection.close()
        assert result["applied"] == 1 and await _paid_count(migrated_url, installation_id) == 1
    finally:
        await database.close()
        await client.close()


async def test_needs_grant_recovery_without_client_sync(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id=GATEWAY_KEY)
    await fake.start()
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    from terlimo_backend.payments import reconcile_payments

    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555002004)
        created = await _create_order(client, token, 1)
        order_id = (await created.json())["payment"]["order_id"]
        webhook = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "amount": 200, "currency": "RUB", "status": "CONFIRMED"},
        )
        assert (await webhook.json())["result"] == "succeeded"
        # No gateway yet: paid right exists, grant is durably pending, nothing enqueued.
        connection = await _connect(migrated_url)
        try:
            row = await connection.fetchrow("SELECT applied_entitlement_id, needs_grant FROM payment_orders WHERE id=$1", order_id)
            outbox0 = await connection.fetchval("SELECT count(*) FROM outbox_operations WHERE operation_type='gateway.apply_grant'")
        finally:
            await connection.close()
        assert row["applied_entitlement_id"] is not None and row["needs_grant"] is True and outbox0 == 0

        # Gateway becomes ready; reconciliation enqueues the grant with no client access.sync.
        gateway_id, endpoints = await _seed_gateway(migrated_url, key=GATEWAY_KEY)
        endpoints["admin_socket"] = fake.socket_path
        connection = await _connect(migrated_url)
        try:
            await connection.execute("UPDATE gateways SET endpoints=$2::jsonb WHERE id=$1", gateway_id, json.dumps(endpoints))
        finally:
            await connection.close()
        connection = await _connect(migrated_url)
        try:
            result = await reconcile_payments(connection, settings, FakePlategaProvider(), limit=10)
        finally:
            await connection.close()
        assert result["status_changed"] == 1
        connection = await _connect(migrated_url)
        try:
            needs = await connection.fetchval("SELECT needs_grant FROM payment_orders WHERE id=$1", order_id)
            enqueued = await connection.fetchval("SELECT count(*) FROM outbox_operations WHERE operation_type='gateway.apply_grant'")
        finally:
            await connection.close()
        assert needs is False and enqueued == 1

        handlers = GatewayControlHandlers(
            settings,
            client_factory=lambda _key, route: GatewayAdminClient(
                socket_path=str(route["admin_socket"]), main_password="fixture-main", timeout_seconds=5
            ),
        ).as_handlers()
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="s4-recover")
        await database.ensure_ready()
        await worker.drain()
        connection = await _connect(migrated_url)
        try:
            state = await connection.fetchval("SELECT state FROM grants")
            applied = await connection.fetchval("SELECT applied_generation FROM grants")
        finally:
            await connection.close()
        assert state == "applied" and int(applied) == 1
    finally:
        await database.close()
        await client.close()
        await fake.stop()


class ServerErrorProvider(FakePlategaProvider):
    def __init__(self):
        super().__init__()
        self.calls = 0

    async def create_payment(self, **kwargs):
        from terlimo_backend.payments import ProviderUnknown

        self.calls += 1
        raise ProviderUnknown()


async def test_accepted_then_500_marks_unknown_and_blocks_new_key(migrated_url, settings_factory):
    provider = ServerErrorProvider()
    client, _settings_obj, database = await _app(settings_factory, migrated_url, provider=provider)
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555003000)
        first = await client.post(
            ORDERS_PATH, headers=_auth(token), json={"months": 1, "idempotency_key": "idem-500"}
        )
        assert first.status == 503 and (await first.json())["code"] == "PAYMENT_PROVIDER_UNKNOWN"
        rows = await _order_rows(migrated_url, installation_id)
        assert len(rows) == 1 and rows[0]["provider_create_state"] == "unknown"
        # Same key repeat: no second provider create.
        repeat = await client.post(
            ORDERS_PATH, headers=_auth(token), json={"months": 1, "idempotency_key": "idem-500"}
        )
        assert repeat.status == 503 and provider.calls == 1
        # A DIFFERENT key from the same installation cannot evade the unresolved unknown order.
        other = await client.post(
            ORDERS_PATH, headers=_auth(token), json={"months": 1, "idempotency_key": "idem-other"}
        )
        assert other.status == 503
        assert (await other.json())["details"]["reason"] == "unresolved_unknown_order"
        assert provider.calls == 1 and len(await _order_rows(migrated_url, installation_id)) == 1
    finally:
        await database.close()
        await client.close()


async def test_reconcile_rejects_fractional_provider_amount(migrated_url, settings_factory):
    client, settings, database = await _app(settings_factory, migrated_url)
    from terlimo_backend.payments import reconcile_payments

    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555003001)
        created = await _create_order(client, token, 1)
        order_id = (await created.json())["payment"]["order_id"]
        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                "UPDATE payment_orders SET status='succeeded', paid_at=now() WHERE id=$1", order_id
            )
        finally:
            await connection.close()
        provider = FakePlategaProvider()
        provider.status_view = {"amount": 200.5, "currency": "RUB", "status": "CONFIRMED"}
        connection = await _connect(migrated_url)
        try:
            result = await reconcile_payments(connection, settings, provider, limit=10)
        finally:
            await connection.close()
        assert result["applied"] == 0 and await _paid_count(migrated_url, installation_id) == 0
    finally:
        await database.close()
        await client.close()


async def test_ensure_grant_failure_keeps_needs_grant_and_recovers(migrated_url, settings_factory, monkeypatch):
    settings = _settings(settings_factory, migrated_url)
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id=GATEWAY_KEY)
    await fake.start()
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    from terlimo_backend.payments import reconcile_payments
    import terlimo_backend.gateway_control as gateway_control

    try:
        gateway_id, endpoints = await _seed_gateway(migrated_url, key=GATEWAY_KEY)
        endpoints["admin_socket"] = fake.socket_path
        connection = await _connect(migrated_url)
        try:
            await connection.execute("UPDATE gateways SET endpoints=$2::jsonb WHERE id=$1", gateway_id, json.dumps(endpoints))
        finally:
            await connection.close()
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555003002)
        created = await _create_order(client, token, 1)
        order_id = (await created.json())["payment"]["order_id"]

        real_ensure = gateway_control.ensure_grant

        async def failing_ensure(*args, **kwargs):
            raise RuntimeError("simulated ensure_grant failure")

        monkeypatch.setattr(gateway_control, "ensure_grant", failing_ensure)
        webhook = await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "amount": 200, "currency": "RUB", "status": "CONFIRMED"},
        )
        assert (await webhook.json())["result"] == "succeeded"
        connection = await _connect(migrated_url)
        try:
            row = await connection.fetchrow("SELECT applied_entitlement_id, needs_grant FROM payment_orders WHERE id=$1", order_id)
            outbox0 = await connection.fetchval("SELECT count(*) FROM outbox_operations WHERE operation_type='gateway.apply_grant'")
        finally:
            await connection.close()
        assert row["applied_entitlement_id"] is not None and row["needs_grant"] is True and outbox0 == 0

        monkeypatch.setattr(gateway_control, "ensure_grant", real_ensure)
        connection = await _connect(migrated_url)
        try:
            result = await reconcile_payments(connection, settings, FakePlategaProvider(), limit=10)
        finally:
            await connection.close()
        assert result["status_changed"] == 1
        handlers = GatewayControlHandlers(
            settings,
            client_factory=lambda _key, route: GatewayAdminClient(
                socket_path=str(route["admin_socket"]), main_password="fixture-main", timeout_seconds=5
            ),
        ).as_handlers()
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="s4-ensure-recover")
        await database.ensure_ready()
        await worker.drain()
        connection = await _connect(migrated_url)
        try:
            state = await connection.fetchval("SELECT state FROM grants")
            applied = await connection.fetchval("SELECT applied_generation FROM grants")
        finally:
            await connection.close()
        assert state == "applied" and int(applied) == 1
    finally:
        await database.close()
        await client.close()
        await fake.stop()


class SlowUnknownProvider(FakePlategaProvider):
    def __init__(self):
        super().__init__()
        self.calls = 0

    async def create_payment(self, **kwargs):
        import asyncio
        from terlimo_backend.payments import ProviderUnknown

        self.calls += 1
        await asyncio.sleep(0.05)
        raise ProviderUnknown()


async def test_concurrent_different_keys_serialize_and_unknown_blocks_new_key(migrated_url, settings_factory):
    import asyncio

    provider = SlowUnknownProvider()
    client, _settings_obj, database = await _app(settings_factory, migrated_url, provider=provider)
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555004000)
        first, second = await asyncio.gather(
            client.post(ORDERS_PATH, headers=_auth(token), json={"months": 1, "idempotency_key": "idem-diff-a"}),
            client.post(ORDERS_PATH, headers=_auth(token), json={"months": 1, "idempotency_key": "idem-diff-b"}),
        )
        # The per-installation lock serializes them; only one provider invoice is ever attempted.
        assert provider.calls == 1
        assert {first.status, second.status} == {503}
        rows = await _order_rows(migrated_url, installation_id)
        assert len(rows) == 1 and rows[0]["provider_create_state"] == "unknown"
    finally:
        await database.close()
        await client.close()


class _PersistenceFailConn:
    """Proxy that fails only the durable provider-result UPDATE (post-provider persistence)."""

    def __init__(self, real):
        self._real = real

    def __getattr__(self, name):
        return getattr(self._real, name)

    async def fetchrow(self, query, *args):
        if "provider_payment_id = $2" in query:
            raise RuntimeError("simulated persistence failure")
        return await self._real.fetchrow(query, *args)


async def test_post_provider_persistence_failure_blocks_new_invoice(migrated_url, settings_factory):
    from terlimo_backend.payments import create_order

    client, settings, database = await _app(settings_factory, migrated_url)
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555004001)
        provider = FakePlategaProvider()
        provider.calls = 0
        original = provider.create_payment

        async def counting(**kwargs):
            provider.calls += 1
            return await original(**kwargs)

        provider.create_payment = counting
        real = await _connect(migrated_url)
        try:
            with pytest.raises(RuntimeError):
                await create_order(
                    _PersistenceFailConn(real), settings, provider,
                    installation_id=installation_id, months=1, idempotency_key="idem-persist-fail",
                )
        finally:
            await real.close()
        rows = await _order_rows(migrated_url, installation_id)
        assert provider.calls == 1 and len(rows) == 1
        assert rows[0]["provider_create_state"] in ("unknown", "in_flight")

        # in_flight OR unknown both reject a new key from the same installation.
        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                "UPDATE payment_orders SET provider_create_state='in_flight' WHERE id=$1", rows[0]["id"]
            )
        finally:
            await connection.close()
        blocked = await client.post(
            ORDERS_PATH, headers=_auth(token), json={"months": 1, "idempotency_key": "idem-after-persist-fail"}
        )
        assert blocked.status == 503
        assert (await blocked.json())["details"]["reason"] == "unresolved_unknown_order"
        assert provider.calls == 1
    finally:
        await database.close()
        await client.close()


async def test_create_requires_client_idempotency_key(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    try:
        pop, token = await _session_token(client)
        missing = await client.post(ORDERS_PATH, headers=_auth(token), json={"months": 1})
        assert missing.status == 400
        assert (await missing.json())["details"]["reason"] == "idempotency_key_required"
        blank = await client.post(ORDERS_PATH, headers=_auth(token), json={"months": 1, "idempotency_key": "  "})
        assert blank.status == 400
    finally:
        await database.close()
        await client.close()


async def test_first_plan_is_exactly_30_days(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555005000)
        created = await _create_order(client, token, 1)
        body = await created.json()
        assert body["payment"]["duration"] == {"unit": "days", "value": 30}
        order_id = body["payment"]["order_id"]
        await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "amount": 200, "currency": "RUB", "status": "CONFIRMED"},
        )
        connection = await _connect(migrated_url)
        try:
            row = await connection.fetchrow("SELECT starts_at, ends_at FROM entitlements WHERE kind='paid'")
        finally:
            await connection.close()
        assert (row["ends_at"] - row["starts_at"]).total_seconds() == 30 * 24 * 3600
    finally:
        await database.close()
        await client.close()


def test_paid_end_calendar_boundaries():
    from terlimo_backend.payments import paid_end

    # First plan is a strict 30*24h interval.
    assert paid_end(datetime(2026, 1, 31, 12), {"unit": "days", "value": 30}) == datetime(2026, 3, 2, 12)
    # Calendar months clamp to end-of-month.
    assert paid_end(datetime(2026, 1, 31, 12), {"unit": "months", "value": 3}) == datetime(2026, 4, 30, 12)
    assert paid_end(datetime(2026, 8, 31, 12), {"unit": "months", "value": 6}) == datetime(2027, 2, 28, 12)
    # Leap year: Feb 29 + 6 calendar months.
    assert paid_end(datetime(2024, 2, 29, 12), {"unit": "months", "value": 6}) == datetime(2024, 8, 29, 12)
    assert paid_end(datetime(2028, 1, 31, 12), {"unit": "months", "value": 3}) == datetime(2028, 4, 30, 12)


async def test_extension_uses_remaining_period_and_is_not_doubled(migrated_url, settings_factory):
    client, settings, database = await _app(settings_factory, migrated_url)
    from terlimo_backend.payments import _add_months

    try:
        pop, token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop.key.fingerprint)
        await _bind(migrated_url, installation_id, 555005001)
        connection = await _connect(migrated_url)
        try:
            account_id = await connection.fetchval(
                "SELECT account_id FROM account_bindings WHERE installation_id=$1 AND status='active'",
                installation_id,
            )
            existing_end = datetime.now(UTC) + timedelta(days=10)
            await connection.execute(
                """
                INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at, device_limit, revision)
                VALUES ($1, 'paid', 'active', now(), $2, 2, 1)
                """,
                account_id,
                existing_end,
            )
        finally:
            await connection.close()
        created = await _create_order(client, token, 3)
        order_id = (await created.json())["payment"]["order_id"]
        await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "amount": 480, "currency": "RUB", "status": "CONFIRMED"},
        )
        connection = await _connect(migrated_url)
        try:
            ends = await connection.fetchval("SELECT ends_at FROM entitlements WHERE kind='paid'")
        finally:
            await connection.close()
        assert ends == _add_months(existing_end, 3)
        # Duplicate callback does not extend a second time.
        await client.post(
            "/public/platega/webhook",
            headers=_webhook_headers(),
            json={"id": f"tx-{order_id}", "eventId": "dup", "amount": 480, "currency": "RUB", "status": "CONFIRMED"},
        )
        connection = await _connect(migrated_url)
        try:
            ends_after = await connection.fetchval("SELECT ends_at FROM entitlements WHERE kind='paid'")
        finally:
            await connection.close()
        assert ends_after == ends
    finally:
        await database.close()
        await client.close()
