"""Gateway-control application service and durable worker handlers (step 03.3).

Backend is the single owner of entitlement/binding state. It computes one finite technical
grant (not_after <= entitlement end and <= the configured TEST lease) and writes the desired
state plus a durable outbox operation atomically. The worker applies the grant through the
existing WDTT management wire, reads the actual gateway state back, and only then marks the
grant applied. RPCs run outside any database transaction or row lock.

Fences: a grant's desired_generation and state are authoritative. An operation whose
generation no longer matches, or whose grant is revoked, is finalized without an RPC and
never resurrects access. The gateway's own generation/lease_seq/revoke tombstone is the
second fence and survives its restart. Delivery is at-least-once with stable operation
identity (identical command bytes) and readback; queued/accepted is never treated as applied.
"""

from __future__ import annotations

import json
import logging
import secrets
import time
from datetime import UTC, datetime, timedelta
from typing import Any

import asyncpg

from .config import Settings
from .gateway_adapter import GatewayError, build_gateway_client
from .operation_timing import log_phase, timed_rpc

logger = logging.getLogger(__name__)

APPLY_OPERATION = "gateway.apply_grant"
REVOKE_OPERATION = "gateway.revoke_grant"
SYNC_PROFILE_OPERATION = "gateway.sync_profile"

TERMINAL_EXTERNAL_CODES = frozenset(
    {"CONFLICT", "IDEMPOTENCY_CONFLICT", "LEASE_CONFLICT", "GRANT_CONFLICT", "NODE_ID_MISMATCH"}
)


def utc_now() -> datetime:
    return datetime.now(UTC)


def rfc3339(moment: datetime) -> str:
    return moment.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def _technical_not_after(entitlement_ends_at: datetime | None, now: datetime, max_lease: int) -> datetime:
    ceiling = now + timedelta(seconds=max_lease)
    if entitlement_ends_at is None:
        return ceiling
    return min(entitlement_ends_at, ceiling)


async def _enqueue(
    connection: asyncpg.Connection,
    *,
    operation_type: str,
    idempotency_key: str,
    payload: dict[str, Any],
    gateway_id: Any,
    target_revision: int,
    account_id: Any = None,
    binding_id: Any = None,
    correlation_id: Any = None,
) -> None:
    await connection.execute(
        """
        INSERT INTO outbox_operations
            (operation_type, payload, idempotency_key, target_revision, gateway_id,
             account_id, binding_id, correlation_id, max_attempts)
        VALUES ($1, $2::jsonb, $3, $4, $5, $6, $7, $8, 12)
        ON CONFLICT (idempotency_key) DO NOTHING
        """,
        operation_type,
        payload,
        idempotency_key,
        target_revision,
        gateway_id,
        account_id,
        binding_id,
        correlation_id,
    )


