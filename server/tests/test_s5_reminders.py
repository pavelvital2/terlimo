"""S5 §11 entitlement pre-expiry reminder sweep: durable provenance, supersession, isolation."""
from __future__ import annotations

import uuid
from datetime import UTC, datetime, timedelta

from terlimo_backend.reminders import STAGE_KINDS, _create_reminder, _supersede_stale, sweep_entitlement_reminders

from test_step036_onboarding_hour_storage import _connect


def _settings(settings_factory, url):
    return settings_factory(url)


async def _seed(
    connection,
    *,
    ends_at,
    kind="paid",
    status="active",
    revision=1,
    bind=True,
    environment="test",
    account_id=None,
):
    if account_id is None:
        account_id = await connection.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified',$1) RETURNING id",
            980000000 + abs(hash(str(uuid.uuid4()))) % 1000000,
        )
    if bind:
        fingerprint = f"fp-{uuid.uuid4().hex}"
        installation_id = await connection.fetchval(
            "INSERT INTO installations (environment, public_key_fingerprint) VALUES ($1,$2) RETURNING id",
            environment,
            fingerprint,
        )
        await connection.execute(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')",
            account_id,
            installation_id,
        )
    entitlement_id = await connection.fetchval(
        """
        INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at, device_limit, revision)
        VALUES ($1,$2,$3, now() - interval '1 day', $4, 2, $5)
        RETURNING id
        """,
        account_id,
        kind,
        status,
        ends_at,
        revision,
    )
    return account_id, entitlement_id


async def _visible(connection, account_id, moment):
    return await connection.fetch(
        "SELECT id, text, show_until FROM announcements WHERE scope_subject_ref=$1 AND (show_until IS NULL OR show_until > $2)",
        account_id,
        moment,
    )


