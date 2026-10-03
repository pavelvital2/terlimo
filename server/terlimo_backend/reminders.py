"""S5 §11 / G39 entitlement reminder sweep — four calendar stages (source-only).

Finite ``trial``/``paid`` rights get up to four independent stages keyed by the entitlement's own
durable revision and deadline: T−3, T−2, T−1 and a separate day-of-expiry message. There are NO
stages for an indefinite right (``ends_at IS NULL``).

Stage selection is by CALENDAR DATE in one fixed zone (``REPORTING_TIMEZONE`` = Europe/Moscow,
matching the reported/showed end date; 09_CONTRACTS_AND_DATA §11). This is a service calendar, not
an Android exact alarm: a missed sweep simply produces the current stage, never a backfill of
already-passed stages.

Durability/idempotency: a reminder is one ``announcement`` plus an ``entitlement_reminders`` row
whose key is ``(entitlement_id, entitlement_revision, stage_end_at, kind)`` — so repeated sweeps,
restarts and retries can create at most one row per account/right/revision/end/stage. A renewal or
replacement bumps ``entitlements.revision``; the next sweep atomically supersedes the old active
rows and hides their announcements, and no stage for the old deadline is ever created again.
Creating the internal announcement/outbox intent is NOT proof of delivery.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime, timedelta
from zoneinfo import ZoneInfo

import asyncpg

from .config import Settings

# stage kind -> calendar days before the end date (0 = day of expiry)
STAGE_OFFSETS: dict[str, int] = {
    "pre_expiry_t3": 3,
    "pre_expiry_t2": 2,
    "pre_expiry_t1": 1,
    "expiry_day": 0,
}
STAGE_KINDS = tuple(STAGE_OFFSETS)
# Backwards-compatible name for the earliest stage (old single-stage slice used one kind).
REMINDER_KIND = "pre_expiry_t3"
NOTIFY_OPERATION = "notify_entitlement_reminder"
REPORTING_TIMEZONE = "Europe/Moscow"
# Only finite commercial rights: trial and paid. Indefinite/imported legacy rights get no stages.
REMINDER_ELIGIBLE_KINDS = ("trial", "paid")
_MAX_LOOKAHEAD_DAYS = 5  # safety bound for candidate scan; stage logic still requires exact delta


def _notification_configured(settings: Settings) -> bool:
    return bool(settings.telegram_notifications_enabled and settings.telegram_bot_token)


def _window_start(now: datetime, settings: Settings) -> datetime:
    """Kept for compatibility with the previous single-window slice exports."""
    return now + timedelta(seconds=settings.reminder_window_seconds)


def _local_date(moment: datetime):
    return moment.astimezone(ZoneInfo(REPORTING_TIMEZONE)).date()


def current_stage(ends_at: datetime, moment: datetime) -> str | None:
    """The single stage whose calendar day is ``moment``; None for late/early/indefinite cases."""
    delta = (_local_date(ends_at) - _local_date(moment)).days
    for stage, offset in STAGE_OFFSETS.items():
        if delta == offset:
            return stage
    return None


def visibility_until(stage: str, ends_at: datetime) -> datetime:
    """How long the internal message stays visible.

    T-3/T-2/T-1 keep the original deadline. The day-of message stays visible until the next
    service-local midnight so a sweep after an early-morning timestamp still reaches the app on
    the same calendar day (no exact-hour promise, no next-day backfill).
    """
    if stage != "expiry_day":
        return ends_at
    local = ends_at.astimezone(ZoneInfo(REPORTING_TIMEZONE))
    next_midnight = datetime(local.year, local.month, local.day, tzinfo=ZoneInfo(REPORTING_TIMEZONE))
    next_midnight = next_midnight + timedelta(days=1)
    return next_midnight.astimezone(UTC)


def stage_text(
    stage: str, ends_at: datetime, moment: datetime | None = None,
    *, entitlement_kind: str = "paid",
) -> str:
    """Text reflects the ACTUAL deadline in the service calendar (future vs expired)."""
    local_date = _local_date(ends_at).isoformat()
    if entitlement_kind == "trial":
        # A trial can end while a separate paid right remains valid. Use an absolute
        # date: the durable Help message can still be read after its creation time.
        remaining = STAGE_OFFSETS[stage]
        countdown = f" (осталось {remaining} дн.)" if remaining else ""
        return (
            f"Дата окончания пробного доступа: {local_date}{countdown}. "
            "Оплаченная подписка имеет отдельный срок."
        )
    if stage == "expiry_day":
        if moment is not None and ends_at <= moment:
            return f"Ваша подписка истекла сегодня ({local_date}). Продлите её, чтобы восстановить доступ."
        return f"Ваша подписка истекает сегодня ({local_date}). Продлите её, чтобы сохранить доступ."
    remaining = STAGE_OFFSETS[stage]
    return (
        f"Ваша подписка истекает {local_date} (осталось {remaining} дн.). "
        "Продлите её, чтобы сохранить доступ."
    )


# Backwards-compatible single-text helper (old callers/tests).
def reminder_text(ends_at: datetime) -> str:
    return stage_text("pre_expiry_t3", ends_at)


_reminder_text = reminder_text


async def _supersede_stale(
    connection: asyncpg.Connection, settings: Settings, moment: datetime, batch: int
) -> int:
    """Atomically supersede ALL stale-revision reminders and hide their announcements."""
    hidden = await connection.fetchval(
        """
        WITH target AS (
            SELECT r.entitlement_id, r.entitlement_revision, r.kind
            FROM entitlement_reminders AS r
            JOIN entitlements AS e ON e.id = r.entitlement_id
            WHERE r.state = 'active' AND r.entitlement_revision <> e.revision
            ORDER BY r.created_at, r.entitlement_id
            LIMIT $2
        ), superseded AS (
            UPDATE entitlement_reminders AS r
            SET state = 'superseded', superseded_at = $1
            FROM target
            WHERE r.entitlement_id = target.entitlement_id
              AND r.entitlement_revision = target.entitlement_revision
              AND r.kind = target.kind
            RETURNING r.announcement_id
        ), hidden AS (
            UPDATE announcements AS a
            SET show_until = $1
            FROM superseded
            WHERE a.id = superseded.announcement_id
              AND (a.show_until IS NULL OR a.show_until > $1)
            RETURNING 1
        )
        SELECT count(*) FROM hidden
        """,
        moment,
        batch,
    )
    return int(hidden or 0)


async def _create_reminder(
    connection: asyncpg.Connection,
    settings: Settings,
    entitlement_id,
    moment: datetime,
    stage: str | None = None,
) -> bool:
    """Create exactly one stage row for the current authoritative revision, if eligible.

    Re-reads and row-locks the entitlement inside the transaction; the stage is recomputed from
    the fresh deadline, so a renewal between candidate SELECT and insert is never mis-staged.
    """
    lock_key = f"entitlement-reminder:{entitlement_id}"
    async with connection.transaction():
        await connection.execute(
            "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", lock_key
        )
        row = await connection.fetchrow(
            """
            SELECT e.id, e.account_id, e.revision, e.ends_at, e.status, e.kind
            FROM entitlements AS e
            WHERE e.id = $1
            FOR UPDATE
            """,
            entitlement_id,
        )
        if row is None:
            return False
        if (
            row["kind"] not in REMINDER_ELIGIBLE_KINDS
            or row["status"] != "active"
            or row["account_id"] is None
            or row["ends_at"] is None
        ):
            return False
        resolved = current_stage(row["ends_at"], moment)
        if resolved is None:
            return False
        # T-3/T-2/T-1 must still be in the future; day-of is allowed for the WHOLE local end
        # date, including after an early-morning timestamp (no exact-hour promise), while the
        # right is still active. This is what makes "message on the day of expiry" possible.
        if resolved != "expiry_day" and not (row["ends_at"] > moment):
            return False
        if stage is not None and stage != resolved:
            return False
        stage = resolved
        bound = await connection.fetchval(
            """
            SELECT 1 FROM account_bindings AS b
            JOIN installations AS i ON i.id = b.installation_id
            WHERE b.account_id = $1 AND b.status = 'active' AND i.environment = $2
            """,
            row["account_id"],
            settings.environment,
        )
        if bound is None:
            return False
        exists = await connection.fetchval(
            """
            SELECT 1 FROM entitlement_reminders
            WHERE entitlement_id = $1 AND entitlement_revision = $2
              AND stage_end_at = $3 AND kind = $4
            """,
            row["id"],
            row["revision"],
            row["ends_at"],
            stage,
        )
        if exists:
            return False
        announcement_id = await connection.fetchval(
            """
            INSERT INTO announcements (environment, scope_subject_ref, kind, text, show_until)
            VALUES ($1, $2, $3, $4, $5)
            RETURNING id
            """,
            settings.environment,
            row["account_id"],
            stage,
            stage_text(stage, row["ends_at"], moment, entitlement_kind=row["kind"]),
            visibility_until(stage, row["ends_at"]),
        )
        await connection.execute(
            """
            INSERT INTO entitlement_reminders
                (entitlement_id, entitlement_revision, stage_end_at, kind, announcement_id)
            VALUES ($1, $2, $3, $4, $5)
            """,
            row["id"],
            row["revision"],
            row["ends_at"],
            stage,
            announcement_id,
        )
        if _notification_configured(settings):
            await connection.execute(
                """
                INSERT INTO outbox_operations (operation_type, payload, idempotency_key, target_revision)
                VALUES ($1, $2::jsonb, $3, $4)
                ON CONFLICT (idempotency_key) DO NOTHING
                """,
                NOTIFY_OPERATION,
                json.dumps({"announcement_id": str(announcement_id), "entitlement_id": str(row["id"])}),
                f"notify-reminder:{row['id']}:{row['revision']}:{stage}",
                row["revision"],
            )
        return True


async def sweep_entitlement_reminders(
    connection: asyncpg.Connection,
    settings: Settings,
    *,
    now: datetime | None = None,
    batch_size: int | None = None,
) -> dict[str, int]:
    """Supersede stale revisions and create at most one current stage per revision.

    Idempotent under repeated calls/restarts; bounded in both passes and safe to retry.
    """
    moment = now or datetime.now(UTC)
    batch = batch_size or settings.cleanup_batch_size
    superseded = await _supersede_stale(connection, settings, moment, batch)
    stage_case = (
        "CASE (date(e.ends_at AT TIME ZONE $5) - date($1 AT TIME ZONE $5))"
        " WHEN 3 THEN 'pre_expiry_t3' WHEN 2 THEN 'pre_expiry_t2'"
        " WHEN 1 THEN 'pre_expiry_t1' WHEN 0 THEN 'expiry_day' END"
    )
    candidates = await connection.fetch(
        f"""
        SELECT e.id, e.ends_at, {stage_case} AS stage
        FROM entitlements AS e
        WHERE e.kind = ANY($3::text[])
          AND e.status = 'active'
          AND e.account_id IS NOT NULL
          AND e.ends_at IS NOT NULL
          AND (date(e.ends_at AT TIME ZONE $5) - date($1 AT TIME ZONE $5)) BETWEEN 0 AND 3
          AND e.ends_at <= $2
          AND EXISTS (
              SELECT 1 FROM account_bindings AS b
              JOIN installations AS i ON i.id = b.installation_id
              WHERE b.account_id = e.account_id
                AND b.status = 'active'
                AND i.environment = $4
          )
          AND NOT EXISTS (
              SELECT 1 FROM entitlement_reminders AS r
              WHERE r.entitlement_id = e.id
                AND r.entitlement_revision = e.revision
                AND r.stage_end_at = e.ends_at
                AND r.kind = {stage_case}
          )
        ORDER BY e.ends_at, e.id
        LIMIT $6
        """,
        moment,
        moment + timedelta(days=_MAX_LOOKAHEAD_DAYS),
        list(REMINDER_ELIGIBLE_KINDS),
        settings.environment,
        REPORTING_TIMEZONE,
        batch,
    )
    created = 0
    for row in candidates:
        stage = row["stage"] or current_stage(row["ends_at"], moment)
        if stage is None:
            continue  # indefinite or outside the four calendar days: no backfill
        created += 1 if await _create_reminder(connection, settings, row["id"], moment, stage) else 0
    enqueued = await _reconcile_notification_intents(connection, settings, moment, batch)
    return {
        "reminders_created": created,
        "reminders_superseded": superseded,
        "reminder_intents_enqueued": enqueued,
    }


async def _reconcile_notification_intents(
    connection: asyncpg.Connection, settings: Settings, moment: datetime, batch: int
) -> int:
    """Enqueue intents for still-valid active reminders that lack one (e.g. channel enabled late).

    Bounded; rows with any existing intent key (pending/processing/done/dead) are excluded before
    ORDER BY/LIMIT so an earliest already-queued stage cannot starve a later one. Reminders whose
    revision moved on (renewal) are excluded, so enabling after an extension sends nothing stale.
    """
    if not _notification_configured(settings):
        return 0
    inserted = await connection.fetchval(
        """
        WITH target AS (
            SELECT r.announcement_id, e.id AS entitlement_id, e.revision, r.kind
            FROM entitlement_reminders AS r
            JOIN entitlements AS e ON e.id = r.entitlement_id
            JOIN announcements AS a ON a.id = r.announcement_id
            WHERE r.state = 'active'
              AND r.entitlement_revision = e.revision
              AND e.status = 'active'
              AND e.ends_at IS NOT NULL
              AND e.ends_at <= $4  -- safety bound: no far-past backfill
              AND (a.show_until IS NULL OR a.show_until > $1)
              AND (
                  e.ends_at > $1
                  OR (r.kind = 'expiry_day'
                      AND date(e.ends_at AT TIME ZONE $5) = date($1 AT TIME ZONE $5))
              )
              AND NOT EXISTS (
                  SELECT 1 FROM outbox_operations AS o
                  WHERE o.idempotency_key = 'notify-reminder:' || e.id || ':' || e.revision || ':' || r.kind
              )
            ORDER BY e.ends_at, e.id
            LIMIT $2
        ), enqueued AS (
            INSERT INTO outbox_operations (operation_type, payload, idempotency_key, target_revision)
            SELECT $3, jsonb_build_object('announcement_id', target.announcement_id::text,
                                          'entitlement_id', target.entitlement_id::text),
                   'notify-reminder:' || target.entitlement_id || ':' || target.revision || ':' || target.kind,
                   target.revision
            FROM target
            ON CONFLICT (idempotency_key) DO NOTHING
            RETURNING 1
        )
        SELECT count(*) FROM enqueued
        """,
        moment,
        batch,
        NOTIFY_OPERATION,
        moment + timedelta(days=_MAX_LOOKAHEAD_DAYS),
        REPORTING_TIMEZONE,
    )
    return int(inserted or 0)
