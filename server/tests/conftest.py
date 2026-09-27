"""Test fixtures: isolated real PostgreSQL via bundled pgserver binaries.

The server runs from a temporary datadir on a unix socket; no host service,
no production database and no network listener are touched.
"""

from __future__ import annotations

import os
import shutil
import tempfile
import uuid
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

import pytest

_RUNTIME = Path(tempfile.mkdtemp(prefix="terlimo-backend-runtime-"))
os.chmod(_RUNTIME, 0o700)
os.environ["XDG_RUNTIME_DIR"] = str(_RUNTIME)

import asyncpg
import pgserver

from terlimo_backend.config import Settings
from terlimo_backend.migrations import runner


def dsn_for(server: pgserver.PostgresServer, database: str) -> str:
    parts = urlsplit(server.get_uri())
    return urlunsplit((parts.scheme, parts.netloc, f"/{database}", parts.query, parts.fragment))


@pytest.fixture(scope="session")
def pg_server():
    pgdata = Path(tempfile.mkdtemp(prefix="terlimo-backend-pg-"))
    server = pgserver.get_server(pgdata)
    server.ensure_pgdata_inited()
    server.ensure_postgres_running()
    try:
        yield server
    finally:
        server.cleanup()
        shutil.rmtree(pgdata, ignore_errors=True)


@pytest.fixture
async def database_url(pg_server) -> str:
    admin_dsn = pg_server.get_uri()
    name = f"terlimo_test_{uuid.uuid4().hex[:12]}"
    admin = await asyncpg.connect(admin_dsn, timeout=10)
    await admin.execute(f'CREATE DATABASE "{name}"')
    await admin.close()
    try:
        yield dsn_for(pg_server, name)
    finally:
        admin = await asyncpg.connect(admin_dsn, timeout=10)
        await admin.execute(f'DROP DATABASE "{name}" WITH (FORCE)')
        await admin.close()


def make_settings(database_url: str, **overrides) -> Settings:
    values = {
        "database_url": database_url,
        "environment": "test",
        "log_level": "WARNING",
        "api_host": "127.0.0.1",
        "api_port": 0,
        "db_pool_min": 1,
        "db_pool_max": 3,
        "db_command_timeout_seconds": 5,
        "worker_poll_interval_seconds": 0.05,
        "worker_lock_timeout_seconds": 30,
        "worker_max_attempts": 8,
        "db_reconnect_cooldown_seconds": 0.2,
    }
    values.update(overrides)
    return Settings(**values)


@pytest.fixture
def settings(database_url) -> Settings:
    return make_settings(database_url)


@pytest.fixture
async def migrated_url(database_url) -> str:
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await runner.apply_migrations(connection)
    finally:
        await connection.close()
    return database_url


@pytest.fixture
def settings_factory():
    return make_settings
