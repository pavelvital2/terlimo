#!/usr/bin/env python3
"""Guarded fixture operation: add an indefinite TEST entitlement for the same account.

Usage: 06_ensure_indefinite_entitlement.py <work> --account-id <uuid> --binding-id <uuid>
       --installation-id <uuid> --old-entitlement-id <uuid>
       [--device-limit 2] [--source-legacy-id step035-test-indefinite-v1] [--dry-run]

One transaction, no deletions:
  1. prestate asserts: account verified, binding active generation 1, installation technical,
     the existing (old) entitlement row still present;
  2. idempotence by a STABLE source_legacy_id: an existing active NULL-ends row for this account
     returns ALREADY (no mutation); any other existing row with that id refuses;
  3. otherwise INSERT one row: kind='paid', status='active', starts_at=now(), ends_at=NULL,
     device_limit, revision=1, source_legacy_id;
  4. post-assert: the session_auth-equivalent selection (active + started + indefinite/future,
     newest created wins) returns the new row.

Rollback guidance: before any grant is applied, `DELETE FROM entitlements WHERE id=<new id>` is
allowed; AFTER a grant was applied, restore from the protected checkpoint instead (no blind
delete). The old expired entitlement is never modified or deleted. No schema/API changes.
"""
from __future__ import annotations

import argparse
import asyncio
import json
import sys

import asyncpg


async def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("work")
    parser.add_argument("--account-id", required=True)
    parser.add_argument("--binding-id", required=True)
    parser.add_argument("--installation-id", required=True)
    parser.add_argument("--old-entitlement-id", required=True)
    parser.add_argument("--grant-id", required=True)
    parser.add_argument("--expected-old-ends-at", default="2026-09-22T13:38:22.990132+00:00")
    parser.add_argument("--expected-outbox-done", type=int, default=11)
    parser.add_argument("--device-limit", type=int, default=2)
    parser.add_argument("--source-legacy-id", default="step035-test-indefinite-v1")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    url = json.loads((__import__("pathlib").Path(args.work) / "state.json").read_text())["database_url"]
    con = await asyncpg.connect(url, timeout=10)
    try:
        async with con.transaction():
            # serialize per account against any concurrent operator write
            await con.execute("SELECT pg_advisory_xact_lock(hashtext($1))", "step035-entitlement:" + args.account_id)
            account = await con.fetchval("SELECT status FROM accounts WHERE id=$1", args.account_id)
            binding = await con.fetchrow(
                "SELECT status, generation FROM account_bindings WHERE id=$1 AND account_id=$2 AND installation_id=$3",
                args.binding_id, args.account_id, args.installation_id,
            )
            installation = await con.fetchval("SELECT state FROM installations WHERE id=$1", args.installation_id)
            old_row = await con.fetchrow(
                "SELECT kind, status, device_limit, ends_at FROM entitlements WHERE id=$1 AND account_id=$2",
                args.old_entitlement_id, args.account_id,
            )
            existing_rows = await con.fetch(
                "SELECT id, status, ends_at, starts_at, device_limit FROM entitlements WHERE account_id=$1 AND source_legacy_id=$2",
                args.account_id, args.source_legacy_id,
            )
            grant = await con.fetchrow(
                "SELECT id, desired_generation, applied_generation, gateway_generation, lease_seq, state FROM grants WHERE id=$1",
                args.grant_id,
            )
            outbox_done = await con.fetchval("SELECT count(*) FROM outbox_operations WHERE status='done'")
            effective_before = await con.fetchval(
                """SELECT count(*) FROM entitlements
                   WHERE account_id=$1 AND kind IN ('trial','paid','imported') AND status='active'
                     AND (starts_at IS NULL OR starts_at <= now()) AND (ends_at IS NULL OR ends_at > now())""",
                args.account_id,
            )
            import datetime as _dt
            expected_old = _dt.datetime.fromisoformat(args.expected_old_ends_at)
            now_check = _dt.datetime.now(_dt.UTC)
            pre_ok = (
                account == "verified" and binding is not None and binding["status"] == "active"
                and int(binding["generation"]) == 1 and installation == "technical"
                and old_row is not None and old_row["kind"] == "paid" and old_row["status"] == "active"
                and int(old_row["device_limit"] or 0) == 2
                and old_row["ends_at"] is not None
                and old_row["ends_at"].astimezone(_dt.UTC) == expected_old.astimezone(_dt.UTC)
                and old_row["ends_at"] < now_check
                and grant is not None and int(grant["desired_generation"]) == 4
                and int(grant["applied_generation"]) == 4 and int(grant["gateway_generation"]) == 1
                and int(grant["lease_seq"]) == 1 and grant["state"] == "applied"
                and int(outbox_done) == args.expected_outbox_done
            )
            if not pre_ok:
                raise RuntimeError("prestate mismatch: ROLLBACK")
            if existing_rows:
                if len(existing_rows) != 1:
                    raise RuntimeError(f"source_legacy_id duplicates ({len(existing_rows)}): ROLLBACK")
                row = existing_rows[0]
                already = (
                    row["status"] == "active" and row["ends_at"] is None
                    and int(row["device_limit"] or 0) == args.device_limit
                    and (row["starts_at"] is None or row["starts_at"] <= now_check)
                    and int(effective_before) == 1
                )
                if already:
                    print(f"ALREADY id={row['id']} status=active ends_at=NULL device_limit={args.device_limit}")
                    return 0
                raise RuntimeError("source_legacy_id exists in a non-indefinite state: ROLLBACK")
            if int(effective_before) != 0:
                raise RuntimeError("an effective commercial entitlement already exists: ROLLBACK")
            if args.dry_run:
                print("DRY_RUN would insert indefinite paid entitlement for the same account")
                return 0
            new_id = await con.fetchval(
                """INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at, device_limit, revision, source_legacy_id)
                   VALUES ($1,'paid','active', now(), NULL, $2, 1, $3) RETURNING id""",
                args.account_id, args.device_limit, args.source_legacy_id,
            )
            effective = await con.fetchval(
                """SELECT id FROM entitlements
                   WHERE account_id=$1 AND kind IN ('trial','paid','imported') AND status='active'
                     AND (starts_at IS NULL OR starts_at <= now()) AND (ends_at IS NULL OR ends_at > now())
                   ORDER BY created_at DESC LIMIT 1""",
                args.account_id,
            )
            source_rows = await con.fetchval(
                "SELECT count(*) FROM entitlements WHERE account_id=$1 AND source_legacy_id=$2",
                args.account_id, args.source_legacy_id,
            )
            effective_after = await con.fetchval(
                """SELECT count(*) FROM entitlements
                   WHERE account_id=$1 AND kind IN ('trial','paid','imported') AND status='active'
                     AND (starts_at IS NULL OR starts_at <= now()) AND (ends_at IS NULL OR ends_at > now())""",
                args.account_id,
            )
            if str(effective) != str(new_id) or int(source_rows) != 1 or int(effective_after) != 1:
                raise RuntimeError("post-assert failed: ROLLBACK")
        print(f"ENTITLEMENT_OK id={new_id} status=active ends_at=NULL device_limit={args.device_limit} source_legacy_id={args.source_legacy_id}")
        return 0
    finally:
        await con.close()


if __name__ == "__main__":
    try:
        raise SystemExit(asyncio.run(main()))
    except RuntimeError as error:
        print(str(error), file=sys.stderr)
        raise SystemExit(1) from error
