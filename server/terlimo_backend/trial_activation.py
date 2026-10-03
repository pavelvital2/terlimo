"""S3-B explicit one-time 7-day trial activation for a registered Telegram identity.

Server-side only: the client never supplies a Telegram id or eligibility. The accepted S3-A
registration (`within_hour` snapshot) grants the *right to ask*, but activation always re-checks
the current trial history for the Telegram id and the mandatory TERLIMO news-channel membership.
No trial starts automatically at registration, no new onboarding hour is created, and an active
paid/imported right is never interrupted. The entitlement is ordinary, so the existing
catalog/access.sync + gateway apply pipeline provisions it unchanged.
"""

from __future__ import annotations

from datetime import datetime, timedelta
from typing import Any, Protocol

import asyncpg
from aiohttp import ClientError, ClientSession, ClientTimeout, web

from .auth_api import ApiError, _error_response, now_utc, rfc3339
from .config import Settings
from .db import Database
from .session_auth import AuthError, authenticate_session

MOBILE_PREFIX = "/api/mobile/v1"
ACTIVATE_PATH = f"{MOBILE_PREFIX}/trial/activate"
TELEGRAM_API = "https://api.telegram.org"
TRIAL_DAYS = 7
TRIAL_SOURCE_PLAN = {"plan_id": "trial-7d", "title": "7 дней", "duration_code": "days:7",
                    "tariff_key": None, "origin": "trial"}
TRIAL_CHANNEL_KEY: web.AppKey = web.AppKey("trial_channel_checker", "ChannelMembershipChecker")


class ChannelMembershipUnavailable(RuntimeError):
    """The membership check could not be answered truthfully (fail closed, never a trial)."""


class ChannelMembershipChecker(Protocol):
    async def is_member(self, telegram_id: int) -> bool: ...


class BotApiChannelMembership:
    """Real getChatMember against the shared product bot; fail-closed on any uncertainty."""

    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._session: ClientSession | None = None

    async def _client(self) -> ClientSession:
        if self._session is None:
            self._session = ClientSession(timeout=ClientTimeout(total=10))
        return self._session

    async def close(self) -> None:
        if self._session is not None:
            await self._session.close()
            self._session = None

    async def is_member(self, telegram_id: int) -> bool:
        channel = self._settings.telegram_trial_channel_id
        if not channel or not self._settings.telegram_bot_token:
            raise ChannelMembershipUnavailable("trial channel membership is not configured")
        session = await self._client()
        try:
            async with session.get(
                f"{TELEGRAM_API}/bot{self._settings.telegram_bot_token}/getChatMember",
                params={"chat_id": channel, "user_id": telegram_id},
            ) as response:
                payload = await response.json()
        except (ClientError, OSError, ValueError) as error:
            raise ChannelMembershipUnavailable("membership check unavailable") from error
        if not payload.get("ok"):
            raise ChannelMembershipUnavailable("membership check rejected")
        status = (payload.get("result") or {}).get("status")
        if status in ("creator", "administrator", "member"):
            return True
        if status in ("left", "kicked", "restricted"):
            return False
        raise ChannelMembershipUnavailable("membership status unknown")


def _bearer_token(request: web.Request) -> str:
    header = request.headers.get("Authorization", "")
    if not header.startswith("Bearer "):
        raise AuthError("SESSION_INVALID", 401)
    return header[len("Bearer ") :].strip()


def _trial_projection(
    row: asyncpg.Record | None, moment: datetime, *, replay: bool = True
) -> dict[str, Any]:
    if row is None:
        return {"state": "none", "starts_at": None, "ends_at": None, "replay": False}
    active = row["status"] == "active" and (row["ends_at"] is None or row["ends_at"] > moment)
    return {
        "state": "active" if active else "used",
        "starts_at": rfc3339(row["starts_at"]) if row["starts_at"] else None,
        "ends_at": rfc3339(row["ends_at"]) if row["ends_at"] else None,
        "replay": replay,
    }


async def _trial_for_account(connection: asyncpg.Connection, account_id: Any) -> asyncpg.Record | None:
    return await connection.fetchrow(
        "SELECT * FROM entitlements WHERE account_id = $1 AND kind = 'trial' FOR UPDATE",
        account_id,
    )


