"""S5 §11 reminder notification seam: revision fence, recoverable channel state, own recipient."""
from __future__ import annotations

import dataclasses
from datetime import UTC, datetime, timedelta

from terlimo_backend.db import Database
from terlimo_backend.reminder_notifications import (
    NOTIFY_OPERATION,
    ReminderNotificationHandlers,
)
from terlimo_backend.reminders import reminder_text, sweep_entitlement_reminders
from terlimo_backend.worker import OutboxWorker

from test_s5_reminders import _connect, _seed, _settings


class FakeTransport:
    def __init__(self, fail_times: int = 0) -> None:
        self.sent: list[tuple[int, str]] = []
        self.fail_times = fail_times

    async def send_message(self, chat_id: int, text: str) -> None:
        if self.fail_times > 0:
            self.fail_times -= 1
            raise RuntimeError("telegram unavailable")
        self.sent.append((chat_id, text))


def _enabled(settings):
    return dataclasses.replace(
        settings, telegram_notifications_enabled=True, telegram_bot_token="test-bot-token"
    )


async def _ops(connection):
    return await connection.fetch(
        "SELECT idempotency_key, status, attempts, last_error FROM outbox_operations WHERE operation_type=$1 ORDER BY id",
        NOTIFY_OPERATION,
    )


def _worker(database, settings, transport, name):
    handlers = ReminderNotificationHandlers(settings, transport=transport).as_handlers()
    return OutboxWorker(database, settings, handlers=handlers, worker_id=name)


async def test_one_notification_intent_per_revision(migrated_url, settings_factory):
    settings = _enabled(_settings(settings_factory, migrated_url))
    c = await _connect(migrated_url)
    try:
        now = datetime.now(UTC)
        await _seed(c, ends_at=now + timedelta(days=2))
        assert (await sweep_entitlement_reminders(c, settings, now=now))["reminders_created"] == 1
        assert (await sweep_entitlement_reminders(c, settings, now=now + timedelta(minutes=1)))["reminders_created"] == 0
        ops = await _ops(c)
        assert len(ops) == 1 and ops[0]["idempotency_key"].startswith("notify-reminder:")
        assert await c.fetchval("SELECT count(*) FROM announcements WHERE kind = ANY(ARRAY['pre_expiry_t3','pre_expiry_t2','pre_expiry_t1','expiry_day'])") == 1
    finally:
        await c.close()


async def test_disabled_channel_does_not_consume_intent_and_later_enable_sends_once(
    migrated_url, settings_factory
):
    base = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    database = Database(base)
    await database.connect()
    try:
        now = datetime.now(UTC)
        await _seed(c, ends_at=now + timedelta(days=2))
        assert (await sweep_entitlement_reminders(c, base, now=now))["reminders_created"] == 1
        assert await _ops(c) == []  # channel off -> no intent is consumed
        transport = FakeTransport()
        assert await _worker(database, base, transport, "notify-off").drain() == 0
        assert transport.sent == []
        # Later enable while the same revision is still current -> exactly one send.
        enabled = _enabled(base)
        assert (await sweep_entitlement_reminders(c, enabled, now=now + timedelta(minutes=1)))["reminder_intents_enqueued"] == 1
        assert await _worker(database, enabled, transport, "notify-on").drain() == 1
        assert len(transport.sent) == 1
        ops = await _ops(c)
        assert len(ops) == 1 and ops[0]["status"] == "done"
        assert await _worker(database, enabled, transport, "notify-on2").drain() == 0
    finally:
        await database.close()
        await c.close()


async def test_disabled_then_renewed_before_enable_produces_no_stale_send(migrated_url, settings_factory):
    base = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    database = Database(base)
    await database.connect()
    try:
        now = datetime.now(UTC)
        account_id, entitlement_id = await _seed(c, ends_at=now + timedelta(days=2))
        assert (await sweep_entitlement_reminders(c, base, now=now))["reminders_created"] == 1
        # Extension happens while the channel is still off; new deadline is outside the window.
        await c.execute(
            "UPDATE entitlements SET ends_at=$2, revision=revision+1 WHERE id=$1",
            entitlement_id,
            now + timedelta(days=20),
        )
        enabled = _enabled(base)
        swept = await sweep_entitlement_reminders(c, enabled, now=now + timedelta(hours=1))
        assert swept["reminders_superseded"] == 1 and swept["reminder_intents_enqueued"] == 0
        transport = FakeTransport()
        assert await _worker(database, enabled, transport, "notify-stale").drain() == 0
        assert transport.sent == []
        assert await c.fetchval("SELECT count(*) FROM outbox_operations WHERE operation_type=$1", NOTIFY_OPERATION) == 0
    finally:
        await database.close()
        await c.close()


