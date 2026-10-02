"""S4 payment slice: server-authoritative quotes and Platega-verified paid access.

Design boundaries:
- The unified backend remains the only authority for paid access. A provider payment identity
  is unique and the pending->succeeded transition is claimed exactly once; a duplicate or a
  second event for the same payment never grants a second interval.
- A payment is created against a proven installation from a server-side quote. Before Telegram
  binding it stays parked on the installation; once the installation is bound to an account the
  paid entitlement is applied exactly once to that account and the normal entitlement/grant
  outbox path carries it to the gateway.
- The provider adapter is injectable. The real HTTP adapter mirrors the verified MiniShop
  Platega create/status/callback shape, but is only used when explicitly enabled; tests use a
  local stub. H2H/QR support is reported as a capability and never fabricated.
"""

from __future__ import annotations

import calendar
from contextlib import asynccontextmanager
import hmac
import json
import logging
import math
import uuid
from dataclasses import dataclass
from decimal import Decimal, InvalidOperation
from datetime import UTC, datetime, timedelta
from typing import Any, Protocol

import asyncpg
from aiohttp import ClientError, ClientSession, ClientTimeout, web

from .auth_api import (
    SCHEMA_VERSION,
    ApiError,
    _error_response,
    _json_body,
    random_hex,
    rfc3339,
)
from .config import Settings
from .db import Database
from .session_auth import AuthError, authenticate_session

logger = logging.getLogger(__name__)

PAYMENT_PROVIDER_NAME = "platega"
QUOTE_PATH = "/api/mobile/v1/payments/quote"
ORDERS_PATH = "/api/mobile/v1/payments/orders"
ORDER_PATH = ORDERS_PATH + "/{order_id}"
WEBHOOK_PATH = "/public/platega/webhook"
SUPPORTED_MONTHS = (1, 3, 6)
CONFIRMED_STATUSES = ("CONFIRMED", "CONFIRM", "SUCCESS", "SUCCEEDED")
FAILED_STATUSES = ("CANCELED", "CANCELLED", "CHARGEBACKED", "FAILED")
TERMINAL_ORDER_STATUSES = ("succeeded", "failed", "canceled", "expired")

PAYMENT_PROVIDER_KEY: web.AppKey = web.AppKey("payment_provider", "PaymentProvider | None")

# Donor defaults for method ids; the live merchant mapping may differ (e.g. international 11),
# so the effective map always comes from settings.platega_method_ids.
PLATEGA_METHOD_IDS = {"sbp": 2, "international": 12, "crypto": 13}


def _method_id_map(settings: Settings) -> dict[str, int]:
    mapping: dict[str, int] = {}
    for item in settings.platega_method_ids.split(","):
        name, _, raw = item.partition(":")
        name = name.strip().lower()
        try:
            value = int(raw.strip())
        except ValueError:
            continue
        if name and value > 0:
            mapping[name] = value
    # Public alias: a bare "card" id entry also provides the provider method "international".
    if "card" in mapping and "international" not in mapping:
        mapping["international"] = mapping["card"]
    return mapping or dict(PLATEGA_METHOD_IDS)


class ProviderUnknown(ApiError):
    """Provider outcome is unknown (timeout / transport). A retry must never create a second
    invoice until reconciliation/identity lookup resolves the first one."""

    def __init__(self) -> None:
        super().__init__("PAYMENT_PROVIDER_UNKNOWN", http=503, retryable=False)


class ProviderMethodUnavailable(ApiError):
    """The selected method is not offered by the configured merchant path (no provider call)."""

    def __init__(self) -> None:
        super().__init__("METHOD_UNAVAILABLE", http=403, retryable=False)


def _add_months(moment: datetime, months: int) -> datetime:
    year = moment.year + (moment.month - 1 + months) // 12
    month = (moment.month - 1 + months) % 12 + 1
    day = min(moment.day, calendar.monthrange(year, month)[1])
    return moment.replace(year=year, month=month, day=day)


# Approved purchase contract (02_TARGET_ARCHITECTURE_FULL.md / 09_CONTRACTS §6 / AT16/AT37):
# the first plan is exactly 30 DAYS; the 3- and 6-month plans are CALENDAR months
# (end-of-month/leap handled by _add_months).
DURATION_SPECS: dict[int, dict[str, Any]] = {
    1: {"unit": "days", "value": 30},
    3: {"unit": "months", "value": 3},
    6: {"unit": "months", "value": 6},
}


def _duration_spec(months: int) -> dict[str, Any]:
    return dict(DURATION_SPECS[months])


# Authoritative issuance-time plan identity for the three selling plans. Title is a snapshot:
# it is written into the entitlement at issuance and never re-read from current offers.
PLAN_SNAPSHOTS: dict[int, dict[str, Any]] = {
    1: {"plan_id": "terlimo-30d", "title": "30 дней", "duration_code": "days:30"},
    3: {"plan_id": "terlimo-3m", "title": "3 месяца", "duration_code": "months:3"},
    6: {"plan_id": "terlimo-6m", "title": "6 месяцев", "duration_code": "months:6"},
}


