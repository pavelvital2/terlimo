#!/usr/bin/env python3
"""Atomic reconciliation of the restored TEST grant (bounded operator artifact).

Usage: 02_reconcile_lease.py <work> <manifest.json>

One transaction, before API/worker start:
  1. exact-manifest guard on the restored row (ids/state/applied=desired=3/wire=1/old lease=3,
     UTC not_after, binding/installation/entitlement exact, join cardinality 1),
  2. full-key lease baseline reset (exactly `UPDATE 1`),
  3. the existing business function `ensure_grant(max_lease_seconds=900)` in the SAME
     transaction: expected `enqueued`, same grant/credential/binding, desired=4/applied=3/
     wire=1/lease=0, exactly one matching pending APPLY outbox operation.
Any mismatch/expired entitlement/revoked state rolls back the whole transaction (including the
reset). A repeat run refuses on the exact-prestate guard without a second mutation. Does not
change product catalog/access contracts, does not fake applied/readback, does not extend terms.
"""
from __future__ import annotations

import asyncio
import datetime as dt
import json
import sys
from pathlib import Path

import asyncpg

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT))
from terlimo_backend.gateway_control import APPLY_OPERATION, ensure_grant

MAX_LEASE_SECONDS = 900


def _as_utc(value: dt.datetime) -> dt.datetime:
    return value.replace(tzinfo=dt.UTC) if value.tzinfo is None else value.astimezone(dt.UTC)


async def main() -> int:
    work, manifest_path = sys.argv[1], sys.argv[2]
    manifest = json.loads(Path(manifest_path).read_text())
    url = json.loads((Path(work) / "state.json").read_text())["database_url"]
    con = await asyncpg.connect(url, timeout=10)
    await con.set_type_codec("jsonb", schema="pg_catalog", encoder=json.dumps, decoder=json.loads)
    expected = manifest["grant"]
    try:
        async with con.transaction():
            rows = await con.fetch(
                """
                SELECT g.*, b.id AS b_id, b.status AS b_status, b.generation AS b_generation,
                       i.id AS i_id, i.state AS i_state,
                       e.id AS e_id, e.account_id AS e_account, e.status AS e_status, e.ends_at AS e_ends
                FROM grants g
                JOIN account_bindings b ON b.id = g.binding_id
                JOIN installations i ON i.id = b.installation_id
                JOIN entitlements e ON e.id = $2 AND e.account_id = b.account_id
                WHERE g.id = $1 FOR UPDATE OF g
                """,
                expected["id"], manifest["entitlement"]["id"],
            )
            if len(rows) != 1:
                raise RuntimeError(f"join cardinality {len(rows)} != 1: ROLLBACK")
            row = rows[0]
            now = dt.datetime.now(dt.UTC)
            expired_entitlement = _as_utc(row["e_ends"]) <= now
            checks = {
                "opaque": str(row["opaque_id"]) == expected["opaque_id"],
                "binding": str(row["binding_id"]) == expected["binding_id"] == str(row["b_id"]),
                "gateway": str(row["gateway_id"]) == expected["gateway_id"],
                "state": row["state"] == expected["state"] == "applied",
                "applied_desired": int(row["applied_generation"]) == int(row["desired_generation"])
                == expected["applied_generation"] == expected["desired_generation"] == 3,
                "wire_generation": int(row["gateway_generation"]) == expected["gateway_generation"] == 1,
                "old_lease": int(row["lease_seq"]) == expected["lease_seq"] == 3,
                "old_expiry": _as_utc(row["not_after"]).isoformat() == expected["not_after_utc"],
                "expiry_in_past": _as_utc(row["not_after"]) < now,
                "binding_status": row["b_status"] == manifest["binding"]["status"] == "active",
                "binding_generation": int(row["b_generation"]) == manifest["binding"]["generation"],
                "installation": str(row["i_id"]) == manifest["installation"]["id"]
                and row["i_state"] == manifest["installation"]["state"] == "technical",
                "entitlement": str(row["e_id"]) == manifest["entitlement"]["id"]
                and str(row["e_account"]) == manifest["entitlement"]["account_id"]
                and row["e_status"] == manifest["entitlement"]["status"] == "active",
                "entitlement_window": _as_utc(row["e_ends"]).isoformat() == manifest["entitlement"]["ends_at_utc"]
                and not expired_entitlement,
                "no_pending_apply": await con.fetchval(
                    "SELECT count(*) FROM outbox_operations WHERE gateway_id=$1 AND binding_id=$2 "
                    "AND operation_type=$3 AND status IN ('pending','processing')",
                    expected["gateway_id"], expected["binding_id"], APPLY_OPERATION,
                ) == 0,
            }
            if not all(checks.values()):
                failed = [name for name, verdict in checks.items() if not verdict]
                if expired_entitlement:
                    print("BLOCKED_EXPIRED entitlement window passed; no extension performed")
                raise RuntimeError(f"prestate mismatch {failed}: ROLLBACK")
            status = await con.execute(
                """UPDATE grants SET lease_seq = 0
                   WHERE id = $1 AND opaque_id = $2 AND binding_id = $3 AND gateway_id = $4
                     AND state = 'applied' AND applied_generation = desired_generation
                     AND gateway_generation = 1 AND lease_seq = 3 AND not_after < now()""",
                expected["id"], expected["opaque_id"], expected["binding_id"], expected["gateway_id"],
            )
            if status != "UPDATE 1":
                raise RuntimeError(f"expected UPDATE 1, got {status}: ROLLBACK")
            outcome = await ensure_grant(
                con,
                binding_id=expected["binding_id"],
                gateway_id=expected["gateway_id"],
                entitlement_id=manifest["entitlement"]["id"],
                max_lease_seconds=MAX_LEASE_SECONDS,
            )
            if outcome != "enqueued":
                raise RuntimeError(f"ensure_grant outcome {outcome}: ROLLBACK")
            after = await con.fetchrow(
                "SELECT desired_generation, applied_generation, gateway_generation, lease_seq, state, not_after FROM grants WHERE id=$1",
                expected["id"],
            )
            post_ok = (
                int(after["desired_generation"]) == expected["desired_generation"] + 1 == 4
                and int(after["applied_generation"]) == expected["applied_generation"] == 3
                and int(after["gateway_generation"]) == 1
                and int(after["lease_seq"]) == 0
                and after["state"] == "pending"
            )
            ops = await con.fetch(
                "SELECT id, status, target_revision FROM outbox_operations WHERE gateway_id=$1 "
                "AND binding_id=$2 AND operation_type=$3 AND status IN ('pending','processing')",
                expected["gateway_id"], expected["binding_id"], APPLY_OPERATION,
            )
            ops_ok = len(ops) == 1 and ops[0]["status"] == "pending" and int(ops[0]["target_revision"]) == 4
            if not (post_ok and ops_ok):
                raise RuntimeError(
                    f"post-reconcile state invalid post_ok={post_ok} ops={[(str(o['id']), o['status'], o['target_revision']) for o in ops]}: ROLLBACK"
                )
        print("RECONCILE_OK desired=4 applied=3 wire=1 lease=0 pending_apply=1")
        return 0
    finally:
        await con.close()


if __name__ == "__main__":
    try:
        raise SystemExit(asyncio.run(main()))
    except RuntimeError as error:
        print(str(error), file=sys.stderr)
        raise SystemExit(1) from error
