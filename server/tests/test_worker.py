"""Durable outbox: claim ownership, restart recovery, idempotency, terminal failures."""

from __future__ import annotations

import asyncio

import asyncpg

from terlimo_backend.db import Database
from terlimo_backend.worker import UNSUPPORTED_OPERATION, OutboxWorker


async def _enqueue(
    connection: asyncpg.Connection,
    operation_type: str,
    idempotency_key: str,
    *,
    max_attempts: int = 8,
) -> bool:
    row = await connection.fetchrow(
        """
        INSERT INTO outbox_operations (operation_type, idempotency_key, max_attempts)
        VALUES ($1, $2, $3)
        ON CONFLICT (idempotency_key) DO NOTHING
        RETURNING id
        """,
        operation_type,
        idempotency_key,
        max_attempts,
    )
    return row is not None


async def _job(connection: asyncpg.Connection, idempotency_key: str) -> asyncpg.Record:
    return await connection.fetchrow(
        "SELECT * FROM outbox_operations WHERE idempotency_key = $1", idempotency_key
    )


async def _effects(connection: asyncpg.Connection, idempotency_key: str) -> int:
    return await connection.fetchval(
        "SELECT count(*) FROM outbox_effect_log WHERE idempotency_key = $1",
        idempotency_key,
    )


async def test_durable_job_survives_worker_restart_and_applies_once(migrated_url, settings_factory):
    settings = settings_factory(migrated_url)
    database = Database(settings)
    await database.connect()
    connection = await asyncpg.connect(migrated_url, timeout=10)
    try:
        assert await _enqueue(connection, "self_check", "restart-1")

        first = OutboxWorker(database, settings, worker_id="worker-first")
        assert await first.drain() == 1
        assert (await _job(connection, "restart-1"))["status"] == "done"
        assert await _effects(connection, "restart-1") == 1

        restarted = OutboxWorker(database, settings, worker_id="worker-second")
        assert await restarted.drain() == 0
        assert await _effects(connection, "restart-1") == 1
    finally:
        await connection.close()
        await database.close()


async def test_duplicate_idempotency_key_is_single_operation_and_single_effect(
    migrated_url, settings_factory
):
    settings = settings_factory(migrated_url)
    database = Database(settings)
    await database.connect()
    connection = await asyncpg.connect(migrated_url, timeout=10)
    try:
        assert await _enqueue(connection, "self_check", "dedupe-1")
        assert not await _enqueue(connection, "self_check", "dedupe-1")
        assert await connection.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE idempotency_key = 'dedupe-1'"
        ) == 1

        worker = OutboxWorker(database, settings, worker_id="worker-dedupe")
        assert await worker.drain() == 1
        assert await _effects(connection, "dedupe-1") == 1
    finally:
        await connection.close()
        await database.close()


async def test_stale_processing_job_is_reclaimed_after_restart(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, worker_lock_timeout_seconds=30)
    database = Database(settings)
    await database.connect()
    connection = await asyncpg.connect(migrated_url, timeout=10)
    try:
        await connection.execute(
            """
            INSERT INTO outbox_operations
                (operation_type, idempotency_key, status, attempts, locked_by, locked_at)
            VALUES ('self_check', 'stale-1', 'processing', 1, 'crashed-worker', now() - interval '1 hour')
            """
        )
        worker = OutboxWorker(database, settings, worker_id="worker-after-crash")
        assert await worker.drain() == 1
        job = await _job(connection, "stale-1")
        assert job["status"] == "done"
        assert job["attempts"] == 2
        assert job["claim_token"] is None
        assert await _effects(connection, "stale-1") == 1
    finally:
        await connection.close()
        await database.close()


async def test_live_slow_handler_is_not_reclaimed_by_second_worker(
    migrated_url, settings_factory
):
    settings = settings_factory(migrated_url, worker_lock_timeout_seconds=1)
    database_a = Database(settings)
    database_b = Database(settings)
    await database_a.connect()
    await database_b.connect()
    connection = await asyncpg.connect(migrated_url, timeout=10)
    invocations: list[str] = []

    async def slow_handler(_connection, _operation):
        invocations.append("handler")
        await asyncio.sleep(1.4)

    try:
        assert await _enqueue(connection, "slow", "slow-1")
        worker_a = OutboxWorker(
            database_a, settings, handlers={"slow": slow_handler}, worker_id="worker-A"
        )
        worker_b = OutboxWorker(
            database_b, settings, handlers={"slow": slow_handler}, worker_id="worker-B"
        )
        first = asyncio.create_task(worker_a.run_once())
        await asyncio.sleep(1.05)
        assert not await worker_b.run_once()
        assert len(invocations) == 1
        assert await first
        job = await _job(connection, "slow-1")
        assert job["status"] == "done"
        assert job["attempts"] == 1
        assert job["claim_token"] is None
        assert len(invocations) == 1
    finally:
        await connection.close()
        await database_a.close()
        await database_b.close()


