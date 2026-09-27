"""0032 s5 card method: down is fail-closed (no loss of card quote snapshots)."""
from __future__ import annotations

import asyncpg
import pytest

from terlimo_backend.migrations import runner


async def _seed_quote(connection, installation_id, method: str, suffix: str = "") -> None:
    await connection.execute(
        """
        INSERT INTO s5_payment_quotes
            (installation_id, idempotency_key, request_digest, plan_id, months, duration_code,
             method, amount_minor, currency, tariff_key, plans_revision, expires_at)
        VALUES ($1, $2, $3, 'terlimo-30d', 1, 'days:30', $4, 20000, 'RUB',
                'terlimo-200-30d-v1', 'rev-1', now() + interval '10 minutes')
        """,
        installation_id,
        f"idem-{method}-{installation_id}-{suffix}",
        f"digest-{method}-{suffix}",
        method,
    )


async def test_0032_down_refuses_when_card_quotes_exist_and_is_lossless(database_url):
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await runner.apply_migrations(connection)
        installation_id = await connection.fetchval(
            "INSERT INTO installations (environment, public_key_fingerprint) VALUES ('test','fp-0032-card') RETURNING id"
        )
        await _seed_quote(connection, installation_id, "card")
        with pytest.raises(asyncpg.PostgresError):
            await runner.rollback_migration(connection, "0032_s5_card_method")
        # fail-closed: the snapshot is still there and the expanded schema still accepts card
        assert await connection.fetchval(
            "SELECT count(*) FROM s5_payment_quotes WHERE installation_id=$1 AND method='card'", installation_id
        ) == 1
        await _seed_quote(connection, installation_id, "card", suffix="b")
        assert await connection.fetchval(
            "SELECT count(*) FROM s5_payment_quotes WHERE installation_id=$1 AND method='card'", installation_id
        ) == 2
    finally:
        await connection.close()


async def test_0032_down_succeeds_when_no_card_quotes(database_url):
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await runner.apply_migrations(connection)
        installation_id = await connection.fetchval(
            "INSERT INTO installations (environment, public_key_fingerprint) VALUES ('test','fp-0032-empty') RETURNING id"
        )
        await _seed_quote(connection, installation_id, "sbp")
        await runner.rollback_migration(connection, "0032_s5_card_method")
        # strict 0024 constraint restored: card is no longer representable, sbp snapshot untouched
        assert await connection.fetchval(
            "SELECT count(*) FROM s5_payment_quotes WHERE installation_id=$1 AND method='sbp'", installation_id
        ) == 1
        with pytest.raises(asyncpg.PostgresError):
            await _seed_quote(connection, installation_id, "card")
    finally:
        await connection.close()
