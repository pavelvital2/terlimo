"""Bounded S5 Android payment surface for TEST with the merchant kept offline.

Quotes are durable and owner-bound. Invoice creation deliberately stays unavailable until
the method-specific merchant path and per-order paid credit proof are implemented.
The established S4 order ledger and provider are never bypassed by this surface.
"""

from __future__ import annotations

import hashlib
import json
import re
import uuid
from datetime import UTC, datetime, timedelta
from typing import Any

from aiohttp import web

from .auth_api import ApiError, _error_response, _json_body, random_hex, rfc3339
from .config import Settings
from .db import Database
from .mobile_account import BASE_LIMIT
from .payments import PAYMENT_PROVIDER_KEY, _envelope, _order_view, create_order, payment_amount
from .session_auth import AuthError, authenticate_session

PREFIX = "/api/mobile/v1"
PLANS_PATH = PREFIX + "/plans"
QUOTES_PATH = PREFIX + "/quotes"
PAYMENTS_PATH = PREFIX + "/payments"
PAYMENT_PATH = PAYMENTS_PATH + "/{payment_id}"
QUOTE_LIFETIME = timedelta(minutes=15)
PLAN_MONTHS = {"terlimo-30d": (1, "days:30", "30 дней"),
               "terlimo-3m": (3, "months:3", "3 месяца"),
               "terlimo-6m": (6, "months:6", "6 месяцев")}
# Public S5 methods. "card" is the app's card button; it is an ALIAS of the Platega provider
# method "international" (the donor bot exposes its card/MIR button through that variant). The
# concrete paymentMethod id is NOT hardcoded here: it always comes from settings (runtime override
# wins over the donor default). Only the PUBLIC method name differs; the provider method stays
# "international".
S5_METHODS = ("sbp", "card", "crypto")
S5_METHOD_PROVIDER = {"sbp": "sbp", "card": "international", "crypto": "crypto"}
_KEY = re.compile(r"^[^\r\n]{16,128}$")


def _methods(settings: Settings) -> list[str]:
    configured = {item.strip().lower() for item in settings.platega_methods.split(",")}
    # Normalize the public alias the SAME way as the provider path does: a bare "card" env counts
    # as its provider method "international", so plans -> quote -> create stay consistent.
    normalized = set(configured)
    for public, provider in S5_METHOD_PROVIDER.items():
        if public in configured:
            normalized.add(provider)
    return [method for method in S5_METHODS if S5_METHOD_PROVIDER[method] in normalized]


def _plans(settings: Settings) -> list[dict[str, Any]]:
    if settings.payment_currency != "RUB":
        raise ApiError("SERVICE_UNAVAILABLE", http=503, retryable=True)
    plans = []
    for plan_id, (months, duration_code, title) in PLAN_MONTHS.items():
        amount = payment_amount(settings, months)
        if amount is None:
            continue
        plans.append({
            "plan_id": plan_id, "title": title, "duration_code": duration_code,
            "base_device_limit": BASE_LIMIT,
            "amount": {"amount_minor": amount * 100, "currency": "RUB"},
            "methods": _methods(settings),
        })
    return plans


def _plans_revision(plans: list[dict[str, Any]]) -> str:
    encoded = json.dumps(plans, sort_keys=True, separators=(",", ":")).encode()
    return str(int.from_bytes(hashlib.sha256(encoded).digest()[:8], "big") % (10**18) + 1)


def _bearer(request: web.Request) -> str:
    header = request.headers.get("Authorization", "")
    if not header.startswith("Bearer "):
        raise AuthError("SESSION_INVALID", 401)
    return header[7:].strip()


def _idempotency_key(request: web.Request) -> str:
    key = request.headers.get("Idempotency-Key", "")
    if not _KEY.fullmatch(key):
        raise ApiError("BAD_MESSAGE", http=400, details={"reason": "idempotency_key_required"})
    return key


def _uuid(value: str, *, code: str) -> uuid.UUID:
    try:
        return uuid.UUID(value)
    except (ValueError, AttributeError):
        raise ApiError(code, http=404) from None


def _quote_view(row: Any) -> dict[str, Any]:
    return {
        "quote_id": str(row["id"]),
        "amount": {"amount_minor": int(row["amount_minor"]), "currency": row["currency"]},
        "duration_code": row["duration_code"], "device_limit": BASE_LIMIT,
        "method": row["method"], "expires_at": rfc3339(row["expires_at"]),
    }