async def ensure_grant(
    connection: asyncpg.Connection,
    *,
    binding_id: Any,
    gateway_id: Any,
    entitlement_id: Any,
    max_lease_seconds: int,
    now: datetime | None = None,
    correlation_id: Any = None,
) -> str:
    """Desired grant for one active binding/entitlement/gateway; atomic with its outbox op."""
    now = now or utc_now()
    row = await connection.fetchrow(
        """
        SELECT binding.id AS binding_id, binding.status AS binding_status,
               binding.account_id AS account_id, binding.installation_id,
               installation.public_key_fingerprint, installation.public_key_spki_b64,
               installation.state AS installation_state, installation.environment,
               entitlement.id AS entitlement_id, entitlement.status AS entitlement_status,
               entitlement.starts_at, entitlement.ends_at, entitlement.kind, entitlement.device_limit, entitlement.paid_base_device_limit, entitlement.created_at AS entitlement_created_at,
               gateway.id AS gateway_id, gateway.gateway_key, gateway.registry_state,
               gateway.environment AS gateway_environment,
               gateway.endpoints AS gateway_endpoints,
               gateway.confirmed_max_workers AS gateway_confirmed_max_workers
        FROM account_bindings AS binding
        JOIN installations AS installation ON installation.id = binding.installation_id
        JOIN entitlements AS entitlement
          ON entitlement.id = $3 AND entitlement.account_id = binding.account_id
        JOIN gateways AS gateway ON gateway.id = $2
        WHERE binding.id = $1
        FOR UPDATE OF binding
        """,
        binding_id,
        gateway_id,
        entitlement_id,
    )
    if row is None:
        # The authoritative query requires entitlement.account_id = binding.account_id; a
        # mismatched or missing pair is a non-granting outcome and writes nothing.
        return "ownership_mismatch"
    if row["binding_status"] != "active" or row["registry_state"] != "registered":
        return "not_active"
    if row["installation_state"] == "revoked":
        return "installation_revoked"
    if row["environment"] != row["gateway_environment"]:
        # installation environment must match the gateway environment; never guess.
        return "environment_mismatch"
    # Ordered handoff: a live installation hour grant must be confirmed-revoked before a
    # commercial grant for the same installation can coexist on the node.
    hour_grant = await connection.fetchrow(
        """
        SELECT * FROM grants
        WHERE installation_id = $1 AND hour_entitlement_id IS NOT NULL AND state <> 'revoked'
        ORDER BY created_at DESC
        LIMIT 1
        FOR UPDATE
        """,
        row["installation_id"],
    )
    if hour_grant is not None and not _readback_revoked(hour_grant):
        await revoke_hour_grants(
            connection,
            installation_id=row["installation_id"],
            hour_entitlement_id=hour_grant["hour_entitlement_id"],
            now=now,
        )
        return "hour_pending"
    # Same eligibility as session_auth: status=active, started, and either finite-future or an
    # allowed indefinite end (ends_at IS NULL). Finite expired, future start and non-active
    # statuses never grant; the technical lease stays bounded by max_lease_seconds.
    if row["entitlement_status"] != "active":
        return "no_entitlement"
    if row["starts_at"] is not None and row["starts_at"] > now:
        return "no_entitlement"
    if row["ends_at"] is not None and row["ends_at"] <= now:
        return "no_entitlement"

    from .payment_products import binding_paid_capacity
    capacity, deadline = await binding_paid_capacity(connection,{"id":row["entitlement_id"],"account_id":row["account_id"],"kind":row["kind"],"ends_at":row["ends_at"],"device_limit":row["device_limit"],"paid_base_device_limit":row["paid_base_device_limit"]},binding_id,now)
    if not capacity:
        return "device_limit_reached"
    not_after = _technical_not_after(deadline, now, max_lease_seconds)
    existing = await connection.fetchrow(
        "SELECT * FROM grants WHERE binding_id = $1 AND gateway_id = $2 FOR UPDATE",
        binding_id,
        gateway_id,
    )
    if existing is not None and existing["state"] == "revoked":
        return "revoked"
    if existing is None:
        grant = await connection.fetchrow(
            """
            INSERT INTO grants
                (binding_id, gateway_id, desired_generation, not_after, state,
                 gateway_credential, lease_seq, gateway_generation)
            VALUES ($1, $2, 1, $3, 'pending', $4, 0, 0)
            RETURNING opaque_id, desired_generation
            """,
            binding_id,
            gateway_id,
            not_after,
            secrets.token_urlsafe(24),
        )
        await _enqueue(
            connection,
            operation_type=APPLY_OPERATION,
            idempotency_key=f"grant:{grant['opaque_id']}:gen:1:apply",
            payload={
                "grant_id": str(grant["opaque_id"]),
                "generation": str(grant["desired_generation"]),
                "action": "apply",
                "not_after": rfc3339(not_after),
            },
            gateway_id=gateway_id,
            target_revision=grant["desired_generation"],
            account_id=row["account_id"],
            binding_id=binding_id,
            correlation_id=correlation_id,
        )
        from .referral_rewards import record_trial_target
        await record_trial_target(connection,entitlement_id=entitlement_id,
            grant_id=await connection.fetchval("SELECT id FROM grants WHERE opaque_id=$1",grant['opaque_id']),generation=1)
        return "enqueued"

    remaining = (existing["not_after"] - now).total_seconds()
    endpoints = row["gateway_endpoints"] if isinstance(row["gateway_endpoints"], dict) else {}
    cap_unconfirmed = (
        existing["state"] == "applied"
        and existing["applied_generation"] == existing["desired_generation"]
        and not _gateway_cap_confirmed(endpoints, row["gateway_confirmed_max_workers"])
    )
    refresh = (
        not_after < existing["not_after"] - timedelta(seconds=1)
        or remaining < max_lease_seconds / 3
        or cap_unconfirmed
        or (row["kind"] == "trial" and existing["applied_at"] is not None
            and existing["applied_at"] < row["entitlement_created_at"])
    )
    if not refresh:
        from .referral_rewards import record_trial_target
        await record_trial_target(connection,entitlement_id=entitlement_id,
            grant_id=existing['id'],generation=existing['desired_generation'])
        if existing["applied_generation"] != existing["desired_generation"]:
            return "pending"
        return "unchanged"
    generation = existing["desired_generation"] + 1
    await connection.execute(
        """
        UPDATE grants
        SET desired_generation = $2, not_after = $3, state = 'pending',
            last_error = NULL, updated_at = now()
        WHERE id = $1
        """,
        existing["id"],
        generation,
        not_after,
    )
    await _enqueue(
        connection,
        operation_type=APPLY_OPERATION,
        idempotency_key=f"grant:{existing['opaque_id']}:gen:{generation}:apply",
        payload={
            "grant_id": str(existing["opaque_id"]),
            "generation": str(generation),
            "action": "apply",
            "not_after": rfc3339(not_after),
        },
        gateway_id=gateway_id,
        target_revision=generation,
        account_id=row["account_id"],
        binding_id=binding_id,
        correlation_id=correlation_id,
    )
    from .referral_rewards import record_trial_target
    await record_trial_target(connection,entitlement_id=entitlement_id,grant_id=existing['id'],generation=generation)
    return "enqueued"


