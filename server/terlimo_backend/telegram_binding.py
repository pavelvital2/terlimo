"""S3-A Telegram registration binding (installation -> confirmed Telegram identity).

Registration is deliberately *not* a trial and never creates an entitlement or data grant. It:
- issues a one-time deep-link token bound to the caller's installation (no data grant required);
- confirms exactly that installation from the bot server (shared key, no client-supplied identity);
- records the server-computed trial eligibility at confirmation time (within hour AND no prior
  trial for that Telegram id) for a future 7-day button; it does not activate anything.

Nothing here trusts a client-provided Telegram id or eligibility.

`trial_available` recorded at confirmation is a *snapshot right for a future 7-day button*, not
the issuance decision: a later trial issuance must independently re-check the current trial
history for that Telegram id and the then-current rules (the stored flag alone must never be the
only source). This module issues no trial and no entitlement.
"""

from __future__ import annotations

import hashlib
import hmac
import secrets
from datetime import UTC, datetime
from typing import Any

import asyncpg
from aiohttp import web

from .auth_api import (
    SCHEMA_VERSION,
    ApiError,
    _error_response,
    _json_body,
    now_utc,
    rfc3339,
)
from .config import Settings
from .db import Database
from .session_auth import AuthError, authenticate_session

MOBILE_PREFIX = "/api/mobile/v1"
LINK_PATH = f"{MOBILE_PREFIX}/registration/telegram/link"
CONFIRM_PATH = f"{MOBILE_PREFIX}/internal/telegram/registration/confirm"
BOT_KEY_HEADER = "X-Telegram-Bot-Key"
CONFIRM_KEYS = frozenset({"token", "telegram_id", "telegram_username"})
PURCHASE_AVAILABLE = True


def registration_enabled(settings: Settings) -> bool:
    return bool(settings.telegram_bot_username and settings.telegram_bot_key)


def _token_hash(raw: str) -> str:
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()


def _bearer_token(request: web.Request) -> str:
    header = request.headers.get("Authorization", "")
    if not header.startswith("Bearer "):
        raise AuthError("SESSION_INVALID", 401)
    return header[len("Bearer ") :].strip()


def _empty_registration() -> dict[str, Any]:
    return {
        "state": "none",
        "telegram_id": None,
        "registered_at": None,
        "within_hour": False,
        "trial_available": False,
        "trial_reason": None,
        "purchase_available": PURCHASE_AVAILABLE,
    }


async def _trial_used(connection: asyncpg.Connection, telegram_id: int) -> bool:
    """A Telegram id has used its trial if any trial entitlement (any status) belongs to it."""
    found = await connection.fetchval(
        """
        SELECT 1
        FROM entitlements AS entitlement
        JOIN accounts AS account ON account.id = entitlement.account_id
        WHERE account.telegram_id = $1 AND entitlement.kind = 'trial'
        LIMIT 1
        """,
        telegram_id,
    )
    return found is not None


async def _hour_active(connection: asyncpg.Connection, installation_id: Any) -> bool:
    found = await connection.fetchval(
        """
        SELECT 1 FROM entitlements
        WHERE kind = 'onboarding_hour' AND installation_id = $1
          AND status = 'active' AND ends_at IS NOT NULL AND ends_at > now()
        LIMIT 1
        """,
        installation_id,
    )
    return found is not None


async def registration_view(
    connection: asyncpg.Connection, installation_id: Any, settings: Settings
) -> dict[str, Any]:
    """Read-only registration projection for /me (never creates rows)."""
    link = await connection.fetchrow(
        """
        SELECT * FROM registration_links
        WHERE installation_id = $1 AND environment = $2
        ORDER BY created_at DESC, id DESC
        LIMIT 1
        """,
        installation_id,
        settings.environment,
    )
    if link is not None and link["status"] == "confirmed":
        if not registration_enabled(settings):
            return _empty_registration()
        return {
            "state": "registered",
            "telegram_id": link["telegram_id"],
            "registered_at": rfc3339(link["confirmed_at"]) if link["confirmed_at"] else None,
            "within_hour": bool(link["within_hour"]),
            "trial_available": bool(link["trial_available"]),
            "trial_reason": link["trial_reason"],
            "purchase_available": PURCHASE_AVAILABLE,
        }
    if link is not None and link["status"] == "pending":
        return {
            "state": "pending",
            "telegram_id": None,
            "registered_at": None,
            "within_hour": False,
            "trial_available": False,
            "trial_reason": None,
            "purchase_available": PURCHASE_AVAILABLE,
            "link_expires_at": rfc3339(link["expires_at"]),
        }
    return _empty_registration()