def _plan_from_quote(
    quote: asyncpg.Record, settings: Settings, months: int, method: str | None = None
) -> dict[str, Any]:
    """Plan snapshot sourced ONLY from a durable, installation-bound, unexpired quote that
    exactly matches the order parameters. Raises ORDER_CONFLICT on any mismatch."""
    from .payment_products import product_of
    product = product_of(quote) if quote is not None else None
    if product and product["kind"] == "device_addon":
        if months != 0 or quote["plan_id"] != "terlimo-extra-device" or quote["method"] != method or quote["expires_at"] <= datetime.now(UTC):
            raise ApiError("ORDER_CONFLICT", http=409)
        return {"plan_id":quote["plan_id"], "title":"Дополнительное устройство", "duration_code":quote["duration_code"], "tariff_key":quote["tariff_key"], "origin":"paid"}
    base = PLAN_SNAPSHOTS[months]
    if (
        quote is None
        or int(quote["months"]) != months
        or quote["plan_id"] != base["plan_id"]
        or quote["duration_code"] != base["duration_code"]
        or int(quote["amount_minor"]) != payment_amount(settings, months) * 100
        or quote["currency"] != settings.payment_currency
        or quote["tariff_key"] != settings.payment_tariff_key
        or (method is not None and quote["method"] != method)
    ):
        raise ApiError("ORDER_CONFLICT", http=409, details={"reason": "quote_mismatch"})
    if quote["expires_at"] <= datetime.now(UTC):
        raise ApiError("ORDER_CONFLICT", http=409, details={"reason": "quote_expired"})
    return {
        "plan_id": quote["plan_id"],
        "title": base["title"],
        "duration_code": quote["duration_code"],
        "tariff_key": quote["tariff_key"],
        "origin": "paid",
    }


def paid_end(moment: datetime, duration: dict[str, Any]) -> datetime:
    """Exclusive end for a paid interval from an immutable duration snapshot."""
    unit = duration.get("unit")
    try:
        value = int(duration.get("value"))
    except (TypeError, ValueError):
        raise ApiError("PAYMENT_DURATION_INVALID", http=500)
    if unit == "days" and value > 0:
        return moment + timedelta(days=value)
    if unit == "months" and value > 0:
        return _add_months(moment, value)
    raise ApiError("PAYMENT_DURATION_INVALID", http=500)


@dataclass(frozen=True)
class ProviderPayment:
    provider_payment_id: str
    pay_url: str | None
    qr: str | None
    variant: str | None


class PaymentProvider(Protocol):
    def capabilities(self) -> dict[str, bool]: ...

    async def create_payment(
        self, *, amount: int | Decimal, currency: str, months: int, order_ref: str, description: str,
        method: str | None = None,
    ) -> ProviderPayment: ...

    async def get_status(self, provider_payment_id: str) -> dict[str, Any] | None: ...


class PlategaHttpProvider:
    """Bounded HTTP adapter over the verified MiniShop Platega create/status shape."""

    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._session: ClientSession | None = None

    def capabilities(self) -> dict[str, bool]:
        # Hosted checkout redirect is the verified capability. TERLIMO's own H2H QR is not a
        # verified Platega capability here, so qr stays explicitly unavailable.
        return {"checkout": True, "qr": False}

    async def _client(self) -> ClientSession:
        if self._session is None:
            self._session = ClientSession(timeout=ClientTimeout(total=self._settings.platega_http_timeout_seconds))
        return self._session

    async def close(self) -> None:
        if self._session is not None:
            await self._session.close()
            self._session = None

    def _headers(self) -> dict[str, str]:
        return {
            "X-MerchantId": self._settings.platega_merchant_id,
            "X-Secret": self._settings.platega_secret,
            "Content-Type": "application/json",
        }

    async def create_payment(
        self, *, amount: int | Decimal, currency: str, months: int, order_ref: str, description: str,
        method: str | None = None,
    ) -> ProviderPayment:
        methods = [m.strip().lower() for m in self._settings.platega_methods.split(",") if m.strip()]
        # Alias-aware: a bare "card" env also enables the provider method "international".
        if "card" in methods and "international" not in methods:
            methods.append("international")
        id_map = _method_id_map(self._settings)
        requested = method.strip().lower() if isinstance(method, str) and method.strip() else None
        if requested is not None and (requested not in methods or requested not in id_map):
            # Fail before any provider call: no invoice can exist for an unmapped method.
            raise ProviderMethodUnavailable()
        single_method = requested if requested is not None else (
            methods[0] if len(methods) == 1 and methods[0] in id_map else None
        )
        path = "/transaction/process" if single_method else "/v2/transaction/process"
        body: dict[str, Any] = {
            "paymentDetails": {"amount": float(amount), "currency": currency},
            "description": description,
            "return": self._settings.platega_return_url,
            "failedUrl": self._settings.platega_failed_url,
            # Donor sends payload as a JSON string, not an object.
            "payload": json.dumps({"order_ref": order_ref, "months": months}),
        }
        if single_method:
            body["paymentMethod"] = int(id_map[single_method])
        session = await self._client()
        try:
            async with session.post(
                f"{self._settings.platega_base_url}{path}",
                json=body,
                headers=self._headers(),
            ) as response:
                if response.status < 200 or response.status >= 300:
                    # No documented proof exists that any non-2xx (including 4xx) precedes a
                    # created transaction, so ALL non-2xx outcomes fail closed as unknown.
                    raise ProviderUnknown()
                data = await response.json()
        except ProviderUnknown:
            raise
        except (ClientError, OSError, ValueError) as error:
            raise ProviderUnknown() from error
        if not isinstance(data, dict):
            raise ProviderUnknown()
        provider_id = data.get("transactionId") or data.get("id")
        pay_url = data.get("redirect") or data.get("url") or data.get("paymentUrl")
        if not provider_id:
            raise ProviderUnknown()
        return ProviderPayment(
            provider_payment_id=str(provider_id),
            pay_url=str(pay_url) if pay_url else None,
            qr=None,
            variant=(requested or ",".join(methods)) or None,
        )

    async def get_status(self, provider_payment_id: str) -> dict[str, Any] | None:
        session = await self._client()
        try:
            async with session.get(
                f"{self._settings.platega_base_url}/transaction/{provider_payment_id}",
                headers=self._headers(),
            ) as response:
                if response.status < 200 or response.status >= 300:
                    return None
                data = await response.json()
        except (ClientError, OSError, ValueError):
            return None
        if not isinstance(data, dict):
            return None
        details = data.get("paymentDetails") if isinstance(data.get("paymentDetails"), dict) else {}
        amount = details.get("amount", data.get("amount"))
        currency = details.get("currency", data.get("currency"))
        return {
            "id": data.get("transactionId") or data.get("id"),
            "status": data.get("status"),
            "amount": amount,
            "currency": currency,
        }