async def revoke_binding_grants(
    connection: asyncpg.Connection,
    *,
    binding_id: Any,
    now: datetime | None = None,
) -> int:
    """Revoke the binding and fence every related grant; atomic with its outbox ops."""
    now = now or utc_now()
    binding = await connection.fetchrow(
        "SELECT id, status, generation, account_id FROM account_bindings WHERE id = $1 FOR UPDATE",
        binding_id,
    )
    if binding is None:
        return 0
    if binding["status"] != "revoked":
        await connection.execute(
            """
            UPDATE account_bindings
            SET status = 'revoked', generation = generation + 1, revoked_at = now()
            WHERE id = $1
            """,
            binding_id,
        )
    grants = await connection.fetch(
        "SELECT * FROM grants WHERE binding_id = $1 AND state <> 'revoked' FOR UPDATE",
        binding_id,
    )
    revoked = 0
    for grant in grants:
        generation = grant["desired_generation"] + 1
        await connection.execute(
            """
            UPDATE grants
            SET desired_generation = $2, state = 'revoked', last_error = NULL, updated_at = now()
            WHERE id = $1
            """,
            grant["id"],
            generation,
        )
        await _enqueue(
            connection,
            operation_type=REVOKE_OPERATION,
            idempotency_key=f"grant:{grant['opaque_id']}:gen:{generation}:revoke",
            payload={
                "grant_id": str(grant["opaque_id"]),
                "generation": str(generation),
                "action": "revoke",
            },
            gateway_id=grant["gateway_id"],
            target_revision=generation,
            account_id=binding["account_id"],
            binding_id=binding_id,
        )
        revoked += 1
    return revoked


def _hour_fence_ok(grant: asyncpg.Record, payload: dict[str, Any]) -> bool:
    """An hour-owned grant operation must name exactly its own hour entitlement."""
    owned = grant["hour_entitlement_id"]
    expected = payload.get("hour_entitlement_id")
    if owned is None and expected is None:
        return True
    if owned is None or expected is None:
        return False
    return str(owned) == str(expected)


def _readback_revoked(grant: asyncpg.Record) -> bool:
    """Actual readback-confirmed revoke, mirroring the catalog admission predicate."""
    import json as _json

    if grant["state"] != "revoked":
        return False
    if grant["desired_generation"] is None or grant["applied_generation"] is None:
        return False
    if int(grant["applied_generation"]) != int(grant["desired_generation"]):
        return False
    readback = grant["last_readback"]
    if isinstance(readback, str):
        try:
            readback = _json.loads(readback)
        except ValueError:
            return False
    return isinstance(readback, dict) and readback.get("revoked") is True


