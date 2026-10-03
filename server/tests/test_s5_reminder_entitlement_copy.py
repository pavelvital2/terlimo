"""Regression for Help showing trial expiry while a paid right remains valid."""
from datetime import UTC, datetime, timedelta
import pytest
import dataclasses
from test_s5_reminder_notifications import FakeTransport
from terlimo_backend.reminder_notifications import ReminderNotificationHandlers
from test_s5_reminders import _connect, _seed, _settings
from terlimo_backend.reminders import sweep_entitlement_reminders, stage_text, STAGE_OFFSETS

async def test_trial_message_does_not_claim_paid_subscription_expires(migrated_url, settings_factory):
    c = await _connect(migrated_url)
    try:
        trial_end = datetime(2026, 10, 3, 7, 41, 20, 364350, tzinfo=UTC)
        paid_end = datetime(2026, 11, 2, 4, 7, 44, 328474, tzinfo=UTC)
        created = datetime(2026, 10, 2, 21, 2, 49, 211600, tzinfo=UTC)
        terminal = datetime(2026, 10, 3, 19, 5, 9, 744771, tzinfo=UTC)
        account, trial = await _seed(c, ends_at=trial_end, kind="trial", revision=2)
        _, paid = await _seed(c, ends_at=paid_end, kind="paid", account_id=account, bind=False)
        settings = _settings(settings_factory, migrated_url)
        assert (await sweep_entitlement_reminders(c, settings, now=created))["reminders_created"] == 1
        row = await c.fetchrow("SELECT a.text,a.show_until FROM announcements a JOIN entitlement_reminders r ON r.announcement_id=a.id WHERE r.entitlement_id=$1", trial)
        assert row["show_until"] > terminal  # actual observed Help visibility
        assert "пробного доступа" in row["text"]
        assert "2026-10-03" in row["text"]
        assert "Ваша подписка истекает" not in row["text"]
        assert "сохранить доступ" not in row["text"]
        announcement = await c.fetchval("SELECT announcement_id FROM entitlement_reminders WHERE entitlement_id=$1", trial)
        transport = FakeTransport()
        enabled = dataclasses.replace(settings, telegram_notifications_enabled=True, telegram_bot_token="test-only")
        handler = ReminderNotificationHandlers(enabled, transport=transport, clock=lambda: terminal)
        assert await handler.notify_reminder(c, {"payload": {"announcement_id": str(announcement)}}) is None
        assert len(transport.sent) == 1
        assert "пробного доступа" in transport.sent[0][1]
        assert "подписка истекает" not in transport.sent[0][1]
        assert await c.fetchval("SELECT ends_at FROM entitlements WHERE id=$1", paid) == paid_end
        assert (await sweep_entitlement_reminders(c, settings, now=created))["reminders_created"] == 0
        # The genuine paid warning on its own T-3 remains visible and actionable.
        paid_t3 = paid_end - timedelta(days=3)
        assert (await sweep_entitlement_reminders(c, settings, now=paid_t3))["reminders_created"] == 1
        paid_text = await c.fetchval("SELECT a.text FROM announcements a JOIN entitlement_reminders r ON r.announcement_id=a.id WHERE r.entitlement_id=$1", paid)
        assert "Ваша подписка истекает 2026-11-02" in paid_text
        assert "сохранить доступ" in paid_text
    finally:
        await c.close()

@pytest.mark.parametrize("stage", STAGE_OFFSETS)
def test_trial_copy_preserves_four_dates_and_paid_warning(stage):
    end = datetime(2026, 10, 3, 7, 41, tzinfo=UTC)
    trial = stage_text(stage, end, entitlement_kind="trial")
    assert "пробного доступа" in trial and "2026-10-03" in trial
    assert "подписка истекает" not in trial
    assert "Ваша подписка истекает" in stage_text(stage, end)
    if STAGE_OFFSETS[stage]:
        assert f"осталось {STAGE_OFFSETS[stage]} дн." in trial
