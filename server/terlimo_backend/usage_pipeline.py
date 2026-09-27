"""Account usage pipeline (contract 09 §11): one per-grant user-data layer.

The gateway reports WireGuard peer counters keyed by registration identity. The backend
dedupes samples with a durable cursor keyed by (gateway, boot, registration), stores only
non-negative deltas, and never fabricates history: absence of trusted coverage is reported
as incomplete, not as zeros. USD values, prices and client-side session counters are out of
scope here (client telemetry lives in the Android session, contract §10).
"""

from __future__ import annotations

import logging
from collections.abc import Callable
from datetime import UTC, datetime, timedelta
from typing import Any
from zoneinfo import ZoneInfo

import asyncpg

from .config import Settings
from .gateway_adapter import GatewayError

logger = logging.getLogger(__name__)

REPORTING_TIMEZONE = "Europe/Moscow"
MSK = ZoneInfo(REPORTING_TIMEZONE)
# Collector cadence is the maintenance interval (300s); freshness allows one conservative
# extra cadence before historical windows stop being reported as current/complete.
USAGE_FRESHNESS_SECONDS = 600
USAGE_WINDOWS = {"today": 1, "last_7_days": 7, "last_30_days": 30}

ClientFactory = Callable[[str, dict[str, Any]], Any]


def build_default_client_factory(settings: Settings) -> ClientFactory:
    from .gateway_adapter import build_gateway_client

    return lambda gateway_key, endpoints: build_gateway_client(
        settings, gateway_key, endpoints, prefer_local_admin=True
    )


async def _resolve_grant_subject(
    connection: asyncpg.Connection, gateway_id: Any, registration_id: str
) -> tuple[Any | None, Any | None]:
    """Attribution fence: the reported registration must be the current applied grant of this
    gateway with an active binding. Stale/foreign peers and moved bindings are never credited."""
    rows = await connection.fetch(
        """
        SELECT binding.installation_id, binding.account_id
        FROM grants AS g
        JOIN account_bindings AS binding ON binding.id = g.binding_id
        JOIN installations AS installation ON installation.id = binding.installation_id
        WHERE g.gateway_id = $1
          AND installation.public_key_fingerprint = $2
          AND binding.status = 'active'
          AND g.state = 'applied'
          AND g.applied_generation IS NOT NULL
          AND g.desired_generation IS NOT NULL
          AND g.applied_generation = g.desired_generation
        """,
        gateway_id,
        registration_id,
    )
    if len(rows) != 1:
        return None, None
    return rows[0]["installation_id"], rows[0]["account_id"]


async def _start_segment(
    connection: asyncpg.Connection, installation_id: Any, observed_at: datetime
) -> None:
    """Open a new contiguous trusted segment at the first trustworthy post-gap sample."""
    await connection.execute(
        """
        INSERT INTO usage_coverage (installation_id, segment_start, last_trusted_at)
        VALUES ($1, $2, $2)
        ON CONFLICT (installation_id) DO UPDATE
        SET segment_start = EXCLUDED.segment_start,
            last_trusted_at = EXCLUDED.last_trusted_at, updated_at = now()
        """,
        installation_id,
        observed_at,
    )


async def mark_coverage_gap(
    connection: asyncpg.Connection, installation_id: Any, at: datetime | None = None
) -> None:
    """Break the contiguous trusted segment: the interval is unknown until a new baseline."""
    if installation_id is None:
        return
    moment = at or datetime.now(UTC)
    await connection.execute(
        """
        INSERT INTO usage_coverage (installation_id, segment_start, last_gap_at)
        VALUES ($1, NULL, $2)
        ON CONFLICT (installation_id) DO UPDATE
        SET segment_start = NULL, last_gap_at = EXCLUDED.last_gap_at, updated_at = now()
        """,
        installation_id,
        moment,
    )


async def _touch_trusted(
    connection: asyncpg.Connection, installation_id: Any, observed_at: datetime
) -> None:
    """Record the latest trusted observation time (freshness fence), even without new bytes."""
    await connection.execute(
        """
        INSERT INTO usage_coverage (installation_id, last_trusted_at)
        VALUES ($1, $2)
        ON CONFLICT (installation_id) DO UPDATE
        SET last_trusted_at = GREATEST(
                COALESCE(usage_coverage.last_trusted_at, EXCLUDED.last_trusted_at),
                EXCLUDED.last_trusted_at
            ),
            updated_at = now()
        """,
        installation_id,
        observed_at,
    )


async def _segment_start(connection: asyncpg.Connection, installation_id: Any) -> datetime | None:
    return await connection.fetchval(
        "SELECT segment_start FROM usage_coverage WHERE installation_id = $1",
        installation_id,
    )


