"""S5 usage pipeline (§11): sequence fence, contiguous segments, attribution, MSK DTO."""

from __future__ import annotations

import asyncio
from datetime import UTC, datetime, timedelta

import pytest
from aiohttp.test_utils import TestClient, TestServer
from test_catalog_access_sync import (
    MOBILE,
    _add_applied_grant,
    _add_gateway,
    _auth,
    _connect,
    _seed_subject,
)

from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.gateway_adapter import GatewayError
from terlimo_backend.usage_pipeline import (
    MSK,
    REPORTING_TIMEZONE,
    apply_usage_response,
    collect_gateway_usage,
    mark_coverage_gap,
    record_final_usage,
)

LEVEL_TS = 1_700_000_000
BOOT1_STARTED = 1_699_000_000
BOOT2_STARTED = 1_701_000_000


class FakeUsageClient:
    def __init__(self, response=None, error=None):
        self.response = response
        self.error = error
        self.calls = []

    async def usage(self, password, node_id):
        self.calls.append((password, node_id))
        if self.error is not None:
            raise self.error
        return self.response


def _snapshot(
    subject,
    gateway_key,
    *,
    rx,
    tx,
    boot="boot-1",
    boot_started_at=BOOT1_STARTED,
    observed=LEVEL_TS,
    sequence=1,
    supported=True,
    peers=None,
):
    if peers is None:
        peers = [
            {"registration_id": subject["fingerprint"], "public_key": "ab" * 32,
             "managed": True, "rx_bytes": rx, "tx_bytes": tx, "last_handshake": observed}
        ]
    return {
        "node_id": gateway_key,
        "boot_id": boot,
        "boot_started_at": boot_started_at,
        "sequence": sequence,
        "observed_at": observed,
        "counters_supported": supported,
        "peers": peers,
    }


async def _seed_usage_subject(database_url, key="gw-usage"):
    subject = await _seed_subject(database_url)
    gateway = await _add_gateway(database_url, key)
    await _add_applied_grant(database_url, subject, gateway, credential=f"cred-{key}")
    return subject, gateway


async def _ticks(database_url, installation_id):
    connection = await _connect(database_url)
    try:
        row = await connection.fetchrow(
            "SELECT COALESCE(sum(rx_delta),0) AS rx, COALESCE(sum(tx_delta),0) AS tx,"
            " count(*) AS n FROM usage_ticks WHERE installation_id = $1",
            installation_id,
        )
        return int(row["rx"]), int(row["tx"]), int(row["n"])
    finally:
        await connection.close()


async def _coverage(database_url, installation_id):
    connection = await _connect(database_url)
    try:
        row = await connection.fetchrow(
            "SELECT segment_start, last_gap_at FROM usage_coverage WHERE installation_id = $1",
            installation_id,
        )
        return dict(row) if row else None
    finally:
        await connection.close()


async def _collect(connection, settings, snapshots):
    client = FakeUsageClient()
    queue = list(snapshots)

    def factory(_key, _endpoints):
        client.response = queue.pop(0)
        return client

    return await collect_gateway_usage(connection, settings, client_factory=factory)


async def test_sequence_fence_ignores_out_of_order_and_replayed_snapshots(
    migrated_url, settings_factory
):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    assert settings.gateway_local_admin_enabled is True
    subject, _gateway = await _seed_usage_subject(migrated_url)
    connection = await _connect(migrated_url)
    try:
        await _collect(connection, settings, [_snapshot(subject, "gw-usage", rx=100, tx=200, sequence=1)])
        await _collect(connection, settings, [_snapshot(subject, "gw-usage", rx=150, tx=260, sequence=2, observed=LEVEL_TS + 1)])
        await _collect(connection, settings, [_snapshot(subject, "gw-usage", rx=120, tx=210, sequence=1, observed=LEVEL_TS + 2)])
        rx_before, tx_before, _n = await _ticks(migrated_url, subject["installation_id"])
        cursor = await connection.fetchrow(
            "SELECT rx_bytes, tx_bytes, sequence FROM usage_cursors WHERE registration_id = $1",
            subject["fingerprint"],
        )
        await _collect(connection, settings, [_snapshot(subject, "gw-usage", rx=160, tx=300, sequence=3, observed=LEVEL_TS + 3)])
        rx, tx, ticks = await _ticks(migrated_url, subject["installation_id"])
    finally:
        await connection.close()
    assert (rx_before, tx_before) == (50, 60)
    assert int(cursor["sequence"]) == 2 and int(cursor["rx_bytes"]) == 150
    assert (rx, tx, ticks) == (60, 100, 2)