def build_provider(settings: Settings) -> PaymentProvider | None:
    if not settings.platega_enabled:
        return None
    if not settings.platega_merchant_id or not settings.platega_secret:
        return None
    return PlategaHttpProvider(settings)


def payment_amount(settings: Settings, months: int) -> int | None:
    if months not in SUPPORTED_MONTHS:
        return None
    price = {1: settings.payment_price_rub_1, 3: settings.payment_price_rub_3, 6: settings.payment_price_rub_6}[months]
    return price if price and price > 0 else None


def _quote_view(settings: Settings, months: int) -> dict[str, Any]:
    amount = payment_amount(settings, months)
    methods = [m.strip() for m in settings.platega_methods.split(",") if m.strip()]
    return {
        "months": months,
        "duration": _duration_spec(months),
        "amount": _rub_json(amount) if amount is not None else None,
        "currency": settings.payment_currency,
        "tariff_key": settings.payment_tariff_key,
        "methods": methods,
    }


def _order_method(order: asyncpg.Record) -> str | None:
    raw = order["quote"]
    if isinstance(raw, str):
        try:
            raw = json.loads(raw)
        except ValueError:
            raw = None
    if isinstance(raw, dict) and isinstance(raw.get("method"), str):
        return raw["method"]
    return None


def _order_duration(order: asyncpg.Record) -> dict[str, Any]:
    raw = order["quote"]
    if isinstance(raw, str):
        try:
            raw = json.loads(raw)
        except ValueError:
            raw = None
    if isinstance(raw, dict) and isinstance(raw.get("duration"), dict):
        return dict(raw["duration"])
    return _duration_spec(int(order["months"]))


def _order_plan(order: asyncpg.Record) -> dict[str, Any] | None:
    raw = order["quote"]
    if isinstance(raw, str):
        try:
            raw = json.loads(raw)
        except ValueError:
            raw = None
    if isinstance(raw, dict) and isinstance(raw.get("plan"), dict):
        plan = raw["plan"]
        if isinstance(plan.get("plan_id"), str):
            return plan
    return None


def _order_view(order: asyncpg.Record) -> dict[str, Any]:
    return {
        "order_id": str(order["id"]),
        "status": order["status"],
        "amount": _rub_json(order["amount"]),
        "currency": order["currency"],
        "months": int(order["months"]),
        "duration": _order_duration(order),
        "tariff_key": order["tariff_key"],
        "provider": order["provider"],
        "method": _order_method(order),
        "pay_url": order["provider_payment_url"],
        "qr": order["provider_qr"],
        "applied": order["applied_entitlement_id"] is not None,
        "paid_at": rfc3339(order["paid_at"]) if order["paid_at"] else None,
    }


def _envelope(payload: dict[str, Any], status: int = 200) -> web.Response:
    body = {
        "request_id": random_hex(16),
        "server_time": rfc3339(datetime.now(UTC)),
        "schema_version": SCHEMA_VERSION,
        "status": "ok",
    }
    body.update(payload)
    return web.json_response(body, status=status)


@asynccontextmanager
async def payment_install_lock(connection: asyncpg.Connection, installation_id: Any):
    """Share the existing session lock with S5 freshness/owner checks.

    PostgreSQL session advisory locks are reentrant on the same connection. S5 holds
    one level across its checks and create_order holds its own level; each releases
    exactly its acquisition even on an error. Other connections wait for both.
    """
    lock_key = f"payment-install:{installation_id}"
    await connection.execute("SELECT pg_advisory_lock(hashtextextended($1, 0))", lock_key)
    try:
        yield
    finally:
        await connection.execute("SELECT pg_advisory_unlock(hashtextextended($1, 0))", lock_key)


