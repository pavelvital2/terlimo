"""Health/readiness: live is process-only, ready checks the real database state."""

from __future__ import annotations

import asyncio
import socket
from pathlib import Path
from urllib.parse import urlsplit

import asyncpg
from aiohttp.test_utils import TestClient, TestServer

from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.migrations import runner


async def test_live_and_ready_follow_database_state(database_url, settings_factory):
    settings = settings_factory(database_url)
    database = Database(settings)
    app = create_app(settings, database)
    client = TestClient(TestServer(app))
    await client.start_server()
    try:
        live = await client.get("/health/live")
        assert live.status == 200
        assert (await live.json())["status"] == "live"

        ready = await client.get("/health/ready")
        assert ready.status == 503
        body = await ready.json()
        assert body["reason"] == "migrations_pending"
        from terlimo_backend.migrations import runner as migration_runner

        assert body["pending_migrations"] == [
            migration.version for migration in migration_runner.discover_versions()
        ]

        connection = await asyncpg.connect(database_url, timeout=10)
        try:
            await runner.apply_migrations(connection)
        finally:
            await connection.close()

        ready = await client.get("/health/ready")
        assert ready.status == 200
        body = await ready.json()
        assert body["status"] == "ready"
        assert body["migrations"] == "applied"
        assert "password" not in str(body)
    finally:
        await client.close()


async def test_ready_reports_database_absence(settings_factory):
    settings = settings_factory(
        "postgresql://terlimo_backend:placeholder@127.0.0.1:1/terlimo_backend_test",
        db_command_timeout_seconds=2,
    )
    database = Database(settings)
    app = create_app(settings, database)
    client = TestClient(TestServer(app))
    await client.start_server()
    try:
        ready = await client.get("/health/ready")
        assert ready.status == 503
        body = await ready.json()
        assert body["reason"] == "database_unavailable"
        assert "placeholder" not in str(body)
    finally:
        await client.close()


class TcpToUnixProxy:
    """Forward TCP to the PostgreSQL unix socket so the DSN can drop and recover."""

    def __init__(self, socket_path: Path, port: int) -> None:
        self._socket_path = socket_path
        self._port = port
        self._server: asyncio.AbstractServer | None = None
        self._writers: set[asyncio.StreamWriter] = set()

    @property
    def port(self) -> int:
        return self._port

    async def _pipe(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        try:
            while chunk := await reader.read(65536):
                writer.write(chunk)
                await writer.drain()
        except (OSError, asyncio.CancelledError):
            pass
        finally:
            writer.close()

    async def _handle(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        try:
            upstream_reader, upstream_writer = await asyncio.open_unix_connection(
                str(self._socket_path)
            )
        except OSError:
            writer.close()
            return
        self._writers.update({writer, upstream_writer})
        try:
            await asyncio.gather(
                self._pipe(reader, upstream_writer), self._pipe(upstream_reader, writer)
            )
        finally:
            self._writers.discard(writer)
            self._writers.discard(upstream_writer)

    async def up(self) -> None:
        if self._server is None:
            self._server = await asyncio.start_server(self._handle, "127.0.0.1", self._port)

    async def down(self) -> None:
        if self._server is not None:
            self._server.close()
            self._server = None
        for writer in list(self._writers):
            writer.close()
            self._writers.discard(writer)


def _find_socket(server) -> Path:
    candidates = [Path(server.pgdata)]
    runtime = getattr(type(server), "runtime_path", None)
    if runtime is not None:
        candidates.append(Path(runtime))
    for directory in candidates:
        for candidate in sorted(directory.glob(".s.PGSQL.*")):
            if not candidate.name.endswith(".lock"):
                return candidate
    raise RuntimeError("PostgreSQL unix socket not found")


def _free_port() -> int:
    probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        probe.bind(("127.0.0.1", 0))
        return probe.getsockname()[1]
    finally:
        probe.close()


async def _wait_ready(client: TestClient, expected: int, timeout: float = 8.0) -> dict:
    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    body: dict = {}
    while loop.time() < deadline:
        response = await client.get("/health/ready")
        body = await response.json()
        if response.status == expected:
            return body
        await asyncio.sleep(0.2)
    raise AssertionError(f"readiness did not reach {expected}: {body}")


async def test_readiness_recovers_in_process_when_database_returns(
    pg_server, database_url, settings_factory
):
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await runner.apply_migrations(connection)
    finally:
        await connection.close()
    database_name = urlsplit(database_url).path.lstrip("/")
    proxy = TcpToUnixProxy(_find_socket(pg_server), _free_port())
    tcp_dsn = f"postgresql://postgres@127.0.0.1:{proxy.port}/{database_name}"
    settings = settings_factory(
        tcp_dsn, db_command_timeout_seconds=2, db_reconnect_cooldown_seconds=0.2
    )
    database = Database(settings)
    app = create_app(settings, database)
    client = TestClient(TestServer(app))
    await client.start_server()
    try:
        # Case 1: API started while the database is unreachable, then it appears.
        response = await client.get("/health/ready")
        body = await response.json()
        assert response.status == 503
        assert body["reason"] == "database_unavailable"

        await proxy.up()
        body = await _wait_ready(client, 200)
        assert body["status"] == "ready"

        # Case 2: a published pool stops answering, then recovers (transient).
        await proxy.down()
        body = await _wait_ready(client, 503)
        assert body["reason"] == "database_unavailable"

        await proxy.up()
        body = await _wait_ready(client, 200)
        assert body["status"] == "ready"
    finally:
        await client.close()
        await database.close()
        await proxy.down()