async def ensure_hour_grant(
    connection: asyncpg.Connection,
    *,
    installation_id: Any,
    hour_entitlement_id: Any,
    gateway_id: Any,
    max_lease_seconds: int,
    now: datetime | None = None,
    correlation_id: Any = None,
) -> str:
    """Desired installation-scoped onboarding_hour grant for one gateway; atomic with outbox."""
    now = now or utc_now()
    row = await connection.fetchrow(
        """
        SELECT installation.id AS installation_id,
               installation.public_key_fingerprint, installation.public_key_spki_b64,
               installation.state AS installation_state, installation.environment,
               entitlement.id AS entitlement_id, entitlement.status AS entitlement_status,
               entitlement.starts_at, entitlement.ends_at, entitlement.kind, entitlement.device_limit, entitlement.paid_base_device_limit,
               gateway.id AS gateway_id, gateway.gateway_key, gateway.registry_state,
               gateway.environment AS gateway_environment,
               gateway.endpoints AS gateway_endpoints,
               gateway.confirmed_max_workers AS gateway_confirmed_max_workers
        FROM installations AS installation
        JOIN entitlements AS entitlement
          ON entitlement.id = $2 AND entitlement.installation_id = installation.id
        JOIN gateways AS gateway ON gateway.id = $3
        WHERE installation.id = $1
        FOR UPDATE OF installation
        """,
        installation_id,
        hour_entitlement_id,
        gateway_id,
    )
    if row is None:
        return "ownership_mismatch"
    if row["registry_state"] != "registered":
        return "not_active"
    if row["installation_state"] == "revoked":
        return "installation_revoked"
    if row["environment"] != row["gateway_environment"]:
        return "environment_mismatch"
    if row["kind"] != "onboarding_hour" or row["entitlement_status"] != "active":
        return "no_entitlement"
    if row["starts_at"] is not None and row["starts_at"] > now:
        return "no_entitlement"
    if row["ends_at"] is None or row["ends_at"] <= now:
        return "no_entitlement"
    # Exclusive subject: a live commercial grant for the same installation must not coexist.
    commercial = await connection.fetchval(
        """
        SELECT 1 FROM grants
        WHERE installation_id = $1 AND binding_id IS NOT NULL AND state <> 'revoked'
        LIMIT 1
        """,
        installation_id,
    )
    if commercial:
        return "not_active"

    not_after = _technical_not_after(row["ends_at"], now, max_lease_seconds)
    existing = await connection.fetchrow(
        "SELECT * FROM grants WHERE hour_entitlement_id = $1 AND gateway_id = $2 FOR UPDATE",
        hour_entitlement_id,
        gateway_id,
    )
    if existing is not None and existing["state"] == "revoked":
        return "revoked"
    if existing is None:
        grant = await connection.fetchrow(
            """
            INSERT INTO grants
                (binding_id, installation_id, hour_entitlement_id, gateway_id,
                 desired_generation, not_after, state, gateway_credential, lease_seq,
                 gateway_generation)
            VALUES (NULL, $1, $2, $3, 1, $4, 'pending', $5, 0, 0)
            RETURNING opaque_id, desired_generation
            """,
            installation_id,
            hour_entitlement_id,
            gateway_id,
            not_after,
            secrets.token_urlsafe(24),
        )
        await _enqueue(
            connection,
            operation_type=APPLY_OPERATION,
            idempotency_key=f"grant:{grant['opaque_id']}:gen:1:apply",
            payload={
                "grant_id": str(grant["opaque_id"]),
                "generation": str(grant["desired_generation"]),
                "action": "apply",
                "not_after": rfc3339(not_after),
                "hour_entitlement_id": str(hour_entitlement_id),
            },
            gateway_id=gateway_id,
            target_revision=grant["desired_generation"],
            account_id=None,
            binding_id=None,
            correlation_id=correlation_id,
        )
        return "enqueued"

    remaining = (existing["not_after"] - now).total_seconds()
    if (
        not_after < existing["not_after"] - timedelta(seconds=1)
        or remaining < max_lease_seconds / 3
    ):
        generation = existing["desired_generation"] + 1
        await connection.execute(
            """
            UPDATE grants
            SET desired_generation = $2, not_after = $3, state = 'pending',
                last_error = NULL, updated_at = now()
            WHERE id = $1
            """,
            existing["id"],
            generation,
            not_after,
        )
        await _enqueue(
            connection,
            operation_type=APPLY_OPERATION,
            idempotency_key=f"grant:{existing['opaque_id']}:gen:{generation}:apply",
            payload={
                "grant_id": str(existing["opaque_id"]),
                "generation": str(generation),
                "action": "apply",
                "not_after": rfc3339(not_after),
                "hour_entitlement_id": str(hour_entitlement_id),
            },
            gateway_id=gateway_id,
            target_revision=generation,
            account_id=None,
            binding_id=None,
            correlation_id=correlation_id,
        )
        return "enqueued"
    if existing["applied_generation"] != existing["desired_generation"]:
        return "pending"
    return "unchanged"


async def revoke_hour_grants(
    connection: asyncpg.Connection,
    *,
    installation_id: Any,
    hour_entitlement_id: Any,
    now: datetime | None = None,
) -> int:
    """Revoke only the data grants owned by this installation hour; never paid grants.

    Bumps the hour entitlement revision so the numeric admission fence changes on revoke.
    """
    now = now or utc_now()
    await connection.execute(
        """
        UPDATE entitlements
        SET revision = revision + 1
        WHERE id = $1 AND installation_id = $2 AND kind = 'onboarding_hour'
        """,
        hour_entitlement_id,
        installation_id,
    )
    grants = await connection.fetch(
        """
        SELECT * FROM grants
        WHERE installation_id = $1 AND hour_entitlement_id = $2 AND state <> 'revoked'
        FOR UPDATE
        """,
        installation_id,
        hour_entitlement_id,
    )
    revoked = 0
    for grant in grants:
        generation = grant["desired_generation"] + 1
        await connection.execute(
            """
            UPDATE grants
            SET desired_generation = $2, state = 'revoked', last_error = NULL, updated_at = now()
            WHERE id = $1
            """,
            grant["id"],
            generation,
        )
        await _enqueue(
            connection,
            operation_type=REVOKE_OPERATION,
            idempotency_key=f"grant:{grant['opaque_id']}:gen:{generation}:revoke",
            payload={
                "grant_id": str(grant["opaque_id"]),
                "generation": str(generation),
                "action": "revoke",
                "hour_entitlement_id": str(hour_entitlement_id),
            },
            gateway_id=grant["gateway_id"],
            target_revision=generation,
            account_id=None,
            binding_id=None,
        )
        revoked += 1
    return revoked


def _target_route(endpoints: dict[str, Any]) -> dict[str, Any]:
    """Transport location only (never a secret): the proven route the RPC used."""
    route: dict[str, Any] = {}
    if isinstance(endpoints.get("admin_socket"), str):
        route["admin_socket"] = endpoints["admin_socket"]
    management = endpoints.get("management")
    if isinstance(management, dict):
        route["management"] = {
            key: management[key]
            for key in ("host", "port", "server_name")
            if key in management
        }
    return route