def _payment_view(payment: dict[str, Any], *, credited_revision: int | None, needs_grant: bool,
                  require_checkout: bool = False) -> dict[str, Any]:
    status = payment["status"]
    # A canceled order may be shown as failed only if no entitlement was credited.
    if status == "canceled" and payment["applied"]:
        raise ApiError("PAYMENT_STATE_INVALID", http=409)
    status_map = {"pending": "pending", "succeeded": "paid", "failed": "failed",
                  "canceled": "failed", "expired": "expired"}
    if status not in status_map:
        raise ApiError("PAYMENT_STATE_INVALID", http=409)
    if require_checkout and status == "pending" and not payment.get("pay_url"):
        # A freshly created pending order without a provider checkout URL is not a usable
        # success; the durable order keeps the provider identity for reconciliation.
        raise ApiError("PAYMENT_CHECKOUT_UNAVAILABLE", http=503, retryable=False,
                       details={"reason": "no_checkout_url"})
    # Public PaymentResponse is frozen for the strict Android parser (5 keys). The selected
    # method and amount are NOT part of it: they live in the quote response and the internal
    # order snapshot (payment_orders), so the wire shape is unchanged.
    return {
        "payment_id": payment["order_id"],
        "payment_status": status_map[status],
        "checkout_reference": payment["pay_url"],
        # This value was written atomically with this order's own paid entitlement credit.
        "credited_entitlement_revision": str(credited_revision) if credited_revision is not None else None,
        # This is a technical grant/readback state, not the invoice/entitlement state.
        "access_application_state": (
            "retryable_failure" if status == "succeeded" and credited_revision is not None and needs_grant
            else "pending" if status == "succeeded" else "not_requested"
        ),
    }