async def create_order(
    connection: asyncpg.Connection,
    settings: Settings,
    provider: PaymentProvider | None,
    *,
    installation_id: Any,
    months: int,
    idempotency_key: str | None,
    quote_id: str | None = None,
    method: str | None = None,
    public_method: str | None = None,
    checkout_owner_account_id: Any = None,
    checkout_owner_binding_id: Any = None,
) -> dict[str, Any]:
    from .payment_products import product_of
    commercial_quote = None
    if quote_id is not None:
        try:
            commercial_quote = await connection.fetchrow("SELECT * FROM s5_payment_quotes WHERE id=$1 AND installation_id=$2",uuid.UUID(str(quote_id)),installation_id)
        except (ValueError, AttributeError):
            raise ApiError("BAD_MESSAGE", http=400) from None
    product = product_of(commercial_quote) if commercial_quote else None
    if product and product["owner_account_id"] != (str(checkout_owner_account_id) if checkout_owner_account_id else None):
        raise ApiError("ORDER_CONFLICT",http=409)
    if months not in SUPPORTED_MONTHS and not (months == 0 and product and product["kind"] == "device_addon"):
        raise ApiError("BAD_MESSAGE", http=400, details={"reason": "unsupported_period"})
    amount = Decimal(int(commercial_quote["amount_minor"])) / 100 if product else payment_amount(settings, months)
    if amount is None:
        raise ApiError("PAYMENT_PRICE_UNAVAILABLE", http=503, retryable=False)
    requested_method = method.strip().lower() if isinstance(method, str) and method.strip() else None
    # The PUBLIC method (e.g. the app's "card") may alias a provider method ("international").
    # Validation/provider selection use the provider name; the order/quote snapshot keeps the
    # public name so the client contract and replay comparison stay stable.
    requested_public = (
        public_method.strip().lower() if isinstance(public_method, str) and public_method.strip()
        else requested_method
    )
    # A client-supplied nonempty idempotency key is mandatory: a server-generated random key
    # makes a client retry non-idempotent.
    key = (idempotency_key or "").strip()
    if not key or len(key) > 128:
        raise ApiError("BAD_MESSAGE", http=400, details={"reason": "idempotency_key_required"})
    quote = _quote_view(settings, months) if months else {"months":0,"amount":_rub_json(amount),"currency":settings.payment_currency,"tariff_key":settings.payment_tariff_key,"duration":{"unit":"until","value":product["valid_until"]}}
    if product:
        quote["product"] = product
    if requested_public is not None:
        # Server-side method snapshot for this order; never a client-supplied dict.
        quote["method"] = requested_public
    source_quote_id = None
    if quote_id is not None:
        try:
            source_quote_id = uuid.UUID(str(quote_id))
        except (ValueError, AttributeError):
            raise ApiError("BAD_MESSAGE", http=400, details={"reason": "bad_quote_id"}) from None
    # Durable replay and all new-create decisions share the installation lock.
    async with payment_install_lock(connection, installation_id):
        existing = await connection.fetchrow(
            "SELECT * FROM payment_orders WHERE idempotency_key = $1", key
        )
        if existing is not None:
            # Durable replay first: an exact same-key retry returns the prior order without any
            # quote freshness/proof check and without a provider call. Conflict is proven before
            # any mutation, so a mismatching replay changes nothing.
            if existing["installation_id"] != installation_id:
                raise ApiError("ORDER_CONFLICT", http=409, details={"reason": "installation_mismatch"})
            if (
                int(existing["months"]) != months
                or _exact_amount(existing["amount"]) != _exact_amount(amount)
                or existing["currency"] != settings.payment_currency
                or existing["tariff_key"] != settings.payment_tariff_key
            ):
                raise ApiError("ORDER_CONFLICT", http=409, details={"reason": "idempotency_key_reused"})
            existing_quote = str(existing["source_quote_id"]) if existing["source_quote_id"] is not None else None
            requested_quote = str(source_quote_id) if source_quote_id is not None else None
            if existing_quote != requested_quote:
                raise ApiError("ORDER_CONFLICT", http=409, details={"reason": "quote_changed"})
            if _order_method(existing) != requested_public:
                raise ApiError("ORDER_CONFLICT", http=409, details={"reason": "method_changed"})
            # Immutable checkout owner is compared, never rewritten: the credit-target
            # account_id/binding_id may legitimately change when the installation is rebound
            # before payment, so it is not used here. Historical rows (NULL owner) are not
            # backfilled on replay; they simply carry no checkout ownership.
            if (
                checkout_owner_account_id is not None
                and existing["checkout_owner_account_id"] is not None
                and existing["checkout_owner_account_id"] != checkout_owner_account_id
            ):
                raise ApiError("ORDER_CONFLICT", http=409, details={"reason": "checkout_owner_mismatch"})
            if existing["provider_create_state"] == "unknown":
                # A previous provider create timed out: the outcome is unknown and no second
                # invoice may be created until identity reconciliation resolves it.
                raise ApiError("PAYMENT_PROVIDER_UNKNOWN", http=503, retryable=False)
            if existing["provider_payment_id"] is not None and existing["provider_create_state"] == "created":
                return {"payment": _order_view(existing)}
            if existing["provider_create_state"] == "in_flight":
                raise ApiError("PAYMENT_PROVIDER_UNKNOWN", http=503, retryable=False)
            order_id = existing["id"]
        else:
            # Bounded evasion guard: while ANY order for this installation has an unresolved
            # provider-create (unknown outcome), a different idempotency key may not start a new
            # invoice. Recovery is reconciliation/operator identity lookup, not client retry.
            unresolved = await connection.fetchval(
                """
                SELECT 1 FROM payment_orders
                WHERE installation_id = $1 AND provider_create_state IN ('in_flight', 'unknown')
                LIMIT 1
                """,
                installation_id,
            )
            if unresolved is not None:
                raise ApiError(
                    "PAYMENT_PROVIDER_UNKNOWN",
                    http=503,
                    retryable=False,
                    details={"reason": "unresolved_unknown_order"},
                )
            if source_quote_id is not None:
                # Server-side proof of the selected plan for a genuinely new order: the durable
                # quote must belong to this installation and match parameters exactly; never an
                # arbitrary caller-supplied dict.
                proof = await connection.fetchrow(
                    "SELECT * FROM s5_payment_quotes WHERE id = $1 AND installation_id = $2 FOR UPDATE",
                    source_quote_id,
                    installation_id,
                )
                quote["plan"] = _plan_from_quote(proof, settings, months, requested_public)
                if product:
                    from .payment_products import validate_credit_target
                    binding = await connection.fetchrow("SELECT id,account_id FROM account_bindings WHERE installation_id=$1 AND status='active'",installation_id)
                    _, review = await validate_credit_target(connection,{"quote":quote,"checkout_owner_binding_id":checkout_owner_binding_id},binding,datetime.now(UTC))
                    if review:
                        raise ApiError("PAYMENT_STATE_INVALID",http=409)
        if requested_method is not None:
            # Alias-aware configured set, identical to the plans/quote path: a bare "card" env also
            # enables its provider method "international".
            configured = {m.strip().lower() for m in settings.platega_methods.split(",") if m.strip()}
            if "card" in configured:
                configured.add("international")
            if requested_method not in configured:
                # Fail before any ledger write or provider call: an unmapped method cannot invoice.
                raise ProviderMethodUnavailable()
        if provider is None:
            raise ApiError("PAYMENT_PROVIDER_UNAVAILABLE", http=503, retryable=True)
        if existing is None:
            try:
                order_id = await connection.fetchval(
                    """
                    INSERT INTO payment_orders
                        (installation_id, provider, idempotency_key, quote, amount, currency, months, tariff_key,
                         provider_create_state, source_quote_id,
                         checkout_owner_account_id, checkout_owner_binding_id)
                    VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8, 'in_flight', $9, $10, $11)
                    RETURNING id
                    """,
                    installation_id,
                    PAYMENT_PROVIDER_NAME,
                    key,
                    json.dumps(quote),
                    Decimal(str(amount)),
                    settings.payment_currency,
                    months,
                    settings.payment_tariff_key,
                    source_quote_id,
                    checkout_owner_account_id,
                    checkout_owner_binding_id,
                )
            except asyncpg.UniqueViolationError:
                # One quote funds at most one order; no provider create happened for this attempt.
                raise ApiError("ORDER_CONFLICT", http=409, details={"reason": "quote_already_used"}) from None
        if existing is not None:
            await connection.execute(
                "UPDATE payment_orders SET provider_create_state = 'in_flight', updated_at = now() WHERE id = $1",
                order_id,
            )
        try:
            payment = await provider.create_payment(
                amount=amount,
                currency=settings.payment_currency,
                months=months,
                order_ref=str(order_id),
                description="TERLIMO +1 device" if months == 0 else f"TERLIMO {months}m",
                method=requested_method,
            )
        except Exception:
            # Any failure (non-2xx/timeout/malformed body/DB error after the provider call) may
            # follow a created transaction: fail closed as unknown, never invite a duplicate
            # invoice. The only safe recovery is operator/merchant identity reconciliation.
            await connection.execute(
                "UPDATE payment_orders SET provider_create_state = 'unknown', updated_at = now() WHERE id = $1",
                order_id,
            )
            raise
        order = await connection.fetchrow(
            """
            UPDATE payment_orders
            SET provider_payment_id = $2, provider_payment_url = $3, provider_qr = $4,
                provider_variant = $5, provider_create_state = 'created',
                provider_created_at = now(), status = 'pending', updated_at = now()
            WHERE id = $1
            RETURNING *
            """,
            order_id,
            payment.provider_payment_id,
            payment.pay_url,
            payment.qr,
            payment.variant,
        )
        return {"payment": _order_view(order)}



