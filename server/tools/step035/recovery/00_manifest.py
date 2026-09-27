#!/usr/bin/env python3
"""Build a non-credential restore manifest from the protected STEP03.5 dump.

Usage: 00_manifest.py <pkg.sql> <manifest.json>
Prints only a verdict; never reads or emits credential values.
"""
from __future__ import annotations

import collections
import datetime as dt
import json
import re
import sys
from pathlib import Path


def _rows(text: str, table: str) -> tuple[list[str], list[dict[str, str]]]:
    match = re.search(rf"COPY public\.{table} \((.*?)\) FROM stdin;\n(.*?)\n\\\.", text, re.DOTALL)
    if match is None:
        raise SystemExit(f"COPY {table} missing")
    columns = [item.strip() for item in match.group(1).split(",")]
    rows = [dict(zip(columns, line.split("\t"))) for line in match.group(2).split("\n") if line]
    return columns, rows


def _utc(value: str) -> str:
    parsed = dt.datetime.fromisoformat(value.replace(" ", "T"))
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.UTC)
    return parsed.astimezone(dt.UTC).isoformat()


def main() -> int:
    text = Path(sys.argv[1]).read_text(encoding="utf-8", errors="replace")
    tables = {}
    for table in (
        "accounts", "installations", "account_bindings", "entitlements", "grants",
        "gateways", "sessions", "outbox_operations", "schema_migrations",
    ):
        tables[table] = _rows(text, table)[1]
    grant = tables["grants"][0]
    binding = tables["account_bindings"][0]
    installation = tables["installations"][0]
    entitlement = tables["entitlements"][0]
    outbox = collections.Counter(row["status"] for row in tables["outbox_operations"])
    manifest = {
        "format": 1,
        "counts": {name: len(rows) for name, rows in tables.items()},
        "outbox_statuses": dict(sorted(outbox.items())),
        "grant": {
            "id": grant["id"], "opaque_id": grant["opaque_id"], "binding_id": grant["binding_id"],
            "gateway_id": grant["gateway_id"], "state": grant["state"],
            "desired_generation": int(grant["desired_generation"]),
            "applied_generation": int(grant["applied_generation"]),
            "gateway_generation": int(grant["gateway_generation"]),
            "lease_seq": int(grant["lease_seq"]),
            "not_after_utc": _utc(grant["not_after"]),
        },
        "binding": {
            "id": binding["id"], "status": binding["status"],
            "generation": int(binding["generation"]),
        },
        "installation": {"id": installation["id"], "state": installation["state"]},
        "entitlement": {
            "id": entitlement["id"], "account_id": entitlement["account_id"],
            "status": entitlement["status"], "ends_at_utc": _utc(entitlement["ends_at"]),
        },
    }
    with open(sys.argv[2], "w") as handle:
        json.dump(manifest, handle, indent=2)
    print("MANIFEST_OK counts=" + ",".join(f"{k}:{v}" for k, v in manifest["counts"].items()))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
