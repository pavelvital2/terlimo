"""Migration CLI: `terlimo-migrate up|down|status`."""

from __future__ import annotations

import argparse
import asyncio
import json
import logging

from .config import load_settings
from .db import connect_once
from .migrations import runner


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="TERLIMO backend migrations")
    subparsers = parser.add_subparsers(dest="command")
    subparsers.add_parser("up", help="apply all pending migrations (default)")
    down = subparsers.add_parser("down", help="revert one applied migration")
    down.add_argument("--version", required=True, help="migration version id to revert")
    subparsers.add_parser("status", help="show applied and pending migrations")
    return parser


async def _run(arguments: argparse.Namespace) -> int:
    settings = load_settings()
    connection = await connect_once(settings.database_url)
    try:
        command = arguments.command or "up"
        if command == "up":
            applied = await runner.apply_migrations(connection)
            print(json.dumps({"applied": applied}, ensure_ascii=False))
        elif command == "down":
            await runner.rollback_migration(connection, arguments.version)
            print(json.dumps({"reverted": arguments.version}, ensure_ascii=False))
        else:
            print(json.dumps(await runner.migration_status(connection), ensure_ascii=False))
    finally:
        await connection.close()
    return 0


def main() -> None:
    arguments = _parser().parse_args()
    logging.basicConfig(level="INFO", format="%(asctime)s %(levelname)s %(name)s %(message)s")
    raise SystemExit(asyncio.run(_run(arguments)))


if __name__ == "__main__":
    main()