async def get_order(
    connection: asyncpg.Connection, *, installation_id: Any, order_id: str
) -> dict[str, Any]:
    order = await connection.fetchrow(
        "SELECT * FROM payment_orders WHERE id = $1 AND installation_id = $2",
        order_id,
        installation_id,
    )
    if order is None:
        raise ApiError("ORDER_NOT_FOUND", http=404)
    return {"payment": _order_view(order)}


async def apply_paid_entitlement(
    connection: asyncpg.Connection, settings: Settings, *, order_id: Any
) -> Any:
    """Apply a succeeded order to the (now) bound account exactly once.

    Safe to call repeatedly: the order row is locked and applied_entitlement_id is the guard.
    """
    async with connection.transaction():
        order = await connection.fetchrow(
            "SELECT * FROM payment_orders WHERE id = $1 FOR UPDATE", order_id
        )
        if order is None or order["status"] != "succeeded":
            return None
        if order["applied_entitlement_id"] is not None:
            return order["applied_entitlement_id"]
        if order["credit_review_reason"] is not None:
            return None
        binding = await connection.fetchrow(
            "SELECT id, account_id FROM account_bindings WHERE installation_id = $1 AND status = 'active'",
            order["installation_id"],
        )
        from .payment_products import order_product, validate_credit_target, paid_limit, renew_slots, slots
        product = order_product(order)
        if binding is None:
            if product:
                await connection.execute("UPDATE payment_orders SET credit_review_reason='owner_unbound' WHERE id=$1",order_id)
            return None
        account_id = binding["account_id"]
        from .mobile_account import BASE_LIMIT

        # Different orders for the same account lock different order rows. Serialize the
        # entitlement read/create/extension so two first payments cannot each mint revision 1
        # and two renewals cannot both extend from the same stale deadline.
        await connection.execute(
            "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
            f"paid-account:{account_id}",
        )

        now = datetime.now(UTC)
        target, review = await validate_credit_target(connection, order, binding, now)
        if review:
            await connection.execute("UPDATE payment_orders SET credit_review_reason=$2 WHERE id=$1",order_id,review)
            return None
        if product and product["kind"] == "device_addon":
            await connection.execute("INSERT INTO paid_extra_slots(entitlement_id,source_order_id,expires_at) VALUES ($1,$2,$3)",target["id"],order_id,target["ends_at"])
            credited = await connection.fetchrow("UPDATE entitlements SET device_limit=$2,revision=revision+1 WHERE id=$1 RETURNING id,revision",target["id"],await paid_limit(connection,target,now))
            receipt = {"valid_from":rfc3339(now),"valid_until":rfc3339(target["ends_at"]),"device_limit":await paid_limit(connection,target,now),"current_device_limit":await paid_limit(connection,target,now)}
            await connection.execute("UPDATE payment_orders SET applied_entitlement_id=$2,account_id=$3,binding_id=$4,credited_entitlement_revision=$5,credited_product=$6::jsonb WHERE id=$1",order_id,credited["id"],account_id,binding["id"],credited["revision"],json.dumps(receipt))
            enqueued = await _enqueue_paid_grant(connection,settings,order_id=order_id)
            await connection.execute("UPDATE payment_orders SET needs_grant=$2 WHERE id=$1",order_id,not enqueued)
            return credited["id"]
        existing = target or await connection.fetchrow(
            """
            SELECT * FROM entitlements
            WHERE account_id = $1 AND kind = 'paid' AND status = 'active'
            ORDER BY ends_at DESC NULLS LAST, created_at DESC
            LIMIT 1
            """,
            account_id,
        )
        now = datetime.now(UTC)
        duration = _order_duration(order)
        credit_start = now
        credit_end = None
        if existing is not None:
            if existing["ends_at"] is None:
                credited = await connection.fetchrow(
                    "UPDATE entitlements SET revision = revision + 1 WHERE id = $1 RETURNING id, revision",
                    existing["id"],
                )
            else:
                credit_start = max(existing["ends_at"], now)
                new_end = paid_end(credit_start, duration)
                credit_end = new_end
                await renew_slots(connection,order,existing["id"],new_end)
                new_limit = BASE_LIMIT + len(await slots(connection,existing["id"],now))
                plan = _order_plan(order)
                # A NEW credited order replaces the active plan snapshot in the same revision; a
                # replay of the same payment never reaches here (applied guard above), and an
                # order without a proven selected plan must not erase a valid snapshot.
                credited = await connection.fetchrow(
                    """
                    UPDATE entitlements
                    SET ends_at = $2, revision = revision + 1,
                        source_plan = $3::jsonb, device_limit = $4, paid_base_device_limit = 2
                    WHERE id = $1
                    RETURNING id, revision
                    """,
                    existing["id"],
                    new_end,
                    plan,
                    new_limit,
                )
        else:
            plan = _order_plan(order)
            credited = await connection.fetchrow(
                """
                INSERT INTO entitlements
                    (account_id, kind, status, starts_at, ends_at, device_limit, revision,
                     source_invoice_id, source_plan)
                VALUES ($1, 'paid', 'active', $2, $3, $4, 1, $5, $6::jsonb)
                RETURNING id, revision
                """,
                account_id,
                now,
                paid_end(now, duration),
                BASE_LIMIT,
                f"platega:{order['provider_payment_id']}",
                plan,
            )
        entitlement_id = credited["id"]
        if existing is None:
            credit_end = paid_end(now,duration)
        final_entitlement = await connection.fetchrow("SELECT * FROM entitlements WHERE id=$1",entitlement_id)
        receipt = {"valid_from":rfc3339(credit_start),"valid_until":rfc3339(credit_end) if credit_end else None,"device_limit":2+len(product["renew_extra_slot_ids"]) if product else 2,"current_device_limit":await paid_limit(connection,final_entitlement,now)}
        await connection.execute(
            """
            UPDATE payment_orders
            SET applied_entitlement_id = $2, account_id = $3, binding_id = $4,
                credited_entitlement_revision = $5, credited_product = $6::jsonb, updated_at = now()
            WHERE id = $1
            """,
            order_id,
            entitlement_id,
            account_id,
            binding["id"],
            int(credited["revision"]),
            json.dumps(receipt),
        )
        # Automatic paid access: enqueue the normal grant/outbox path for the bound account so a
        # paid webhook does not depend on a later client access.sync. Gateway unavailability is
        # handled by the existing outbox retries.

        # Automatic paid access via the normal grant/outbox path; if no ready gateway or the
        # enqueue fails, needs_grant stays true and reconciliation retries without any client
        # access.sync.
        enqueued = await _enqueue_paid_grant(connection, settings, order_id=order_id)
        await connection.execute(
            "UPDATE payment_orders SET needs_grant = $2, updated_at = now() WHERE id = $1",
            order_id,
            not enqueued,
        )
        return entitlement_id


