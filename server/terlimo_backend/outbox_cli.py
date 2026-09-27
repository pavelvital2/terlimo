"""Internal outbox CLI: enqueue and inspect durable operations.

This is an operator/diagnostic tool for the 03.1 runtime, not a mobile API.
Real gateway-control producers arrive in 03.3.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging

from .config import load_settings
from .db import connect_once


async def _enqueue(arguments: argparse.Namespace) -> int:
    try:
        payload = json.loads(arguments.payload)
    except json.JSONDecodeError as exc:
        raise SystemExit(f"--payload must be valid JSON: {exc}") from exc
    settings = load_settings()
    connection = await connect_once(settings.database_url)
    try:
        row = await connection.fetchrow(
            """
            INSERT INTO outbox_operations
                (operation_type, payload, idempotency_key, max_attempts)
            VALUES ($1, $2::jsonb, $3, $4)
            ON CONFLICT (idempotency_key) DO NOTHING
            RETURNING id, operation_type, status
            """,
            arguments.type,
            json.dumps(payload),
            arguments.idempotency_key,
            arguments.max_attempts,
        )
        if row is None:
            existing = await connection.fetchrow(
                "SELECT id, operation_type, status FROM outbox_operations WHERE idempotency_key = $1",
                arguments.idempotency_key,
            )
            print(json.dumps({"enqueued": False, "existing": dict(existing)}, default=str))
        else:
            print(json.dumps({"enqueued": True, "operation": dict(row)}, default=str))
    finally:
        await connection.close()
    return 0


async def _status(_: argparse.Namespace) -> int:
    settings = load_settings()
    connection = await connect_once(settings.database_url)
    try:
        rows = await connection.fetch(
            "SELECT status, count(*) AS count FROM outbox_operations GROUP BY status ORDER BY status"
        )
        print(json.dumps({row["status"]: row["count"] for row in rows}))
    finally:
        await connection.close()
    return 0


def main() -> None:
    parser = argparse.ArgumentParser(description="TERLIMO outbox diagnostic CLI")
    subparsers = parser.add_subparsers(dest="command", required=True)
    enqueue = subparsers.add_parser("enqueue")
    enqueue.add_argument("--type", required=True, dest="type")
    enqueue.add_argument("--idempotency-key", required=True)
    enqueue.add_argument("--payload", default="{}")
    enqueue.add_argument("--max-attempts", type=int, default=8)
    subparsers.add_parser("status")
    arguments = parser.parse_args()
    logging.basicConfig(level="WARNING")
    if arguments.command == "enqueue":
        raise SystemExit(asyncio.run(_enqueue(arguments)))
    raise SystemExit(asyncio.run(_status(arguments)))


if __name__ == "__main__":
    main()