async def _mark_gateway_gaps(connection: asyncpg.Connection, gateway_id: Any) -> None:
    for row in await connection.fetch(
        """
        SELECT DISTINCT binding.installation_id
        FROM grants AS g
        JOIN account_bindings AS binding ON binding.id = g.binding_id
        WHERE g.gateway_id = $1 AND g.state <> 'revoked'
        """,
        gateway_id,
    ):
        await mark_coverage_gap(connection, row["installation_id"])


async def _apply_peer_sample(
    connection: asyncpg.Connection,
    *,
    gateway_id: Any,
    boot_id: str,
    boot_started_at: int,
    registration_id: str,
    sequence: int,
    rx_bytes: int,
    tx_bytes: int,
    observed_at: datetime,
) -> dict[str, Any]:
    cursor = await connection.fetchrow(
        """
        SELECT boot_id, boot_started_at, rx_bytes, tx_bytes, sequence, closed,
               installation_id, account_id
        FROM usage_cursors
        WHERE gateway_id = $1 AND registration_id = $2
        FOR UPDATE
        """,
        gateway_id,
        registration_id,
    )
    installation_id, account_id = await _resolve_grant_subject(
        connection, gateway_id, registration_id
    )
    if installation_id is None:
        return {"outcome": "unknown"}

    async def _store_cursor(*, closed: bool) -> None:
        await connection.execute(
            """
            INSERT INTO usage_cursors
                (gateway_id, registration_id, boot_id, boot_started_at, rx_bytes, tx_bytes,
                 sequence, closed, installation_id, account_id)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
            ON CONFLICT (gateway_id, registration_id) DO UPDATE
            SET boot_id = EXCLUDED.boot_id, boot_started_at = EXCLUDED.boot_started_at,
                rx_bytes = EXCLUDED.rx_bytes, tx_bytes = EXCLUDED.tx_bytes,
                sequence = EXCLUDED.sequence, closed = EXCLUDED.closed,
                installation_id = EXCLUDED.installation_id, account_id = EXCLUDED.account_id,
                updated_at = now()
            """,
            gateway_id,
            registration_id,
            boot_id,
            boot_started_at,
            rx_bytes,
            tx_bytes,
            sequence,
            closed,
            installation_id,
            account_id,
        )

    if cursor is not None and int(cursor["boot_started_at"]) > boot_started_at:
        # Older boot generation replay: never let it touch the current cursor/segment.
        return {"outcome": "stale", "rx_delta": 0, "tx_delta": 0}
    if cursor is not None and not cursor["closed"] and cursor["boot_id"] == boot_id:
        if int(cursor["sequence"]) >= sequence:
            return {"outcome": "stale", "rx_delta": 0, "tx_delta": 0}
        if rx_bytes < int(cursor["rx_bytes"]) or tx_bytes < int(cursor["tx_bytes"]):
            # Peer re-add inside the same boot: unknown interval -> gap + new baseline.
            await _store_cursor(closed=False)
            await mark_coverage_gap(connection, installation_id, observed_at)
            await _start_segment(connection, installation_id, observed_at)
            return {"outcome": "peer_reset", "rx_delta": 0, "tx_delta": 0}
        if await _segment_start(connection, installation_id) is None:
            # Segment was broken (collector failure/final-read failure): the next sample is a
            # baseline; its counters may span the unknown interval and must not be credited.
            await _store_cursor(closed=False)
            await _start_segment(connection, installation_id, observed_at)
            return {"outcome": "rebaseline", "rx_delta": 0, "tx_delta": 0}
        rx_delta = rx_bytes - int(cursor["rx_bytes"])
        tx_delta = tx_bytes - int(cursor["tx_bytes"])
        await _store_cursor(closed=False)
        await _touch_trusted(connection, installation_id, observed_at)
        if rx_delta == 0 and tx_delta == 0:
            return {"outcome": "unchanged", "rx_delta": 0, "tx_delta": 0}
    else:
        # First sample, closed peer reappearing, new/unknown boot: establish a baseline only.
        # Bytes observed before the collection segment started are unknown history.
        await _store_cursor(closed=False)
        if cursor is not None and cursor["boot_id"] != boot_id:
            await mark_coverage_gap(connection, installation_id, observed_at)
        await _start_segment(connection, installation_id, observed_at)
        return {"outcome": "baseline", "rx_delta": 0, "tx_delta": 0}
    inserted = await connection.fetchval(
        """
        INSERT INTO usage_ticks
            (gateway_id, boot_id, registration_id, installation_id, account_id,
             observed_at, rx_delta, tx_delta)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
        ON CONFLICT (gateway_id, registration_id, observed_at) DO NOTHING
        RETURNING id
        """,
        gateway_id,
        boot_id,
        registration_id,
        installation_id,
        account_id,
        observed_at,
        rx_delta,
        tx_delta,
    )
    if inserted is None:
        return {"outcome": "duplicate", "rx_delta": 0, "tx_delta": 0}
    await _touch_trusted(connection, installation_id, observed_at)
    return {"outcome": "credited", "rx_delta": rx_delta, "tx_delta": tx_delta}