async def apply_parked_payments(
    connection: asyncpg.Connection, settings: Settings, *, installation_id: Any
) -> int:
    """Apply every succeeded-but-unapplied order for an installation (called after binding)."""
    rows = await connection.fetch(
        """
        SELECT id FROM payment_orders
        WHERE installation_id = $1 AND status = 'succeeded' AND applied_entitlement_id IS NULL
        ORDER BY paid_at, id
        """,
        installation_id,
    )
    applied = 0
    for row in rows:
        if await apply_paid_entitlement(connection, settings, order_id=row["id"]) is not None:
            applied += 1
    return applied


def _exact_amount(value: Any) -> int | None:
    """Exact RUB minor units; reject bool, nonfinite and fractions of a kopeck."""
    if type(value) is bool or not isinstance(value,(int,float,Decimal)):
        return None
    try:
        amount = Decimal(str(value))
        minor = amount*100
        if not minor.is_finite() or minor <= 0 or minor != minor.to_integral_value():
            return None
        return int(minor)
    except (InvalidOperation, ValueError):
        return None


def _rub_json(value):
    amount = Decimal(str(value))
    return int(amount) if amount == amount.to_integral_value() else float(amount)


def _event_status(raw: str | None) -> str:
    status = (raw or "").upper()
    if status in CONFIRMED_STATUSES:
        return "succeeded"
    if status in FAILED_STATUSES:
        return "canceled"
    return "pending"