async def create_registration_link(
    connection: asyncpg.Connection,
    settings: Settings,
    *,
    installation_id: Any,
) -> dict[str, Any]:
    if not registration_enabled(settings):
        raise ApiError("REGISTRATION_DISABLED", http=503)
    async with connection.transaction():
        installation = await connection.fetchrow(
            "SELECT id, state FROM installations WHERE id = $1 FOR UPDATE", installation_id
        )
        if installation is None:
            raise ApiError("PROOF_INVALID", http=404)
        if installation["state"] == "revoked":
            raise ApiError("DEVICE_REVOKED", http=403)
        confirmed = await connection.fetchrow(
            """
            SELECT * FROM registration_links
            WHERE installation_id = $1 AND environment = $2 AND status = 'confirmed'
            ORDER BY confirmed_at DESC, id DESC LIMIT 1
            """,
            installation_id,
            settings.environment,
        )
        if confirmed is not None:
            return {
                "state": "registered",
                "telegram_id": confirmed["telegram_id"],
                "registered_at": rfc3339(confirmed["confirmed_at"]),
                "within_hour": bool(confirmed["within_hour"]),
                "trial_available": bool(confirmed["trial_available"]),
                "trial_reason": confirmed["trial_reason"],
                "purchase_available": PURCHASE_AVAILABLE,
            }
        await connection.execute(
            """
            UPDATE registration_links SET status = 'expired'
            WHERE installation_id = $1 AND status = 'pending'
            """,
            installation_id,
        )
        raw = secrets.token_urlsafe(32)
        expires_at = now_utc().timestamp() + settings.registration_token_ttl_seconds
        await connection.execute(
            """
            INSERT INTO registration_links
                (token_sha256, installation_id, environment, status, expires_at)
            VALUES ($1, $2, $3, 'pending', to_timestamp($4))
            """,
            _token_hash(raw),
            installation_id,
            settings.environment,
            expires_at,
        )
        return {
            "state": "pending",
            "token": raw,
            "bot_username": settings.telegram_bot_username,
            "deep_link": f"https://t.me/{settings.telegram_bot_username}?start={raw}",
            "expires_at": rfc3339(datetime.fromtimestamp(expires_at, UTC)),
            "expires_in": settings.registration_token_ttl_seconds,
        }


async def confirm_registration(
    connection: asyncpg.Connection,
    settings: Settings,
    *,
    token: str,
    telegram_id: int,
    telegram_username: str | None,
) -> dict[str, Any]:
    if not registration_enabled(settings):
        raise ApiError("REGISTRATION_DISABLED", http=503)
    async with connection.transaction():
        link = await connection.fetchrow(
            "SELECT * FROM registration_links WHERE token_sha256 = $1 FOR UPDATE",
            _token_hash(token),
        )
        if link is None:
            raise ApiError("REGISTRATION_UNKNOWN", http=404)
        if link["status"] == "confirmed":
            if link["telegram_id"] != telegram_id:
                raise ApiError("REGISTRATION_CONFLICT", http=409)
            return _confirmed_result(link)
        if link["status"] == "expired" or link["expires_at"] <= now_utc():
            await connection.execute(
                "UPDATE registration_links SET status = 'expired' WHERE id = $1", link["id"]
            )
            raise ApiError("REGISTRATION_EXPIRED", http=410)
        installation = await connection.fetchrow(
            "SELECT id, state FROM installations WHERE id = $1 FOR UPDATE",
            link["installation_id"],
        )
        if installation is None or installation["state"] == "revoked":
            raise ApiError("DEVICE_REVOKED", http=403)

        account_id = await connection.fetchval(
            "SELECT id FROM accounts WHERE telegram_id = $1", telegram_id
        )
        if account_id is None:
            account_id = await connection.fetchval(
                """
                INSERT INTO accounts (status, telegram_id) VALUES ('verified', $1)
                ON CONFLICT (telegram_id) DO UPDATE SET updated_at = now()
                RETURNING id
                """,
                telegram_id,
            )
        existing = await connection.fetchrow(
            """
            SELECT id, account_id FROM account_bindings
            WHERE installation_id = $1 AND status = 'active'
            """,
            link["installation_id"],
        )
        if existing is not None and existing["account_id"] != account_id:
            raise ApiError("REGISTRATION_CONFLICT", http=409)
        if existing is None:
            await connection.execute(
                """
                INSERT INTO account_bindings (account_id, installation_id, status)
                VALUES ($1, $2, 'active')
                """,
                account_id,
                link["installation_id"],
            )
        within_hour = await _hour_active(connection, link["installation_id"])
        trial_used = await _trial_used(connection, telegram_id)
        if trial_used:
            trial_available, reason = False, "trial_already_used"
        elif within_hour:
            trial_available, reason = True, "within_hour_no_prior_trial"
        else:
            trial_available, reason = False, "hour_expired"
        confirmed = await connection.fetchrow(
            """
            UPDATE registration_links
            SET status = 'confirmed', confirmed_at = now(), telegram_id = $2,
                within_hour = $3, trial_available = $4, trial_reason = $5
            WHERE id = $1
            RETURNING *
            """,
            link["id"],
            telegram_id,
            within_hour,
            trial_available,
            reason,
        )
        # A paid order created before Telegram binding stays parked on the installation and is
        # applied exactly once to the account that has just been bound (S4).
        from .payments import apply_parked_payments

        await apply_parked_payments(
            connection, settings, installation_id=link["installation_id"]
        )
        return _confirmed_result(confirmed)


