#!/usr/bin/env python3
"""Bounded read-only wait for the pre-phone post-worker state.

Usage: 04_wait_post_worker.py <work> <manifest.json> [--timeout 120] [--interval 2]

Polls (SELECT only; no writes, no new apply, no reconcile) until:
  grant desired=applied=applied+1, gateway_generation==manifest wire, lease_seq>=1, state applied,
  apply readback persisted (last_readback not null), and NO pending/processing outbox operations
  for this gateway/binding.
Terminal non-zero exits without any mutation:
  2 = apply/failed|dead or grant state failed/revoked
  3 = entitlement window passed (expiry reported, never extended)
  4 = bounded timeout (operator wait exceeded)
"""
from __future__ import annotations

import argparse
import asyncio
import datetime as dt
import json
import time
from pathlib import Path

import asyncpg


async def _snapshot(con, manifest):
    grant = manifest["grant"]
    row = await con.fetchrow(
        "SELECT desired_generation, applied_generation, gateway_generation, lease_seq, state, "
        "last_readback IS NOT NULL AS has_readback FROM grants WHERE id=$1",
        grant["id"],
    )
    statuses = dict(
        await con.fetch(
            "SELECT status, count(*) FROM outbox_operations WHERE gateway_id=$1 AND binding_id=$2 GROUP BY status",
            grant["gateway_id"], grant["binding_id"],
        )
    )
    pending = sum(statuses.get(name, 0) for name in ("pending", "processing"))
    op = await con.fetchrow(
        "SELECT status FROM outbox_operations WHERE gateway_id=$1 AND binding_id=$2 "
        "AND operation_type='gateway.apply_grant' ORDER BY created_at DESC, id DESC LIMIT 1",
        grant["gateway_id"], grant["binding_id"],
    )
    ends = await con.fetchval("SELECT ends_at FROM entitlements WHERE id=$1", manifest["entitlement"]["id"])
    return row, statuses, pending, (op["status"] if op else None), ends


async def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("work")
    parser.add_argument("manifest")
    parser.add_argument("--timeout", type=float, default=120.0)
    parser.add_argument("--interval", type=float, default=2.0)
    args = parser.parse_args()
    manifest = json.loads(Path(args.manifest).read_text())
    expected = manifest["grant"]
    url = json.loads((Path(args.work) / "state.json").read_text())["database_url"]
    deadline = time.monotonic() + args.timeout
    connect_budget = max(1.0, min(10.0, args.timeout))
    con = await asyncpg.connect(url, timeout=connect_budget)
    try:
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                print("TIMEOUT_STALL bounded wait exhausted before a snapshot could complete")
                return 4
            try:
                row, statuses, pending, op_status, ends = await asyncio.wait_for(
                    _snapshot(con, manifest), timeout=remaining
                )
            except TimeoutError:
                print("TIMEOUT_STALL snapshot exceeded the bounded wait")
                return 4
            now = dt.datetime.now(dt.UTC)
            ends_utc = ends.replace(tzinfo=dt.UTC) if ends.tzinfo is None else ends.astimezone(dt.UTC)
            # Entitlement expiry is terminal unconditionally: never masked by an applied/confirmed
            # post-worker state, and never extended by this wait. (A short grant lease expiring
            # while the entitlement is alive is a different, truthful-pending case.)
            if ends_utc <= now:
                print("EXPIRED_ENTITLEMENT window passed; no extension performed")
                return 3
            if row["state"] in ("revoked", "failed") or op_status in ("failed", "dead"):
                print(f"TERMINAL state={row['state']} apply_op={op_status}")
                return 2
            ready = (
                int(row["desired_generation"]) == int(row["applied_generation"]) == expected["applied_generation"] + 1
                and int(row["gateway_generation"]) == expected["gateway_generation"]
                and int(row["lease_seq"]) >= 1
                and row["state"] == "applied"
                and row["has_readback"]
                and pending == 0
                and op_status == "done"
            )
            if ready:
                print(f"POST_WORKER_OK statuses={json.dumps(statuses, sort_keys=True)}")
                return 0
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                print(f"TIMEOUT pending={pending} apply_op={op_status} state={row['state']} lease={row['lease_seq']}")
                return 4
            await asyncio.sleep(min(args.interval, remaining))
    finally:
        await con.close()


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
