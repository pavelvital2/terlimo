"""S5 §11 outbound reminder notification over the single official product bot.

The approved model (02_TARGET_ARCHITECTURE_FULL §5.4, O05) names one Telegram adapter inside the
Backend as the notification channel; no separate payment/notification bot is introduced.
This module implements that seam for the entitlement pre-expiry reminder:

* the internal announcement is durable and is never removed by a notification outcome;
* before sending, the authoritative reminder/entitlement state is re-read and fenced: a
  superseded reminder, a moved ``entitlements.revision``, an inactive/ended entitlement or an
  expired announcement is skipped, so a renewal cancels the queued external send;
* the recipient is derived from the announcement's own account (``scope_subject_ref``) and that
  account's verified ``telegram_id`` with an active binding — never a caller-supplied chat id;
* when the channel is not configured the handler releases its own queued intent (fenced delete)
  instead of consuming it, so a later enable can still notify the still-valid reminder;
* a transport/API failure raises so the durable outbox retries with backoff.

Honest guarantee: delivery is at-least-once and the pre-send fence is a point-in-time check. A
renewal committing between the check and the ``sendMessage`` call can still deliver one message
with the pre-renewal date; the next sweep cancels any further send. Delivery is never inferred
from an Android notification.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any

import asyncpg

from .config import Settings
from .reminders import _local_date, stage_text
from .telegram_bot import AiohttpTelegramTransport, TelegramTransport

NOTIFY_OPERATION = "notify_entitlement_reminder"

# Non-terminal skip reasons: no send happened, the internal announcement stays the source of truth.
SKIP_CHANNEL_UNAVAILABLE = "telegram_channel_unavailable"
SKIP_RECIPIENT_UNAVAILABLE = "telegram_recipient_unavailable"
SKIP_NOT_ADDRESSED = "announcement_not_account_scoped"
SKIP_STALE = "reminder_superseded_or_inactive"


def _payload(operation: asyncpg.Record) -> dict[str, Any]:
    payload = operation["payload"]
    if isinstance(payload, str):
        try:
            payload = json.loads(payload)
        except ValueError:
            payload = {}
    return payload if isinstance(payload, dict) else {}


class ReminderNotificationHandlers:
    def __init__(self, settings: Settings, transport: TelegramTransport | None = None,
                 clock=None) -> None:
        self._settings = settings
        self._clock = clock or (lambda: datetime.now(UTC))
        if transport is not None:
            self._transport = transport
        elif settings.telegram_notifications_enabled and settings.telegram_bot_token:
            self._transport = AiohttpTelegramTransport(settings.telegram_bot_token)
        else:
            self._transport = None

    def as_handlers(self) -> dict[str, Any]:
        return {NOTIFY_OPERATION: self.notify_reminder}

    async def notify_reminder(
        self, connection: asyncpg.Connection, operation: asyncpg.Record
    ) -> tuple[str, str] | None:
        payload = _payload(operation)
        announcement_id = payload.get("announcement_id")
        if not announcement_id:
            return ("done", SKIP_NOT_ADDRESSED)
        row = await connection.fetchrow(
            """
            SELECT a.text, a.scope_subject_ref, a.show_until,
                   acc.telegram_id, acc.status AS account_status,
                   r.state AS reminder_state, r.entitlement_revision,
                   e.revision AS current_revision, e.status AS entitlement_status,
                   e.ends_at AS entitlement_ends_at, e.kind AS entitlement_kind, r.kind AS stage_kind
            FROM announcements AS a
            JOIN entitlement_reminders AS r ON r.announcement_id = a.id
            JOIN entitlements AS e ON e.id = r.entitlement_id
            LEFT JOIN accounts AS acc ON acc.id = a.scope_subject_ref
            WHERE a.id = $1
            """,
            announcement_id,
        )
        if row is None or row["scope_subject_ref"] is None:
            return ("done", SKIP_NOT_ADDRESSED)
        now = self._clock()
        # Authoritative pre-send fence: a renewal/revision change, revocation or expiry cancels
        # the queued external send instead of delivering a stale date.
        if row["reminder_state"] != "active" or row["entitlement_revision"] != row["current_revision"]:
            return ("done", SKIP_STALE)
        ends_at = row["entitlement_ends_at"]
        expired = ends_at is not None and ends_at <= now
        # The day-of stage is allowed for the WHOLE local end date (no exact-hour promise), so a
        # sweep after an early-morning timestamp can still deliver the same-day message; other
        # stages must still be in the future.
        same_day_expiry = (
            expired
            and row["stage_kind"] == "expiry_day"
            and ends_at is not None
            and _local_date(ends_at) == _local_date(now)
        )
        if (
            row["entitlement_status"] != "active"
            or ends_at is None
            or (expired and not same_day_expiry)
        ):
            return ("done", SKIP_STALE)
        if row["show_until"] is not None and row["show_until"] <= now and not same_day_expiry:
            return ("done", SKIP_STALE)
        # Allowed channel: explicit opt-in + configured official bot token. If the channel is off
        # we release this queued intent (fenced delete) so enabling later can re-schedule it for
        # the still-valid reminder, rather than terminally consuming it.
        if (
            not self._settings.telegram_notifications_enabled
            or not self._settings.telegram_bot_token
            or self._transport is None
        ):
            await connection.execute(
                "DELETE FROM outbox_operations WHERE id = $1 AND claim_token = $2 AND status = 'processing'",
                operation["id"],
                operation["claim_token"],
            )
            return ("done", SKIP_CHANNEL_UNAVAILABLE)
        if row["telegram_id"] is None or row["account_status"] != "verified":
            return ("done", SKIP_RECIPIENT_UNAVAILABLE)
        bound = await connection.fetchval(
            "SELECT 1 FROM account_bindings WHERE account_id = $1 AND status = 'active'",
            row["scope_subject_ref"],
        )
        if bound is None:
            return ("done", SKIP_RECIPIENT_UNAVAILABLE)
        # Text is recomputed from the authoritative deadline in the service calendar; only the
        # account's own verified Telegram id is addressed. A transport error propagates so the
        # durable outbox retries. A successful call is the only evidence of an attempt.
        await self._transport.send_message(int(row["telegram_id"]), stage_text(row["stage_kind"], row["entitlement_ends_at"], now, entitlement_kind=row["entitlement_kind"]))
        return None