async def test_stale_queued_intent_is_fenced_after_renewal(migrated_url, settings_factory):
    settings = _enabled(_settings(settings_factory, migrated_url))
    c = await _connect(migrated_url)
    database = Database(settings)
    await database.connect()
    try:
        now = datetime.now(UTC)
        account_id, entitlement_id = await _seed(c, ends_at=now + timedelta(days=2))
        assert (await sweep_entitlement_reminders(c, settings, now=now))["reminders_created"] == 1
        old_text = await c.fetchval("SELECT text FROM announcements WHERE kind = ANY(ARRAY['pre_expiry_t3','pre_expiry_t2','pre_expiry_t1','expiry_day'])")
        assert "истекает" in old_text
        # Renewal before the worker drains: the queued external send must not deliver the old date.
        new_end = now + timedelta(days=6)
        await c.execute(
            "UPDATE entitlements SET ends_at=$2, revision=revision+1 WHERE id=$1", entitlement_id, new_end
        )
        transport = FakeTransport()
        assert await _worker(database, settings, transport, "notify-fenced").drain() == 1
        assert transport.sent == []
        ops = await _ops(c)
        assert len(ops) == 1 and ops[0]["status"] == "done"
    finally:
        await database.close()
        await c.close()


async def test_reminder_text_uses_moscow_calendar_date(migrated_url, settings_factory):
    late_utc = datetime(2026, 12, 31, 22, 30, tzinfo=UTC)  # 2027-01-01 in Europe/Moscow
    assert "2027-01-01" in reminder_text(late_utc)
    settings = _enabled(_settings(settings_factory, migrated_url))
    c = await _connect(migrated_url)
    try:
        now = late_utc - timedelta(days=1)
        await _seed(c, ends_at=late_utc)
        assert (await sweep_entitlement_reminders(c, settings, now=now))["reminders_created"] == 1
        text = await c.fetchval("SELECT text FROM announcements WHERE kind = ANY(ARRAY['pre_expiry_t3','pre_expiry_t2','pre_expiry_t1','expiry_day'])")
        assert "2027-01-01" in text
    finally:
        await c.close()


async def test_only_own_verified_recipient_is_addressed(migrated_url, settings_factory):
    settings = _enabled(_settings(settings_factory, migrated_url))
    c = await _connect(migrated_url)
    database = Database(settings)
    await database.connect()
    try:
        now = datetime.now(UTC)
        a_id, _ = await _seed(c, ends_at=now + timedelta(days=2))
        b_id, _ = await _seed(c, ends_at=now + timedelta(days=2, hours=1))
        a_tg = await c.fetchval("SELECT telegram_id FROM accounts WHERE id=$1", a_id)
        b_tg = await c.fetchval("SELECT telegram_id FROM accounts WHERE id=$1", b_id)
        assert (await sweep_entitlement_reminders(c, settings, now=now))["reminders_created"] == 2
        transport = FakeTransport()
        assert await _worker(database, settings, transport, "notify-own").drain() == 2
        assert {chat for chat, _ in transport.sent} == {a_tg, b_tg} and len(transport.sent) == 2
    finally:
        await database.close()
        await c.close()


async def test_retry_without_duplicate_intent_and_eventual_single_send(migrated_url, settings_factory):
    settings = _enabled(_settings(settings_factory, migrated_url))
    c = await _connect(migrated_url)
    database = Database(settings)
    await database.connect()
    try:
        now = datetime.now(UTC)
        await _seed(c, ends_at=now + timedelta(days=2))
        assert (await sweep_entitlement_reminders(c, settings, now=now))["reminders_created"] == 1
        transport = FakeTransport(fail_times=1)
        assert await _worker(database, settings, transport, "notify-a").drain() == 1
        ops = await _ops(c)
        assert len(ops) == 1 and ops[0]["status"] == "pending" and ops[0]["attempts"] == 1
        assert transport.sent == []
        await c.execute("UPDATE outbox_operations SET available_at=now() WHERE operation_type=$1", NOTIFY_OPERATION)
        assert await _worker(database, settings, transport, "notify-b").drain() == 1
        ops = await _ops(c)
        assert len(ops) == 1 and ops[0]["status"] == "done"
        assert len(transport.sent) == 1
    finally:
        await database.close()
        await c.close()