async def test_stale_claim_cannot_start_handler_or_overwrite_new_result(
    migrated_url, settings_factory
):
    settings = settings_factory(migrated_url, worker_lock_timeout_seconds=1)
    database_a = Database(settings)
    database_b = Database(settings)
    await database_a.connect()
    await database_b.connect()
    connection = await asyncpg.connect(migrated_url, timeout=10)
    invocations_a: list[str] = []
    invocations_b: list[str] = []

    async def handler_a(_connection, _operation):
        invocations_a.append("A")

    async def handler_b(_connection, _operation):
        invocations_b.append("B")

    try:
        assert await _enqueue(connection, "hold", "fence-1")
        worker_a = OutboxWorker(
            database_a, settings, handlers={"hold": handler_a}, worker_id="worker-A"
        )
        worker_b = OutboxWorker(
            database_b, settings, handlers={"hold": handler_b}, worker_id="worker-B"
        )
        async with database_a.acquire() as pool_connection:
            stale = await worker_a._claim(pool_connection)
        assert stale is not None and stale["claim_token"] is not None
        await connection.execute(
            "UPDATE outbox_operations SET lease_expires_at = now() - interval '5 seconds' WHERE id = $1",
            stale["id"],
        )

        assert await worker_b.run_once()
        assert invocations_a == [] and invocations_b == ["B"]
        job = await _job(connection, "fence-1")
        assert job["status"] == "done" and job["attempts"] == 2

        async with database_a.acquire() as pool_connection:
            # A stale owner must not change the new result and must not run the handler.
            assert not await worker_a._finalize(pool_connection, stale, status="done")
            assert not await worker_a._finalize(
                pool_connection, stale, status="pending", error="RuntimeError", backoff_seconds=1.0
            )
            await worker_a._process(pool_connection, stale)
        assert invocations_a == []
        after = await _job(connection, "fence-1")
        assert after["status"] == "done"
        assert after["attempts"] == 2
        assert after["last_error"] is None
    finally:
        await connection.close()
        await database_a.close()
        await database_b.close()


async def test_unknown_operation_type_is_terminal_failure(migrated_url, settings_factory):
    settings = settings_factory(migrated_url)
    database = Database(settings)
    await database.connect()
    connection = await asyncpg.connect(migrated_url, timeout=10)
    try:
        assert await _enqueue(connection, "apply_grant", "unknown-1")
        worker = OutboxWorker(database, settings, worker_id="worker-unknown")
        assert await worker.drain() == 1
        job = await _job(connection, "unknown-1")
        assert job["status"] == "failed"
        assert job["last_error"] == UNSUPPORTED_OPERATION
        assert job["claim_token"] is None
        assert await _effects(connection, "unknown-1") == 0
        assert await worker.drain() == 0
        assert (await _job(connection, "unknown-1"))["status"] == "failed"
    finally:
        await connection.close()
        await database.close()


async def test_failing_handler_is_not_success(migrated_url, settings_factory):
    settings = settings_factory(migrated_url)
    database = Database(settings)
    await database.connect()
    connection = await asyncpg.connect(migrated_url, timeout=10)

    async def explode(_connection, _operation):
        raise RuntimeError("handler failure")

    try:
        assert await _enqueue(connection, "explode", "failing-1", max_attempts=1)
        worker = OutboxWorker(
            database, settings, handlers={"explode": explode}, worker_id="worker-failing"
        )
        assert await worker.drain() == 1
        job = await _job(connection, "failing-1")
        assert job["status"] == "dead"
        assert job["last_error"] == "RuntimeError"
        assert await _effects(connection, "failing-1") == 0
    finally:
        await connection.close()
        await database.close()
