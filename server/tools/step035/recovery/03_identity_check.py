#!/usr/bin/env python3
"""Identity/transition check vs manifest.

Usage: 03_identity_check.py <work> <manifest.json> --stage reconcile|post-worker

reconcile   : desired=4, applied=3, wire=1, lease=0, state pending; exactly one pending APPLY op
              (target 4); all manifest counts unchanged; 9 pre-existing ops stay done.
post-worker : desired=applied=4, wire=1, lease>=1, state applied; all ops done; counts unchanged;
              a lease that has since expired is still valid (truthful pending via renewal later).
"""
from __future__ import annotations

import argparse
import asyncio
import json
import sys
from pathlib import Path

import asyncpg


async def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("work")
    parser.add_argument("manifest")
    parser.add_argument("--stage", choices=["reconcile", "post-worker"], required=True)
    args = parser.parse_args()
    manifest = json.loads(Path(args.manifest).read_text())
    grant = manifest["grant"]
    url = json.loads((Path(args.work) / "state.json").read_text())["database_url"]
    con = await asyncpg.connect(url, timeout=10)
    try:
        counts = {table: await con.fetchval(f"SELECT count(*) FROM {table}") for table in manifest["counts"]}
        expected_counts = dict(manifest["counts"])
        if args.stage == "reconcile":
            expected_counts["outbox_operations"] = manifest["counts"]["outbox_operations"] + 1
        if args.stage == "post-worker":
            expected_counts["outbox_operations"] = manifest["counts"]["outbox_operations"] + 2
        statuses = dict(await con.fetch("SELECT status, count(*) FROM outbox_operations GROUP BY status"))
        row = await con.fetchrow(
            "SELECT opaque_id, desired_generation, applied_generation, gateway_generation, lease_seq, state, "
            "gateway_credential IS NOT NULL AS has_credential FROM grants WHERE id=$1",
            grant["id"],
        )
        checks = {"counts": counts == expected_counts, "credential_present": row["has_credential"]}
        if args.stage == "reconcile":
            checks.update(
                grant_identity=str(row["opaque_id"]) == grant["opaque_id"],
                desired=int(row["desired_generation"]) == grant["desired_generation"] + 1 == 4,
                applied=int(row["applied_generation"]) == grant["applied_generation"] == 3,
                wire=int(row["gateway_generation"]) == 1,
                lease=int(row["lease_seq"]) == 0,
                state=row["state"] == "pending",
                ops=statuses == {"done": manifest["outbox_statuses"].get("done", 0), "pending": 1},
            )
        else:
            checks.update(
                grant_identity=str(row["opaque_id"]) == grant["opaque_id"],
                applied_desired=int(row["applied_generation"]) == int(row["desired_generation"]) == grant["applied_generation"] + 1 == 4,
                wire=int(row["gateway_generation"]) == 1,
                lease_at_least_one=int(row["lease_seq"]) >= 1,
                state=row["state"] == "applied",
                ops=statuses == {"done": manifest["outbox_statuses"].get("done", 0) + 2},
            )
        failed = [name for name, verdict in checks.items() if not verdict]
        if failed:
            detail = {"stage": args.stage, "failed": failed, "counts": counts,
                      "expected_counts": expected_counts, "statuses": statuses}
            print("IDENTITY_MISMATCH " + json.dumps(detail, sort_keys=True), file=sys.stderr)
            return 1
        print(f"IDENTITY_OK stage={args.stage} counts_ok")
        return 0
    finally:
        await con.close()


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
