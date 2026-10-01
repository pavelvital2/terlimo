"""Durable outbox worker with claim ownership fencing.

One PostgreSQL-backed queue. A claim is a unique token plus a bounded lease.
Every finalization (done, unknown type, retry, dead) is guarded by the exact
token and the processing status, so a worker that lost its lease can never
overwrite the new owner's result. A live long handler renews its lease with a
bounded heartbeat; a crashed worker's row is reclaimed after the lease
expires.

Delivery model: at-least-once. The handler and its result are intentionally not
wrapped in one long row lock, because future gateway RPCs (03.3) must run
outside a database transaction. Exactly-once external effects are NOT promised
here; gateway control must rely on a stable idempotency key/generation and
readback. The internal self_check operation is idempotent by its own unique
effect key.
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import secrets
import signal
import socket
import time
from collections.abc import Awaitable, Callable

import asyncpg

from .config import Settings, load_settings
from .db import Database
from .operation_timing import log_phase

logger = logging.getLogger(__name__)

OperationHandler = Callable[[asyncpg.Connection, asyncpg.Record], Awaitable[None]]

UNSUPPORTED_OPERATION = "unsupported_operation_type"

CLAIM_SQL = """
    UPDATE outbox_operations AS operation
    SET status = 'processing',
        claim_token = gen_random_uuid(),
        locked_by = $1,
        locked_at = now(),
        lease_expires_at = now() + make_interval(secs => $2),
        attempts = operation.attempts + 1,
        updated_at = now()
    WHERE operation.id = (
        SELECT id
        FROM outbox_operations
        WHERE (status = 'pending' AND available_at <= now())
           OR (
                status = 'processing'
                AND (lease_expires_at IS NULL OR lease_expires_at < now())
           )
        ORDER BY available_at, id
        FOR UPDATE SKIP LOCKED
        LIMIT 1
    )
    RETURNING operation.*
"""

OWNERSHIP_SQL = """
    SELECT 1
    FROM outbox_operations
    WHERE id = $1 AND status = 'processing' AND claim_token = $2
"""

RENEW_SQL = """
    UPDATE outbox_operations
    SET lease_expires_at = now() + make_interval(secs => $3),
        locked_at = now(),
        updated_at = now()
    WHERE id = $1 AND status = 'processing' AND claim_token = $2
    RETURNING id
"""

FINALIZE_SQL = """
    UPDATE outbox_operations
    SET status = $3,
        last_error = $4,
        available_at = CASE
            WHEN $5::double precision > 0 THEN now() + make_interval(secs => $5)
            ELSE available_at
        END,
        locked_by = NULL,
        locked_at = NULL,
        lease_expires_at = NULL,
        claim_token = NULL,
        updated_at = now()
    WHERE id = $1 AND status = 'processing' AND claim_token = $2
    RETURNING id
