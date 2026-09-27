"""Versioned SQL migrations for the unified backend.

The tracking table and apply/skip semantics follow the reviewed donor chain
runner (MiniShop cb8caf41 backend/db/migrator/engine.py, table
`schema_migrations`), adapted to asyncpg and extended with a per-version
checksum plus a reversible `down` step.
"""

from __future__ import annotations

import hashlib
import logging
from dataclasses import dataclass
from pathlib import Path

import asyncpg

logger = logging.getLogger(__name__)

VERSIONS_DIR = Path(__file__).resolve().parent / "versions"
MIGRATION_ADVISORY_LOCK_ID = 817512404897421337


class MigrationError(RuntimeError):
    pass


class MigrationDriftError(MigrationError):
    """An already applied version changed on disk."""


@dataclass(frozen=True)
class MigrationVersion:
    version: str
    path: Path
    checksum: str

    @property
    def down_path(self) -> Path:
        return self.path.with_name(self.path.stem + ".down.sql")


def discover_versions(versions_dir: Path | None = None) -> list[MigrationVersion]:
    directory = versions_dir or VERSIONS_DIR
    versions: list[MigrationVersion] = []
    for path in sorted(directory.glob("*.sql")):
        if path.name.endswith(".down.sql"):
            continue
        checksum = hashlib.sha256(path.read_bytes()).hexdigest()
        versions.append(MigrationVersion(version=path.stem, path=path, checksum=checksum))
    if not versions:
        raise MigrationError(f"no migration versions found in {directory}")
    return versions


def latest_version(versions_dir: Path | None = None) -> str:
    return discover_versions(versions_dir)[-1].version


async def _ensure_migrations_table(connection: asyncpg.Connection) -> None:
    await connection.execute(
        """
        CREATE TABLE IF NOT EXISTS schema_migrations (
            id text PRIMARY KEY,
            checksum text NOT NULL,
            applied_at timestamptz NOT NULL DEFAULT now()
        )
        """
    )


async def _applied(connection: asyncpg.Connection) -> dict[str, str]:
    try:
        rows = await connection.fetch("SELECT id, checksum FROM schema_migrations")
    except asyncpg.UndefinedTableError:
        # A fresh database without the tracking table has no applied versions.
        return {}
    return {row["id"]: row["checksum"] for row in rows}


def _verify_no_drift(
    versions: list[MigrationVersion], applied: dict[str, str]
) -> list[MigrationVersion]:
    for migration in versions:
        recorded = applied.get(migration.version)
        if recorded is not None and recorded != migration.checksum:
            raise MigrationDriftError(
                f"migration {migration.version} changed after it was applied"
            )
    return [migration for migration in versions if migration.version not in applied]


async def pending_versions(
    connection: asyncpg.Connection, versions_dir: Path | None = None
) -> list[str]:
    versions = discover_versions(versions_dir)
    applied = await _applied(connection)
    return [migration.version for migration in _verify_no_drift(versions, applied)]


async def apply_migrations(
    connection: asyncpg.Connection, versions_dir: Path | None = None
) -> list[str]:
    """Apply pending versions in order. Repeatable: applied versions are skipped."""
    versions = discover_versions(versions_dir)
    applied_versions: list[str] = []
    await connection.execute("SELECT pg_advisory_lock($1)", MIGRATION_ADVISORY_LOCK_ID)
    try:
        async with connection.transaction():
            await _ensure_migrations_table(connection)
        applied = await _applied(connection)
        pending = _verify_no_drift(versions, applied)
        for migration in pending:
            sql = migration.path.read_text(encoding="utf-8")
            async with connection.transaction():
                await connection.execute(sql)
                await connection.execute(
                    "INSERT INTO schema_migrations (id, checksum) VALUES ($1, $2)",
                    migration.version,
                    migration.checksum,
                )
            logger.info("migration applied: %s", migration.version)
            applied_versions.append(migration.version)
        return applied_versions
    finally:
        await connection.execute("SELECT pg_advisory_unlock($1)", MIGRATION_ADVISORY_LOCK_ID)


async def rollback_migration(
    connection: asyncpg.Connection, version: str, versions_dir: Path | None = None
) -> None:
    """Revert exactly one applied version using its `.down.sql` file."""
    versions = {migration.version: migration for migration in discover_versions(versions_dir)}
    migration = versions.get(version)
    if migration is None:
        raise MigrationError(f"unknown migration version: {version}")
    if not migration.down_path.exists():
        raise MigrationError(f"migration {version} has no down script")
    await connection.execute("SELECT pg_advisory_lock($1)", MIGRATION_ADVISORY_LOCK_ID)
    try:
        applied = await _applied(connection)
        if version not in applied:
            raise MigrationError(f"migration {version} is not applied")
        if applied[version] != migration.checksum:
            raise MigrationDriftError(f"migration {version} changed after it was applied")
        sql = migration.down_path.read_text(encoding="utf-8")
        async with connection.transaction():
            await connection.execute(sql)
            await connection.execute("DELETE FROM schema_migrations WHERE id = $1", version)
        logger.info("migration reverted: %s", version)
    finally:
        await connection.execute("SELECT pg_advisory_unlock($1)", MIGRATION_ADVISORY_LOCK_ID)


async def migration_status(
    connection: asyncpg.Connection, versions_dir: Path | None = None
) -> dict[str, object]:
    versions = discover_versions(versions_dir)
    applied = await _applied(connection)
    pending = [migration.version for migration in _verify_no_drift(versions, applied)]
    return {
        "applied": [migration.version for migration in versions if migration.version in applied],
        "pending": pending,
        "latest": versions[-1].version,
    }
