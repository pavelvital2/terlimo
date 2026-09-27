"""S5 §11 four-stage reminder cadence (T-3/T-2/T-1/day-of): boundaries, renewal, idempotency."""
from __future__ import annotations

import dataclasses
from datetime import UTC, datetime, timedelta
from zoneinfo import ZoneInfo

from test_s5_reminders import _connect, _seed, _settings

from terlimo_backend.reminders import (
    REMINDER_ELIGIBLE_KINDS,
    STAGE_KINDS,
    current_stage,
    stage_text,
    sweep_entitlement_reminders,
)

MSK = ZoneInfo("Europe/Moscow")


def _msk(day: datetime.date, hour: int = 12) -> datetime:
    return datetime(day.year, day.month, day.day, hour, tzinfo=MSK)


def _base():
    return datetime(2026, 10, 15, tzinfo=MSK).date()


async def _stages(connection, entitlement_id):
    rows = await connection.fetch(
        "SELECT kind, state, stage_end_at FROM entitlement_reminders WHERE entitlement_id=$1 ORDER BY kind",
        entitlement_id,
    )
    return [(r["kind"], r["state"]) for r in rows]


async def test_four_calendar_boundaries_paid_and_trial(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        base = _base()
        expectations = {"pre_expiry_t3": 3, "pre_expiry_t2": 2, "pre_expiry_t1": 1, "expiry_day": 0}
        index = 0
        for kind in REMINDER_ELIGIBLE_KINDS:
            for stage, offset in expectations.items():
                # isolate each case on its own deadline so earlier fixtures do not overlap the sweep
                end_date = base + timedelta(days=index * 10)
                ends = _msk(end_date)
                _, entitlement_id = await _seed(c, ends_at=ends, kind=kind)
                moment = _msk(end_date - timedelta(days=offset), hour=10)
                index += 1
                assert current_stage(ends, moment) == stage
                created = await sweep_entitlement_reminders(c, settings, now=moment)
                assert created["reminders_created"] == 1, (kind, stage)
                assert await _stages(c, entitlement_id) == [(stage, "active")]
                # adjacent calendar days never create a second stage for the same date
                assert (await sweep_entitlement_reminders(c, settings, now=moment))["reminders_created"] == 0
    finally:
        await c.close()


async def test_indefinite_right_has_no_stage(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        _, entitlement_id = await _seed(c, ends_at=None, kind="paid")
        now = datetime.now(UTC)
        assert (await sweep_entitlement_reminders(c, settings, now=now))["reminders_created"] == 0
        assert await _stages(c, entitlement_id) == []
    finally:
        await c.close()


async def test_late_install_creates_only_current_stage_no_backfill(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        base = _base()
        # entitlement appears/sweep runs only on the T-1 calendar day; T-3/T-2 are already past
        ends = _msk(base)
        _, entitlement_id = await _seed(c, ends_at=ends, kind="paid")
        moment = _msk(base - timedelta(days=1), hour=10)
        assert (await sweep_entitlement_reminders(c, settings, now=moment))["reminders_created"] == 1
        assert await _stages(c, entitlement_id) == [("pre_expiry_t1", "active")]
    finally:
        await c.close()


async def test_renewal_cancels_future_stages_and_keeps_new_revision(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        base = _base()
        ends = _msk(base)
        account_id, entitlement_id = await _seed(c, ends_at=ends, kind="paid")
        t3 = _msk(base - timedelta(days=3), hour=10)
        assert (await sweep_entitlement_reminders(c, settings, now=t3))["reminders_created"] == 1
        old_announcement = await c.fetchval(
            "SELECT announcement_id FROM entitlement_reminders WHERE entitlement_id=$1", entitlement_id
        )
        # Renewal before the next stage: new revision and later deadline.
        new_end = _msk(base + timedelta(days=30))
        await c.execute(
            "UPDATE entitlements SET ends_at=$2, revision=revision+1 WHERE id=$1", entitlement_id, new_end
        )
        swept = await sweep_entitlement_reminders(c, settings, now=t3 + timedelta(hours=2))
        assert swept["reminders_superseded"] == 1
        assert await _stages(c, entitlement_id) == [("pre_expiry_t3", "superseded")]
        # the superseded announcement is hidden and never re-created for the old deadline
        assert await c.fetchval(
            "SELECT show_until FROM announcements WHERE id=$1", old_announcement
        ) <= t3 + timedelta(hours=2)
        assert (await sweep_entitlement_reminders(c, settings, now=_msk(base - timedelta(days=2), hour=10)))[
            "reminders_created"
        ] == 0
        # the new term gets its own single T-3 stage exactly on its date
        new_t3 = datetime(new_end.year, new_end.month, new_end.day, 10, tzinfo=MSK) - timedelta(days=3)
        assert (await sweep_entitlement_reminders(c, settings, now=new_t3))["reminders_created"] == 1
        states = await _stages(c, entitlement_id)
        assert ("pre_expiry_t3", "superseded") in states and states.count(("pre_expiry_t3", "active")) == 1
        visible = await c.fetch(
            "SELECT 1 FROM announcements WHERE scope_subject_ref=$1 AND (show_until IS NULL OR show_until > $2)",
            account_id,
            new_t3,
        )
        assert len(visible) == 1
    finally:
        await c.close()


async def test_created_outbox_intent_is_not_delivery(migrated_url, settings_factory):
    base_settings = _settings(settings_factory, migrated_url)
    enabled = dataclasses.replace(base_settings, telegram_notifications_enabled=True, telegram_bot_token="t")
    c = await _connect(migrated_url)
    try:
        base = _base()
        ends = _msk(base)
        moment = _msk(base - timedelta(days=2), hour=10)
        # channel disabled: internal message exists, no outbox intent, nothing delivered
        _, ent_off = await _seed(c, ends_at=ends, kind="paid")
        assert (await sweep_entitlement_reminders(c, base_settings, now=moment))["reminders_created"] == 1
        assert await c.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE idempotency_key LIKE $1", f"notify-reminder:{ent_off}:%"
        ) == 0
        assert await c.fetchval(
            "SELECT count(*) FROM announcements WHERE scope_subject_ref=(SELECT account_id FROM entitlements WHERE id=$1)",
            ent_off,
        ) == 1
        # channel enabled: exactly one intent for the stage, still not a delivery claim
        _, ent_on = await _seed(c, ends_at=ends, kind="paid")
        assert (await sweep_entitlement_reminders(c, enabled, now=moment))["reminder_intents_enqueued"] == 1
        assert await c.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE idempotency_key LIKE $1", f"notify-reminder:{ent_on}:%"
        ) == 1
        # no delivered marker is fabricated by creating the intent
        cols = [r["column_name"] for r in await c.fetch(
            "SELECT column_name FROM information_schema.columns WHERE table_name='outbox_operations'"
        )]
        assert "delivered_at" not in cols
    finally:
        await c.close()


def test_stage_text_future_and_day_of():
    base = _base()
    ends = _msk(base)
    assert "осталось 3 дн." in stage_text("pre_expiry_t3", ends)
    assert "осталось 1 дн." in stage_text("pre_expiry_t1", ends)
    day_of = stage_text("expiry_day", ends)
    assert "сегодня" in day_of and ends.astimezone(MSK).date().isoformat() in day_of
    assert STAGE_KINDS == ("pre_expiry_t3", "pre_expiry_t2", "pre_expiry_t1", "expiry_day")


async def test_single_right_walks_all_four_stages_no_backfill(migrated_url, settings_factory):
    base_settings = _settings(settings_factory, migrated_url)
    enabled = dataclasses.replace(base_settings, telegram_notifications_enabled=True, telegram_bot_token="t")
    c = await _connect(migrated_url)
    try:
        base = _base()
        ends = _msk(base)
        for kind in REMINDER_ELIGIBLE_KINDS:
            account_id, entitlement_id = await _seed(c, ends_at=ends, kind=kind)
            seen = []
            for stage, offset in (("pre_expiry_t3", 3), ("pre_expiry_t2", 2),
                                  ("pre_expiry_t1", 1), ("expiry_day", 0)):
                moment = _msk(base - timedelta(days=offset), hour=10)
                assert (await sweep_entitlement_reminders(c, enabled, now=moment))["reminders_created"] == 1
                # repeat sweep on the same date (retry) and a simulated restart create nothing new
                assert (await sweep_entitlement_reminders(c, enabled, now=moment))["reminders_created"] == 0
                assert (await sweep_entitlement_reminders(c, enabled, now=moment + timedelta(minutes=5)))[
                    "reminders_created"
                ] == 0
                seen.append(stage)
            rows = await c.fetch(
                "SELECT kind, state FROM entitlement_reminders WHERE entitlement_id=$1 ORDER BY stage_end_at, kind",
                entitlement_id,
            )
            assert {r["kind"] for r in rows} == set(seen) and len(rows) == 4
            assert all(r["state"] == "active" for r in rows)
            assert len(await c.fetch(
                "SELECT 1 FROM announcements WHERE scope_subject_ref=$1", account_id
            )) == 4
            per_stage = await c.fetch(
                "SELECT idempotency_key FROM outbox_operations WHERE idempotency_key LIKE $1",
                f"notify-reminder:{entitlement_id}:%",
            )
            assert len(per_stage) == 4 and len({r["idempotency_key"] for r in per_stage}) == 4
    finally:
        await c.close()


async def test_expiry_day_fires_after_early_timestamp_with_expired_text(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        base = _base()
        ends = _msk(base, hour=8)  # early-morning deadline
        _, entitlement_id = await _seed(c, ends_at=ends, kind="paid")
        # before the timestamp: future wording
        before = _msk(base, hour=6)
        assert (await sweep_entitlement_reminders(c, settings, now=before))["reminders_created"] == 1
        text = await c.fetchval(
            "SELECT text FROM announcements WHERE scope_subject_ref=(SELECT account_id FROM entitlements WHERE id=$1)",
            entitlement_id,
        )
        assert "истекает сегодня" in text
        # a second right, swept only AFTER the early timestamp on the SAME calendar day
        _, ent_after = await _seed(c, ends_at=ends, kind="trial")
        after = _msk(base, hour=10)
        assert (await sweep_entitlement_reminders(c, settings, now=after))["reminders_created"] == 1
        text_after = await c.fetchval(
            "SELECT text FROM announcements WHERE scope_subject_ref=(SELECT account_id FROM entitlements WHERE id=$1)",
            ent_after,
        )
        assert "истекла сегодня" in text_after
        assert await _stages(c, ent_after) == [("expiry_day", "active")]
    finally:
        await c.close()


class _FakeTransport:
    def __init__(self):
        self.sent = []

    async def send_message(self, chat_id, text):
        self.sent.append((chat_id, text))


def _today_local_at(hour: int, minute: int = 0) -> datetime:
    today = datetime.now(UTC).astimezone(MSK).date()
    return datetime(today.year, today.month, today.day, hour, minute, tzinfo=MSK)


async def test_expiry_day_full_path_after_early_timestamp_same_day(migrated_url, settings_factory):
    """One controlled (real same-day) clock domain: GET visibility, intent, handler send, no dup."""
    from test_s5_announcements import ANN, _app, _session

    from terlimo_backend.reminder_notifications import ReminderNotificationHandlers

    base_settings = _settings(settings_factory, migrated_url)
    enabled = dataclasses.replace(base_settings, telegram_notifications_enabled=True, telegram_bot_token="t")
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        now = datetime.now(UTC)
        # ends_at early TODAY (already passed), so this is the post-timestamp day-of case
        ends = _today_local_at(0, 5).astimezone(UTC)
        assert ends <= now
        token = await _session(client, migrated_url, bind_tg=991301)
        account_id = await c.fetchval("SELECT id FROM accounts WHERE telegram_id=$1", 991301)
        entitlement_id = await c.fetchval(
            """
            INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at, device_limit, revision)
            VALUES ($1,'paid','active', now() - interval '1 day', $2, 2, 1) RETURNING id
            """,
            account_id, ends,
        )
        # (1) session-auth GET shows the message with the expired wording, visible until next midnight
        assert (await sweep_entitlement_reminders(c, enabled, now=now))["reminders_created"] == 1
        body = await (await client.get(ANN, headers={"Authorization": f"Bearer {token}"})).json()
        texts = [a["text"] for a in body["announcements"]]
        assert any("истекла сегодня" in t for t in texts), texts
        show_until = await c.fetchval(
            "SELECT show_until FROM announcements WHERE scope_subject_ref=(SELECT account_id FROM entitlements WHERE id=$1)",
            entitlement_id,
        )
        assert show_until > now
        # (2) exactly one outbox intent for the day-of stage
        ops = await c.fetch(
            "SELECT * FROM outbox_operations WHERE idempotency_key=$1",
            f"notify-reminder:{entitlement_id}:1:expiry_day",
        )
        assert len(ops) == 1
        # (3) handler in the SAME clock domain sends once, expired wording, no duplicate intent
        transport = _FakeTransport()
        handler = ReminderNotificationHandlers(enabled, transport=transport, clock=lambda: now).notify_reminder
        assert await handler(c, ops[0]) is None
        assert len(transport.sent) == 1 and "истекла сегодня" in transport.sent[0][1]
        assert await c.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE idempotency_key=$1",
            f"notify-reminder:{entitlement_id}:1:expiry_day",
        ) == 1
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_expiry_day_created_before_timestamp_then_next_day_no_stale_send(migrated_url, settings_factory):
    from terlimo_backend.reminder_notifications import ReminderNotificationHandlers

    base_settings = _settings(settings_factory, migrated_url)
    enabled = dataclasses.replace(base_settings, telegram_notifications_enabled=True, telegram_bot_token="t")
    c = await _connect(migrated_url)
    try:
        # (4) creation shortly BEFORE the timestamp, same day
        ends = _today_local_at(23, 30).astimezone(UTC)
        if ends <= datetime.now(UTC):
            ends = _today_local_at(23, 59).astimezone(UTC)
        _, entitlement_id = await _seed(c, ends_at=ends, kind="trial")
        sweep = await sweep_entitlement_reminders(c, enabled, now=datetime.now(UTC))
        assert sweep["reminders_created"] == 1
        op = await c.fetchrow(
            "SELECT * FROM outbox_operations WHERE idempotency_key=$1",
            f"notify-reminder:{entitlement_id}:1:expiry_day",
        )
        transport = _FakeTransport()
        # drain the day AFTER (after next local midnight): must not send stale
        tomorrow = _msk(_today_local_at(12).date() + timedelta(days=1), hour=12)
        handler = ReminderNotificationHandlers(enabled, transport=transport, clock=lambda: tomorrow).notify_reminder
        outcome = await handler(c, op)
        assert outcome is not None and transport.sent == []
        # (5) next-day invisibility: show_until (next local midnight) is now in the past
        show_until = await c.fetchval(
            "SELECT show_until FROM announcements WHERE scope_subject_ref=(SELECT account_id FROM entitlements WHERE id=$1)",
            entitlement_id,
        )
        assert show_until <= tomorrow
        # (6) a renewal before a same-day drain cancels the old stage and prevents its send
        ends2 = _today_local_at(0, 5).astimezone(UTC)
        _, ent_ren = await _seed(c, ends_at=ends2, kind="paid")
        now = datetime.now(UTC)
        assert (await sweep_entitlement_reminders(c, enabled, now=now))["reminders_created"] == 1
        op2 = await c.fetchrow(
            "SELECT * FROM outbox_operations WHERE idempotency_key=$1",
            f"notify-reminder:{ent_ren}:1:expiry_day",
        )
        await c.execute(
            "UPDATE entitlements SET ends_at=$2, revision=revision+1 WHERE id=$1",
            ent_ren, now + timedelta(days=30),
        )
        await sweep_entitlement_reminders(c, enabled, now=now + timedelta(minutes=1))
        transport2 = _FakeTransport()
        handler2 = ReminderNotificationHandlers(enabled, transport=transport2, clock=lambda: now).notify_reminder
        assert await handler2(c, op2) is not None and transport2.sent == []
    finally:
        await c.close()


async def test_day_of_created_before_timestamp_drained_after_same_day(migrated_url, settings_factory):
    """before < ends_at < after < next local midnight, all one service-local date, one clock domain."""
    from terlimo_backend.reminder_notifications import ReminderNotificationHandlers

    base_settings = _settings(settings_factory, migrated_url)
    enabled = dataclasses.replace(base_settings, telegram_notifications_enabled=True, telegram_bot_token="t")
    c = await _connect(migrated_url)
    try:
        day = (datetime.now(UTC).astimezone(MSK).date() + timedelta(days=1))
        ends = datetime(day.year, day.month, day.day, 12, 0, tzinfo=MSK)
        before = datetime(day.year, day.month, day.day, 11, 55, tzinfo=MSK)
        after = datetime(day.year, day.month, day.day, 12, 10, tzinfo=MSK)
        next_midnight = datetime(day.year, day.month, day.day, tzinfo=MSK) + timedelta(days=1)
        assert before < ends < after < next_midnight
        _, entitlement_id = await _seed(c, ends_at=ends, kind="paid")
        first = await sweep_entitlement_reminders(c, enabled, now=before)
        assert first["reminders_created"] == 1 and first["reminder_intents_enqueued"] == 0
        assert await sweep_entitlement_reminders(c, enabled, now=before) == {
            "reminders_created": 0, "reminders_superseded": 0, "reminder_intents_enqueued": 0,
        }
        ops = await c.fetch(
            "SELECT * FROM outbox_operations WHERE idempotency_key=$1",
            f"notify-reminder:{entitlement_id}:1:expiry_day",
        )
        assert len(ops) == 1
        show_until = await c.fetchval(
            "SELECT show_until FROM announcements WHERE scope_subject_ref=(SELECT account_id FROM entitlements WHERE id=$1)",
            entitlement_id,
        )
        assert show_until == next_midnight.astimezone(UTC) and show_until > after
        transport = _FakeTransport()
        handler = ReminderNotificationHandlers(enabled, transport=transport, clock=lambda: after).notify_reminder
        assert await handler(c, ops[0]) is None
        assert len(transport.sent) == 1 and "истекла сегодня" in transport.sent[0][1]
        assert await c.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE idempotency_key LIKE $1",
            f"notify-reminder:{entitlement_id}:%",
        ) == 1
        assert (await sweep_entitlement_reminders(c, enabled, now=after))["reminders_created"] == 0
    finally:
        await c.close()


async def test_day_of_channel_off_then_enabled_after_timestamp_reconciles_once(migrated_url, settings_factory):
    from terlimo_backend.reminder_notifications import ReminderNotificationHandlers

    base_settings = _settings(settings_factory, migrated_url)
    enabled = dataclasses.replace(base_settings, telegram_notifications_enabled=True, telegram_bot_token="t")
    c = await _connect(migrated_url)
    try:
        day = (datetime.now(UTC).astimezone(MSK).date() + timedelta(days=1))
        ends = datetime(day.year, day.month, day.day, 12, 0, tzinfo=MSK)
        before = datetime(day.year, day.month, day.day, 11, 55, tzinfo=MSK)
        after = datetime(day.year, day.month, day.day, 12, 10, tzinfo=MSK)
        _, entitlement_id = await _seed(c, ends_at=ends, kind="trial")
        # channel OFF at creation: message exists, no intent
        assert (await sweep_entitlement_reminders(c, base_settings, now=before))["reminders_created"] == 1
        assert await c.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE idempotency_key LIKE $1",
            f"notify-reminder:{entitlement_id}:%",
        ) == 0
        # enabled AFTER the timestamp, same local day: bounded reconciliation enqueues exactly one
        swept = await sweep_entitlement_reminders(c, enabled, now=after)
        assert swept["reminder_intents_enqueued"] == 1 and swept["reminders_created"] == 0
        assert (await sweep_entitlement_reminders(c, enabled, now=after))["reminder_intents_enqueued"] == 0
        op = await c.fetchrow(
            "SELECT * FROM outbox_operations WHERE idempotency_key=$1",
            f"notify-reminder:{entitlement_id}:1:expiry_day",
        )
        transport = _FakeTransport()
        handler = ReminderNotificationHandlers(enabled, transport=transport, clock=lambda: after).notify_reminder
        assert await handler(c, op) is None
        assert len(transport.sent) == 1 and "истекла сегодня" in transport.sent[0][1]
    finally:
        await c.close()