async def record_webhook(
    connection: asyncpg.Connection,
    settings: Settings,
    *,
    merchant: str,
    secret: str,
    body: dict[str, Any],
) -> dict[str, Any]:
    if not settings.platega_merchant_id or not settings.platega_secret:
        raise ApiError("PAYMENT_PROVIDER_UNAVAILABLE", http=503, retryable=True)
    if not hmac.compare_digest(merchant or "", settings.platega_merchant_id) or not hmac.compare_digest(
        secret or "", settings.platega_secret
    ):
        raise ApiError("PAYMENT_AUTH", http=403)
    provider_payment_id = body.get("id") or body.get("transactionId")
    raw_status = body.get("status")
    if provider_payment_id is None or raw_status is None:
        raise ApiError("BAD_MESSAGE", http=400)
    order = await connection.fetchrow(
        "SELECT * FROM payment_orders WHERE provider = $1 AND provider_payment_id = $2",
        PAYMENT_PROVIDER_NAME,
        str(provider_payment_id),
    )
    if order is None:
        return {"result": "ignored"}
    event_id = body.get("eventId") or body.get("event_id") or f"{provider_payment_id}:{raw_status}"
    amount = body.get("amount")
    currency = body.get("currency")
    kind = _event_status(str(raw_status))
    await connection.execute(
        """
        INSERT INTO payment_events (order_id, provider, provider_event_id, kind, amount, currency, raw_status)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (provider, provider_event_id) WHERE provider_event_id IS NOT NULL DO NOTHING
        """,
        order["id"],
        PAYMENT_PROVIDER_NAME,
        str(event_id),
        kind,
        Decimal(_exact_amount(amount))/100 if _exact_amount(amount) is not None else None,
        str(currency) if currency is not None else None,
        str(raw_status),
    )
    if kind == "pending":
        return {"result": "pending"}
    if kind == "canceled":
        await connection.execute(
            "UPDATE payment_orders SET status = 'canceled', updated_at = now() WHERE id = $1 AND status = 'pending'",
            order["id"],
        )
        return {"result": "canceled"}
    # success: amount and currency are REQUIRED and must match the immutable snapshot exactly
    # before any paid right. Fractions of a kopeck and non-finite amounts are never accepted.
    if type(amount) is bool or not isinstance(amount, (int, float)):
        return {"result": "amount_missing"}
    paid_minor = _exact_amount(amount)
    if paid_minor is None:
        return {"result": "amount_mismatch"}
    if not isinstance(currency, str) or not currency:
        return {"result": "currency_missing"}
    if paid_minor != _exact_amount(order["amount"]):
        return {"result": "amount_mismatch"}
    if currency.upper() != order["currency"].upper():
        return {"result": "currency_mismatch"}
    async with connection.transaction():
        claimed = await connection.fetchval(
            """
            UPDATE payment_orders
            SET status = 'succeeded', paid_at = now(), updated_at = now()
            WHERE id = $1 AND status <> 'succeeded'
            RETURNING id
            """,
            order["id"],
        )
        if claimed is None:
            return {"result": "duplicate"}
        # Status transition and paid-entitlement application commit together, so a crash can
        # never leave a succeeded-but-unapplied order that later events silently skip.
        await apply_paid_entitlement(connection, settings, order_id=order["id"])
    return {"result": "succeeded"}


async def reconcile_payments(
    connection: asyncpg.Connection,
    settings: Settings,
    provider: PaymentProvider | None,
    *,
    limit: int = 50,
) -> dict[str, int]:
    """Bounded provider-status reconciliation.

    Covers two cases with the provider's own status call (OWNER_EXECUTION_RULES):
    - pending orders with a provider payment id that actually completed;
    - succeeded-but-unapplied orders (crash between claim and entitlement application).
    """
    if provider is None:
        return {"checked": 0, "applied": 0, "status_changed": 0}
    rows = await connection.fetch(
        """
        SELECT * FROM payment_orders
        WHERE credit_review_reason IS NULL AND ((status = 'pending' AND provider_payment_id IS NOT NULL)
           OR (status = 'succeeded' AND (applied_entitlement_id IS NULL OR needs_grant)))
        ORDER BY updated_at, id
        LIMIT $1
        """,
        limit,
    )
    applied = 0
    status_changed = 0
    for order in rows:
        info: dict[str, Any] | None = None
        if order["status"] == "pending" or (
            order["status"] == "succeeded" and order["applied_entitlement_id"] is None
        ):
            try:
                info = await provider.get_status(order["provider_payment_id"])
            except Exception:  # noqa: BLE001 - a provider failure must never break the sweep
                info = None
            if isinstance(info, dict):
                # Trust only a status that names exactly this payment identity and whose
                # immutable amount/currency match the order; anything else is never a grant.
                if str(info.get("id") or "") != str(order["provider_payment_id"]):
                    continue
                status_amount = _exact_amount(info.get("amount"))
                if status_amount is None or status_amount != _exact_amount(order["amount"]):
                    continue
                if not isinstance(info.get("currency"), str) or info["currency"].upper() != order["currency"].upper():
                    continue
            elif order["status"] == "pending":
                continue
        if order["status"] == "succeeded" and order["applied_entitlement_id"] is None:
            if await apply_paid_entitlement(connection, settings, order_id=order["id"]) is not None:
                applied += 1
            continue
        if order["status"] == "succeeded" and order["needs_grant"]:
            if await _enqueue_paid_grant(connection, settings, order_id=order["id"]):
                await connection.execute(
                    "UPDATE payment_orders SET needs_grant = false, updated_at = now() WHERE id = $1",
                    order["id"],
                )
                status_changed += 1
            continue
        kind = _event_status(str(info.get("status") if info else None))
        if kind == "succeeded":
            async with connection.transaction():
                claimed = await connection.fetchval(
                    """
                    UPDATE payment_orders
                    SET status = 'succeeded', paid_at = now(), updated_at = now()
                    WHERE id = $1 AND status <> 'succeeded'
                    RETURNING id
                    """,
                    order["id"],
                )
                if claimed is not None:
                    await apply_paid_entitlement(connection, settings, order_id=order["id"])
                    status_changed += 1
        elif kind == "canceled":
            await connection.execute(
                "UPDATE payment_orders SET status = 'canceled', updated_at = now() WHERE id = $1 AND status = 'pending'",
                order["id"],
            )
            status_changed += 1
    return {"checked": len(rows), "applied": applied, "status_changed": status_changed}


