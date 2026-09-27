"""§20 account-wide usage: immutable historical owner, all devices/gateways, MSK windows,
conservative account-scope coverage (no false complete, no cross-account transfer)."""

from __future__ import annotations

import hashlib
import secrets
from datetime import UTC, datetime, timedelta

from aiohttp.test_utils import TestClient, TestServer
from test_catalog_access_sync import (
    MOBILE,
    _add_gateway,
    _auth,
    _connect,
    _seed_subject,
)

from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.usage_pipeline import MSK


async def _add_device(database_url, account_id, *, status="active"):
    connection = await _connect(database_url)
    try:
        fingerprint = hashlib.sha256(secrets.token_bytes(32)).hexdigest()
        installation_id = await connection.fetchval(
            """
            INSERT INTO installations
                (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test', 'android', $1, 'spki', 'technical') RETURNING id
            """,
            fingerprint,
        )
        binding_id = await connection.fetchval(
            """
            INSERT INTO account_bindings (account_id, installation_id, status)
            VALUES ($1, $2, $3) RETURNING id
            """,
            account_id,
            installation_id,
            status,
        )
        return installation_id, binding_id, fingerprint
    finally:
        await connection.close()


async def _tick(database_url, gateway_id, installation_id, account_id, observed, rx, tx, boot="b"):
    connection = await _connect(database_url)
    try:
        await connection.execute(
            """
            INSERT INTO usage_ticks
                (gateway_id, boot_id, registration_id, installation_id, account_id,
                 observed_at, rx_delta, tx_delta)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
            """,
            gateway_id,
            boot,
            hashlib.sha256(secrets.token_bytes(16)).hexdigest(),
            installation_id,
            account_id,
            observed,
            rx,
            tx,
        )
    finally:
        await connection.close()


async def _coverage(database_url, installation_id, segment_start, last_trusted):
    connection = await _connect(database_url)
    try:
        await connection.execute(
            """
            INSERT INTO usage_coverage (installation_id, segment_start, last_trusted_at)
            VALUES ($1, $2, $3)
            ON CONFLICT (installation_id) DO UPDATE
            SET segment_start = EXCLUDED.segment_start, last_trusted_at = EXCLUDED.last_trusted_at
            """,
            installation_id,
            segment_start,
            last_trusted,
        )
    finally:
        await connection.close()


async def _usage(client, subject):
    response = await client.get(f"{MOBILE}/usage", headers=_auth(subject["token"]))
    assert response.status == 200, await response.text()
    body = await response.json()
    return {bucket["period"]: bucket for bucket in body["buckets"]}, body


async def test_account_totals_all_devices_and_gateways_exclude_foreign(
    migrated_url, settings_factory
):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    subject_a = await _seed_subject(migrated_url)
    gateway1 = await _add_gateway(migrated_url, "gw-acc-1")
    gateway2 = await _add_gateway(migrated_url, "gw-acc-2")
    device2, _binding2, _fp2 = await _add_device(migrated_url, subject_a["account_id"])
    foreign = await _seed_subject(migrated_url)

    now = datetime.now(UTC)
    today_start = now.astimezone(MSK).replace(hour=0, minute=0, second=0, microsecond=0)

    def at(days_ago: int, msk_hour: int) -> datetime:
        return (today_start - timedelta(days=days_ago) + timedelta(hours=msk_hour)).astimezone(UTC)

    # Same device over two gateways, second device in the same account, MSK boundary tick,
    # a foreign account, and one unattributed (NULL) tick that must never be backfilled.
    await _tick(migrated_url, gateway1, subject_a["installation_id"], subject_a["account_id"], at(0, 1), 10, 20)
    await _tick(migrated_url, gateway2, subject_a["installation_id"], subject_a["account_id"], at(0, 2), 1, 2, boot="b2")
    await _tick(migrated_url, gateway1, device2, subject_a["account_id"], at(0, 3), 5, 5, boot="b3")
    await _tick(migrated_url, gateway1, subject_a["installation_id"], subject_a["account_id"], at(1, 23), 7, 8, boot="b4")
    await _tick(migrated_url, gateway1, foreign["installation_id"], foreign["account_id"], at(0, 4), 999, 999, boot="b5")
    await _tick(migrated_url, gateway1, subject_a["installation_id"], None, at(0, 5), 500, 500, boot="b6")

    for installation in (subject_a["installation_id"], device2):
        await _coverage(
            migrated_url, installation, today_start - timedelta(days=31), now
        )
    await _coverage(
        migrated_url, foreign["installation_id"], today_start - timedelta(days=31), now
    )

    database = Database(settings)
    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    try:
        buckets, body = await _usage(client, subject_a)
    finally:
        await client.close()
        await database.close()

    assert (buckets["today"]["rx_bytes"], buckets["today"]["tx_bytes"]) == (16, 27)
    assert (buckets["7d"]["rx_bytes"], buckets["7d"]["tx_bytes"]) == (23, 35)
    assert (buckets["30d"]["rx_bytes"], buckets["30d"]["tx_bytes"]) == (23, 35)
    assert all(buckets[p]["complete"] is True for p in ("today", "7d", "30d"))
    assert body["coverage_start"] is not None and body["as_of"] is not None