async def _trial_history_for_telegram(
    connection: asyncpg.Connection, telegram_id: int, account_id: Any
) -> bool:
    found = await connection.fetchval(
        """
        SELECT 1
        FROM entitlements AS e
        JOIN accounts AS a ON a.id = e.account_id
        WHERE a.telegram_id = $1 AND e.kind = 'trial' AND e.account_id <> $2
        LIMIT 1
        """,
        telegram_id,
        account_id,
    )
    return found is not None


async def _active_commercial(connection: asyncpg.Connection, account_id: Any, moment: datetime) -> bool:
    found = await connection.fetchval(
        """
        SELECT 1 FROM entitlements
        WHERE account_id = $1 AND kind IN ('paid', 'imported') AND status = 'active'
          AND (starts_at IS NULL OR starts_at <= $2)
          AND (ends_at IS NULL OR ends_at > $2)
        LIMIT 1
        """,
        account_id,
        moment,
    )
    return found is not None


async def _registration_snapshot(connection: asyncpg.Connection, installation_id: Any) -> asyncpg.Record | None:
    return await connection.fetchrow(
        """
        SELECT * FROM registration_links
        WHERE installation_id = $1 AND status = 'confirmed'
        ORDER BY confirmed_at DESC, id DESC
        LIMIT 1
        """,
        installation_id,
    )


async def trial_status(
    connection: asyncpg.Connection, settings: Settings, context: Any
) -> dict[str, Any]:
    """Read-only S3-B projection for /me; performs no external call and creates nothing."""
    moment = now_utc()
    account_id = context.account_id
    if account_id is None:
        binding = await connection.fetchval(
            "SELECT account_id FROM account_bindings WHERE installation_id = $1 AND status = 'active'",
            context.installation_id,
        )
        account_id = binding
    trial = None
    if account_id is not None:
        trial = await connection.fetchrow(
            "SELECT * FROM entitlements WHERE account_id = $1 AND kind = 'trial'",
            account_id,
        )
    if trial is not None:
        projection = _trial_projection(trial, moment)
        state = projection["state"]
        reason = None if state == "active" else "trial_already_used"
        return {**projection, "state": state, "can_activate": False, "reason": reason}
    if account_id is not None and await connection.fetchval(
        "SELECT imported_trial_used FROM referral_benefits WHERE account_id=$1",account_id
    ):
        return {"state":"used","can_activate":False,"reason":"trial_already_used",
                "starts_at":None,"ends_at":None,"replay":False}
    registration = await _registration_snapshot(connection, context.installation_id)
    if registration is None:
        return {"state": "none", "can_activate": False, "reason": "registration_required",
                "starts_at": None, "ends_at": None, "replay": False}
    if account_id is not None and await _active_commercial(connection, account_id, moment):
        return {"state": "ineligible", "can_activate": False, "reason": "subscription_active",
                "starts_at": None, "ends_at": None, "replay": False}
    if not registration["within_hour"]:
        return {"state": "ineligible", "can_activate": False,
                "reason": "hour_expired_before_registration",
                "starts_at": None, "ends_at": None, "replay": False}
    if not settings.telegram_trial_channel_id or not settings.telegram_bot_token:
        # Missing channel credential is never an available trial; it stays fail-closed until the
        # owner provides the official channel and bot token.
        return {"state": "ineligible", "can_activate": False,
                "reason": "channel_check_unavailable",
                "starts_at": None, "ends_at": None, "replay": False}
    return {"state": "available", "can_activate": True, "reason": None,
            "starts_at": None, "ends_at": None, "replay": False}