def _decode_jsonb(value: Any) -> dict[str, Any] | None:
    """Decode a jsonb column that may arrive as dict (codec) or string (raw connection)."""
    if isinstance(value, dict):
        return value
    if isinstance(value, str):
        try:
            decoded = json.loads(value)
        except ValueError:
            return None
        return decoded if isinstance(decoded, dict) else None
    return None


def _usable_route(route: Any) -> dict[str, Any] | None:
    """A route is usable only if it carries a real transport location (never a name match)."""
    decoded = _decode_jsonb(route)
    if not decoded:
        return None
    admin_socket = decoded.get("admin_socket")
    if isinstance(admin_socket, str) and admin_socket:
        return decoded
    management = decoded.get("management")
    if (
        isinstance(management, dict)
        and isinstance(management.get("host"), str)
        and management.get("host")
        and management.get("port") is not None
    ):
        return decoded
    return None


def _proven_revoke_target(grant: asyncpg.Record) -> tuple[str | None, dict[str, Any] | None]:
    """Resolve the proven identity/route for an actual revoke.

    Only actually persisted proof counts: the immutable `target_route` captured at apply time.
    There is deliberately **no** fallback to the current registry route by name: a missing or
    malformed proven route means the original route cannot be reconstructed, so no RPC is sent
    (`proven_target_unavailable`) and the actual revoke is never claimed. New grants always carry
    the recorded target; legacy rows without it fail closed.
    """
    readback = _decode_jsonb(grant["last_readback"])
    target_node = grant["target_node_id"] or (
        str(readback.get("node_id")) if readback and readback.get("node_id") else None
    )
    if not isinstance(target_node, str) or not target_node:
        return None, None
    route = _usable_route(grant["target_route"])
    if route is None:
        return None, None
    return target_node, route


def _gateway_node_id(endpoints: dict[str, Any], gateway_key: str) -> str | None:
    """Single node identity: endpoints.node_id must exist and equal the registry key."""
    node_id = endpoints.get("node_id") if isinstance(endpoints, dict) else None
    if not isinstance(node_id, str) or not node_id or node_id != gateway_key:
        return None
    return node_id


def _validated_vk_hashes(payload: Any) -> list[str] | None:
    """None = field absent (old node) or malformed: never overwrite last-good."""
    if not isinstance(payload, list) or len(payload) > 8:
        return None
    result: list[str] = []
    for item in payload:
        if not isinstance(item, str) or not item or len(item) > 128:
            return None
        result.append(item)
    return result


def _gateway_cap_confirmed(endpoints: dict[str, Any], confirmed: Any) -> bool:
    target = endpoints.get("target_workers") if isinstance(endpoints, dict) else None
    return type(target) is int and type(confirmed) is int and confirmed > 0 and confirmed == target


def _readback_matches_apply(
    readback: dict[str, Any],
    *,
    expected: dict[str, Any],
    not_after_epoch: int,
    expected_generation: int | None = None,
    expected_lease_seq: int | None = None,
) -> bool:
    try:
        matches = (
            str(readback.get("grant_id")) == expected["grant_id"]
            and str(readback.get("registration_id")) == expected["registration_id"]
            and str(readback.get("node_id")) == expected["node_id"]
            and readback.get("revoked") is False
            and int(readback.get("expires_at", -1)) == not_after_epoch
            and readback.get("runtime_applied") is True
        )
        if expected_generation is not None:
            matches = matches and int(readback.get("generation", -1)) == expected_generation
        if expected_lease_seq is not None:
            matches = matches and int(readback.get("lease_seq", -1)) == expected_lease_seq
        return matches
    except (TypeError, ValueError):
        return False


def _readback_matches_revoke(readback: dict[str, Any], *, expected_generation: int) -> bool:
    try:
        return (
            readback.get("revoked") is True
            and int(readback.get("generation", -1)) == expected_generation
        )
    except (TypeError, ValueError):
        return False