async def apply_usage_response(
    connection: asyncpg.Connection,
    *,
    gateway_id: Any,
    gateway_key: str,
    response: dict[str, Any],
    observed_fallback: datetime | None = None,
) -> dict[str, Any]:
    """Atomic per-gateway admission: a single advisory xact lock serializes concurrent
    snapshots (collector and final read included), so a first insert can never race with a
    replay and an older sequence can never overwrite a newer cursor."""
    async with connection.transaction():
        await connection.execute(
            "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", f"usage:{gateway_id}"
        )
        return await _apply_usage_response_locked(
            connection,
            gateway_id=gateway_id,
            gateway_key=gateway_key,
            response=response,
            observed_fallback=observed_fallback,
        )


async def _apply_usage_response_locked(
    connection: asyncpg.Connection,
    *,
    gateway_id: Any,
    gateway_key: str,
    response: dict[str, Any],
    observed_fallback: datetime | None = None,
) -> dict[str, Any]:
    """Validate one node usage snapshot and persist it under the cursor fence."""
    node_id = response.get("node_id")
    if not isinstance(node_id, str) or node_id != gateway_key:
        raise TypeError("usage response identity mismatch")
    boot_id = response.get("boot_id")
    if not isinstance(boot_id, str) or not boot_id or len(boot_id) > 64:
        raise TypeError("usage response boot_id invalid")
    boot_started_at = response.get("boot_started_at")
    if type(boot_started_at) is not int or boot_started_at <= 0:
        raise TypeError("usage response boot_started_at invalid")
    observed_raw = response.get("observed_at")
    if type(observed_raw) is not int or observed_raw <= 0:
        raise TypeError("usage response observed_at invalid")
    sequence = response.get("sequence")
    if type(sequence) is not int or sequence <= 0:
        raise TypeError("usage response sequence invalid")
    observed_at = datetime.fromtimestamp(observed_raw, UTC)
    if observed_fallback is not None:
        passed = observed_fallback
        if passed.tzinfo is None:
            passed = passed.replace(tzinfo=UTC)
        if passed < observed_at - timedelta(minutes=5):
            observed_at = passed
    supported = response.get("counters_supported")
    peers = response.get("peers")
    if not isinstance(peers, list):
        raise TypeError("usage response peers invalid")
    if supported is not True:
        await _mark_gateway_gaps(connection, gateway_id)
        return {"counters_supported": False, "credited": 0, "unknown": 0}
    credited = duplicates = unknown = baselines = resets = stale = 0
    seen_registrations: set[str] = set()
    for peer in peers:
        if not isinstance(peer, dict):
            unknown += 1
            continue
        registration_id = peer.get("registration_id")
        rx_bytes, tx_bytes = peer.get("rx_bytes"), peer.get("tx_bytes")
        if (
            not isinstance(registration_id, str)
            or not registration_id
            or len(registration_id) > 128
            or type(rx_bytes) is not int
            or type(tx_bytes) is not int
            or rx_bytes < 0
            or tx_bytes < 0
        ):
            unknown += 1
            continue
        seen_registrations.add(registration_id)
        outcome = await _apply_peer_sample(
            connection,
            gateway_id=gateway_id,
            boot_id=boot_id,
            boot_started_at=boot_started_at,
            registration_id=registration_id,
            sequence=sequence,
            rx_bytes=rx_bytes,
            tx_bytes=tx_bytes,
            observed_at=observed_at,
        )
        kind = outcome["outcome"]
        if kind == "unknown":
            unknown += 1
        elif kind == "credited":
            credited += 1
        elif kind == "duplicate":
            duplicates += 1
        elif kind == "baseline":
            baselines += 1
        elif kind in ("boot_reset", "peer_reset"):
            resets += 1
        elif kind == "stale":
            stale += 1
    absent = await connection.fetch(
        """
        SELECT registration_id, installation_id FROM usage_cursors
        WHERE gateway_id = $1 AND closed = false
        """,
        gateway_id,
    )
    for row in absent:
        if row["registration_id"] in seen_registrations:
            continue
        # A known open peer disappearing from a trusted snapshot means the interval until the
        # removal is unknown: close it with an explicit gap (one-time).
        await mark_coverage_gap(connection, row["installation_id"], observed_at)
        await connection.execute(
            "UPDATE usage_cursors SET closed = true, updated_at = now() WHERE gateway_id = $1 AND registration_id = $2",
            gateway_id,
            row["registration_id"],
        )
    return {
        "counters_supported": True,
        "credited": credited,
        "duplicate": duplicates,
        "unknown": unknown,
        "baseline": baselines,
        "reset": resets,
        "stale": stale,
    }