"""


async def handle_self_check(connection: asyncpg.Connection, operation: asyncpg.Record) -> None:
    """Internal durable self-check: one effect per idempotency key."""
    await connection.execute(
        """
        INSERT INTO outbox_effect_log (idempotency_key, operation_type)
        VALUES ($1, $2)
        ON CONFLICT (idempotency_key) DO NOTHING
        """,
        operation["idempotency_key"],
        operation["operation_type"],
    )


DEFAULT_HANDLERS: dict[str, OperationHandler] = {"self_check": handle_self_check}


def _handler_outcome(outcome) -> tuple[str, str | None]:
    """Handlers may return None (done), a status string, or (status, reason).

    Unknown statuses are rejected so a handler cannot silently bypass finalization.
    """
    if outcome is None:
        return "done", None
    if isinstance(outcome, str):
        if outcome == "done":
            return "done", None
        if outcome == "failed":
            return "failed", "terminal_handler_failure"
    if isinstance(outcome, tuple) and outcome:
        status = outcome[0]
        reason = outcome[1] if len(outcome) > 1 else None
        if status in ("done", "failed"):
            return status, reason
    raise ValueError("bad handler outcome")


def new_worker_id() -> str:
    return f"{socket.gethostname()}:{os.getpid()}:{secrets.token_hex(4)}"


class OutboxWorker:
    def __init__(
        self,
        database: Database,
        settings: Settings,
        handlers: dict[str, OperationHandler] | None = None,
        worker_id: str | None = None,
    ) -> None:
        self._db = database
        self._settings = settings
        self._handlers = dict(DEFAULT_HANDLERS)
        if handlers:
            self._handlers.update(handlers)
        self.worker_id = worker_id or new_worker_id()

    async def run_once(self) -> bool:
        """Claim and process at most one operation. Returns True when one was claimed."""
        pool_started = time.monotonic()
        async with self._db.acquire() as connection:
            pool_ms = (time.monotonic() - pool_started) * 1000
            claim_started = time.monotonic()
            operation = await self._claim(connection)
            if operation is None:
                return False
            log_phase(operation, "claim", claim_started, pool_ms=pool_ms)
            await self._process(connection, operation)
            return True

    async def drain(self) -> int:
        processed = 0
        while await self.run_once():
            processed += 1
        return processed

    async def run_forever(self, stop_event: asyncio.Event) -> None:
        while not stop_event.is_set():
            processed = await self.run_once()
            if processed:
                continue
            try:
                await asyncio.wait_for(
                    stop_event.wait(), timeout=self._settings.worker_poll_interval_seconds
                )
            except TimeoutError:
                continue

    async def _claim(self, connection: asyncpg.Connection) -> asyncpg.Record | None:
        async with connection.transaction():
            return await connection.fetchrow(
                CLAIM_SQL,
                self.worker_id,
                float(self._settings.worker_lock_timeout_seconds),
            )

    async def _owns(self, connection: asyncpg.Connection, operation: asyncpg.Record) -> bool:
        owned = await connection.fetchval(
            OWNERSHIP_SQL, operation["id"], operation["claim_token"]
        )
        return owned == 1

    async def _finalize(
        self,
        connection: asyncpg.Connection,
        operation: asyncpg.Record,
        *,
        status: str,
        error: str | None = None,
        backoff_seconds: float = 0.0,
    ) -> bool:
        """Token-fenced finalization; returns False when the claim was already lost."""
        started = time.monotonic()
        log_phase(operation, "finalize_begin")
        finalized = await connection.fetchval(
            FINALIZE_SQL,
            operation["id"],
            operation["claim_token"],
            status,
            error,
            float(backoff_seconds),
        )
        log_phase(operation, "finalize_end", started, result=status if finalized else "fenced")
        return finalized is not None

    async def _renew_lease(self, operation: asyncpg.Record) -> None:
        lease_seconds = float(self._settings.worker_lock_timeout_seconds)
        interval = max(lease_seconds / 3.0, 0.1)
        while True:
            await asyncio.sleep(interval)
            try:
                async with self._db.acquire() as connection:
                    renewed = await connection.fetchval(
                        RENEW_SQL, operation["id"], operation["claim_token"], lease_seconds
                    )
                if renewed is None:
                    logger.warning(
                        "operation %s lease renewal lost; handler result will be fenced",
                        operation["id"],
                    )
                    return
            except asyncio.CancelledError:
                raise
            except Exception:
                logger.exception("lease renewal failed for operation %s", operation["id"])

    async def _process(self, connection: asyncpg.Connection, operation: asyncpg.Record) -> None:
        if not await self._owns(connection, operation):
            logger.warning(
                "operation %s is no longer owned before handler start; skipping",
                operation["id"],
            )
            return

        handler = self._handlers.get(operation["operation_type"])
        if handler is None:
            if not await self._finalize(
                connection, operation, status="failed", error=UNSUPPORTED_OPERATION
            ):
                logger.warning(
                    "operation %s lost its claim before unknown-type finalization",
                    operation["id"],
                )
            else:
                logger.error(
                    "operation %s has unsupported type %r; marked failed",
                    operation["id"],
                    operation["operation_type"],
                )
            return

        heartbeat = asyncio.create_task(self._renew_lease(operation))
        handler_started = time.monotonic()
        log_phase(operation, "handler_begin")
        try:
            outcome = await handler(connection, operation)
        except Exception as exc:
            log_phase(operation, "handler_end", handler_started, result="exception")
            attempts = int(operation["attempts"])
            max_attempts = int(operation["max_attempts"])
            terminal = attempts >= max_attempts
            status = "dead" if terminal else "pending"
            backoff_seconds = 0.0 if terminal else float(min(2**attempts, 60))
            if await self._finalize(
                connection,
                operation,
                status=status,
                error=type(exc).__name__,
                backoff_seconds=backoff_seconds,
            ):
                logger.exception(
                    "operation %s (%s) failed (attempt %s/%s, status=%s)",
                    operation["id"],
                    operation["operation_type"],
                    attempts,
                    max_attempts,
                    status,
                )
            else:
                logger.warning(
                    "operation %s failed after losing its claim; new result preserved",
                    operation["id"],
                )
        else:
            log_phase(operation, "handler_end", handler_started)
            outcome_status, outcome_reason = _handler_outcome(outcome)
            if outcome_status == "failed":
                if await self._finalize(
                    connection,
                    operation,
                    status="failed",
                    error=outcome_reason or "terminal_handler_failure",
                ):
                    logger.error(
                        "operation %s (%s) terminally failed: %s",
                        operation["id"],
                        operation["operation_type"],
                        outcome_reason,
                    )
                else:
                    logger.warning(
                        "operation %s failed after losing its claim; new result preserved",
                        operation["id"],
                    )
            elif await self._finalize(connection, operation, status="done"):
                logger.info(
                    "operation %s (%s) applied", operation["id"], operation["operation_type"]
                )
            else:
                logger.warning(
                    "operation %s completed after losing its claim; result not recorded",
                    operation["id"],
                )
        finally:
            heartbeat.cancel()
            try:
                await heartbeat
            except asyncio.CancelledError:
                pass


def main() -> None:
    parser = argparse.ArgumentParser(description="TERLIMO durable outbox worker")
    parser.add_argument(
        "--once",
        action="store_true",
        help="process available operations until the queue is empty, then exit",
    )
    arguments = parser.parse_args()
    settings = load_settings()
    logging.basicConfig(
        level=settings.log_level,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )

    async def run() -> None:
        from .gateway_adapter import build_bootstrap_client_factory
        from .gateway_control import GatewayControlHandlers
        from .onboarding_hour import OnboardingHourHandlers, build_secret_cipher
        from .reminder_notifications import ReminderNotificationHandlers

        database = Database(settings)
        await database.connect()
        handlers = dict(DEFAULT_HANDLERS)
        handlers.update(GatewayControlHandlers(settings).as_handlers())
        handlers.update(ReminderNotificationHandlers(settings).as_handlers())
        handlers.update(
            OnboardingHourHandlers(
                cipher=build_secret_cipher(settings),
                client_factory=build_bootstrap_client_factory(settings),
            ).as_handlers()
        )
        worker = OutboxWorker(database, settings, handlers=handlers)
        try:
            if arguments.once:
                processed = await worker.drain()
                logger.info("worker drained %d operation(s)", processed)
                return
            stop_event = asyncio.Event()
            loop = asyncio.get_running_loop()
            for signum in (signal.SIGINT, signal.SIGTERM):
                loop.add_signal_handler(signum, stop_event.set)
            logger.info("worker %s started (db=%s)", worker.worker_id, settings.redacted_database_url)
            await worker.run_forever(stop_event)
            logger.info("worker %s stopped", worker.worker_id)
        finally:
            await database.close()

    asyncio.run(run())


if __name__ == "__main__":
    main()