async def test_before_window_creates_nothing(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        now = datetime.now(UTC)
        account_id, entitlement_id = await _seed(c, ends_at=now + timedelta(days=10))
        assert await sweep_entitlement_reminders(c, settings, now=now) == {
            "reminders_created": 0,
            "reminders_superseded": 0,
            "reminder_intents_enqueued": 0,
        }
        assert await c.fetchval("SELECT count(*) FROM entitlement_reminders") == 0
        assert await _visible(c, account_id, now) == []
    finally:
        await c.close()


async def test_enter_window_creates_once_and_repeat_sweep_is_idempotent(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        now = datetime.now(UTC)
        ends_at = now + timedelta(days=2)
        account_id, entitlement_id = await _seed(c, ends_at=ends_at)
        first = await sweep_entitlement_reminders(c, settings, now=now)
        assert first == {"reminders_created": 1, "reminders_superseded": 0, "reminder_intents_enqueued": 0}
        again = await sweep_entitlement_reminders(c, settings, now=now + timedelta(minutes=5))
        assert again == {"reminders_created": 0, "reminders_superseded": 0, "reminder_intents_enqueued": 0}
        rows = await c.fetch("SELECT entitlement_revision, state, announcement_id FROM entitlement_reminders")
        assert len(rows) == 1 and rows[0]["entitlement_revision"] == 1 and rows[0]["state"] == "active"
        visible = await _visible(c, account_id, now)
        assert len(visible) == 1 and "истекает" in visible[0]["text"]
        assert visible[0]["show_until"] == ends_at
    finally:
        await c.close()


async def test_extension_supersedes_old_and_creates_exactly_one_new(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        now = datetime.now(UTC)
        account_id, entitlement_id = await _seed(c, ends_at=now + timedelta(days=2))
        assert (await sweep_entitlement_reminders(c, settings, now=now))["reminders_created"] == 1
        old_id = await c.fetchval("SELECT announcement_id FROM entitlement_reminders WHERE entitlement_id=$1", entitlement_id)
        # Renewal/extension bumps the authoritative revision and moves the deadline out of window.
        new_end = now + timedelta(days=6)
        await c.execute(
            "UPDATE entitlements SET ends_at=$2, revision=revision+1 WHERE id=$1", entitlement_id, new_end
        )
        swept = await sweep_entitlement_reminders(c, settings, now=now + timedelta(hours=1))
        assert swept == {"reminders_created": 0, "reminders_superseded": 1, "reminder_intents_enqueued": 0}
        # Old reminder is superseded and its announcement is hidden immediately.
        assert await c.fetchval("SELECT state FROM entitlement_reminders WHERE announcement_id=$1", old_id) == "superseded"
        assert await c.fetchval("SELECT show_until FROM announcements WHERE id=$1", old_id) <= now + timedelta(hours=1)
        assert await _visible(c, account_id, now + timedelta(hours=1)) == []
        assert await c.fetchval("SELECT count(*) FROM entitlement_reminders") == 1
        # Once the new revision enters the window, exactly one new reminder exists.
        in_window = new_end - timedelta(days=1)
        assert (await sweep_entitlement_reminders(c, settings, now=in_window))["reminders_created"] == 1
        assert (await sweep_entitlement_reminders(c, settings, now=in_window + timedelta(minutes=1)))["reminders_created"] == 0
        states = await c.fetch("SELECT entitlement_revision, state FROM entitlement_reminders ORDER BY entitlement_revision")
        assert [(r["entitlement_revision"], r["state"]) for r in states] == [(1, "superseded"), (2, "active")]
        visible = await _visible(c, account_id, in_window)
        assert len(visible) == 1 and visible[0]["show_until"] == new_end
    finally:
        await c.close()


async def test_account_isolation_and_no_cross_account_marker(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        now = datetime.now(UTC)
        a_id, _ = await _seed(c, ends_at=now + timedelta(days=1))
        b_id, _ = await _seed(c, ends_at=now + timedelta(days=1))
        assert (await sweep_entitlement_reminders(c, settings, now=now))["reminders_created"] == 2
        rows = await c.fetch(
            "SELECT scope_subject_ref FROM announcements WHERE kind = ANY($1::text[]) ORDER BY scope_subject_ref", list(STAGE_KINDS)
        )
        assert {r["scope_subject_ref"] for r in rows} == {a_id, b_id}
        assert len(await _visible(c, a_id, now)) == 1 and len(await _visible(c, b_id, now)) == 1
    finally:
        await c.close()


async def test_ineligible_entitlements_are_not_notified(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        now = datetime.now(UTC)
        ends_at = now + timedelta(days=1)
        await _seed(c, ends_at=ends_at, kind="onboarding_hour")
        await _seed(c, ends_at=ends_at, status="expired")
        await _seed(c, ends_at=ends_at, bind=False)
        await _seed(c, ends_at=None)
        await _seed(c, ends_at=now - timedelta(days=1))  # past calendar day: no stage
        recent = await _seed(c, ends_at=now + timedelta(hours=1))
        stale_binding = await _seed(c, ends_at=ends_at)
        await c.execute(
            "UPDATE account_bindings SET status='revoked' WHERE account_id=$1", stale_binding[0]
        )
        result = await sweep_entitlement_reminders(c, settings, now=now)
        assert result == {"reminders_created": 1, "reminders_superseded": 0, "reminder_intents_enqueued": 0}
        row = await c.fetchrow("SELECT scope_subject_ref FROM announcements WHERE kind = ANY($1::text[])", list(STAGE_KINDS))
        assert row["scope_subject_ref"] == recent[0]
    finally:
        await c.close()


async def test_supersede_is_atomic_and_retry_safe(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        now = datetime.now(UTC)
        account_id, entitlement_id = await _seed(c, ends_at=now + timedelta(days=2))
        assert (await sweep_entitlement_reminders(c, settings, now=now))["reminders_created"] == 1
        old_id = await c.fetchval("SELECT announcement_id FROM entitlement_reminders WHERE entitlement_id=$1", entitlement_id)
        await c.execute("UPDATE entitlements SET revision=revision+1, ends_at=$2 WHERE id=$1", entitlement_id, now + timedelta(days=6))
        # Fault injection: supersede runs, then the surrounding transaction aborts before commit.
        try:
            async with c.transaction():
                await _supersede_stale(c, settings, now + timedelta(hours=1), 100)
                raise RuntimeError("injected failure")
        except RuntimeError:
            pass
        # Rollback must leave no partial state: reminder still active and message still visible.
        assert await c.fetchval("SELECT state FROM entitlement_reminders WHERE announcement_id=$1", old_id) == "active"
        assert await c.fetchval("SELECT show_until FROM announcements WHERE id=$1", old_id) > now + timedelta(hours=1)
        # Retry succeeds and hides atomically; a second retry is idempotent.
        assert (await sweep_entitlement_reminders(c, settings, now=now + timedelta(hours=1)))["reminders_superseded"] == 1
        assert await c.fetchval("SELECT state FROM entitlement_reminders WHERE announcement_id=$1", old_id) == "superseded"
        assert await _visible(c, account_id, now + timedelta(hours=1)) == []
        assert (await sweep_entitlement_reminders(c, settings, now=now + timedelta(hours=2)))["reminders_superseded"] == 0
    finally:
        await c.close()


async def test_renewal_between_candidate_select_and_insert_uses_authoritative_revision(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        now = datetime.now(UTC)
        account_id, entitlement_id = await _seed(c, ends_at=now + timedelta(days=2))
        # Simulated renewal committing after the candidate SELECT but before the insert.
        await c.execute("UPDATE entitlements SET revision=revision+1 WHERE id=$1", entitlement_id)
        assert await _create_reminder(c, settings, entitlement_id, now) is True
        rows = await c.fetch("SELECT entitlement_revision FROM entitlement_reminders WHERE entitlement_id=$1", entitlement_id)
        assert [r["entitlement_revision"] for r in rows] == [2]
        # A retry for the same authoritative revision does not duplicate.
        assert await _create_reminder(c, settings, entitlement_id, now) is False
        # If the fresh deadline is outside the window, no reminder is created at all.
        outside, entitlement2 = await _seed(c, ends_at=now + timedelta(days=30))
        await c.execute("UPDATE entitlements SET revision=revision+1 WHERE id=$1", entitlement2)
        assert await _create_reminder(c, settings, entitlement2, now) is False
        assert await c.fetchval("SELECT count(*) FROM entitlement_reminders WHERE entitlement_id=$1", entitlement2) == 0
    finally:
        await c.close()


async def test_batch_size_one_does_not_starve_later_eligible_accounts(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        now = datetime.now(UTC)
        unbound_id, unbound_ent = await _seed(c, ends_at=now + timedelta(hours=12), bind=False)
        first_id, first_ent = await _seed(c, ends_at=now + timedelta(days=1))
        second_id, second_ent = await _seed(c, ends_at=now + timedelta(days=1, hours=1))
        # Batch of 1: the earlier unbound row must not consume the slot, and already-reminded
        # current revisions must not reappear, so every eligible account is eventually served.
        for _ in range(4):
            assert (await sweep_entitlement_reminders(c, settings, now=now, batch_size=1))["reminders_created"] in (0, 1)
        rows = await c.fetch(
            "SELECT entitlement_id, entitlement_revision, state FROM entitlement_reminders ORDER BY entitlement_id"
        )
        assert {(r["entitlement_id"], r["entitlement_revision"], r["state"]) for r in rows} == {
            (first_ent, 1, "active"),
            (second_ent, 1, "active"),
        }
        assert await c.fetchval("SELECT count(*) FROM entitlement_reminders WHERE entitlement_id=$1", unbound_ent) == 0
        # No duplicates on further sweeps.
        assert (await sweep_entitlement_reminders(c, settings, now=now, batch_size=1))["reminders_created"] == 0
        assert await c.fetchval("SELECT count(*) FROM entitlement_reminders") == 2
        assert len(await _visible(c, first_id, now)) == 1 and len(await _visible(c, second_id, now)) == 1
        assert await _visible(c, unbound_id, now) == []
    finally:
        await c.close()