def register_s5_payment_routes(app: web.Application, settings: Settings, database: Database) -> None:
    async def plans(request: web.Request) -> web.Response:
        fallback = random_hex(16)
        try:
            listed = _plans(settings)
            return _envelope({"plans": listed, "plans_revision": _plans_revision(listed)})
        except ApiError as error:
            return _error_response(fallback, error)

    async def quote(request: web.Request) -> web.Response:
        fallback = random_hex(16)
        try:
            token = _bearer(request)
            key = _idempotency_key(request)
            body = await _json_body(request)
            if set(body) != {"plan_id", "duration_code", "method"} or not all(
                isinstance(body[item], str) for item in body
            ):
                raise ApiError("BAD_MESSAGE", http=400)
            digest = hashlib.sha256(json.dumps(body, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
            async with database.acquire() as connection:
                context = await authenticate_session(connection, settings, token)
                existing = await connection.fetchrow(
                    "SELECT * FROM s5_payment_quotes WHERE installation_id=$1 AND idempotency_key=$2",
                    context.installation_id, key,
                )
                if existing is not None:
                    if existing["request_digest"] != digest:
                        raise ApiError("IDEMPOTENCY_CONFLICT", http=409)
                    return _envelope(_quote_view(existing))
                plan = next((item for item in _plans(settings) if item["plan_id"] == body["plan_id"]), None)
                if plan is None or plan["duration_code"] != body["duration_code"]:
                    raise ApiError("BAD_MESSAGE", http=400)
                if body["method"] not in plan["methods"]:
                    raise ApiError("METHOD_UNAVAILABLE", http=403)
                listed = _plans(settings)
                row = await connection.fetchrow(
                    """
                    INSERT INTO s5_payment_quotes
                        (installation_id,idempotency_key,request_digest,plan_id,months,duration_code,
                         method,amount_minor,currency,tariff_key,plans_revision,expires_at)
                    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
                    ON CONFLICT (installation_id,idempotency_key) DO NOTHING RETURNING *
                    """,
                    context.installation_id, key, digest, body["plan_id"],
                    PLAN_MONTHS[body["plan_id"]][0], body["duration_code"], body["method"],
                    plan["amount"]["amount_minor"], plan["amount"]["currency"],
                    settings.payment_tariff_key, _plans_revision(listed), datetime.now(UTC) + QUOTE_LIFETIME,
                )
                if row is None:
                    row = await connection.fetchrow(
                        "SELECT * FROM s5_payment_quotes WHERE installation_id=$1 AND idempotency_key=$2",
                        context.installation_id, key,
                    )
                    if row["request_digest"] != digest:
                        raise ApiError("IDEMPOTENCY_CONFLICT", http=409)
            return _envelope(_quote_view(row))
        except AuthError as error:
            return _error_response(fallback, ApiError(error.code, http=error.http, retryable=error.retryable))
        except ApiError as error:
            return _error_response(fallback, error)

    async def create(request: web.Request) -> web.Response:
        fallback = random_hex(16)
        try:
            token = _bearer(request)
            key = _idempotency_key(request)
            body = await _json_body(request)
            if set(body) != {"quote_id"} or not isinstance(body["quote_id"], str):
                raise ApiError("BAD_MESSAGE", http=400)
            quote_id = _uuid(body["quote_id"], code="QUOTE_EXPIRED")
            async with database.acquire() as connection:
                context = await authenticate_session(connection, settings, token)
                row = await connection.fetchrow(
                    "SELECT * FROM s5_payment_quotes WHERE id=$1 AND installation_id=$2",
                    quote_id, context.installation_id,
                )
                if row is None:
                    raise ApiError("NOT_FOUND", http=404)
                # Durable replay wins over freshness: an exact same-key retry must reach
                # create_order (which returns its prior order) even after quote expiry. Only a
                # genuinely new order may fail QUOTE_EXPIRED.
                durable = await connection.fetchrow(
                    "SELECT 1 FROM payment_orders WHERE installation_id=$1 AND idempotency_key=$2",
                    context.installation_id,
                    key,
                )
                if row["expires_at"] <= datetime.now(UTC) and durable is None:
                    raise ApiError("QUOTE_EXPIRED", http=409)
                # Minimal quote->order wiring: create_order applies the C4 invariants (durable
                # same-key replay, exact installation-bound quote proof, no second provider
                # create). With the provider disabled create_order fails closed BEFORE any
                # ledger write or provider call (PAYMENT_PROVIDER_UNAVAILABLE).
                if row["method"] not in _methods(settings):
                    raise ApiError("METHOD_UNAVAILABLE", http=403)
                try:
                    result = await create_order(
                        connection,
                        settings,
                        app[PAYMENT_PROVIDER_KEY],
                        installation_id=context.installation_id,
                        months=int(row["months"]),
                        idempotency_key=key,
                        quote_id=str(quote_id),
                        method=S5_METHOD_PROVIDER.get(row["method"], row["method"]),
                        public_method=row["method"],
                        checkout_owner_account_id=context.account_id,
                        checkout_owner_binding_id=context.binding["id"] if context.binding else None,
                    )
                except ApiError as error:
                    if error.code == "PAYMENT_PROVIDER_UNAVAILABLE":
                        # S5 frozen wire code for an unavailable merchant path.
                        raise ApiError("SERVICE_UNAVAILABLE", http=503, retryable=True) from error
                    raise
                full = await connection.fetchrow(
                    "SELECT credited_entitlement_revision, needs_grant FROM payment_orders WHERE id=$1",
                    uuid.UUID(result["payment"]["order_id"]),
                )
            return _envelope(_payment_view(
                result["payment"],
                credited_revision=full["credited_entitlement_revision"] if full else None,
                needs_grant=bool(full["needs_grant"]) if full else False,
                require_checkout=True,
            ))
        except AuthError as error:
            return _error_response(fallback, ApiError(error.code, http=error.http, retryable=error.retryable))
        except ApiError as error:
            return _error_response(fallback, error)

    async def status(request: web.Request) -> web.Response:
        fallback = random_hex(16)
        try:
            token = _bearer(request)
            payment_id = _uuid(request.match_info["payment_id"], code="PAYMENT_NOT_FOUND")
            async with database.acquire() as connection:
                context = await authenticate_session(connection, settings, token)
                order = await connection.fetchrow(
                    "SELECT * FROM payment_orders WHERE id=$1 AND installation_id=$2",
                    payment_id, context.installation_id,
                )
            if order is None:
                raise ApiError("PAYMENT_NOT_FOUND", http=404)
            return _envelope(_payment_view(
                _order_view(order), credited_revision=order["credited_entitlement_revision"],
                needs_grant=order["needs_grant"],
            ))
        except AuthError as error:
            return _error_response(fallback, ApiError(error.code, http=error.http, retryable=error.retryable))
        except ApiError as error:
            return _error_response(fallback, error)

    app.router.add_get(PLANS_PATH, plans)
    app.router.add_post(QUOTES_PATH, quote)
    app.router.add_post(PAYMENTS_PATH, create)
    app.router.add_get(PAYMENT_PATH, status)
