"""PostgreSQL access for the unified backend.

One product, one database. The pool is created only from the configured
DATABASE_URL; there is no fallback store. Readiness can attempt a bounded
reconnect for the same DSN without restarting the process, and a broken pool
is closed and never published half-initialized.
"""

from __future__ import annotations

import asyncio
import json
import logging
import time
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

import asyncpg

from .config import Settings

logger = logging.getLogger(__name__)


class DatabaseUnavailable(RuntimeError):
    """Raised when PostgreSQL cannot be reached or a required query fails."""


class Database:
    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._pool: asyncpg.Pool | None = None
        # Serializes pool creation so concurrent readiness probes cannot create
        # parallel pools or publish a failed one.
        self._lock = asyncio.Lock()
        self._retry_after = 0.0

    @property
    def pool(self) -> asyncpg.Pool | None:
        return self._pool

    async def connect(self) -> None:
        """Initial connect used by startup paths; raises when PostgreSQL is unreachable."""
        async with self._lock:
            if self._pool is not None:
                return
            await self._create_pool_locked()

    async def ensure_ready(self) -> bool:
        """Bounded readiness probe: reconnect in-process for the same DSN.

        Returns True only when a live pool answers. While the database is down
        this returns False (readiness stays 503); after it appears the next
        probe creates the pool. A previously published pool that stops
        answering is closed before a reconnect attempt. No other DSN is used.
        """
        cooldown = self._settings.db_reconnect_cooldown_seconds
        try:
            await asyncio.wait_for(self._lock.acquire(), timeout=max(cooldown, 0.05))
        except TimeoutError:
            # Another probe is already (re)connecting; report not ready instead of queueing.
            return False
        try:
            if self._pool is not None:
                if await self._ping_pool_locked():
                    return True
                await self._close_pool_locked()
            if time.monotonic() < self._retry_after:
                return False
            try:
                await self._create_pool_locked()
                return True
            except Exception:  # noqa: BLE001 - any connect failure means not ready
                self._retry_after = time.monotonic() + cooldown
                logger.warning(
                    "database is not ready for %s; readiness stays 503",
                    self._settings.redacted_database_url,
                )
                return False
        finally:
            self._lock.release()

    @staticmethod
    async def _init_connection(connection: asyncpg.Connection) -> None:
        # JSON/JSONB round-trips as Python objects; existing callers also accept text.
        await connection.set_type_codec(
            "json", schema="pg_catalog", encoder=json.dumps, decoder=json.loads
        )
        await connection.set_type_codec(
            "jsonb", schema="pg_catalog", encoder=json.dumps, decoder=json.loads
        )

    async def _create_pool_locked(self) -> None:
        pool: asyncpg.Pool | None = None
        try:
            pool = await asyncpg.create_pool(
                self._settings.database_url,
                min_size=self._settings.db_pool_min,
                max_size=self._settings.db_pool_max,
                command_timeout=self._settings.db_command_timeout_seconds,
                timeout=self._settings.db_command_timeout_seconds,
                init=self._init_connection,
            )
        except Exception:
            if pool is not None:
                await pool.close()
            raise
        self._pool = pool
        self._retry_after = 0.0
        logger.info(
            "PostgreSQL pool ready for %s (env=%s)",
            self._settings.redacted_database_url,
            self._settings.environment,
        )

    async def _ping_pool_locked(self) -> bool:
        if self._pool is None:
            return False
        try:
            async with self._pool.acquire() as connection:
                await connection.fetchval("SELECT 1")
            return True
        except Exception:  # noqa: BLE001 - any failure means the pool is unusable
            return False

    async def _close_pool_locked(self) -> None:
        pool = self._pool
        self._pool = None
        if pool is not None:
            try:
                await pool.close()
            except Exception:  # noqa: BLE001 - closing a broken pool must not raise
                logger.warning("closing an unusable PostgreSQL pool failed")

    async def close(self) -> None:
        async with self._lock:
            await self._close_pool_locked()

    @asynccontextmanager
    async def acquire(self) -> AsyncIterator[asyncpg.Connection]:
        if self._pool is None:
            raise DatabaseUnavailable("database pool is not initialized")
        async with self._pool.acquire() as connection:
            yield connection

    async def ping(self) -> bool:
        """Fast pool liveness check used after a successful connect."""
        if self._pool is None:
            return False
        return await self._ping_pool_locked()


async def connect_once(database_url: str, timeout: float = 10.0) -> asyncpg.Connection:
    """Open a single connection for one-shot tools (migrations, outbox CLI)."""
    return await asyncpg.connect(database_url, timeout=timeout)