class GatewayControlHandlers:
    """Durable outbox handlers for gateway.apply_grant / gateway.revoke_grant."""

    def __init__(self, settings: Settings, client_factory=None) -> None:
        self._settings = settings
        self._client_factory = client_factory or self._default_client

    def _default_client(self, gateway_key: str, endpoints: dict[str, Any]):
        return build_gateway_client(self._settings, gateway_key, endpoints)

    async def sync_profile(self, connection: asyncpg.Connection, operation: asyncpg.Record):
        """Coalesced per-gateway profile snapshot: RPC outside any transaction + fenced write.

        Fences: (a) the claim token is still owned by this processing row, (b) the gateway
        snapshot epoch equals this operation's target epoch, (c) registry identity/endpoints are
        unchanged. The canonical route frozen into the payload must still match the current
        registry *before* the RPC, and the same registry comparison is repeated atomically in
        the final UPDATE, so a replacement (or key change) during the RPC can never make an old
        readback current. A replaced/removed gateway or an invalid route means no RPC at all and
        last-good is kept; an old operation is never retargeted to a new endpoint.
        """
        payload = self._parse(operation)
        gateway_id = operation["gateway_id"]
        target_epoch = int(operation["target_revision"] or 0)
        node_id = payload.get("node_id")
        route = payload.get("route")
        if not isinstance(node_id, str) or not node_id or not isinstance(route, dict):
            return
        row = await connection.fetchrow(
            "SELECT gateway_key, snapshot_epoch, endpoints FROM gateways WHERE id = $1",
            gateway_id,
        )
        if row is None or int(row["snapshot_epoch"]) != target_epoch:
            return  # superseded or removed: a newer operation (if any) owns the snapshot
        if node_id != row["gateway_key"]:
            return  # registry identity replaced: never retarget the snapshot
        endpoints_now = _decode_jsonb(row["endpoints"]) or {}
        expected_endpoints = payload.get("endpoints")
        if not isinstance(expected_endpoints, dict) or _target_route(expected_endpoints) != route:
            # Missing/null/malformed payload endpoints: no RPC and last-good kept. There is no
            # legacy fallback: an operation without the frozen registry snapshot cannot fence a
            # registry replacement, so it must abstain instead of writing a retired route.
            return
        if endpoints_now != expected_endpoints:
            return  # registry replaced before the RPC: no status from a stale route
        effective_route = _usable_route(_target_route(expected_endpoints))
        if effective_route is None:
            return  # invalid route: no RPC, last-good kept
        client = self._client_factory(row["gateway_key"], effective_route)
        profile = await client.engine_status()
        if not isinstance(profile, dict) or profile.get("node_id") != node_id:
            raise GatewayError("READBACK_MISMATCH")
        if "vk_hashes" not in profile:
            return  # old node without the field: keep last-good
        validated = _validated_vk_hashes(profile.get("vk_hashes"))
        if validated is None:
            return  # malformed readback: keep last-good
        # Atomic registry recheck: a replacement during the RPC changes endpoints and the
        # write matches nothing, so the old readback can never become current.
        await connection.execute(
            """
            UPDATE gateways SET vk_hashes = $2::jsonb
            WHERE id = $1 AND snapshot_epoch = $3 AND gateway_key = $4
              AND endpoints IS NOT DISTINCT FROM $7::jsonb
              AND EXISTS (
                  SELECT 1 FROM outbox_operations
                  WHERE id = $5 AND claim_token = $6 AND status = 'processing'
              )
            """,
            gateway_id,
            validated,
            target_epoch,
            node_id,
            operation["id"],
            operation["claim_token"],
            expected_endpoints,
        )
        return

    def as_handlers(self) -> dict[str, Any]:
        return {
            APPLY_OPERATION: self.apply_grant,
            REVOKE_OPERATION: self.revoke_grant,
            SYNC_PROFILE_OPERATION: self.sync_profile,
        }

    async def _load(self, connection: asyncpg.Connection, grant_id: str) -> asyncpg.Record | None:
        return await connection.fetchrow(
            """
            SELECT g.*, COALESCE(binding.installation_id, g.installation_id) AS installation_id,
                   binding.account_id AS account_id,
                   installation.public_key_fingerprint,
                   installation.public_key_spki_b64,
                   gateway.gateway_key, gateway.endpoints
            FROM grants AS g
            LEFT JOIN account_bindings AS binding ON binding.id = g.binding_id
            JOIN installations AS installation
              ON installation.id = COALESCE(binding.installation_id, g.installation_id)
            JOIN gateways AS gateway ON gateway.id = g.gateway_id
            WHERE g.opaque_id = $1
            """,
            grant_id,
        )

    @staticmethod
    def _parse(operation: asyncpg.Record) -> dict[str, Any]:
        payload = operation["payload"]
        if isinstance(payload, str):
            payload = json.loads(payload)
        return payload

    async def apply_grant(self, connection: asyncpg.Connection, operation: asyncpg.Record):
        payload = self._parse(operation)
        grant = await self._load(connection, payload["grant_id"])
        if grant is None:
            return ("failed", "grant_missing")
        generation = str(grant["desired_generation"])
        if grant["state"] == "revoked" or generation != str(payload["generation"]):
            return ("failed", "superseded_by_newer_generation")
        if not _hour_fence_ok(grant, payload):
            return ("failed", "superseded_by_newer_generation")

        # A durable apply queued before an extra deadline must not extend it afterwards.
        # Do not mutate operation bytes on retry: a changed deadline requires normal ensure_grant
        # to enqueue a new generation, preserving the existing RPC idempotency boundary.
        if grant["binding_id"] is not None:
            from .payment_products import current_binding_paid_capacity
            capacity, deadline = await current_binding_paid_capacity(connection,grant["binding_id"])
            if not capacity:
                return ("failed", "DEVICE_LIMIT_REACHED")
            if deadline is not None and grant["not_after"] > deadline:
                return ("failed", "commercial_deadline_changed")

        endpoints = grant["endpoints"]
        if isinstance(endpoints, str):
            endpoints = json.loads(endpoints)
        endpoints = endpoints or {}
        node_id = _gateway_node_id(endpoints, grant["gateway_key"])
        if node_id is None:
            return ("failed", "node_identity_mismatch")
        client = self._client_factory(grant["gateway_key"], endpoints)
        password = grant["gateway_credential"]
        if not password:
            return ("failed", "missing_gateway_credential")
        not_after = grant["not_after"]
        not_after_epoch = int(not_after.timestamp())
        operation_id = operation["idempotency_key"]
        if grant["lease_seq"] == 0:
            expected_generation = 1
            expected_lease_seq = 1
        else:
            expected_generation = int(grant["gateway_generation"] or 1)
            expected_lease_seq = int(grant["lease_seq"]) + 1
        expected = {
            "grant_id": str(grant["opaque_id"]),
            "registration_id": grant["public_key_fingerprint"],
            "node_id": node_id,
        }
        try:
            if grant["lease_seq"] == 0:
                readback = await timed_rpc(
                    operation,
                    "provision",
                    client.grant_provision(
                        password=password,
                        grant_id=expected["grant_id"],
                        registration_id=expected["registration_id"],
                        node_id=node_id,
                        public_key_spki=grant["public_key_spki_b64"],
                        generation="1",
                        lease_seq="1",
                        operation_id=operation_id,
                        expires_at=not_after_epoch,
                    ),
                )
                gateway_generation = 1
            else:
                readback = await timed_rpc(
                    operation,
                    "refresh",
                    client.refresh_lease(
                        password=password,
                        grant_id=expected["grant_id"],
                        registration_id=expected["registration_id"],
                        node_id=node_id,
                        public_key_spki=grant["public_key_spki_b64"],
                        generation=str(grant["gateway_generation"] or 1),
                        lease_seq=str(grant["lease_seq"] + 1),
                        expected_seq=str(grant["lease_seq"]),
                        operation_id=operation_id,
                        expires_at=not_after_epoch,
                    ),
                )
                gateway_generation = int(grant["gateway_generation"] or 1)
        except GatewayError as error:
            if error.code not in TERMINAL_EXTERNAL_CODES:
                raise
            try:
                readback = await timed_rpc(operation, "get", client.grant_get(password, node_id))
            except GatewayError:
                raise error
            if not _readback_matches_apply(
                readback,
                expected=expected,
                not_after_epoch=not_after_epoch,
                expected_generation=expected_generation,
                expected_lease_seq=expected_lease_seq,
            ):
                if error.code in ("CONFLICT", "LEASE_CONFLICT", "GRANT_CONFLICT"):
                    return ("failed", "gateway_state_conflict")
                raise
            gateway_generation = int(readback.get("generation", gateway_generation))

        if not _readback_matches_apply(
            readback,
            expected=expected,
            not_after_epoch=not_after_epoch,
            expected_generation=expected_generation,
            expected_lease_seq=expected_lease_seq,
        ):
            raise GatewayError("READBACK_MISMATCH")

        if not _gateway_cap_confirmed(endpoints, readback.get("max_workers")):
            # No verified admission without a readback-confirmed worker cap that matches the
            # declared target_workers (unknown/0/mismatch never becomes applied).
            return ("failed", "worker_cap_mismatch")

        lease_seq = int(readback.get("lease_seq", grant["lease_seq"] + 1))
        log_phase(operation, "readback_confirmed")
        # One short DB transaction publishes the whole apply result: grant state, confirmed cap,
        # proven target route, snapshot epoch and the coalesced profile operation. If the dirty
        # event cannot be enqueued, the state that would let a retry skip the snapshot is rolled
        # back too, so the standard apply retry (remote apply is idempotent by operation key)
        # re-publishes it. Network RPC/readback stay OUTSIDE any transaction.
        publish_started = time.monotonic()
        log_phase(operation, "publish_begin")
        async with connection.transaction():
            updated = await connection.fetchval(
                """
                UPDATE grants
                SET applied_generation = $2, state = 'applied',
                    applied_not_after = $3, applied_at = now(),
                    lease_seq = $4, gateway_generation = $5,
                    last_readback = $6::jsonb, last_error = NULL, updated_at = now()
                WHERE id = $1 AND desired_generation = $2 AND state <> 'revoked'
                RETURNING id
                """,
                grant["id"],
                int(payload["generation"]),
                datetime.fromtimestamp(int(readback["expires_at"]), UTC),
                lease_seq,
                gateway_generation,
                readback,
            )
            if updated is None:
                log_phase(operation, "publish_superseded", publish_started)
                return ("failed", "superseded_during_apply")
            from .referral_rewards import confirm_trial_target
            await confirm_trial_target(connection,grant_id=grant['id'])
            await connection.execute(
                "UPDATE gateways SET confirmed_max_workers = $2 WHERE id = $1",
                grant["gateway_id"],
                int(readback["max_workers"]),
            )
            await connection.execute(
                """
                UPDATE grants
                SET target_node_id = $2, target_route = $3::jsonb
                WHERE id = $1
                """,
                grant["id"],
                str(readback["node_id"]),
                _target_route(endpoints),
            )
            # Node-level profile snapshot is captured by a coalesced per-gateway outbox operation
            # (never inline: two different grants must not overwrite the shared snapshot). The new
            # epoch is the fence: only an operation whose target epoch equals the current gateway
            # epoch may write, so a stale claim owner can never overwrite a fresher snapshot. The
            # frozen registry endpoints are carried in the payload and rechecked atomically by
            # the handler, so a registry replacement neither accepts an old readback nor lets
            # this operation retarget a new endpoint.
            epoch = await connection.fetchval(
                "UPDATE gateways SET snapshot_epoch = snapshot_epoch + 1 WHERE id = $1 RETURNING snapshot_epoch",
                grant["gateway_id"],
            )
            if epoch is not None:
                await _enqueue(
                    connection,
                    operation_type=SYNC_PROFILE_OPERATION,
                    idempotency_key=f"profile:{grant['gateway_id']}:{int(epoch)}",
                    payload={
                        "node_id": str(readback["node_id"]),
                        "route": _target_route(endpoints),
                        "epoch": int(epoch),
                        "endpoints": endpoints,
                    },
                    gateway_id=grant["gateway_id"],
                    target_revision=int(epoch),
                    account_id=grant["account_id"],
                    binding_id=grant["binding_id"],
                )
        log_phase(operation, "publish_end", publish_started)
        return None

    async def revoke_grant(self, connection: asyncpg.Connection, operation: asyncpg.Record):
        payload = self._parse(operation)
        grant = await self._load(connection, payload["grant_id"])
        if grant is None:
            return ("failed", "grant_missing")
        generation = int(payload["generation"])
        if grant["state"] == "revoked" and grant["applied_generation"] == generation:
            return None
        if grant["state"] != "revoked" or grant["desired_generation"] != generation:
            return ("failed", "superseded_by_newer_generation")
        if not _hour_fence_ok(grant, payload):
            return ("failed", "superseded_by_newer_generation")

        # The actual revoke must follow the identity/route that confirmed the applied grant:
        # a registry identity change (or a missing route for a legacy row) must not retarget the
        # revoke or leak the credential to a substituted endpoint.
        node_id, revoke_route = _proven_revoke_target(grant)
        if node_id is None or not revoke_route:
            return ("failed", "proven_target_unavailable")
        client = self._client_factory(grant["gateway_key"], revoke_route)
        password = grant["gateway_credential"]
        if not password:
            return ("failed", "missing_gateway_credential")
        not_after = grant["applied_not_after"] or grant["not_after"]
        not_after_epoch = int(not_after.timestamp())
        expected_gateway_generation = max(int(grant["gateway_generation"] or 0), 1) + 1
        # Final sample before peer deletion, where possible; a failure becomes an explicit
        # coverage gap instead of silently losing or fabricating bytes.
        from .usage_pipeline import record_final_usage

        try:
            await record_final_usage(connection, grant=grant, client=client, node_id=node_id)
        except Exception:
            logger.exception("final usage sample failed")
        try:
            readback = await client.grant_revoke(
                password=password,
                grant_id=str(grant["opaque_id"]),
                registration_id=grant["public_key_fingerprint"],
                node_id=node_id,
                public_key_spki=grant["public_key_spki_b64"],
                generation=str(expected_gateway_generation),
                lease_seq=str(grant["lease_seq"]),
                operation_id=operation["idempotency_key"],
                expires_at=not_after_epoch,
            )
        except GatewayError as error:
            if error.code not in TERMINAL_EXTERNAL_CODES:
                raise
            try:
                readback = await client.grant_get(password, node_id)
            except GatewayError:
                raise error
            if not _readback_matches_revoke(
                readback, expected_generation=expected_gateway_generation
            ):
                raise

        if not _readback_matches_revoke(readback, expected_generation=expected_gateway_generation):
            raise GatewayError("READBACK_MISMATCH")

        updated = await connection.fetchval(
            """
            UPDATE grants
            SET applied_generation = $2, state = 'revoked', applied_at = now(),
                gateway_generation = $3, lease_seq = $4,
                last_readback = $5::jsonb, last_error = NULL, updated_at = now()
            WHERE id = $1 AND desired_generation = $2 AND state = 'revoked'
            RETURNING id
            """,
            grant["id"],
            generation,
            int(readback.get("generation", expected_gateway_generation)),
            int(readback.get("lease_seq", grant["lease_seq"])),
            readback,
        )
        if updated is None:
            return ("failed", "superseded_during_revoke")
        return None