async def test_segments_boot_reset_and_stale_old_boot(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    assert settings.environment == "test"
    subject, _gateway = await _seed_usage_subject(migrated_url, key="gw-usage-seg")
    connection = await _connect(migrated_url)
    try:
        # First trusted sample: baseline, segment starts here, no pre-collection credit.
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-seg", rx=1000, tx=2000, sequence=1)])
        assert await _ticks(migrated_url, subject["installation_id"]) == (0, 0, 0)
        first_coverage = await _coverage(migrated_url, subject["installation_id"])
        assert first_coverage["segment_start"] is not None

        # Peer re-add inside the same boot: gap + new segment, nothing credited yet.
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-seg", rx=30, tx=40, sequence=2, observed=LEVEL_TS + 10)])
        assert await _ticks(migrated_url, subject["installation_id"]) == (0, 0, 0)
        reset_coverage = await _coverage(migrated_url, subject["installation_id"])
        assert reset_coverage["last_gap_at"] is not None
        assert reset_coverage["segment_start"] > first_coverage["segment_start"]

        # Post-reset bytes are defensible and credited on the next increasing sample.
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-seg", rx=50, tx=70, sequence=3, observed=LEVEL_TS + 20)])
        assert await _ticks(migrated_url, subject["installation_id"]) == (20, 30, 1)

        # New boot generation: explicit gap + baseline, totals unchanged.
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-seg", rx=5, tx=6, boot="boot-2", boot_started_at=BOOT2_STARTED, sequence=1, observed=LEVEL_TS + 30)])
        assert await _ticks(migrated_url, subject["installation_id"]) == (20, 30, 1)
        boot_coverage = await _coverage(migrated_url, subject["installation_id"])
        assert boot_coverage["segment_start"] > reset_coverage["segment_start"]

        # Stale replay from the older boot cannot corrupt the current cursor or segment.
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-seg", rx=9999, tx=9999, boot="boot-1", boot_started_at=BOOT1_STARTED, sequence=9, observed=LEVEL_TS + 40)])
        assert await _ticks(migrated_url, subject["installation_id"]) == (20, 30, 1)
        cursor = await connection.fetchrow(
            "SELECT boot_started_at, rx_bytes FROM usage_cursors WHERE registration_id = $1",
            subject["fingerprint"],
        )
        assert int(cursor["boot_started_at"]) == BOOT2_STARTED and int(cursor["rx_bytes"]) == 5
    finally:
        await connection.close()


async def test_rebaseline_after_collector_failure_and_recovery(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    assert settings.environment == "test"
    subject, _gateway = await _seed_usage_subject(migrated_url, key="gw-usage-recover")
    connection = await _connect(migrated_url)
    try:
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-recover", rx=100, tx=100, sequence=1)])
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-recover", rx=150, tx=140, sequence=2, observed=LEVEL_TS + 1)])
        assert await _ticks(migrated_url, subject["installation_id"]) == (50, 40, 1)

        # Collector failure/final-read failure breaks the segment explicitly.
        await mark_coverage_gap(connection, subject["installation_id"], datetime.now(UTC))
        assert (await _coverage(migrated_url, subject["installation_id"]))["segment_start"] is None

        # The first sample after the gap may span unknown bytes: baseline only, no credit.
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-recover", rx=400, tx=400, sequence=3, observed=LEVEL_TS + 10)])
        assert await _ticks(migrated_url, subject["installation_id"]) == (50, 40, 1)
        assert (await _coverage(migrated_url, subject["installation_id"]))["segment_start"] is not None

        # Trusted increments inside the new segment are credited again.
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-recover", rx=450, tx=430, sequence=4, observed=LEVEL_TS + 11)])
        assert await _ticks(migrated_url, subject["installation_id"]) == (100, 70, 2)
    finally:
        await connection.close()


async def test_absent_peer_closes_segment_once(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    assert settings.environment == "test"
    subject, _gateway = await _seed_usage_subject(migrated_url, key="gw-usage-absent")
    connection = await _connect(migrated_url)
    try:
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-absent", rx=100, tx=100, sequence=1)])
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-absent", rx=180, tx=160, sequence=2, observed=LEVEL_TS + 1)])
        assert await _ticks(migrated_url, subject["installation_id"]) == (80, 60, 1)

        # A trusted snapshot without the known peer means the removal interval is unknown.
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-absent", rx=0, tx=0, sequence=3, observed=LEVEL_TS + 2, peers=[])])
        assert await _ticks(migrated_url, subject["installation_id"]) == (80, 60, 1)
        coverage = await _coverage(migrated_url, subject["installation_id"])
        assert coverage["segment_start"] is None and coverage["last_gap_at"] is not None
        closed = await connection.fetchval(
            "SELECT closed FROM usage_cursors WHERE registration_id = $1", subject["fingerprint"]
        )
        assert closed is True

        # Peer reappears: treated as a fresh baseline (closed cursor), no historic volume.
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-absent", rx=500, tx=500, sequence=4, observed=LEVEL_TS + 3)])
        assert await _ticks(migrated_url, subject["installation_id"]) == (80, 60, 1)
        await _collect(connection, settings, [_snapshot(subject, "gw-usage-absent", rx=510, tx=520, sequence=5, observed=LEVEL_TS + 4)])
        assert await _ticks(migrated_url, subject["installation_id"]) == (90, 80, 2)
    finally:
        await connection.close()


async def test_collector_unknown_peer_and_unsupported_counters(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    assert settings.environment == "test"
    subject, gateway = await _seed_usage_subject(migrated_url, key="gw-usage2")
    unknown = _snapshot(
        subject, "gw-usage2", rx=999, tx=999,
        peers=[{"registration_id": "ff" * 32, "public_key": "cd" * 32, "managed": True,
                "rx_bytes": 999, "tx_bytes": 999, "last_handshake": LEVEL_TS}],
    )
    connection = await _connect(migrated_url)
    try:
        outcome = await apply_usage_response(
            connection, gateway_id=gateway, gateway_key="gw-usage2", response=unknown,
        )
        assert outcome["unknown"] == 1 and outcome["credited"] == 0
        assert await _ticks(migrated_url, subject["installation_id"]) == (0, 0, 0)

        unsupported = _snapshot(subject, "gw-usage2", rx=0, tx=0, supported=False)
        await apply_usage_response(
            connection, gateway_id=gateway, gateway_key="gw-usage2", response=unsupported,
        )
        assert (await _coverage(migrated_url, subject["installation_id"]))["segment_start"] is None
    finally:
        await connection.close()


async def test_attribution_fence_rejects_foreign_gateway_and_stale_grant(
    migrated_url, settings_factory
):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    assert settings.environment == "test"
    subject, gateway = await _seed_usage_subject(migrated_url, key="gw-usage-own")
    other_gateway = await _add_gateway(migrated_url, "gw-usage-other")
    connection = await _connect(migrated_url)
    try:
        outcome = await apply_usage_response(
            connection, gateway_id=other_gateway, gateway_key="gw-usage-other",
            response=_snapshot(subject, "gw-usage-other", rx=500, tx=500, sequence=1),
        )
        assert outcome["unknown"] == 1 and outcome["credited"] == 0
        assert await _ticks(migrated_url, subject["installation_id"]) == (0, 0, 0)

        await connection.execute(
            "UPDATE grants SET state = 'pending', applied_generation = NULL WHERE gateway_id = $1",
            gateway,
        )
        outcome = await apply_usage_response(
            connection, gateway_id=gateway, gateway_key="gw-usage-own",
            response=_snapshot(subject, "gw-usage-own", rx=500, tx=500, sequence=1),
        )
        assert outcome["unknown"] == 1

        await connection.execute(
            "UPDATE grants SET state = 'applied', applied_generation = 1 WHERE gateway_id = $1",
            gateway,
        )
        await connection.execute(
            "UPDATE account_bindings SET status = 'revoked' WHERE id = $1", subject["binding_id"]
        )
        outcome = await apply_usage_response(
            connection, gateway_id=gateway, gateway_key="gw-usage-own",
            response=_snapshot(subject, "gw-usage-own", rx=500, tx=500, sequence=1),
        )
        assert outcome["unknown"] == 1 and await _ticks(migrated_url, subject["installation_id"]) == (0, 0, 0)
    finally:
        await connection.close()


async def test_usage_dto_msk_windows_segments_and_incomplete_history(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    subject = await _seed_subject(migrated_url)
    await _add_gateway(migrated_url, "gw-usage-dto")
    database = Database(settings)
    now = datetime.now(UTC)
    msk_now = now.astimezone(MSK)
    today = msk_now.replace(hour=0, minute=0, second=0, microsecond=0)

    def at(days_ago: int, hour: int = 12) -> datetime:
        return (today - timedelta(days=days_ago) + timedelta(hours=hour)).astimezone(UTC)

    connection = await _connect(migrated_url)
    try:
        for offset, hour, rx, tx in ((0, 1, 10, 20), (1, 3, 5, 5), (10, 4, 7, 8), (40, 5, 100, 100)):
            await connection.execute(
                """
                INSERT INTO usage_ticks (gateway_id, boot_id, registration_id, installation_id,
                                         account_id, observed_at, rx_delta, tx_delta)
                VALUES ((SELECT id FROM gateways LIMIT 1), 'boot-dto', $1, $2, $3, $4, $5, $6)
                """,
                subject["fingerprint"], subject["installation_id"], subject["account_id"],
                at(offset, hour), rx, tx,
            )
        # Current contiguous segment older than 30 days, fresh trusted sample, past gap
        # history: complete true and as_of must be the sample time, not wall clock.
        await connection.execute(
            "INSERT INTO usage_coverage (installation_id, segment_start, last_gap_at, last_trusted_at)"
            " VALUES ($1, $2, $3, $4)",
            subject["installation_id"], now - timedelta(days=31), now - timedelta(days=40), now,
        )
    finally:
        await connection.close()

    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    try:
        response = await client.get(f"{MOBILE}/usage", headers=_auth(subject["token"]))
        assert response.status == 200, await response.text()
        body = await response.json()
        assert body["timezone"] == REPORTING_TIMEZONE
        by_period = {bucket["period"]: bucket for bucket in body["buckets"]}
        assert [bucket["period"] for bucket in body["buckets"]] == ["today", "7d", "30d"]
        assert (by_period["today"]["rx_bytes"], by_period["today"]["tx_bytes"]) == (10, 20)
        assert (by_period["7d"]["rx_bytes"], by_period["7d"]["tx_bytes"]) == (15, 25)
        assert (by_period["30d"]["rx_bytes"], by_period["30d"]["tx_bytes"]) == (22, 33)
        assert all(bucket["complete"] is True for bucket in body["buckets"])
        assert body["coverage_start"].startswith((now - timedelta(days=31)).strftime("%Y-%m-%d"))
        assert body["as_of"] is not None
        assert abs(datetime.fromisoformat(body["as_of"]) - now) < timedelta(seconds=5)
        assert body["server_time"] >= body["as_of"]

        connection = await _connect(migrated_url)
        try:
            # Full window but stale trusted observation: not current accounting.
            await connection.execute(
                "UPDATE usage_coverage SET last_trusted_at = now() - interval '1 hour'"
                " WHERE installation_id = $1",
                subject["installation_id"],
            )
        finally:
            await connection.close()
        stale = await (await client.get(f"{MOBILE}/usage", headers=_auth(subject["token"]))).json()
        assert all(bucket["complete"] is False for bucket in stale["buckets"])
        stale_30d = {bucket["period"]: bucket for bucket in stale["buckets"]}["30d"]
        assert (stale_30d["rx_bytes"], stale_30d["tx_bytes"]) == (
            by_period["30d"]["rx_bytes"], by_period["30d"]["tx_bytes"],
        )
        assert stale["as_of"] is not None  # sample time, not wall clock

        connection = await _connect(migrated_url)
        try:
            # Fresh again but only a young segment: still incomplete until a full window.
            await connection.execute(
                "UPDATE usage_coverage SET segment_start = now(), last_trusted_at = now()"
                " WHERE installation_id = $1",
                subject["installation_id"],
            )
        finally:
            await connection.close()
        fresh = await (await client.get(f"{MOBILE}/usage", headers=_auth(subject["token"]))).json()
        assert all(bucket["complete"] is False for bucket in fresh["buckets"])
        fresh_30d = {bucket["period"]: bucket for bucket in fresh["buckets"]}["30d"]
        assert (fresh_30d["rx_bytes"], fresh_30d["tx_bytes"]) == (
            by_period["30d"]["rx_bytes"], by_period["30d"]["tx_bytes"],
        )

        management = await _seed_subject(migrated_url, management_only=True)
        denied = await client.get(f"{MOBILE}/usage", headers=_auth(management["token"]))
        assert denied.status == 403
        no_entitlement = await _seed_subject(migrated_url, entitlement_ends_in=None)
        missing = await client.get(f"{MOBILE}/usage", headers=_auth(no_entitlement["token"]))
        assert missing.status == 403
        assert (await missing.json())["code"] == "SUBSCRIPTION_MISSING"
    finally:
        await client.close()
        await database.close()


async def test_final_sample_before_revoke_marks_gap_on_failure(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    assert settings.database_url == migrated_url
    subject, gateway = await _seed_usage_subject(migrated_url, key="gw-usage-final")
    connection = await _connect(migrated_url)
    try:
        grant = await connection.fetchrow(
            "SELECT * FROM grants WHERE binding_id = $1 AND gateway_id = $2",
            subject["binding_id"], gateway,
        )
        failing = FakeUsageClient(error=GatewayError("GATEWAY_UNREACHABLE", "fixture"))
        assert await record_final_usage(connection, grant=grant, client=failing, node_id="gw-usage-final") is False
        assert (await _coverage(migrated_url, subject["installation_id"]))["segment_start"] is None

        good = FakeUsageClient(response=_snapshot(subject, "gw-usage-final", rx=7, tx=9, sequence=1))
        assert await record_final_usage(connection, grant=grant, client=good, node_id="gw-usage-final") is True
        # Final read establishes a baseline and closes the peer; no fabricated bytes.
        assert await _ticks(migrated_url, subject["installation_id"]) == (0, 0, 0)
        closed = await connection.fetchval(
            "SELECT closed FROM usage_cursors WHERE registration_id = $1", subject["fingerprint"]
        )
        assert closed is True
    finally:
        await connection.close()


async def _wait_advisory_waiters(database_url, expected, timeout=10.0):
    deadline = asyncio.get_event_loop().time() + timeout
    while True:
        connection = await _connect(database_url)
        try:
            waiting = await connection.fetchval(
                "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted"
            )
        finally:
            await connection.close()
        if waiting >= expected:
            return
        if asyncio.get_event_loop().time() > deadline:
            raise AssertionError(f"advisory waiters never reached {expected}: {waiting}")
        await asyncio.sleep(0.05)


async def test_concurrent_first_samples_are_serialized(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    assert settings.environment == "test"
    subject, gateway = await _seed_usage_subject(migrated_url, key="gw-usage-race")

    async def apply(response):
        connection = await _connect(migrated_url)
        try:
            return await apply_usage_response(
                connection, gateway_id=gateway, gateway_key="gw-usage-race", response=response
            )
        finally:
            await connection.close()

    holder = await _connect(migrated_url)
    await holder.execute("BEGIN")
    await holder.fetchval(
        "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", f"usage:{gateway}"
    )
    # Deterministic FIFO order: seq2 reaches the lock queue first; seq1 must not overwrite it.
    newer = asyncio.create_task(
        apply(_snapshot(subject, "gw-usage-race", rx=200, tx=220, sequence=2, observed=LEVEL_TS + 1))
    )
    await _wait_advisory_waiters(migrated_url, 1)
    older = asyncio.create_task(
        apply(_snapshot(subject, "gw-usage-race", rx=100, tx=110, sequence=1, observed=LEVEL_TS))
    )
    await _wait_advisory_waiters(migrated_url, 2)
    await holder.execute("COMMIT")
    await holder.close()
    results = await asyncio.gather(newer, older)
    assert sum(item["baseline"] for item in results) == 1
    assert sum(item["stale"] for item in results) == 1
    assert sum(item["credited"] for item in results) == 0
    connection = await _connect(migrated_url)
    try:
        cursor = await connection.fetchrow(
            "SELECT sequence, rx_bytes, tx_bytes FROM usage_cursors WHERE registration_id = $1",
            subject["fingerprint"],
        )
        assert (int(cursor["sequence"]), int(cursor["rx_bytes"]), int(cursor["tx_bytes"])) == (2, 200, 220)
    finally:
        await connection.close()
    assert await _ticks(migrated_url, subject["installation_id"]) == (0, 0, 0)

    await apply(_snapshot(subject, "gw-usage-race", rx=260, tx=300, sequence=3, observed=LEVEL_TS + 2))
    assert await _ticks(migrated_url, subject["installation_id"]) == (60, 80, 1)


async def test_concurrent_first_samples_reverse_commit_accounts_once(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    assert settings.environment == "test"
    subject, gateway = await _seed_usage_subject(migrated_url, key="gw-usage-race2")

    async def apply(response):
        connection = await _connect(migrated_url)
        try:
            return await apply_usage_response(
                connection, gateway_id=gateway, gateway_key="gw-usage-race2", response=response
            )
        finally:
            await connection.close()

    holder = await _connect(migrated_url)
    await holder.execute("BEGIN")
    await holder.fetchval(
        "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", f"usage:{gateway}"
    )
    # Deterministic reverse order: seq1 reaches the lock queue first, seq2 then credits the
    # seq1->seq2 delta exactly once.
    older = asyncio.create_task(
        apply(_snapshot(subject, "gw-usage-race2", rx=100, tx=110, sequence=1, observed=LEVEL_TS))
    )
    await _wait_advisory_waiters(migrated_url, 1)
    newer = asyncio.create_task(
        apply(_snapshot(subject, "gw-usage-race2", rx=200, tx=220, sequence=2, observed=LEVEL_TS + 1))
    )
    await _wait_advisory_waiters(migrated_url, 2)
    await holder.execute("COMMIT")
    await holder.close()
    results = await asyncio.gather(older, newer)
    assert sum(item["baseline"] for item in results) == 1
    assert sum(item["credited"] for item in results) == 1
    assert sum(item["stale"] for item in results) == 0
    assert await _ticks(migrated_url, subject["installation_id"]) == (100, 110, 1)

    await apply(_snapshot(subject, "gw-usage-race2", rx=260, tx=300, sequence=3, observed=LEVEL_TS + 2))
    # Total equals seq3-seq1 exactly: no double counting across the commit order.
    assert await _ticks(migrated_url, subject["installation_id"]) == (160, 190, 2)


async def test_usage_freshness_after_collection_stall(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    subject = await _seed_subject(migrated_url)
    await _add_gateway(migrated_url, "gw-usage-stall")
    database = Database(settings)
    now = datetime.now(UTC)
    connection = await _connect(migrated_url)
    try:
        await connection.execute(
            "INSERT INTO usage_ticks (gateway_id, boot_id, registration_id, installation_id,"
            " account_id, observed_at, rx_delta, tx_delta)"
            " VALUES ((SELECT id FROM gateways LIMIT 1), 'boot-stall', $1, $2, $3, $4, 10, 20)",
            subject["fingerprint"], subject["installation_id"], subject["account_id"], now,
        )
        await connection.execute(
            "INSERT INTO usage_coverage (installation_id, segment_start, last_trusted_at)"
            " VALUES ($1, $2, $3)",
            subject["installation_id"], now - timedelta(days=31), now,
        )
    finally:
        await connection.close()

    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    try:
        live = await (await client.get(f"{MOBILE}/usage", headers=_auth(subject["token"]))).json()
        assert all(bucket["complete"] is True for bucket in live["buckets"])
        live_30d = {bucket["period"]: bucket for bucket in live["buckets"]}["30d"]
        connection = await _connect(migrated_url)
        try:
            # Collector stopped: last trusted observation is older than cadence grace.
            await connection.execute(
                "UPDATE usage_coverage SET last_trusted_at = now() - interval '1 hour'"
                " WHERE installation_id = $1",
                subject["installation_id"],
            )
        finally:
            await connection.close()
        stalled = await (await client.get(f"{MOBILE}/usage", headers=_auth(subject["token"]))).json()
        assert all(bucket["complete"] is False for bucket in stalled["buckets"])
        stalled_30d = {bucket["period"]: bucket for bucket in stalled["buckets"]}["30d"]
        assert (stalled_30d["rx_bytes"], stalled_30d["tx_bytes"]) == (
            live_30d["rx_bytes"], live_30d["tx_bytes"],
        )
        assert stalled["as_of"] != live["server_time"]

    finally:
        await client.close()
        await database.close()

    # Recovery opens a new segment at the next trusted sample and refreshes last_trusted_at.
    connection = await _connect(migrated_url)
    try:
        gateway = await connection.fetchval(
            "SELECT id FROM gateways WHERE gateway_key = 'gw-usage-stall'"
        )
        await apply_usage_response(
            connection,
            gateway_id=gateway,
            gateway_key="gw-usage-stall",
            response=_snapshot(subject, "gw-usage-stall", rx=5, tx=5, sequence=1),
        )
        coverage = await connection.fetchrow(
            "SELECT segment_start, last_trusted_at FROM usage_coverage WHERE installation_id = $1",
            subject["installation_id"],
        )
        assert coverage["segment_start"] is not None and coverage["last_trusted_at"] is not None
    finally:
        await connection.close()


def test_collector_prefers_local_admin_socket_without_silent_fallback():
    from dataclasses import replace

    from terlimo_backend.config import load_settings
    from terlimo_backend.gateway_adapter import (
        GatewayAdminClient,
        ManagementTlsClient,
        build_gateway_client,
    )

    base = load_settings(require_database=False)
    endpoints = {
        "admin_socket": "/tmp/usage-admin.sock",
        "management": {"host": "gw.example", "port": 8443, "server_name": "gw.example"},
        "node_id": "node-1",
    }
    local = replace(
        base,
        environment="test",
        gateway_local_admin_enabled=True,
        gateway_admin_main_password="fixture-main",
        gateway_management_ca_file="/tmp/ca.pem",
        gateway_management_cert_file="/tmp/cert.pem",
        gateway_management_key_file="/tmp/key.pem",
    )
    assert isinstance(
        build_gateway_client(local, "gw-1", endpoints, prefer_local_admin=True),
        GatewayAdminClient,
    )
    disabled = replace(local, gateway_local_admin_enabled=False)
    try:
        build_gateway_client(disabled, "gw-1", endpoints, prefer_local_admin=True)
    except GatewayError as error:
        assert error.code == "GATEWAY_LOCAL_ADMIN_DISABLED"
    else:  # pragma: no cover - explicit assertion
        raise AssertionError("local admin must not silently fall back to mTLS")
    production = replace(local, environment="production")
    try:
        build_gateway_client(production, "gw-1", endpoints, prefer_local_admin=True)
    except GatewayError as error:
        assert error.code == "GATEWAY_LOCAL_ADMIN_FORBIDDEN"
    else:  # pragma: no cover - explicit assertion
        raise AssertionError("local admin must be forbidden outside TEST")
    default = build_gateway_client(local, "gw-1", endpoints)
    assert isinstance(default, ManagementTlsClient)


def _canonical_usage_schema():
    import json as _json
    from pathlib import Path as _Path

    schema_path = _Path("/tmp/opencode/contracts-perpetual/schemas/usage.json")
    if not schema_path.exists():
        pytest.skip("canonical contracts-perpetual usage schema unavailable")
    document = _json.loads(schema_path.read_text())
    return {"$ref": "#/$defs/UsageResponse", "$defs": document["$defs"]}


async def test_usage_dto_matches_canonical_bucket_schema(migrated_url, settings_factory):
    import jsonschema

    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    subject = await _seed_subject(migrated_url)
    await _add_gateway(migrated_url, "gw-usage-schema")
    database = Database(settings)
    now = datetime.now(UTC)
    connection = await _connect(migrated_url)
    try:
        await connection.execute(
            "INSERT INTO usage_ticks (gateway_id, boot_id, registration_id, installation_id,"
            " account_id, observed_at, rx_delta, tx_delta)"
            " VALUES ((SELECT id FROM gateways LIMIT 1), 'boot-schema', $1, $2, $3, $4, 11, 22)",
            subject["fingerprint"], subject["installation_id"], subject["account_id"], now,
        )
        await connection.execute(
            "INSERT INTO usage_coverage (installation_id, segment_start, last_trusted_at)"
            " VALUES ($1, $2, $3)",
            subject["installation_id"], now - timedelta(days=31), now,
        )
    finally:
        await connection.close()

    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    try:
        response = await client.get(f"{MOBILE}/usage", headers=_auth(subject["token"]))
        assert response.status == 200, await response.text()
        body = await response.json()
    finally:
        await client.close()
        await database.close()

    jsonschema.validate(instance=body, schema=_canonical_usage_schema())
    for forbidden in ("today", "last_7_days", "last_30_days", "complete"):
        assert forbidden not in body, f"forbidden flat field {forbidden} leaked"
    assert [bucket["period"] for bucket in body["buckets"]] == ["today", "7d", "30d"]
    for bucket in body["buckets"]:
        assert isinstance(bucket["rx_bytes"], int) and bucket["rx_bytes"] >= 0
        assert isinstance(bucket["tx_bytes"], int) and bucket["tx_bytes"] >= 0
        assert isinstance(bucket["complete"], bool)


async def test_usage_dto_empty_history_buckets_null_as_of(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    subject = await _seed_subject(migrated_url)
    await _add_gateway(migrated_url, "gw-usage-empty")
    database = Database(settings)

    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    try:
        response = await client.get(f"{MOBILE}/usage", headers=_auth(subject["token"]))
        assert response.status == 200, await response.text()
        body = await response.json()
    finally:
        await client.close()
        await database.close()

    assert body["coverage_start"] is None
    # Source-correct: no trusted sample means as_of stays null rather than fabricating
    # freshness. Canonical usage.json currently requires a string, so this response
    # intentionally fails that draft constraint until the null-as_of amendment lands.
    assert body["as_of"] is None
    assert [bucket["period"] for bucket in body["buckets"]] == ["today", "7d", "30d"]
    for bucket in body["buckets"]:
        assert bucket["rx_bytes"] == 0 and bucket["tx_bytes"] == 0
        assert bucket["complete"] is False


async def test_usage_dto_bucket_completion_is_evidence_based(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    subject = await _seed_subject(migrated_url)
    await _add_gateway(migrated_url, "gw-usage-today")
    database = Database(settings)
    now = datetime.now(UTC)
    today_start = now.astimezone(MSK).replace(hour=0, minute=0, second=0, microsecond=0)
    connection = await _connect(migrated_url)
    try:
        await connection.execute(
            "INSERT INTO usage_ticks (gateway_id, boot_id, registration_id, installation_id,"
            " account_id, observed_at, rx_delta, tx_delta)"
            " VALUES ((SELECT id FROM gateways LIMIT 1), 'boot-today', $1, $2, $3, $4, 7, 9)",
            subject["fingerprint"], subject["installation_id"], subject["account_id"], now,
        )
        await connection.execute(
            "INSERT INTO usage_coverage (installation_id, segment_start, last_trusted_at)"
            " VALUES ($1, $2, $3)",
            subject["installation_id"], today_start.astimezone(UTC), now,
        )
    finally:
        await connection.close()

    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    try:
        response = await client.get(f"{MOBILE}/usage", headers=_auth(subject["token"]))
        assert response.status == 200, await response.text()
        body = await response.json()
    finally:
        await client.close()
        await database.close()

    by_period = {bucket["period"]: bucket for bucket in body["buckets"]}
    assert by_period["today"]["complete"] is True
    assert by_period["7d"]["complete"] is False
    assert by_period["30d"]["complete"] is False