async def _enqueue_paid_grant(
    connection: asyncpg.Connection, settings: Settings, *, order_id: Any
) -> bool:
    """Durable grant/outbox enqueue for an applied paid order; returns True when enqueued."""
    order = await connection.fetchrow(
        "SELECT installation_id, applied_entitlement_id FROM payment_orders WHERE id = $1", order_id
    )
    if order is None or order["applied_entitlement_id"] is None:
        return False
    binding = await connection.fetchrow(
        "SELECT id FROM account_bindings WHERE installation_id = $1 AND status = 'active'",
        order["installation_id"],
    )
    if binding is None:
        return False
    environment = await connection.fetchval(
        "SELECT environment FROM installations WHERE id = $1", order["installation_id"]
    )
    from .gateway_control import ensure_grant
    from .onboarding_hour import select_gateway

    try:
        gateway = await select_gateway(connection, environment)
    except Exception:  # noqa: BLE001
        return False
    if gateway is None:
        return False
    try:
        # Savepoint: a failing grant/enqueue (including a SQL error) must not poison the
        # enclosing paid-apply transaction; the order stays durably needs_grant.
        async with connection.transaction():
            outcome = await ensure_grant(
                connection,
                binding_id=binding["id"],
                gateway_id=gateway["id"],
                entitlement_id=order["applied_entitlement_id"],
                max_lease_seconds=settings.gateway_max_lease_seconds,
            )
    except Exception:  # noqa: BLE001
        return False
    return outcome in ("enqueued", "unchanged", "pending")


def register_payment_routes(
    app: web.Application, settings: Settings, database: Database, provider: PaymentProvider | None = None
) -> None:
    if provider is None:
        provider = build_provider(settings)
    app[PAYMENT_PROVIDER_KEY] = provider

    def _bearer(request: web.Request) -> str:
        header = request.headers.get("Authorization", "")
        if not header.startswith("Bearer "):
            raise AuthError("SESSION_INVALID", 401)
        return header[len("Bearer ") :].strip()

    async def _quote(request: web.Request) -> web.Response:
        fallback = random_hex(16)
        try:
            token = _bearer(request)
            body = await _json_body(request)
            if not isinstance(body, dict) or set(body) - {"months"} or "months" not in body:
                raise ApiError("BAD_MESSAGE", http=400)
            if type(body["months"]) is not int:
                raise ApiError("BAD_MESSAGE", http=400)
            async with database.acquire() as connection:
                await authenticate_session(connection, settings, token)
            if body["months"] not in SUPPORTED_MONTHS:
                raise ApiError("BAD_MESSAGE", http=400, details={"reason": "unsupported_period"})
            if payment_amount(settings, body["months"]) is None:
                raise ApiError("PAYMENT_PRICE_UNAVAILABLE", http=503)
            return _envelope({"quote": _quote_view(settings, body["months"])})
        except AuthError as error:
            return _error_response(fallback, ApiError(error.code, http=error.http, retryable=error.retryable, request_id=fallback))
        except ApiError as error:
            return _error_response(fallback, error)

    async def _create(request: web.Request) -> web.Response:
        fallback = random_hex(16)
        try:
            token = _bearer(request)
            body = await _json_body(request)
            allowed = {"months", "idempotency_key"}
            if not isinstance(body, dict) or set(body) - allowed or "months" not in body:
                raise ApiError("BAD_MESSAGE", http=400)
            if type(body["months"]) is not int:
                raise ApiError("BAD_MESSAGE", http=400)
            idem = body.get("idempotency_key")
            if not isinstance(idem, str) or not idem.strip():
                raise ApiError("BAD_MESSAGE", http=400, details={"reason": "idempotency_key_required"})
            async with database.acquire() as connection:
                context = await authenticate_session(connection, settings, token)
                result = await create_order(
                    connection, settings, app[PAYMENT_PROVIDER_KEY],
                    installation_id=context.installation_id, months=body["months"], idempotency_key=idem,
                )
            capabilities = (app[PAYMENT_PROVIDER_KEY].capabilities() if app[PAYMENT_PROVIDER_KEY] else {"checkout": False, "qr": False})
            result["capability"] = capabilities
            return _envelope(result)
        except AuthError as error:
            return _error_response(fallback, ApiError(error.code, http=error.http, retryable=error.retryable, request_id=fallback))
        except ApiError as error:
            return _error_response(fallback, error)

    async def _status(request: web.Request) -> web.Response:
        fallback = random_hex(16)
        try:
            token = _bearer(request)
            async with database.acquire() as connection:
                context = await authenticate_session(connection, settings, token)
                result = await get_order(connection, installation_id=context.installation_id, order_id=request.match_info["order_id"])
            return _envelope(result)
        except AuthError as error:
            return _error_response(fallback, ApiError(error.code, http=error.http, retryable=error.retryable, request_id=fallback))
        except ApiError as error:
            return _error_response(fallback, error)

    async def _webhook(request: web.Request) -> web.Response:
        if settings.platega_owner_routing_enabled:
            from .payment_owner_routing import route_callback

            return await route_callback(request, settings, database, record_webhook)
        try:
            body = await _json_body(request)
            if not isinstance(body, dict):
                raise ApiError("BAD_MESSAGE", http=400)
            async with database.acquire() as connection:
                result = await record_webhook(
                    connection, settings,
                    merchant=request.headers.get("X-MerchantId", ""),
                    secret=request.headers.get("X-Secret", ""),
                    body=body,
                )
            return web.json_response({"status": "ok", **result})
        except ApiError as error:
            return _error_response(random_hex(16), error)

    app.router.add_post(QUOTE_PATH, _quote)
    app.router.add_post(ORDERS_PATH, _create)
    app.router.add_get(ORDER_PATH, _status)
    app.router.add_post(WEBHOOK_PATH, _webhook)
