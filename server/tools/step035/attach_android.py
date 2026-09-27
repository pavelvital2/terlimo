#!/usr/bin/env python3
"""STEP03.5 attach/detach the synthetic TEST fixture to an Android installation.

The phone enrolls itself through the real PoP API first; this script only binds the synthetic
TEST account/binding/entitlement to that **own** installation fingerprint in the package
PostgreSQL. It never touches sessions, never transfers any key, and is a closed TEST fixture —
not Telegram trusted-login provenance (that product gate remains open).

Usage:
    .venv/bin/python tools/step035/attach_android.py --fingerprint <hex64>
    .venv/bin/python tools/step035/attach_android.py --fingerprint <hex64> --revoke
"""

from __future__ import annotations

import argparse
import asyncio
import importlib.util
import json
import re
import secrets
from datetime import UTC, datetime, timedelta
from pathlib import Path

import asyncpg


def _load_run_module():
    spec = importlib.util.spec_from_file_location(
        "step035_run_isolated", Path(__file__).resolve().parent / "run_isolated.py"
    )
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


run_module = _load_run_module()

HEX64 = re.compile(r"^[0-9a-f]{64}$")


async def connect(database_url: str) -> asyncpg.Connection:
    connection = await asyncpg.connect(database_url, timeout=10)
    await connection.set_type_codec(
        "jsonb", schema="pg_catalog", encoder=json.dumps, decoder=json.loads
    )
    return connection


async def revoke(database_url: str, fingerprint: str) -> dict:
    connection = await connect(database_url)
    try:
        installation = await connection.fetchrow(
            """
            SELECT installation.id, installation.environment
            FROM installations AS installation
            WHERE installation.public_key_fingerprint = $1
            """,
            fingerprint,
        )
        if installation is None:
            return {"ok": False, "reason": "installation_unknown"}
        binding = await connection.fetchrow(
            "SELECT id, status FROM account_bindings WHERE installation_id = $1",
            installation["id"],
        )
        if binding is None:
            return {"ok": False, "reason": "no_fixture_binding"}
        from terlimo_backend.gateway_control import revoke_binding_grants

        await revoke_binding_grants(connection, binding_id=binding["id"])
        deadline = datetime.now(UTC) + timedelta(seconds=45)
        while datetime.now(UTC) < deadline:
            grants = await connection.fetch(
                "SELECT state, applied_generation, desired_generation, last_readback "
                "FROM grants WHERE binding_id = $1",
                binding["id"],
            )
            if grants and all(
                row["state"] == "revoked"
                and row["applied_generation"] == row["desired_generation"]
                and (row["last_readback"] or {}).get("revoked") is True
                for row in grants
            ):
                return {"ok": True, "revoked_grants": len(grants)}
            await asyncio.sleep(0.5)
        return {"ok": False, "reason": "actual_revoke_not_confirmed"}
    finally:
        await connection.close()


async def attach(database_url: str, fingerprint: str, ttl_seconds: int) -> dict:
    connection = await connect(database_url)
    try:
        installation = await connection.fetchrow(
            """
            SELECT id, environment, state FROM installations
            WHERE public_key_fingerprint = $1
            """,
            fingerprint,
        )
        if installation is None:
            return {
                "ok": False,
                "reason": "installation_unknown: let Android complete its own PoP enrollment first",
            }
        if installation["state"] == "revoked":
            return {"ok": False, "reason": "installation_revoked"}
        existing = await connection.fetchrow(
            "SELECT id, account_id FROM account_bindings WHERE installation_id = $1",
            installation["id"],
        )
        if existing is not None:
            return {"ok": False, "reason": "binding_already_exists", "binding_id": str(existing["id"])}
        account_id = await connection.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified', $1) RETURNING id",
            secrets.randbelow(1 << 40),
        )
        binding_id = await connection.fetchval(
            """
            INSERT INTO account_bindings (account_id, installation_id, status)
            VALUES ($1, $2, 'active') RETURNING id
            """,
            account_id,
            installation["id"],
        )
        ends_at = datetime.now(UTC) + timedelta(seconds=ttl_seconds)
        entitlement_id = await connection.fetchval(
            """
            INSERT INTO entitlements
                (account_id, kind, status, starts_at, ends_at, device_limit, revision)
            VALUES ($1, 'paid', 'active', now() - interval '1 minute', $2, 2, 1)
            RETURNING id
            """,
            account_id,
            ends_at,
        )
        return {
            "ok": True,
            "fixture": "synthetic TEST, paid (not trial), single installation only",
            "installation_ref": fingerprint,
            "account_ref": str(account_id),
            "binding_ref": str(binding_id),
            "entitlement_ref": str(entitlement_id),
            "entitlement_expires_at": ends_at.strftime("%Y-%m-%dT%H:%M:%SZ"),
            "next": [
                "Android requests a fresh PoP session with scope access:sync",
                "Android GET /me -> GET /gateways -> POST /access/sync",
                "Android connects to the catalog transport with access.device_ref/password/generation/lease_seq",
                "cleanup: attach_android.py --fingerprint <hex64> --revoke, then run_isolated.py --cleanup",
            ],
        }
    finally:
        await connection.close()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--work", default=str(run_module.WORK_ROOT / "pkg"))
    parser.add_argument("--fingerprint", required=True)
    parser.add_argument("--ttl-seconds", type=int, default=7200)
    parser.add_argument("--revoke", action="store_true")
    arguments = parser.parse_args()
    if not HEX64.fullmatch(arguments.fingerprint):
        raise SystemExit("fingerprint must be lowercase hex sha256")
    try:
        work, _marker = run_module.resolve_work(arguments.work, require_owned=True)
    except RuntimeError as error:
        raise SystemExit(str(error)) from error
    state_path = work / "state.json"
    if not state_path.exists():
        raise SystemExit(f"state file not found: {state_path} (start run_isolated.py --serve first)")
    state = json.loads(state_path.read_text(encoding="utf-8"))
    if arguments.revoke:
        result = asyncio.run(revoke(state["database_url"], arguments.fingerprint))
    else:
        result = asyncio.run(
            attach(state["database_url"], arguments.fingerprint, arguments.ttl_seconds)
        )
    print(json.dumps(result, indent=2))
    return 0 if result.get("ok") else 1


if __name__ == "__main__":
    raise SystemExit(main())
