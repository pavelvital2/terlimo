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
from dataclasses import replace
from decimal import Decimal
from datetime import UTC, datetime, timedelta
from typing import Any

from aiohttp import web

from .auth_api import ApiError, _error_response, _json_body, random_hex, rfc3339
from .config import Settings
from .db import Database
from .mobile_account import BASE_LIMIT
from .payments import PAYMENT_PROVIDER_KEY, _envelope, _order_view, create_order, payment_amount
from .s5_checkout_receipts import CheckoutPolicy, issue_checkout_receipt
from .session_auth import AuthError, authenticate_session
from .payments import payment_install_lock
from .payment_products import ADDON_PLAN, quote_product, product_of, public_product, active_paid, slots, order_product

PREFIX = "/api/mobile/v1"
# Explicitly injected policy value; absent means production default disabled (CHECKOUT_POLICY_DENIED).
CHECKOUT_POLICY_KEY = "s5_checkout_policy"
# Bounded opaque path id per the frozen contract (PathId 1..128): transport accepts a bounded
# segment, while only a canonical generated UUID resolves in storage.
_BOUNDED_PATH_ID = re.compile(r"^[A-Za-z0-9._~-]{1,128}$")
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


CONTROL_TARIFF_SUFFIX = ":s5-control-v1"


def _buyer_settings(settings: Settings, account_id: Any) -> Settings:
    # Authentication supplies account_id; no client amount/identity field is accepted.
    if not account_id or not settings.s5_control_account_id:
        return settings
    try:
        allowed = uuid.UUID(settings.s5_control_account_id) == uuid.UUID(str(account_id))
    except (ValueError, AttributeError):
        raise ApiError("SERVICE_UNAVAILABLE", http=503) from None
    if not allowed:
        return settings
    return replace(settings,
        payment_price_rub_1=settings.s5_control_price_rub_1 or settings.payment_price_rub_1,
        payment_price_rub_3=settings.s5_control_price_rub_3 or settings.payment_price_rub_3,
        payment_tariff_key=settings.payment_tariff_key + CONTROL_TARIFF_SUFFIX)


def _quoted_settings(settings: Settings, account_id: Any, row: Any, *, durable: bool = False) -> Settings:
    # Existing server quotes/orders keep their amount across offer activation/removal.
    # Low-value quotes cannot fund a new invoice after rebinding to another account.
    if row["tariff_key"].endswith(CONTROL_TARIFF_SUFFIX) and not durable:
        if _buyer_settings(settings, account_id).payment_tariff_key != row["tariff_key"]:
            raise ApiError("QUOTE_EXPIRED", http=409)
    months = int(row["months"])
    if not months:
        return replace(settings,payment_tariff_key=row["tariff_key"])
    return replace(settings, payment_tariff_key=row["tariff_key"],
                   **{f"payment_price_rub_{months}": Decimal(int(row["amount_minor"])) / 100})


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


def _quote_view(row: Any, *, contract2: bool = False) -> dict[str, Any]:
    product = product_of(row)
    result = {
        "quote_id": str(row["id"]),
        "amount": {"amount_minor": int(row["amount_minor"]), "currency": row["currency"]},
        "duration_code": row["duration_code"], "device_limit": product["device_limit"] if product else BASE_LIMIT,
        "method": row["method"], "expires_at": rfc3339(row["expires_at"]),
    }
    if contract2:
        result["product"] = public_product(product) if product else None
    return result


def _contract2(request):
    return request.query.get("payment_contract") == "2"


