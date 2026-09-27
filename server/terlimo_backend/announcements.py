"""S5 account announcements (core): durable one-way messages + idempotent read markers.

Scope of this slice: authenticated GET /api/mobile/v1/announcements and idempotent
POST /api/mobile/v1/announcements/{id}/read. No reminder/cron and no delivery claim: an Android
notification attempt is never treated as message delivery.

Response DTO follows the approved mobile-v1 schemas/announcement.json contract exactly:
{announcement_id, title, text, action{type,label?}, valid_until, unread, revision}. The durable
store has no user-facing title, no label and no separate revision column, so the compat view is
honest: title is an empty string (allowed by the schema), action.type comes from the stored
allowlisted kind (anything else maps to "none" and never leaks a URL), valid_until mirrors
show_until, unread inverts the read marker and revision is the row's created_at microsecond stamp
(a durable, monotonic, decimal integer as a string).
"""

from __future__ import annotations

import json
import uuid
from datetime import UTC, datetime, timedelta
from typing import Any

import asyncpg
from aiohttp import web

from .auth_api import SCHEMA_VERSION, ApiError, _error_response, random_hex, rfc3339
from .config import Settings
from .db import Database
from .session_auth import AuthError, authenticate_session

MOBILE_PREFIX = "/api/mobile/v1"
ANNOUNCEMENTS_PATH = f"{MOBILE_PREFIX}/announcements"
ANNOUNCEMENT_READ_PATH = f"{MOBILE_PREFIX}/announcements/{{announcement_id}}/read"
ALLOWED_ACTIONS = frozenset({"refresh_catalog"})
IDEMPOTENCY_HEADER = "Idempotency-Key"
MAX_TEXT = 2000


def _envelope(payload: dict[str, Any]) -> web.Response:
    body = {
        "request_id": random_hex(16),
        "server_time": rfc3339(datetime.now(UTC)),
        "schema_version": SCHEMA_VERSION,
        "status": "ok",
    }
    body.update(payload)
    return web.json_response(body)


def _bearer(request: web.Request) -> str:
    header = request.headers.get("Authorization", "")
    if not header.startswith("Bearer "):
        raise AuthError("SESSION_INVALID", 401)
    return header[len("Bearer ") :].strip()


def _action_view(raw: Any) -> dict[str, Any]:
    """Approved AnnouncementAction only: stored kind -> contract type; anything else is neutral.

    A non-allowlisted or malformed action never becomes a URL and is reported as {"type": "none"}.
    """
    payload = raw
    if isinstance(payload, str):
        try:
            payload = json.loads(payload)
        except ValueError:
            payload = None
    if not isinstance(payload, dict):
        return {"type": "none"}
    kind = payload.get("kind")
    if kind not in ALLOWED_ACTIONS:
        return {"type": "none"}
    view: dict[str, Any] = {"type": kind}
    label = payload.get("label")
    if isinstance(label, str) and 0 < len(label) <= 64:
        view["label"] = label
    return view


def _revision(created_at: datetime) -> str:
    """Durable monotonic revision: the row's creation instant in whole microseconds."""
    micros = (created_at - datetime(1970, 1, 1, tzinfo=UTC)) // timedelta(microseconds=1)
    return str(micros)


def _announcement_view(row: asyncpg.Record, *, read: bool) -> dict[str, Any]:
    return {
        "announcement_id": str(row["id"]),
        "title": "",
        "text": row["text"],
        "action": _action_view(row["action"]),
        "valid_until": rfc3339(row["show_until"]) if row["show_until"] else None,
        "unread": not read,
        "revision": _revision(row["created_at"]),
    }


def register_announcement_routes(app: web.Application, settings: Settings, database: Database) -> None:
    async def list_announcements(request: web.Request) -> web.Response:
        fallback = random_hex(16)
        try:
            token = _bearer(request)
            async with database.acquire() as connection:
                context = await authenticate_session(connection, settings, token)
                if context.account_id is None:
                    # Unbound sessions have no account message surface.
                    return _envelope({"announcements": [], "unread_count": 0})
                rows = await connection.fetch(
                    """
                    SELECT a.*, r.account_id AS read_by_account
                    FROM announcements AS a
                    LEFT JOIN announcement_reads AS r
                      ON r.announcement_id = a.id AND r.account_id = $2
                    WHERE a.environment = $1
                      AND (a.show_until IS NULL OR a.show_until > now())
                      AND (a.scope_subject_ref IS NULL OR a.scope_subject_ref = $2)
                      AND r.account_id IS NULL
                    ORDER BY a.created_at DESC, a.id
                    """,
                    settings.environment,
                    context.account_id,
                )
            items = [_announcement_view(row, read=False) for row in rows]
            return _envelope({"announcements": items, "unread_count": len(items)})
        except AuthError as error:
            return _error_response(fallback, ApiError(error.code, http=error.http, retryable=error.retryable, request_id=fallback))
        except ApiError as error:
            return _error_response(fallback, error)

    async def read_announcement(request: web.Request) -> web.Response:
        fallback = random_hex(16)
        try:
            token = _bearer(request)
            idempotency_key = request.headers.get(IDEMPOTENCY_HEADER, "")
            if not 16 <= len(idempotency_key) <= 128:
                raise ApiError("BAD_MESSAGE", http=400, details={"reason": "idempotency_key_required"})
            try:
                announcement_id = uuid.UUID(request.match_info["announcement_id"])
            except (ValueError, AttributeError):
                raise ApiError("NOT_FOUND", http=404) from None
            async with database.acquire() as connection:
                context = await authenticate_session(connection, settings, token)
                if context.account_id is None:
                    raise ApiError("NOT_FOUND", http=404)
                # Atomic visibility + read insert: only global or exact current-account scope,
                # environment-bound and unexpired; no check/insert race.
                inserted = await connection.fetchrow(
                    """
                    INSERT INTO announcement_reads (announcement_id, account_id)
                    SELECT a.id, $2 FROM announcements AS a
                    WHERE a.id = $1 AND a.environment = $3
                      AND (a.show_until IS NULL OR a.show_until > now())
                      AND (a.scope_subject_ref IS NULL OR a.scope_subject_ref = $2)
                    ON CONFLICT (announcement_id, account_id) DO NOTHING
                    RETURNING read_at
                    """,
                    announcement_id,
                    context.account_id,
                    settings.environment,
                )
                if inserted is not None:
                    read_at = inserted["read_at"]
                else:
                    read_at = await connection.fetchval(
                        """
                        SELECT r.read_at FROM announcement_reads AS r
                        JOIN announcements AS a ON a.id = r.announcement_id
                        WHERE r.announcement_id = $1 AND r.account_id = $2
                          AND a.environment = $3
                          AND (a.show_until IS NULL OR a.show_until > now())
                          AND (a.scope_subject_ref IS NULL OR a.scope_subject_ref = $2)
                        """,
                        announcement_id,
                        context.account_id,
                        settings.environment,
                    )
                    if read_at is None:
                        raise ApiError("NOT_FOUND", http=404)
            return _envelope({
                "announcement_id": str(announcement_id),
                "read": True,
                "read_at": rfc3339(read_at),
            })
        except AuthError as error:
            return _error_response(fallback, ApiError(error.code, http=error.http, retryable=error.retryable, request_id=fallback))
        except ApiError as error:
            return _error_response(fallback, error)

    app.router.add_get(ANNOUNCEMENTS_PATH, list_announcements)
    app.router.add_post(ANNOUNCEMENT_READ_PATH, read_announcement)
