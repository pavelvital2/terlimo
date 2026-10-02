"""Opt-in callback ownership selection inside the existing API; no payment ledger."""
from __future__ import annotations

import asyncio
import hmac
import json
from typing import Any
from urllib.parse import urlsplit

from aiohttp import ClientSession, ClientTimeout, web

MAX_BODY = 65_536
LEGACY_STATUSES = frozenset({
    "SUBSCRIPTION_ACTIVATED", "SUBSCRIPTION_PAST_DUE", "SUBSCRIPTION_CANCELLED",
    "SUBSCRIPTION_CANCELED", "SUBSCRIPTION_FAILED",
})


class RoutingError(Exception):
    def __init__(self, code: str, status: int = 503):
        self.code, self.status = code, status


def configured_url(value: str) -> str:
    """Only operator-configured endpoints; never resolve a destination from a callback."""
    parsed = urlsplit(value)
    if (parsed.scheme not in {"http", "https"} or not parsed.hostname or
            parsed.username or parsed.password or parsed.query or parsed.fragment):
        raise RoutingError("owner_route_unconfigured")
    return value


def canonical_id(body: dict[str, Any]) -> str:
    if "id" in body and "transactionId" in body and body["id"] != body["transactionId"]:
        raise RoutingError("invalid_transaction_id", 400)
    value = body.get("id", body.get("transactionId"))
    if not isinstance(value, str) or not value.strip() or len(value) > 1024:
        raise RoutingError("invalid_transaction_id", 400)
    return value



async def dispatch(raw: bytes, settings: Any, database: Any, finalize: Any,
                   http: Any, headers: dict[str, str]) -> web.Response:
    """Called after merchant authentication; HTTP and DB are injectable for offline checks."""
    try:
        body = json.loads(raw)
    except (ValueError, UnicodeDecodeError):
        raise RoutingError("bad_message", 400) from None
    if not isinstance(body, dict):
        raise RoutingError("bad_message", 400)
    lookup_url = configured_url(settings.platega_old_ownership_url)
    webhook_url = configured_url(settings.platega_old_webhook_url)
    folded = {str(key).lower(): value for key, value in body.items()}
    legacy = (str(folded.get("status", "")).upper() in LEGACY_STATUSES or
              bool(folded.get("subscriptionid")))
    if not legacy:
        transaction_id = canonical_id(body)
        async with database.acquire() as connection:
            new_found = await connection.fetchval(
                "SELECT EXISTS (SELECT 1 FROM payment_orders WHERE provider = $1 AND provider_payment_id = $2)",
                "platega", transaction_id,
            )
        async with http.get(lookup_url, params={"transaction_id": transaction_id},
                            headers=headers, allow_redirects=False) as response:
            if response.status != 200:
                raise RoutingError("owner_lookup_failed")
            data = bytearray()
            while True:
                chunk = await response.content.read(min(8192, MAX_BODY + 1 - len(data)))
                if not chunk:
                    break
                data.extend(chunk)
                if len(data) > MAX_BODY:
                    raise RoutingError("owner_lookup_invalid")
            try:
                lookup = json.loads(data)
            except (ValueError, UnicodeDecodeError):
                raise RoutingError("owner_lookup_invalid") from None
            if (not isinstance(lookup, dict) or set(lookup) != {"ok", "found"} or
                    lookup["ok"] is not True or type(lookup["found"]) is not bool or
                    type(new_found) is not bool):
                raise RoutingError("owner_lookup_invalid")
            old_found = lookup["found"]
        if new_found and old_found:
            raise RoutingError("owner_conflict")
        if not new_found and not old_found:
            # Includes callbacks racing the create response's transaction-ID persistence.
            raise RoutingError("owner_unmatched")
        if new_found:
            async with database.acquire() as connection:
                result = await finalize(connection, settings, merchant=headers["X-MerchantId"],
                                        secret=headers["X-Secret"], body=body)
            if result.get("result") == "ignored":
                raise RoutingError("owner_unmatched")
            return web.json_response({"status": "ok", **result})
    # Recurring legacy shapes are exclusively understood by the existing old core.
    async with http.post(webhook_url, data=raw,
                         headers={**headers, "Content-Type": "application/json"},
                         allow_redirects=False) as response:
        if not 200 <= response.status < 300:
            # Do not turn any old finalizer failure into successful delivery or follow redirects.
            if 300 <= response.status < 400:
                raise RoutingError("old_callback_redirect")
            raise RoutingError("old_callback_failed", response.status)
        return web.json_response({"status": "ok", "result": "old_delivered"}, status=response.status)


async def route_callback(request: web.Request, settings: Any, database: Any,
                         finalize: Any, *, session_factory: Any = ClientSession) -> web.Response:
    merchant, secret = settings.platega_merchant_id, settings.platega_secret
    if not merchant or not secret:
        return web.json_response({"error": "owner_route_unconfigured"}, status=503)
    headers = {"X-MerchantId": request.headers.get("X-MerchantId", ""),
               "X-Secret": request.headers.get("X-Secret", "")}
    if not (hmac.compare_digest(headers["X-MerchantId"], merchant) and
            hmac.compare_digest(headers["X-Secret"], secret)):
        return web.json_response({"error": "payment_auth"}, status=403)
    try:
        # Entire read + both lookups + one finalizer fit inside the provider callback budget.
        timeout = min(20, max(1, settings.platega_owner_routing_timeout_seconds))
        async with asyncio.timeout(timeout):
            raw = await request.read()
            if len(raw) > MAX_BODY:
                raise RoutingError("body_too_large", 413)
            async with session_factory(timeout=ClientTimeout(total=timeout), trust_env=False) as http:
                return await dispatch(raw, settings, database, finalize, http, headers)
    except RoutingError as error:
        return web.json_response({"error": error.code}, status=error.status)
    except Exception as error:
        # Core ApiError preserves its status; IO/DB failures remain retryable. No raw errors/keys.
        status = getattr(error, "http", 503)
        if type(status) is not int or not 400 <= status <= 599:
            status = 503
        return web.json_response({"error": "owner_route_failed"}, status=status)