def _with_product(payload, order, contract2):
    if contract2:
        product = order_product(order)
        value = public_product(product) if product else None
        payload["product"] = value
        payload["credit_state"] = "needs_review" if order["credit_review_reason"] else "applied" if order["applied_entitlement_id"] else "unapplied"
        payload["credit_review_reason"] = order["credit_review_reason"]
        credited = order["credited_product"]
        payload["credited_product"] = json.loads(credited) if isinstance(credited,str) else credited
    return payload


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
            buyer = settings
            if request.headers.get("Authorization"):
                async with database.acquire() as connection:
                    context = await authenticate_session(connection, settings, _bearer(request))
                    buyer = _buyer_settings(settings, context.account_id)
            listed = _plans(buyer)
            if _contract2(request):
                for item in listed:
                    item["product"] = None
            if _contract2(request) and request.headers.get("Authorization"):
                async with database.acquire() as connection:
                    target = await active_paid(connection,context.account_id)
                    if target:
                        extra_amount, extra_limit, product = await quote_product(connection,buyer,context.account_id,addon=True,selected=[],months=0,base_amount_minor=0)
                        if extra_amount > 0:
                            listed.append({"plan_id":ADDON_PLAN,"title":"Дополнительное устройство","duration_code":"until:"+product["valid_until"],"base_device_limit":BASE_LIMIT,"amount":{"amount_minor":extra_amount,"currency":"RUB"},"methods":_methods(buyer),"product":public_product(product)})
                    for item in listed:
                        if item["plan_id"] != ADDON_PLAN:
                            _, _, product = await quote_product(connection,buyer,context.account_id,addon=False,selected=[],months=PLAN_MONTHS[item["plan_id"]][0],base_amount_minor=item["amount"]["amount_minor"])
                            item["product"] = public_product(product)
            return _envelope({"plans": listed, "plans_revision": _plans_revision(listed)})
        except AuthError as error:
            return _error_response(fallback, ApiError(error.code, http=error.http, retryable=error.retryable))
        except ApiError as error:
            return _error_response(fallback, error)

    async def quote(request: web.Request) -> web.Response:
        fallback = random_hex(16)
        try:
            token = _bearer(request)
            key = _idempotency_key(request)
            body = await _json_body(request)
            expected = {"plan_id", "duration_code", "method"}
            if _contract2(request) and "renew_extra_slot_ids" in body:
                expected.add("renew_extra_slot_ids")
            if set(body) != expected or not all(isinstance(body[item], str) for item in ("plan_id","duration_code","method")) or ("renew_extra_slot_ids" in body and (not isinstance(body["renew_extra_slot_ids"],list) or not all(isinstance(x,str) for x in body["renew_extra_slot_ids"]))):
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
                    if product_of(existing) and not _contract2(request):
                        raise ApiError("BAD_MESSAGE",http=400)
                    return _envelope(_quote_view(existing,contract2=_contract2(request)))
                buyer = _buyer_settings(settings, context.account_id)
                addon = body["plan_id"] == ADDON_PLAN
                product = None
                if addon and not _contract2(request):
                    raise ApiError("BAD_MESSAGE",http=400)
                if addon:
                    if body["method"] not in _methods(buyer):
                        raise ApiError("METHOD_UNAVAILABLE",http=403)
                    amount, device_limit, product = await quote_product(connection,buyer,context.account_id,addon=True,selected=body.get("renew_extra_slot_ids",[]),months=0,base_amount_minor=0)
                    if body["duration_code"] != "until:"+product["valid_until"]:
                        raise ApiError("BAD_MESSAGE",http=400)
                    if amount <= 0:
                        raise ApiError("PAYMENT_STATE_INVALID",http=409)
                    months = 0
                    plan = {"amount":{"amount_minor":amount,"currency":"RUB"},"methods":_methods(buyer)}
                else:
                    plan = next((item for item in _plans(buyer) if item["plan_id"] == body["plan_id"]), None)
                    if plan is None or plan["duration_code"] != body["duration_code"]:
                        raise ApiError("BAD_MESSAGE", http=400)
                    if body["method"] not in plan["methods"]:
                        raise ApiError("METHOD_UNAVAILABLE", http=403)
                    months = PLAN_MONTHS[body["plan_id"]][0]
                    if _contract2(request):
                        if context.account_id is None:
                            raise ApiError("ACCESS_DENIED",http=403)
                        amount, device_limit, product = await quote_product(connection,buyer,context.account_id,addon=False,selected=body.get("renew_extra_slot_ids",[]),months=months,base_amount_minor=plan["amount"]["amount_minor"])
                        plan["amount"]["amount_minor"] = amount
                listed = _plans(buyer)
                expires = datetime.now(UTC) + QUOTE_LIFETIME
                if addon:
                    target = await active_paid(connection,context.account_id)
                    expires = min(expires,target["ends_at"])
                row = await connection.fetchrow(
                    """
                    INSERT INTO s5_payment_quotes
                        (installation_id,idempotency_key,request_digest,plan_id,months,duration_code,
                         method,amount_minor,currency,tariff_key,plans_revision,expires_at,product)
                    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::jsonb)
                    ON CONFLICT (installation_id,idempotency_key) DO NOTHING RETURNING *
                    """,
                    context.installation_id, key, digest, body["plan_id"],
                    months, body["duration_code"], body["method"],
                    plan["amount"]["amount_minor"], plan["amount"]["currency"],
                    buyer.payment_tariff_key, _plans_revision(listed), expires, json.dumps(product) if product else None,
                )
                if row is None:
                    row = await connection.fetchrow(
                        "SELECT * FROM s5_payment_quotes WHERE installation_id=$1 AND idempotency_key=$2",
                        context.installation_id, key,
                    )
                    if row["request_digest"] != digest:
                        raise ApiError("IDEMPOTENCY_CONFLICT", http=409)
            return _envelope(_quote_view(row,contract2=_contract2(request)))
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
                # Wait for any prior create before reading durable state or rejecting
                # expiry. create_order reenters this same connection/session lock.
                async with payment_install_lock(connection, context.installation_id):
                    # Durable replay wins over freshness: an exact same-key retry must reach
                    # create_order (which returns its prior order) even after quote expiry. Only a
                    # genuinely new order may fail QUOTE_EXPIRED.
                    durable = await connection.fetchrow(
                        "SELECT 1 FROM payment_orders WHERE installation_id=$1 AND idempotency_key=$2",
                        context.installation_id,
                        key,
                    )
                    product = product_of(row)
                    if product and not _contract2(request):
                        raise ApiError("BAD_MESSAGE",http=400)
                    if product and durable is None and (not context.binding or context.binding["status"] != "active" or product["owner_account_id"] != str(context.account_id)):
                        raise ApiError("PAYMENT_NOT_FOUND",http=404)
                    if row["expires_at"] <= datetime.now(UTC) and durable is None:
                        raise ApiError("QUOTE_EXPIRED", http=409)
                    # Minimal quote->order wiring: create_order applies the C4 invariants (durable
                    # same-key replay, exact installation-bound quote proof, no second provider
                    # create). With the provider disabled create_order fails closed BEFORE any
                    # ledger write or provider call (PAYMENT_PROVIDER_UNAVAILABLE).
                    if durable is None and row["method"] not in _methods(settings):
                        raise ApiError("METHOD_UNAVAILABLE", http=403)
                    try:
                        result = await create_order(
                            connection,
                            _quoted_settings(settings, context.account_id, row, durable=durable is not None),
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
                        "SELECT * FROM payment_orders WHERE id=$1",
                        uuid.UUID(result["payment"]["order_id"]),
                    )
            return _envelope(_with_product(_payment_view(
                result["payment"],
                credited_revision=full["credited_entitlement_revision"] if full else None,
                needs_grant=bool(full["needs_grant"]) if full else False,
                require_checkout=True,
            ),full,_contract2(request)))
        except AuthError as error:
            return _error_response(fallback, ApiError(error.code, http=error.http, retryable=error.retryable))
        except ApiError as error:
            return _error_response(fallback, error)

    async def checkout_session(request: web.Request) -> web.Response:
        fallback = random_hex(16)
        try:
            token = _bearer(request)
            key = _idempotency_key(request)
            raw_id = request.match_info["payment_id"]
            if not _BOUNDED_PATH_ID.fullmatch(raw_id or ""):
                # Bounded opaque grammar; slash/traversal/escape/oversize never reach storage.
                raise ApiError("PAYMENT_NOT_FOUND", http=404)
            try:
                order_id = uuid.UUID(raw_id)
            except (ValueError, AttributeError):
                # Allowed non-UUID opaque ids resolve to no storage row: neutral 404.
                raise ApiError("PAYMENT_NOT_FOUND", http=404) from None
            if str(order_id) != raw_id:
                # UUID aliases (32-hex, braces, urn, ...) are bounded opaque transport ids but
                # never resolve in storage: only the canonical generated form does.
                raise ApiError("PAYMENT_NOT_FOUND", http=404)
            policy = request.app.get(CHECKOUT_POLICY_KEY)
            if policy is not None and not isinstance(policy, CheckoutPolicy):
                policy = None
            async with database.acquire() as connection:
                context = await authenticate_session(connection, settings, token)
                if "payment:write" not in context.scopes:
                    raise ApiError("ACCESS_DENIED", http=403)
                receipt = await issue_checkout_receipt(
                    connection,
                    order_id=order_id,
                    account_id=context.account_id,
                    installation_id=context.installation_id,
                    idempotency_key=key,
                    policy=policy,
                )
            origins = receipt["allowed_origins"]
            redirects = receipt["allowed_redirects"]
            return _envelope({
                "checkout_session_id": str(receipt["id"]),
                "policy_version": receipt["policy_version"],
                "expires_at": rfc3339(receipt["expires_at"]),
                "allowed_origins": json.loads(origins) if isinstance(origins, str) else list(origins),
                "allowed_redirects": json.loads(redirects) if isinstance(redirects, str) else list(redirects),
            })
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
            return _envelope(_with_product(_payment_view(
                _order_view(order), credited_revision=order["credited_entitlement_revision"],
                needs_grant=order["needs_grant"],
            ),order,_contract2(request)))
        except AuthError as error:
            return _error_response(fallback, ApiError(error.code, http=error.http, retryable=error.retryable))
        except ApiError as error:
            return _error_response(fallback, error)

    app.router.add_get(PLANS_PATH, plans)
    app.router.add_post(QUOTES_PATH, quote)
    app.router.add_post(PAYMENTS_PATH, create)
    app.router.add_get(PAYMENT_PATH, status)
    app.router.add_post(PAYMENT_PATH + "/checkout-session", checkout_session)