def _confirmed_result(link: asyncpg.Record) -> dict[str, Any]:
    return {
        "registration_state": "registered",
        "telegram_id": link["telegram_id"],
        "registered_at": rfc3339(link["confirmed_at"]) if link["confirmed_at"] else None,
        "within_hour": bool(link["within_hour"]),
        "trial_available": bool(link["trial_available"]),
        "trial_reason": link["trial_reason"],
        "purchase_available": PURCHASE_AVAILABLE,
    }


def register_telegram_registration_routes(
    app: web.Application, settings: Settings, database: Database
) -> None:
    app.router.add_post(LINK_PATH, _link_handler(settings, database))
    app.router.add_post(CONFIRM_PATH, _confirm_handler(settings, database))


def _registration_link_response(request_id: str, registration: dict[str, Any]) -> web.Response:
    """Mobile v1 success envelope for the registration-link route.

    The route previously returned the flat registration dict; the agreed mobile-v1 envelope
    (and the strict Android client) require request_id/server_time/schema_version/status with
    the registration projection nested, and reject unknown top-level fields.

    The strict client link decoder accepts only the six pending fields, and for an already
    registered installation only ``{"state": "registered"}``; the richer registered fields
    stay on the separate /me registration projection.
    """
    projection: dict[str, Any] = registration
    if registration.get("state") == "registered":
        projection = {"state": "registered"}
    return web.json_response(
        {
            "request_id": request_id,
            "server_time": rfc3339(now_utc()),
            "schema_version": SCHEMA_VERSION,
            "status": "ok",
            "registration": projection,
        }
    )


def _link_handler(settings: Settings, database: Database):
    async def handler(request: web.Request) -> web.Response:
        from .auth_api import random_hex

        fallback_id = random_hex(16)
        try:
            token = _bearer_token(request)
            async with database.acquire() as connection:
                context = await authenticate_session(connection, settings, token)
                result = await create_registration_link(
                    connection, settings, installation_id=context.installation_id
                )
            return _registration_link_response(fallback_id, result)
        except AuthError as error:
            return _error_response(
                fallback_id,
                ApiError(error.code, http=error.http, retryable=error.retryable, request_id=fallback_id),
            )
        except ApiError as error:
            return _error_response(fallback_id, error)
        except (asyncpg.PostgresError, OSError):
            return _error_response(fallback_id, ApiError("SERVICE_UNAVAILABLE", request_id=fallback_id))

    return handler


def _confirm_handler(settings: Settings, database: Database):
    async def handler(request: web.Request) -> web.Response:
        from .auth_api import random_hex

        fallback_id = random_hex(16)
        try:
            if not registration_enabled(settings):
                raise ApiError("REGISTRATION_DISABLED", http=503)
            supplied = request.headers.get(BOT_KEY_HEADER, "")
            if not supplied or not hmac.compare_digest(supplied, settings.telegram_bot_key):
                raise ApiError("REGISTRATION_AUTH", http=403)
            body = await _json_body(request)
            if not isinstance(body, dict) or set(body) - CONFIRM_KEYS or "token" not in body or "telegram_id" not in body:
                raise ApiError("BAD_MESSAGE")
            token = body["token"]
            telegram_id = body["telegram_id"]
            username = body.get("telegram_username")
            if not isinstance(token, str) or not 8 <= len(token) <= 256:
                raise ApiError("BAD_MESSAGE")
            if type(telegram_id) is not int or telegram_id <= 0:
                raise ApiError("BAD_MESSAGE")
            if username is not None and (not isinstance(username, str) or len(username) > 64):
                raise ApiError("BAD_MESSAGE")
            async with database.acquire() as connection:
                result = await confirm_registration(
                    connection, settings, token=token, telegram_id=telegram_id,
                    telegram_username=username,
                )
            result["request_id"] = fallback_id
            return web.json_response(result)
        except ApiError as error:
            return _error_response(fallback_id, error)
        except (asyncpg.PostgresError, OSError):
            return _error_response(fallback_id, ApiError("SERVICE_UNAVAILABLE", request_id=fallback_id))

    return handler