async def activate_trial(
    connection: asyncpg.Connection,
    settings: Settings,
    checker: ChannelMembershipChecker,
    *,
    installation_id: Any,
) -> dict[str, Any]:
    moment = now_utc()
    async with connection.transaction():
        account_id = await connection.fetchval(
            "SELECT account_id FROM account_bindings WHERE installation_id=$1 AND status='active'",
            installation_id,
        )
        if account_id is None:
            raise ApiError("REGISTRATION_REQUIRED", http=403)
        # Shared with paid/reward writers: account lock precedes binding/entitlement rows.
        # The first read is only a lock key; recheck authoritative binding under row lock.
        await connection.execute("SELECT pg_advisory_xact_lock(hashtextextended($1,0))",f"paid-account:{account_id}")
        locked_account = await connection.fetchval(
            "SELECT account_id FROM account_bindings WHERE installation_id=$1 AND status='active' FOR UPDATE",
            installation_id,
        )
        if locked_account is None:
            raise ApiError("REGISTRATION_REQUIRED", http=403)
        if locked_account != account_id:
            raise ApiError("SERVICE_UNAVAILABLE",http=503,retryable=True)
        # Serialize per Telegram identity/account so concurrent activation cannot mint two trials.
        await connection.execute(
            "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
            f"trial:{account_id}",
        )
        telegram_id = await connection.fetchval(
            "SELECT telegram_id FROM accounts WHERE id = $1", account_id
        )
        if type(telegram_id) is not int:
            raise ApiError("REGISTRATION_REQUIRED", http=403)
        existing = await trial_eligibility(connection, account_id, telegram_id, moment)
        if existing is not None:
            return {"trial": _trial_projection(existing, moment)}
        registration = await _registration_snapshot(connection, installation_id)
        if registration is None:
            raise ApiError("REGISTRATION_REQUIRED", http=403)
        if not registration["within_hour"]:
            raise ApiError(
                "TRIAL_NOT_ELIGIBLE", http=403,
                details={"reason": "hour_expired_before_registration"},
            )
        try:
            member = await checker.is_member(telegram_id)
        except ChannelMembershipUnavailable as error:
            raise ApiError("TRIAL_CHECK_UNAVAILABLE", http=503, retryable=True) from error
        if not member:
            raise ApiError(
                "CHANNEL_MEMBERSHIP_REQUIRED", http=403,
                details={"reason": "subscribe_to_official_channel_then_retry"},
            )
        row = await insert_trial(connection, account_id, moment)
        return {"trial": _trial_projection(row, moment, replay=False), "account_state": "ACTIVE_TRIAL"}


async def trial_eligibility(connection, account_id, telegram_id, moment, *,
                            trusted=False, locked=True):
    """Shared once-only/commercial checks. No mutation or external I/O."""
    query = "SELECT * FROM entitlements WHERE account_id=$1 AND kind='trial'"
    existing = await connection.fetchrow(query + (" FOR UPDATE" if locked else ""), account_id)
    if existing is not None:
        return existing
    benefit = await connection.fetchrow(
        "SELECT history_state,imported_trial_used FROM referral_benefits WHERE account_id=$1", account_id)
    if benefit is not None and benefit['imported_trial_used']:
        raise ApiError("TRIAL_ALREADY_USED", http=409)
    if await _trial_history_for_telegram(connection, telegram_id, account_id):
        raise ApiError("TRIAL_ALREADY_USED", http=409)
    if await _active_commercial(connection, account_id, moment):
        raise ApiError("SUBSCRIPTION_ACTIVE", http=409)
    if trusted and (benefit is None or benefit['history_state'] != 'ready'):
        raise ApiError("SERVICE_UNAVAILABLE", http=503, retryable=True)
    return None


async def insert_trial(connection, account_id, moment):
    """Single trial insertion engine; caller holds paid-account then trial locks."""
    from .mobile_account import BASE_LIMIT
    from .referral_rewards import trial_bonus_days
    bonus_days = await trial_bonus_days(connection, account_id)
    days = TRIAL_DAYS + bonus_days
    source_plan = dict(TRIAL_SOURCE_PLAN)
    if bonus_days:
        source_plan.update(plan_id="trial-10d", title="10 дней", duration_code="days:10", referral_bonus_days=bonus_days)

    row = await connection.fetchrow(
        """
        INSERT INTO entitlements
            (account_id, kind, status, starts_at, ends_at, device_limit, revision, source_plan)
        VALUES ($1, 'trial', 'active', $2, $3, $4, 1, $5::jsonb)
        RETURNING *
        """,
        account_id,
        moment,
        moment + timedelta(days=days),
        BASE_LIMIT,
        source_plan,
    )
    return row


def register_trial_routes(app: web.Application, settings: Settings, database: Database) -> None:
    app[TRIAL_CHANNEL_KEY] = BotApiChannelMembership(settings)
    app.router.add_post(ACTIVATE_PATH, _activate_handler(settings, database))


def _activate_handler(settings: Settings, database: Database):
    async def handler(request: web.Request) -> web.Response:
        from .auth_api import random_hex

        fallback_id = random_hex(16)
        try:
            token = _bearer_token(request)
            async with database.acquire() as connection:
                context = await authenticate_session(connection, settings, token)
                checker = request.app.get(TRIAL_CHANNEL_KEY)
                if checker is None:
                    checker = BotApiChannelMembership(settings)
                result = await activate_trial(
                    connection, settings, checker, installation_id=context.installation_id
                )
            result["status"] = "ok"
            result["request_id"] = fallback_id
            return web.json_response(result)
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