async def test_unbound_recipient_is_skipped_not_sent(migrated_url, settings_factory):
    settings = _enabled(_settings(settings_factory, migrated_url))
    c = await _connect(migrated_url)
    database = Database(settings)
    await database.connect()
    try:
        now = datetime.now(UTC)
        account_id, _ = await _seed(c, ends_at=now + timedelta(days=2))
        assert (await sweep_entitlement_reminders(c, settings, now=now))["reminders_created"] == 1
        await c.execute("UPDATE account_bindings SET status='revoked' WHERE account_id=$1", account_id)
        transport = FakeTransport()
        assert await _worker(database, settings, transport, "notify-unbound").drain() == 1
        assert transport.sent == []
        assert (await _ops(c))[0]["status"] == "done"
    finally:
        await database.close()
        await c.close()


async def test_reconcile_pending_first_does_not_starve_later_reminder(migrated_url, settings_factory):
    base = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        now = datetime.now(UTC)
        await _seed(c, ends_at=now + timedelta(days=1))
        await _seed(c, ends_at=now + timedelta(days=1, hours=1))
        assert (await sweep_entitlement_reminders(c, base, now=now))["reminders_created"] == 2
        enabled = _enabled(base)
        # batch_size=1: the first (earliest) reminder gets its intent, stays pending, and must not
        # block the second reminder from being queued on the next bounded sweep.
        assert (await sweep_entitlement_reminders(c, enabled, now=now, batch_size=1))["reminder_intents_enqueued"] == 1
        assert (await sweep_entitlement_reminders(c, enabled, now=now, batch_size=1))["reminder_intents_enqueued"] == 1
        ops = await _ops(c)
        assert len(ops) == 2 and len({op["idempotency_key"] for op in ops}) == 2
        assert all(op["status"] == "pending" for op in ops)
        assert (await sweep_entitlement_reminders(c, enabled, now=now, batch_size=1))["reminder_intents_enqueued"] == 0
    finally:
        await c.close()


async def test_reconcile_done_first_does_not_starve_later_reminder(migrated_url, settings_factory):
    base = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    database = Database(base)
    await database.connect()
    try:
        now = datetime.now(UTC)
        await _seed(c, ends_at=now + timedelta(days=1))
        await _seed(c, ends_at=now + timedelta(days=1, hours=1))
        assert (await sweep_entitlement_reminders(c, base, now=now))["reminders_created"] == 2
        enabled = _enabled(base)
        assert (await sweep_entitlement_reminders(c, enabled, now=now, batch_size=1))["reminder_intents_enqueued"] == 1
        transport = FakeTransport()
        assert await _worker(database, enabled, transport, "notify-done-first").drain() == 1
        assert (await _ops(c))[0]["status"] == "done"
        # A done intent is still excluded, so the later valid reminder is enqueued next.
        assert (await sweep_entitlement_reminders(c, enabled, now=now, batch_size=1))["reminder_intents_enqueued"] == 1
        assert await _worker(database, enabled, transport, "notify-done-second").drain() == 1
        assert len(transport.sent) == 2
        assert all(op["status"] == "done" for op in await _ops(c))
    finally:
        await database.close()
        await c.close()


async def test_channel_toggle_fenced_delete_then_reenable_sends_once(migrated_url, settings_factory):
    base = _settings(settings_factory, migrated_url)
    enabled = _enabled(base)
    c = await _connect(migrated_url)
    database = Database(base)
    await database.connect()
    try:
        now = datetime.now(UTC)
        await _seed(c, ends_at=now + timedelta(days=2))
        assert (await sweep_entitlement_reminders(c, enabled, now=now))["reminders_created"] == 1
        assert len(await _ops(c)) == 1
        # Channel switched off with a queued intent: the claimed handler releases it, no send.
        transport = FakeTransport()
        assert await _worker(database, base, transport, "notify-toggle-off").drain() == 1
        assert transport.sent == []
        assert await _ops(c) == []
        # Re-enable: reconciliation re-queues exactly one valid intent; one send, no tight loop.
        assert (await sweep_entitlement_reminders(c, enabled, now=now + timedelta(minutes=1)))["reminder_intents_enqueued"] == 1
        assert await _worker(database, enabled, transport, "notify-toggle-on").drain() == 1
        assert len(transport.sent) == 1
        assert (await _ops(c))[0]["status"] == "done"
        assert await _worker(database, enabled, transport, "notify-toggle-on2").drain() == 0
    finally:
        await database.close()
        await c.close()