async def test_history_survives_revoke_and_rebind_never_transfers(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    subject_a = await _seed_subject(migrated_url)
    gateway = await _add_gateway(migrated_url, "gw-acc-rev")
    moved, binding, _fp = await _add_device(migrated_url, subject_a["account_id"])
    subject_b = await _seed_subject(migrated_url)

    now = datetime.now(UTC)
    today_start = now.astimezone(MSK).replace(hour=0, minute=0, second=0, microsecond=0)
    await _tick(migrated_url, gateway, moved, subject_a["account_id"], today_start + timedelta(hours=1), 40, 60)
    await _coverage(migrated_url, moved, today_start - timedelta(days=31), now)
    await _coverage(migrated_url, subject_a["installation_id"], today_start - timedelta(days=31), now)
    await _coverage(migrated_url, subject_b["installation_id"], today_start - timedelta(days=31), now)

    # The device leaves account A (revoked binding) and is attached to account B afterwards.
    connection = await _connect(migrated_url)
    try:
        await connection.execute(
            "UPDATE account_bindings SET status='revoked', generation=generation+1, revoked_at=now() WHERE id=$1",
            binding,
        )
        await connection.execute(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')",
            subject_b["account_id"],
            moved,
        )
    finally:
        await connection.close()

    database = Database(settings)
    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    try:
        buckets_a, _ = await _usage(client, subject_a)
        buckets_b, _body_b = await _usage(client, subject_b)

        # A keeps its recorded history, B never inherits it; both sides are conservative
        # incomplete while the installation has binding rows for two accounts (the mutable
        # per-installation coverage row is not accepted as proof for either account).
        assert buckets_a["today"]["rx_bytes"] == 40
        assert buckets_a["today"]["tx_bytes"] == 60
        assert buckets_b["today"]["rx_bytes"] == 0
        assert buckets_b["today"]["tx_bytes"] == 0
        for label, buckets in (("A", buckets_a), ("B", buckets_b)):
            assert all(buckets[p]["complete"] is False for p in ("today", "7d", "30d")), label

        # B's later collector activity updates the shared coverage row: it must not become A's
        # proof either (no cross-account coverage ownership).
        await _coverage(migrated_url, moved, today_start - timedelta(days=31), now)
        buckets_a_after, _ = await _usage(client, subject_a)
        buckets_b_after, _ = await _usage(client, subject_b)
        assert buckets_a_after["today"]["rx_bytes"] == 40
        assert buckets_b_after["today"]["rx_bytes"] == 0
        assert buckets_a_after["today"]["complete"] is False
        assert buckets_b_after["today"]["complete"] is False
    finally:
        await client.close()
        await database.close()


async def test_retired_contributors_are_conservative(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    gateway = await _add_gateway(migrated_url, "gw-acc-retired")
    now = datetime.now(UTC)
    today_start = now.astimezone(MSK).replace(hour=0, minute=0, second=0, microsecond=0)

    # Scenario A: a device retired inside the window has no final-tail proof, even with a
    # trusted sample shortly before revoke.
    subject_a = await _seed_subject(migrated_url)
    retired, binding_retired, _fp = await _add_device(migrated_url, subject_a["account_id"])
    await _tick(migrated_url, gateway, subject_a["installation_id"], subject_a["account_id"], now, 1, 1)
    await _coverage(migrated_url, subject_a["installation_id"], today_start - timedelta(days=31), now)
    await _coverage(migrated_url, retired, today_start - timedelta(days=31), now - timedelta(seconds=300))
    connection = await _connect(migrated_url)
    try:
        await connection.execute(
            "UPDATE account_bindings SET status='revoked', generation=generation+1, revoked_at=now() WHERE id=$1",
            binding_retired,
        )
    finally:
        await connection.close()

    # Scenario B: a veteran retired strictly before the window with no later account ticks.
    subject_b = await _seed_subject(migrated_url)
    veteran, binding_veteran, _fp2 = await _add_device(migrated_url, subject_b["account_id"])
    await _tick(migrated_url, gateway, subject_b["installation_id"], subject_b["account_id"], now, 5, 5, boot="b2")
    await _tick(
        migrated_url, gateway, veteran, subject_b["account_id"],
        now - timedelta(days=41), 7, 7, boot="b3",
    )
    await _coverage(migrated_url, subject_b["installation_id"], today_start - timedelta(days=31), now)
    await _coverage(migrated_url, veteran, today_start - timedelta(days=60), now - timedelta(days=40))
    connection = await _connect(migrated_url)
    try:
        await connection.execute(
            "UPDATE account_bindings SET status='revoked', generation=generation+1,"
            " revoked_at=now() - interval '40 days' WHERE id=$1",
            binding_veteran,
        )
    finally:
        await connection.close()

    database = Database(settings)
    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    try:
        buckets_a, _ = await _usage(client, subject_a)
        assert buckets_a["today"]["rx_bytes"] == 1
        assert all(buckets_a[p]["complete"] is False for p in ("today", "7d", "30d"))

        buckets_b, _ = await _usage(client, subject_b)
        assert buckets_b["today"]["rx_bytes"] == 5
        assert all(buckets_b[p]["complete"] is True for p in ("today", "7d", "30d"))

        # A late account tick after retirement invalidates even the strictly-before-window case.
        await _tick(
            migrated_url, gateway, veteran, subject_b["account_id"], now, 9, 9, boot="b4"
        )
        buckets_b, _ = await _usage(client, subject_b)
        assert buckets_b["today"]["rx_bytes"] == 14
        assert buckets_b["today"]["complete"] is False
    finally:
        await client.close()
        await database.close()


async def test_foreign_binding_ticks_do_not_leave_account_proof(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    subject_a = await _seed_subject(migrated_url)
    gateway = await _add_gateway(migrated_url, "gw-acc-foreign")
    subject_b = await _seed_subject(migrated_url)
    # Installation X has A-owned ticks but only a B binding row remains.
    foreign, _binding_b, _fp = await _add_device(migrated_url, subject_b["account_id"])
    now = datetime.now(UTC)
    today_start = now.astimezone(MSK).replace(hour=0, minute=0, second=0, microsecond=0)
    await _tick(migrated_url, gateway, subject_a["installation_id"], subject_a["account_id"], now, 1, 1)
    await _tick(migrated_url, gateway, foreign, subject_a["account_id"], now, 50, 70, boot="b2")
    await _coverage(migrated_url, subject_a["installation_id"], today_start - timedelta(days=31), now)
    await _coverage(migrated_url, foreign, today_start - timedelta(days=31), now)

    database = Database(settings)
    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    try:
        buckets_a, _ = await _usage(client, subject_a)
        buckets_b, _ = await _usage(client, subject_b)
    finally:
        await client.close()
        await database.close()

    # A's contributor list must not silently drop the installation whose ownership proof is
    # missing: totals include A's ticks, but completeness stays conservative.
    assert buckets_a["today"]["rx_bytes"] == 51
    assert buckets_a["today"]["tx_bytes"] == 71
    assert all(buckets_a[p]["complete"] is False for p in ("today", "7d", "30d"))
    # B never inherits A's historical ticks.
    assert buckets_b["today"]["rx_bytes"] == 0
    assert buckets_b["today"]["tx_bytes"] == 0
