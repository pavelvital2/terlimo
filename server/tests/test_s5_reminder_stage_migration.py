"""0031 reminder-stage migration: down must not strand visible announcements or queued intents."""
from __future__ import annotations

import asyncpg

from terlimo_backend.migrations import runner


async def test_0031_up_down_hides_announcements_and_cancels_queued_outbox(database_url):
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await runner.apply_migrations(connection)
        account_id = await connection.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified', 771234567) RETURNING id"
        )
        installation_id = await connection.fetchval(
            "INSERT INTO installations (environment, public_key_fingerprint) VALUES ('test','fp-0031') RETURNING id"
        )
        await connection.execute(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')",
            account_id, installation_id,
        )
        entitlement_id = await connection.fetchval(
            """
            INSERT INTO entitlements (account_id, kind, status, ends_at, revision)
            VALUES ($1,'paid','active', now() + interval '2 days', 1) RETURNING id
            """,
            account_id,
        )
        # A stage created by the 0031 code path: announcement + reminder row + queued outbox intent.
        announcement_id = await connection.fetchval(
            """
            INSERT INTO announcements (environment, scope_subject_ref, kind, text, show_until)
            VALUES ('test', $1, 'pre_expiry_t2', 'synthetic t2', now() + interval '2 days')
            RETURNING id
            """,
            account_id,
        )
        await connection.execute(
            """
            INSERT INTO entitlement_reminders (entitlement_id, entitlement_revision, stage_end_at, kind, announcement_id)
            VALUES ($1, 1, now() + interval '2 days', 'pre_expiry_t2', $2)
            """,
            entitlement_id, announcement_id,
        )
        await connection.execute(
            """
            INSERT INTO outbox_operations (operation_type, payload, idempotency_key)
            VALUES ('notify_entitlement_reminder', '{}'::jsonb, $1)
            """,
            f"notify-reminder:{entitlement_id}:1:pre_expiry_t2",
        )
        await runner.rollback_migration(connection, "0031_reminder_stages")
        # removed stage row is gone; its announcement is hidden; its queued intent is cancelled
        assert await connection.fetchval(
            "SELECT count(*) FROM entitlement_reminders WHERE kind='pre_expiry_t2'"
        ) == 0
        assert await connection.fetchval(
            "SELECT show_until > now() FROM announcements WHERE id=$1", announcement_id
        ) is False
        assert await connection.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE idempotency_key=$1",
            f"notify-reminder:{entitlement_id}:1:pre_expiry_t2",
        ) == 0
        # column/schema reverted
        assert await connection.fetchval(
            "SELECT count(*) FROM information_schema.columns WHERE table_name='entitlement_reminders' AND column_name='stage_end_at'"
        ) == 0
    finally:
        await connection.close()


async def test_0031_backfills_stage_end_from_announcement_not_current_ends_at(database_url):
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await runner.apply_migrations(connection)
        await runner.rollback_migration(connection, "0031_reminder_stages")  # back to 0030 shape
        account_id = await connection.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified', 771234568) RETURNING id"
        )
        installation_id = await connection.fetchval(
            "INSERT INTO installations (environment, public_key_fingerprint) VALUES ('test','fp-0031b') RETURNING id"
        )
        await connection.execute(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')",
            account_id, installation_id,
        )
        # Renewal BEFORE cleanup: entitlement currently points at the NEW deadline (revision 2),
        # while the old 0030 reminder belongs to revision 1 with its original show_until.
        entitlement_id = await connection.fetchval(
            """
            INSERT INTO entitlements (account_id, kind, status, ends_at, revision)
            VALUES ($1,'paid','active', now() + interval '30 days', 2) RETURNING id
            """,
            account_id,
        )
        old_end = await connection.fetchval("SELECT now() + interval '2 days'")
        announcement_id = await connection.fetchval(
            """
            INSERT INTO announcements (environment, scope_subject_ref, kind, text, show_until)
            VALUES ('test', $1, 'pre_expiry_3d', 'old t3', $2) RETURNING id
            """,
            account_id, old_end,
        )
        await connection.execute(
            """
            INSERT INTO entitlement_reminders (entitlement_id, entitlement_revision, kind, announcement_id)
            VALUES ($1, 1, 'pre_expiry_3d', $2)
            """,
            entitlement_id, announcement_id,
        )
        await runner.apply_migrations(connection)
        row = await connection.fetchrow(
            "SELECT stage_end_at, kind FROM entitlement_reminders WHERE entitlement_id=$1", entitlement_id
        )
        assert row["kind"] == "pre_expiry_t3"
        assert abs((row["stage_end_at"] - old_end).total_seconds()) < 1  # original end, not +30d
    finally:
        await connection.close()


async def test_0031_superseded_history_backfill_is_inert(database_url):
    """For an already-superseded 0030 row the original end may be unrecoverable; the backfilled
    value is documented as historical and must never drive active identity/reconcile/send."""
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await runner.apply_migrations(connection)
        await runner.rollback_migration(connection, "0031_reminder_stages")
        account_id = await connection.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified', 771234569) RETURNING id"
        )
        installation_id = await connection.fetchval(
            "INSERT INTO installations (environment, public_key_fingerprint) VALUES ('test','fp-0031c') RETURNING id"
        )
        await connection.execute(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')",
            account_id, installation_id,
        )
        entitlement_id = await connection.fetchval(
            """
            INSERT INTO entitlements (account_id, kind, status, ends_at, revision)
            VALUES ($1,'paid','active', now() + interval '30 days', 2) RETURNING id
            """,
            account_id,
        )
        # historical superseded row whose show_until was shortened when it was superseded
        shortened = await connection.fetchval("SELECT now() - interval '1 day'")
        announcement_id = await connection.fetchval(
            """
            INSERT INTO announcements (environment, scope_subject_ref, kind, text, show_until)
            VALUES ('test', $1, 'pre_expiry_3d', 'historical', $2) RETURNING id
            """,
            account_id, shortened,
        )
        await connection.execute(
            """
            INSERT INTO entitlement_reminders (entitlement_id, entitlement_revision, kind, announcement_id, state, superseded_at)
            VALUES ($1, 1, 'pre_expiry_3d', $2, 'superseded', now())
            """,
            entitlement_id, announcement_id,
        )
        await runner.apply_migrations(connection)
        row = await connection.fetchrow(
            "SELECT state, stage_end_at FROM entitlement_reminders WHERE entitlement_id=$1", entitlement_id
        )
        assert row["state"] == "superseded"  # inert: not active identity, not reconciled, not sent
        assert abs((row["stage_end_at"] - shortened).total_seconds()) < 1
        assert await connection.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE idempotency_key LIKE $1",
            f"notify-reminder:{entitlement_id}:%",
        ) == 0
    finally:
        await connection.close()