async def collect_gateway_usage(
    connection: asyncpg.Connection,
    settings: Settings,
    *,
    client_factory: ClientFactory | None = None,
) -> dict[str, Any]:
    """Pull one usage snapshot per registered gateway and persist deltas atomically."""
    factory = client_factory or build_default_client_factory(settings)
    gateways = await connection.fetch(
        """
        SELECT gateway.id, gateway.gateway_key, gateway.endpoints,
               (
                   SELECT g.gateway_credential FROM grants AS g
                   WHERE g.gateway_id = gateway.id
                     AND g.gateway_credential IS NOT NULL
                     AND g.state <> 'revoked'
                   ORDER BY g.created_at DESC
                   LIMIT 1
               ) AS credential
        FROM gateways AS gateway
        WHERE gateway.environment = $1 AND gateway.registry_state = 'registered'
        ORDER BY gateway.gateway_key
        """,
        settings.environment,
    )
    collected = 0
    for gateway in gateways:
        credential = gateway["credential"]
        if not credential:
            continue
        endpoints = gateway["endpoints"]
        if isinstance(endpoints, str):
            import json

            endpoints = json.loads(endpoints)
        client = factory(gateway["gateway_key"], endpoints or {})
        try:
            response = await client.usage(credential, gateway["gateway_key"])
        except GatewayError as error:
            logger.warning("usage readback failed for gateway %s: %s", gateway["gateway_key"], error.code)
            await _mark_gateway_gaps(connection, gateway["id"])
            continue
        try:
            async with connection.transaction():
                await apply_usage_response(
                    connection,
                    gateway_id=gateway["id"],
                    gateway_key=gateway["gateway_key"],
                    response=response,
                )
        except (TypeError, ValueError) as error:
            logger.warning("usage response rejected for gateway %s: %s", gateway["gateway_key"], error)
            await _mark_gateway_gaps(connection, gateway["id"])
            continue
        collected += 1
    return {"gateways": len(gateways), "collected": collected}


async def _mark_gateway_gaps(connection: asyncpg.Connection, gateway_id: Any) -> None:
    for row in await connection.fetch(
        """
        SELECT DISTINCT binding.installation_id
        FROM grants AS g
        JOIN account_bindings AS binding ON binding.id = g.binding_id
        WHERE g.gateway_id = $1 AND g.state <> 'revoked'
        """,
        gateway_id,
    ):
        await mark_coverage_gap(connection, row["installation_id"])


async def _grant_installation_id(connection: asyncpg.Connection, grant: asyncpg.Record) -> Any:
    if grant["installation_id"] is not None:
        return grant["installation_id"]
    return await connection.fetchval(
        "SELECT installation_id FROM account_bindings WHERE id = $1", grant["binding_id"]
    )


async def record_final_usage(
    connection: asyncpg.Connection,
    *,
    grant: asyncpg.Record,
    client: Any,
    node_id: str,
) -> bool:
    """Best-effort final sample before peer deletion; failure is an explicit coverage gap."""
    installation_id = await _grant_installation_id(connection, grant)
    password = grant["gateway_credential"]
    if not password:
        await mark_coverage_gap(connection, installation_id)
        return False
    try:
        response = await client.usage(password, node_id)
    except GatewayError:
        await mark_coverage_gap(connection, installation_id)
        return False
    try:
        async with connection.transaction():
            await apply_usage_response(
                connection,
                gateway_id=grant["gateway_id"],
                gateway_key=response.get("node_id") if isinstance(response, dict) else node_id,
                response=response,
            )
            for peer in response.get("peers", []) if isinstance(response, dict) else []:
                registration_id = peer.get("registration_id") if isinstance(peer, dict) else None
                if isinstance(registration_id, str) and registration_id:
                    await connection.execute(
                        """
                        UPDATE usage_cursors SET closed = true, updated_at = now()
                        WHERE gateway_id = $1 AND registration_id = $2
                        """,
                        grant["gateway_id"],
                        registration_id,
                    )
    except (TypeError, ValueError):
        await mark_coverage_gap(connection, installation_id)
        return False
    return True
