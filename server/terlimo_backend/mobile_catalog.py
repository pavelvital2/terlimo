"""Verified catalog (GET /gateways) and access sync (POST /access/sync), step 03.4.

Both routes use the shared session authorizer and the accepted C01 DTOs. The catalog exposes
only confirmed applied technical descriptors (never intent), strictly validated against the
contract bounds, and a persisted monotonic subject revision that fingerprints the material
registry + subject grant state (never ephemeral times). Access/sync first re-authorizes the
fresh session, then resolves the account-bound durable receipt; only a genuinely new operation
is admitted against the volatile catalog/binding revisions. Desired grants, the parent
operation and the receipt commit atomically; RPC happens later in the existing worker, so
accepted is never reported as applied.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import ipaddress
import json
import logging
import re
import uuid
from datetime import datetime, timedelta
from typing import Any

import asyncpg
from aiohttp import web

from .auth_api import ApiError, _error_response
from .config import Settings
from .db import Database
from .gateway_control import ensure_grant, ensure_hour_grant
from .mobile_account import (
    _aggregate_states,
    _bearer_token,
    _grant_node_state,
    _operation_payload,
    _operation_state,
    request_id_for,
    rfc3339,
)
from .pop import canonical_json
from .session_auth import AuthError, SessionContext, authenticate_session, now_utc

logger = logging.getLogger(__name__)

SCHEMA_VERSION = "1.0"
ELIGIBLE_ACCOUNT_STATES = ("ACTIVE_TRIAL", "ACTIVE_PAID")
# Data subject kinds. A commercial binding grant and an installation-scoped onboarding_hour
# grant are distinct subjects; every subject query uses exactly one predicate (never a broad OR)
# so a paid and an hour grant can never be returned/revoked through the wrong subject.
SUBJECT_BINDING = "binding"
SUBJECT_HOUR = "hour"
SYNC_OPERATION = "access.sync"
IDEMPOTENCY_HEADER = "Idempotency-Key"
SNAPSHOT_ATTEMPTS = 3
# Provisional technical backoff hints for the nonterminal catalog answers (profile values are
# PENDING S1-B02); they are hints only and extend no right, lease or timeout.
ACCESS_PENDING_RETRY_AFTER_MS = 1000
ACCESS_FAILURE_RETRY_AFTER_MS = 5000
_HOSTNAME = re.compile(
    r"^(?=.{1,253}$)"
    r"[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?"
    r"(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$"
)


def _is_port(value: Any) -> bool:
    # bool is an int subclass; a port must be a real integer.
    return type(value) is int and 1 <= value <= 65535


def _valid_peer_host(value: Any) -> bool:
    if not isinstance(value, str) or not value:
        return False
    try:
        ipaddress.ip_address(value)
        return True
    except ValueError:
        pass
    return _HOSTNAME.fullmatch(value) is not None


def _canonical_spki(value: Any) -> str | None:
    """Strict unpadded base64url of exactly 32 bytes; non-canonical input is rejected."""
    if not isinstance(value, str) or len(value) != 43:
        return None
    try:
        raw = base64.b64decode(value + "=", altchars=b"-_", validate=True)
    except (binascii.Error, ValueError):
        return None
    if len(raw) != 32:
        return None
    if base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii") != value:
        return None
    return value


def _strict_capabilities(value: Any) -> list[str] | None:
    raw: Any = value
    if isinstance(raw, dict):
        raw = raw.get("capabilities")
    if raw is None:
        return []
    if not isinstance(raw, list):
        return None
    result: list[str] = []
    for item in raw:
        if not isinstance(item, str) or not item or len(item) > 64:
            return None
        if item not in result:
            result.append(item)
    return result[:16]


def _optional_fields(endpoints: dict[str, Any]) -> dict[str, Any] | None:
    fields: dict[str, Any] = {}
    region = endpoints.get("region")
    if region is not None:
        if not isinstance(region, str) or len(region) > 64:
            return None
        fields["region"] = region
    country_code = endpoints.get("country_code")
    if country_code is not None:
        if not isinstance(country_code, str) or len(country_code) > 8:
            return None
        fields["country_code"] = country_code
    target_workers = endpoints.get("target_workers")
    if target_workers is not None:
        if type(target_workers) is not int or not 1 <= target_workers <= 1024:
            return None
        fields["target_workers"] = target_workers
    return fields


def _transport_descriptor(endpoints: Any) -> dict[str, Any] | None:
    if not isinstance(endpoints, dict):
        return None
    peer_ip = endpoints.get("peer_ip")
    pin = _canonical_spki(endpoints.get("dtls_spki_sha256"))
    if not _valid_peer_host(peer_ip) or pin is None:
        return None
    if not _is_port(endpoints.get("dtls_port")) or not _is_port(endpoints.get("wg_port")):
        return None
    return {
        "protocol": "wdtt-v17",
        "peer_ip": peer_ip,
        "dtls_port": endpoints["dtls_port"],
        "wg_port": endpoints["wg_port"],
        "dtls_spki_sha256": pin,
    }


def _descriptor_valid(row: asyncpg.Record) -> bool:
    return (
        isinstance(row["endpoints"], dict)
        and _transport_descriptor(row["endpoints"]) is not None
        and _optional_fields(row["endpoints"]) is not None
        and _strict_capabilities(row["capabilities"]) is not None
    )


def _registry_error(row: asyncpg.Record) -> str | None:
    """Explicit registry admission error; never a silent rename or fallback.

    Single node identity: the registry key must equal endpoints.node_id. The advertised
    target_workers must be declared (1..1024) and later confirmed by the readback.
    """
    endpoints = row["endpoints"] if isinstance(row["endpoints"], dict) else None
    node_id = endpoints.get("node_id") if endpoints else None
    if not isinstance(node_id, str) or not node_id or node_id != row["gateway_key"]:
        return "registry_node_identity_mismatch"
    if not _descriptor_valid(row):
        return "registry_descriptors_incomplete"
    return None


def _confirmed_worker_cap(row: asyncpg.Record) -> int | None:
    endpoints = row["endpoints"] if isinstance(row["endpoints"], dict) else {}
    target = endpoints.get("target_workers")
    confirmed = row.get("confirmed_max_workers")
    if not isinstance(target, int) or type(confirmed) is not int:
        return None
    if confirmed <= 0 or confirmed != target:
        return None
    return confirmed


def _wire_generation(row: asyncpg.Record) -> str | None:
    """Node wire/auth generation (access.generation), never the management generation.

    The management queue fences (desired/applied_generation) and the node auth generation
    (gateway_generation, saved from the readback) are separate by contract. Missing (0 default),
    negative or malformed values have no truthful wire descriptor: return None so callers stay
    fail-closed and never substitute applied_generation.
    """
    value = row["gateway_generation"]
    if value is None:
        return None
    try:
        generation = int(value)
    except (TypeError, ValueError):
        return None
    return str(generation) if generation >= 1 else None


def _access_descriptor(grant: asyncpg.Record) -> dict[str, Any]:
    generation = _wire_generation(grant)
    if generation is None:
        # Unreachable for served rows (admission validates the wire generation first); kept as
        # a defensive fail-closed: never publish a false confirmed descriptor.
        raise ApiError(
            "SERVICE_UNAVAILABLE",
            http=503,
            retryable=True,
            details={"reason": "gateway_wire_generation_unavailable"},
        )
    descriptor = {
        "grant_id": str(grant["opaque_id"]),
        "device_ref": grant["public_key_fingerprint"],
        "password": grant["gateway_credential"],
        "generation": generation,
        "lease_seq": str(grant["lease_seq"]),
        "not_after": rfc3339(grant["not_after"]),
    }
    hashes = grant["vk_hashes"]
    if isinstance(hashes, list) and hashes:
        descriptor["vk_hashes"] = [item for item in hashes if isinstance(item, str)]
    return descriptor


def _grant_contract_state(grant: asyncpg.Record) -> str:
    state = grant["state"]
    if state == "revoked":
        if _revoke_confirmed(grant):
            return "revoked"
        return "enrolling" if state == "applying" else "pending"
    return {
        "pending": "pending",
        "applying": "enrolling",
        "applied": "active",
        "failed": "pending",
    }.get(state, "pending")


def _timestamp(value: datetime | None) -> str | None:
    return rfc3339(value) if value is not None else None


def _subject_grant_row(row: asyncpg.Record) -> dict[str, Any]:
    credential = row["gateway_credential"]
    return {
        "gateway_id": row["gateway_key"],
        "grant_id": str(row["opaque_id"]) if row["opaque_id"] is not None else None,
        "state": row["state"],
        "desired_generation": (
            str(row["desired_generation"]) if row["desired_generation"] is not None else None
        ),
        "applied_generation": (
            str(row["applied_generation"]) if row["applied_generation"] is not None else None
        ),
        "lease_seq": (str(row["lease_seq"]) if row["lease_seq"] is not None else None),
        "revoke_confirmed": _revoke_confirmed(row),
        "not_after": _timestamp(row["not_after"]),
        "credential_sha256": (
            hashlib.sha256(credential.encode("utf-8")).hexdigest() if credential else None
        ),
    }


def _revoke_confirmed(grant: asyncpg.Record) -> bool:
    """An actual (readback-confirmed) revoke, not merely a desired DB revoke."""
    try:
        state = grant["state"]
    except KeyError:
        state = grant["grant_state"]
    if state != "revoked":
        return False
    desired = grant["desired_generation"]
    applied = grant["applied_generation"]
    if desired is None or applied is None or int(applied) != int(desired):
        return False
    readback = grant["last_readback"]
    if isinstance(readback, str):
        try:
            readback = json.loads(readback)
        except ValueError:
            return False
    return isinstance(readback, dict) and readback.get("revoked") is True


def _data_subject(context: SessionContext) -> tuple[str, Any] | None:
    """Resolve the single active data subject for this session, or None.

    Commercial (binding) subject keeps priority per the accepted hour contract; the
    installation-scoped hour is a subject only while the hour is authoritative-active. A
    management-only control session is never a data subject.
    """
    if context.management_only:
        return None
    if (
        context.binding is not None
        and context.binding["status"] == "active"
        and context.account_state in ELIGIBLE_ACCOUNT_STATES
    ):
        return (SUBJECT_BINDING, context.binding["id"])
    hour = context.onboarding_hour
    if (
        hour is not None
        and hour["status"] == "active"
        and hour["ends_at"] is not None
        and hour["ends_at"] > context.evaluated_at
    ):
        return (SUBJECT_HOUR, hour["id"])
    return None


def _subject_hour(context: SessionContext) -> asyncpg.Record | None:
    hour = context.onboarding_hour
    if (
        hour is not None
        and hour["status"] == "active"
        and hour["ends_at"] is not None
        and hour["ends_at"] > context.evaluated_at
    ):
        return hour
    return None


def _admission_fence(context: SessionContext, subject: tuple[str, Any]) -> str:
    """Backward-compatible numeric admission fence in the existing binding_revision slot.

    Binding subject: the real account binding generation (unchanged). Hour subject: a stable
    numeric fence derived from the hour entitlement incarnation/revision, never the catalog
    revision and never the current time; a new hour incarnation or revoke changes it.
    """
    kind, _ident = subject
    if kind == SUBJECT_BINDING:
        assert context.binding is not None
        return str(int(context.binding["generation"]))
    hour = context.onboarding_hour
    assert hour is not None
    return str(int(hour["revision"]))


def _grant_effective_applied(grant: asyncpg.Record | None, evaluated_at: datetime) -> bool:
    """The exact eligibility projection used for advertisement (time-aware, strict >)."""
    return bool(
        grant is not None
        and grant["state"] == "applied"
        and grant["applied_generation"] is not None
        and grant["desired_generation"] is not None
        and int(grant["applied_generation"]) == int(grant["desired_generation"])
        and int(grant["lease_seq"] or 0) >= 1
        and grant["not_after"] is not None
        and grant["not_after"] > evaluated_at
        and grant["gateway_credential"]
    )


def _catalog_fingerprint(
    context: SessionContext,
    registry_rows: list[asyncpg.Record],
    grant_rows: list[asyncpg.Record],
    evaluated_at: datetime,
    subject: tuple[str, Any],
) -> str:
    """Material-only fingerprint: no issued_at/now, so GET never churns the revision.

    Effective eligibility is included as a boolean computed against the transaction's single
    evaluated_at, so a time-only grant expiry changes visibility and the revision without any
    SQL write, while repeated GETs between transitions stay stable.
    """
    grant_by_key = {row["gateway_key"]: row for row in grant_rows}
    registry_entries = []
    for row in registry_rows:
        grant = grant_by_key.get(row["gateway_key"])
        descriptor_valid = _descriptor_valid(row)
        advertised = (
            row["registry_state"] == "registered"
            and descriptor_valid
            and _confirmed_worker_cap(row) is not None
            and _grant_effective_applied(grant, evaluated_at)
        )
        registry_entries.append(
            {
                "gateway_id": row["gateway_key"],
                "name": row["display_name"],
                "endpoints": row["endpoints"],
                "capabilities": row["capabilities"],
                "registry_state": row["registry_state"],
                "vk_hashes": row["vk_hashes"],
                "descriptor_valid": descriptor_valid,
                "confirmed_max_workers": row["confirmed_max_workers"],
                "advertised": advertised,
            }
        )
    entitlement = None
    if context.active_entitlement is not None:
        row = context.active_entitlement
        entitlement = {
            "id": str(row["id"]),
            "kind": row["kind"],
            "status": row["status"],
            "ends_at": _timestamp(row["ends_at"]),
        }
    binding = context.binding
    hour = context.onboarding_hour
    material = {
        "environment": context.environment,
        "installation_ref": context.installation_ref,
        "account_ref": str(context.account_id) if context.account_id is not None else None,
        "account_state": context.account_state,
        "management_only": context.management_only,
        "subject": {"kind": subject[0], "id": str(subject[1])},
        "hour": (
            {
                "id": str(hour["id"]),
                "status": hour["status"],
                "revision": int(hour["revision"]),
                "ends_at": _timestamp(hour["ends_at"]),
            }
            if subject[0] == SUBJECT_HOUR and hour is not None
            else None
        ),
        "binding": (
            {
                "id": str(binding["id"]),
                "status": binding["status"],
                "generation": int(binding["generation"]),
            }
            if binding is not None
            else None
        ),
        "entitlement": entitlement,
        "registry": registry_entries,
        "grants": [
            {
                **_subject_grant_row(row),
                "effective_applied": _grant_effective_applied(row, evaluated_at),
            }
            for row in grant_rows
        ],
        "confirmed_worker_caps": {
            row["gateway_key"]: row["confirmed_max_workers"] for row in registry_rows
        },
    }
    canonical = json.dumps(material, sort_keys=True, separators=(",", ":"), default=str)
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


async def _subject_catalog_revision(
    connection: asyncpg.Connection,
    context: SessionContext,
    evaluated_at: datetime,
    subject: tuple[str, Any],
) -> int:
    """Persisted monotonic subject revision over registry + visible grant state."""
    kind, ident = subject
    registry_rows = await connection.fetch(
        """
        SELECT gateway_key, display_name, endpoints, capabilities, registry_state,
               confirmed_max_workers, vk_hashes
        FROM gateways
        WHERE environment = $1
        ORDER BY gateway_key
        """,
        context.environment,
    )
    if kind == SUBJECT_BINDING:
        grant_rows = await connection.fetch(
            """
            SELECT gateway.gateway_key, g.opaque_id, g.state, g.desired_generation,
                   g.applied_generation, g.lease_seq, g.not_after, g.gateway_credential,
                   g.last_readback
            FROM grants AS g
            JOIN gateways AS gateway ON gateway.id = g.gateway_id
            WHERE g.binding_id = $1
            ORDER BY gateway.gateway_key
            """,
            ident,
        )
    else:
        grant_rows = await connection.fetch(
            """
            SELECT gateway.gateway_key, g.opaque_id, g.state, g.desired_generation,
                   g.applied_generation, g.lease_seq, g.not_after, g.gateway_credential,
                   g.last_readback
            FROM grants AS g
            JOIN gateways AS gateway ON gateway.id = g.gateway_id
            WHERE g.hour_entitlement_id = $1
            ORDER BY gateway.gateway_key
            """,
            ident,
        )
    fingerprint = _catalog_fingerprint(
        context, list(registry_rows), list(grant_rows), evaluated_at, subject
    )
    created = await connection.fetchval(
        """
        INSERT INTO catalog_revisions (installation_id, account_id, revision, fingerprint)
        VALUES ($1, $2, 1, $3)
        ON CONFLICT (installation_id) DO NOTHING
        RETURNING revision
        """,
        context.installation_id,
        context.account_id,
        fingerprint,
    )
    if created is not None:
        return int(created)
    row = await connection.fetchrow(
        """
        SELECT revision, fingerprint FROM catalog_revisions
        WHERE installation_id = $1
        FOR UPDATE
        """,
        context.installation_id,
    )
    if row["fingerprint"] != fingerprint:
        revision = int(row["revision"]) + 1
        await connection.execute(
            """
            UPDATE catalog_revisions
            SET revision = $2, fingerprint = $3, account_id = $4, updated_at = now()
            WHERE installation_id = $1
            """,
            context.installation_id,
            revision,
            fingerprint,
            context.account_id,
        )
        return revision
    return int(row["revision"])


async def _eligible_gateways(
    connection: asyncpg.Connection,
    *,
    environment: str,
    subject: tuple[str, Any],
    evaluated_at: datetime,
) -> tuple[list[asyncpg.Record], list[asyncpg.Record], list[asyncpg.Record]]:
    kind, ident = subject
    if kind == SUBJECT_BINDING:
        rows = await connection.fetch(
            """
            SELECT gateway.*,
                   g.opaque_id, g.applied_generation, g.desired_generation, g.lease_seq,
                   g.not_after, g.state AS grant_state, g.gateway_credential,
                   installation.public_key_fingerprint
            FROM gateways AS gateway
            LEFT JOIN grants AS g
              ON g.gateway_id = gateway.id
             AND g.binding_id = $2
             AND g.state = 'applied'
             AND g.applied_generation IS NOT NULL
             AND g.applied_generation = g.desired_generation
             AND g.not_after > $3
            LEFT JOIN account_bindings AS binding ON binding.id = g.binding_id
            LEFT JOIN installations AS installation ON installation.id = binding.installation_id
            WHERE gateway.environment = $1 AND gateway.registry_state = 'registered'
            ORDER BY gateway.gateway_key
            """,
            environment,
            ident,
            evaluated_at,
        )
    else:
        rows = await connection.fetch(
            """
            SELECT gateway.*,
                   g.opaque_id, g.applied_generation, g.desired_generation, g.lease_seq,
                   g.not_after, g.state AS grant_state, g.gateway_credential,
                   installation.public_key_fingerprint
            FROM gateways AS gateway
            LEFT JOIN grants AS g
              ON g.gateway_id = gateway.id
             AND g.hour_entitlement_id = $2
             AND g.state = 'applied'
             AND g.applied_generation IS NOT NULL
             AND g.applied_generation = g.desired_generation
             AND g.not_after > $3
            LEFT JOIN installations AS installation ON installation.id = g.installation_id
            WHERE gateway.environment = $1 AND gateway.registry_state = 'registered'
            ORDER BY gateway.gateway_key
            """,
            environment,
            ident,
            evaluated_at,
        )
    for row in rows:
        reason = _registry_error(row)
        if reason is not None:
            # One inconsistent/incomplete registered node is a whole-registry error: never a
            # partial catalog, never a silent rename/fallback.
            logger.error("registry rejected %s: %s", row["gateway_key"], reason)
            raise ApiError(
                "SERVICE_UNAVAILABLE",
                http=503,
                retryable=True,
                details={"reason": reason},
            )
    complete = list(rows)
    eligible = [
        row
        for row in complete
        if row["grant_state"] == "applied"
        and row["applied_generation"] is not None
        and row["applied_generation"] == row["desired_generation"]
        and row["gateway_credential"]
    ]
    return rows, complete, eligible


def _admission_class(row: asyncpg.Record, evaluated_at: datetime) -> str:
    """Classify one registered gateway for the verified catalog decision.

    Uses the actual desired generation and the durable attempt (outbox) state, not only the
    grant state: a desired application without a matching attempt/readback is never credited
    and never treated as an authoritative removal.
    """
    grant_state = row["grant_state"]
    if grant_state is None:
        # A registered gateway with no desired application yet is not an authoritative absence.
        return "pending"
    if grant_state == "revoked":
        if _revoke_confirmed(row):
            return "authoritative_absent"
        if row["op_status"] in ("pending", "processing"):
            return "pending"
        # A desired revoke whose actual readback is not confirmed is never reported as a
        # removal: the last-good catalog is kept and the actual state stays explicit.
        return "failure"
    if grant_state == "failed":
        return "failure"
    desired = row["desired_generation"]
    applied = row["applied_generation"]
    not_after = row["not_after"]
    confirmed = (
        grant_state == "applied"
        and applied is not None
        and desired is not None
        and int(applied) == int(desired)
        and row["gateway_credential"]
        and int(row["lease_seq"] or 0) >= 1
        and _confirmed_worker_cap(row) is not None
        and _wire_generation(row) is not None
    )
    if confirmed and not_after is not None and not_after > evaluated_at:
        return "served"
    if confirmed:
        # Applied generation that expired while the right is still active: renewal is required,
        # not a removal.
        return "pending"
    if (
        grant_state == "applied"
        and applied is not None
        and desired is not None
        and int(applied) == int(desired)
        and row["gateway_credential"]
        and int(row["lease_seq"] or 0) >= 1
        and _confirmed_worker_cap(row) is None
    ):
        # Applied grant without a readback-confirmed worker cap: not an authoritative state,
        # a verification refresh is required (never a removal and never a silent advertise).
        return "pending"
    if (
        grant_state == "applied"
        and applied is not None
        and desired is not None
        and int(applied) == int(desired)
        and row["gateway_credential"]
        and int(row["lease_seq"] or 0) >= 1
        and _wire_generation(row) is None
    ):
        # Management state is applied, but the node wire/auth generation is absent or invalid:
        # there is no truthful descriptor. Stay pending (verification refresh / revoke first),
        # never a removal and never applied_generation substituted into the wire field.
        return "pending"
    op_status = row["op_status"]
    op_target = row["op_target"]
    if op_status is None or desired is None or op_target is None or int(op_target) != int(desired):
        return "failure"
    if op_status in ("pending", "processing"):
        return "pending"
    # done without confirmed readback, failed or dead are application failures.
    return "failure"


def _recoverable_expired_apply_failure(
    row: asyncpg.Record, subject: tuple[str, Any], evaluated_at: datetime
) -> bool:
    """Allow only the current binding's expired failed apply to be superseded by sync."""
    return (
        subject[0] == SUBJECT_BINDING
        and row["grant_binding_id"] == subject[1]
        and row["grant_state"] != "revoked"
        and row["not_after"] is not None
        and row["not_after"] <= evaluated_at
        and row["desired_generation"] is not None
        and row["op_type"] == "gateway.apply_grant"
        and row["op_status"] in ("dead", "failed")
        and row["op_gateway_id"] == row["gateway_row_id"]
        and row["op_binding_id"] == subject[1]
        and row["op_target"] == row["desired_generation"]
        and row["op_grant_id"] == str(row["opaque_id"])
        and row["op_generation"] == str(row["desired_generation"])
        and row["op_action"] == "apply"
    )


async def _catalog_admission(
    connection: asyncpg.Connection,
    *,
    environment: str,
    subject: tuple[str, Any],
    evaluated_at: datetime,
    recover_expired_apply_failure: bool = False,
) -> tuple[list[asyncpg.Record], list[str]]:
    kind, ident = subject
    if kind == SUBJECT_BINDING:
        rows = await connection.fetch(
            """
            SELECT gateway.id AS gateway_row_id, gateway.gateway_key, gateway.display_name,
                   gateway.endpoints, gateway.capabilities, gateway.confirmed_max_workers,
                   g.id AS grant_row_id, g.opaque_id, g.binding_id AS grant_binding_id,
                   g.state AS grant_state,
                   g.desired_generation, g.applied_generation, g.gateway_generation,
                   g.lease_seq, g.not_after,
                   g.gateway_credential, g.last_readback, gateway.vk_hashes,
                   installation.public_key_fingerprint,
                   op.operation_type AS op_type, op.status AS op_status,
                   op.gateway_id AS op_gateway_id, op.binding_id AS op_binding_id,
                   op.payload->>'grant_id' AS op_grant_id,
                   op.payload->>'generation' AS op_generation,
                   op.payload->>'action' AS op_action,
                   op.target_revision AS op_target, op.attempts AS op_attempts,
                   op.last_error AS op_last_error
            FROM gateways AS gateway
            LEFT JOIN grants AS g
              ON g.gateway_id = gateway.id AND g.binding_id = $2
            LEFT JOIN account_bindings AS binding ON binding.id = g.binding_id
            LEFT JOIN installations AS installation ON installation.id = binding.installation_id
            LEFT JOIN LATERAL (
                SELECT o.operation_type, o.status, o.target_revision, o.attempts, o.last_error,
                       o.gateway_id, o.binding_id, o.payload
                FROM outbox_operations AS o
                WHERE o.gateway_id = gateway.id AND o.binding_id = $2
                ORDER BY o.created_at DESC, o.id DESC
                LIMIT 1
            ) AS op ON true
            WHERE gateway.environment = $1 AND gateway.registry_state = 'registered'
            ORDER BY gateway.gateway_key
            """,
            environment,
            ident,
        )
    else:
        rows = await connection.fetch(
            """
            SELECT gateway.id AS gateway_row_id, gateway.gateway_key, gateway.display_name,
                   gateway.endpoints, gateway.capabilities, gateway.confirmed_max_workers,
                   g.id AS grant_row_id, g.opaque_id, g.state AS grant_state,
                   g.desired_generation, g.applied_generation, g.gateway_generation,
                   g.lease_seq, g.not_after,
                   g.gateway_credential, g.last_readback, gateway.vk_hashes,
                   installation.public_key_fingerprint,
                   op.operation_type AS op_type, op.status AS op_status,
                   op.target_revision AS op_target, op.attempts AS op_attempts,
                   op.last_error AS op_last_error
            FROM gateways AS gateway
            LEFT JOIN grants AS g
              ON g.gateway_id = gateway.id AND g.hour_entitlement_id = $2
            LEFT JOIN installations AS installation ON installation.id = g.installation_id
            LEFT JOIN LATERAL (
                SELECT o.operation_type, o.status, o.target_revision, o.attempts, o.last_error
                FROM outbox_operations AS o
                WHERE o.gateway_id = gateway.id
                  AND o.payload->>'hour_entitlement_id' = $2::text
                ORDER BY o.created_at DESC, o.id DESC
                LIMIT 1
            ) AS op ON true
            WHERE gateway.environment = $1 AND gateway.registry_state = 'registered'
            ORDER BY gateway.gateway_key
            """,
            environment,
            ident,
        )
    if not rows:
        known = await connection.fetchval(
            """
            SELECT EXISTS (SELECT 1 FROM gateways WHERE environment = $1)
                OR EXISTS (SELECT 1 FROM registry_environments WHERE environment = $1)
            """,
            environment,
        )
        if not known:
            # An uninitialized/unknown environment is never presented as a removal of everything.
            raise ApiError(
                "SERVICE_UNAVAILABLE",
                http=503,
                retryable=True,
                details={"reason": "registry_descriptors_incomplete"},
            )
        # A published environment whose registered set is empty (disabled/removed tombstones or
        # an authoritative physical removal recorded by the registry lifecycle): the empty
        # verified catalog is authoritative, not an unknown/uninitialized registry.
        return [], []
    for row in rows:
        reason = _registry_error(row)
        if reason is not None:
            logger.error("registry rejected %s: %s", row["gateway_key"], reason)
            raise ApiError(
                "SERVICE_UNAVAILABLE",
                http=503,
                retryable=True,
                details={"reason": reason},
            )
    served: list[asyncpg.Record] = []
    statuses: list[str] = []
    for row in rows:
        verdict = _admission_class(row, evaluated_at)
        if (
            verdict == "failure"
            and recover_expired_apply_failure
            and _recoverable_expired_apply_failure(row, subject, evaluated_at)
        ):
            verdict = "pending"  # sync will advance this grant under ensure_grant's row lock
        statuses.append(verdict)
        if verdict == "served":
            served.append(row)
    return served, statuses


class CatalogService:
    def __init__(self, settings: Settings, database: Database) -> None:
        self._settings = settings
        self._db = database

    # ------------------------------------------------------------------ catalog

    async def get_gateways(self, request: web.Request) -> dict[str, Any]:
        request_id = request_id_for(request)
        token = _bearer_token(request)
        views = request.query.getall("view", [])
        if views and views != ["browse"]:
            raise ApiError(
                "BAD_MESSAGE", details={"reason": "invalid_catalog_view"}, request_id=request_id
            )
        browse = views == ["browse"]
        # Snapshot + revision from one REPEATABLE READ snapshot with bounded retry, like /me.
        for _ in range(SNAPSHOT_ATTEMPTS):
            try:
                async with self._db.acquire() as connection:
                    try:
                        async with connection.transaction(isolation="repeatable_read"):
                            context = await authenticate_session(connection, self._settings, token)
                            # Explicit list projection never waits for data grant admission.
                            # Authenticate first; revoked/expired sessions still fail normally.
                            if browse:
                                return await self._browse_body(connection, context, request_id)
                            subject = _data_subject(context)
                            if subject is None:
                                # Browse projection: server list before/without a credentialed
                                # data admission. No revision, no access/transport, no secrets.
                                return await self._browse_body(connection, context, request_id)
                            self._require_data_subject(context, request_id, subject)
                            return await self._catalog_body(connection, context, request_id, subject)
                    except ApiError as error:
                        if error.code in ("ACCESS_SYNC_PENDING", "SERVICE_UNAVAILABLE"):
                            # Revision metadata continuity only (no grants/outbox/receipts): the
                            # error rollback must not make the advertised admission token stale.
                            async with connection.transaction():
                                await _subject_catalog_revision(connection, context, now_utc(), subject)
                        raise
            except (asyncpg.SerializationError, asyncpg.DeadlockDetectedError):
                logger.warning("gateways snapshot retry after serialization conflict")
                continue
        raise ApiError(
            "SERVICE_UNAVAILABLE",
            http=503,
            retryable=True,
            details={"reason": "snapshot_conflict"},
            request_id=request_id,
        )

    async def _browse_body(
        self, connection: asyncpg.Connection, context: SessionContext, request_id: str
    ) -> dict[str, Any]:
        evaluated_at = now_utc()
        rows = await connection.fetch(
            """
            SELECT gateway_key, display_name, endpoints FROM gateways
            WHERE environment = $1 AND registry_state = 'registered'
            ORDER BY gateway_key
            """,
            context.environment,
        )
        gateways: list[dict[str, Any]] = []
        for row in rows:
            optional = _optional_fields(row["endpoints"]) if isinstance(row["endpoints"], dict) else None
            optional = optional or {}
            entry: dict[str, Any] = {
                "gateway_id": row["gateway_key"],
                "name": row["display_name"] or row["gateway_key"],
            }
            if optional.get("region") is not None:
                entry["region"] = optional["region"]
            if optional.get("country_code") is not None:
                entry["country_code"] = optional["country_code"]
            gateways.append(entry)
        return {
            "request_id": request_id,
            "server_time": rfc3339(now_utc()),
            "schema_version": SCHEMA_VERSION,
            "status": "ok",
            "issued_at": rfc3339(evaluated_at),
            "valid_until": rfc3339(
                evaluated_at + timedelta(seconds=self._settings.catalog_validity_seconds)
            ),
            "catalog_mode": "browse",
            "gateways": gateways,
        }

    async def _catalog_body(
        self,
        connection: asyncpg.Connection,
        context: SessionContext,
        request_id: str,
        subject: tuple[str, Any],
    ) -> dict[str, Any]:
        evaluated_at = now_utc()
        issued_at = evaluated_at
        revision = await _subject_catalog_revision(connection, context, evaluated_at, subject)
        served, statuses = await _catalog_admission(
            connection,
            environment=context.environment,
            subject=subject,
            evaluated_at=evaluated_at,
        )
        tokens = {
            "catalog_revision": str(revision),
            "binding_revision": _admission_fence(context, subject),
        }
        if "failure" in statuses:
            # An actual application failure is never an authoritative removal and never a
            # verified catalog; the client keeps its last-good catalog.
            raise ApiError(
                "SERVICE_UNAVAILABLE",
                http=503,
                retryable=True,
                retry_after_ms=ACCESS_FAILURE_RETRY_AFTER_MS,
                details={"reason": "access_application_failed", **tokens},
                request_id=request_id,
            )
        if "pending" in statuses:
            # Nonterminal desired application (lost POST response, server-triggered or initial):
            # whole-response pending with admission tokens only; no catalog body.
            raise ApiError(
                "ACCESS_SYNC_PENDING",
                http=409,
                retryable=True,
                retry_after_ms=ACCESS_PENDING_RETRY_AFTER_MS,
                details=tokens,
                request_id=request_id,
            )
        # Response validity is bounded by the cache window AND the applicable deadlines.
        valid_until = issued_at + timedelta(seconds=self._settings.catalog_validity_seconds)
        entitlement = context.active_entitlement
        if entitlement is not None and entitlement["ends_at"] is not None:
            valid_until = min(valid_until, entitlement["ends_at"])
        subject_hour = _subject_hour(context) if subject[0] == SUBJECT_HOUR else None
        if subject_hour is not None and subject_hour["ends_at"] is not None:
            valid_until = min(valid_until, subject_hour["ends_at"])
        gateways: list[dict[str, Any]] = []
        for row in served:
            transport = _transport_descriptor(row["endpoints"])
            if row["not_after"] is not None:
                valid_until = min(valid_until, row["not_after"])
            optional = _optional_fields(row["endpoints"]) or {}
            capabilities = _strict_capabilities(row["capabilities"]) or []
            gateway: dict[str, Any] = {
                "gateway_id": row["gateway_key"],
                "name": row["display_name"] or row["gateway_key"],
                "transport": transport,
                "access": _access_descriptor(row),
            }
            if optional.get("region") is not None:
                gateway["region"] = optional["region"]
            if optional.get("country_code") is not None:
                gateway["country_code"] = optional["country_code"]
            if capabilities:
                gateway["capabilities"] = capabilities
            if optional.get("target_workers") is not None:
                gateway["target_workers"] = optional["target_workers"]
            gateways.append(gateway)
        return {
            "request_id": request_id,
            "server_time": rfc3339(now_utc()),
            "schema_version": SCHEMA_VERSION,
            "status": "ok",
            "revision": str(revision),
            "valid_until": rfc3339(valid_until),
            "issued_at": rfc3339(issued_at),
            "gateways": gateways,
        }

    # --------------------------------------------------------------- access sync

    async def sync_access(self, request: web.Request) -> dict[str, Any]:
        request_id = request_id_for(request)
        token = _bearer_token(request)
        idempotency_key = request.headers.get(IDEMPOTENCY_HEADER, "")
        if not 16 <= len(idempotency_key) <= 128:
            raise ApiError(
                "BAD_MESSAGE",
                details={"reason": "idempotency_key_required"},
                request_id=request_id,
            )
        try:
            body = await request.json()
        except (ValueError, UnicodeError):
            raise ApiError("BAD_MESSAGE", request_id=request_id) from None
        if not isinstance(body, dict) or set(body) != {"catalog_revision", "binding_revision"}:
            raise ApiError("BAD_MESSAGE", request_id=request_id)
        catalog_revision = self._revision(body.get("catalog_revision"), request_id)
        binding_revision = self._revision(body.get("binding_revision"), request_id)
        for _ in range(SNAPSHOT_ATTEMPTS):
            try:
                async with (
                    self._db.acquire() as connection,
                    connection.transaction(isolation="repeatable_read"),
                ):
                    return await self._sync_transaction(
                        connection,
                        token,
                        request_id,
                        idempotency_key,
                        catalog_revision,
                        binding_revision,
                    )
            except (asyncpg.SerializationError, asyncpg.DeadlockDetectedError):
                logger.warning("access sync snapshot retry after serialization conflict")
                continue
        raise ApiError(
            "SERVICE_UNAVAILABLE",
            http=503,
            retryable=True,
            details={"reason": "snapshot_conflict"},
            request_id=request_id,
        )

    async def _sync_transaction(
        self,
        connection: asyncpg.Connection,
        token: str,
        request_id: str,
        idempotency_key: str,
        catalog_revision: int,
        binding_revision: int,
    ) -> dict[str, Any]:
        evaluated_at = now_utc()
        # Fresh authorization/account state/ownership/revocation always run first.
        context = await authenticate_session(
            connection, self._settings, token, required_scope="access:sync", now=evaluated_at
        )
        subject = _data_subject(context)
        if subject is None:
            raise ApiError("ACCESS_DENIED", http=403, request_id=request_id)
        self._require_data_subject(context, request_id, subject)
        kind, ident = subject
        digest = self._sync_digest(context, subject, catalog_revision, binding_revision)
        if kind == SUBJECT_BINDING:
            receipt_id = await connection.fetchval(
                """
                INSERT INTO operation_receipts
                    (environment, account_ref, installation_ref, op, idempotency_key,
                     business_digest, result, result_expires_at, retain_until)
                VALUES ($1, $2, $3, $4, $5, $6, NULL, NULL, NULL)
                ON CONFLICT (environment, account_ref, installation_ref, op, idempotency_key)
                    WHERE environment IS NOT NULL AND account_ref IS NOT NULL
                DO NOTHING
                RETURNING id
                """,
                context.environment,
                str(context.account_id),
                context.installation_ref,
                SYNC_OPERATION,
                idempotency_key,
                digest,
            )
        else:
            receipt_id = await connection.fetchval(
                """
                INSERT INTO operation_receipts
                    (environment, account_ref, installation_ref, op, idempotency_key,
                     business_digest, result, result_expires_at, retain_until)
                VALUES ($1, NULL, $2, $3, $4, $5, NULL, NULL, NULL)
                ON CONFLICT (environment, installation_ref, op, idempotency_key)
                    WHERE environment IS NOT NULL
                DO NOTHING
                RETURNING id
                """,
                context.environment,
                context.installation_ref,
                SYNC_OPERATION,
                idempotency_key,
                digest,
            )
        if receipt_id is not None:
            # New operation: volatile catalog/subject admission happens only here.
            live_revision = await _subject_catalog_revision(
                connection, context, evaluated_at, subject
            )
            if live_revision != catalog_revision:
                raise ApiError(
                    "REVISION_CONFLICT",
                    http=409,
                    details={"reason": "catalog_revision_stale"},
                    request_id=request_id,
                )
            if int(_admission_fence(context, subject)) != binding_revision:
                raise ApiError(
                    "REVISION_CONFLICT",
                    http=409,
                    details={"reason": "binding_revision_stale"},
                    request_id=request_id,
                )
            correlation = uuid.uuid4()
            targets = await self._apply_sync(connection, context, subject, correlation, evaluated_at)
            if kind == SUBJECT_BINDING:
                await connection.execute(
                    """
                    INSERT INTO outbox_operations
                        (id, operation_type, payload, idempotency_key, correlation_id,
                         account_id, binding_id, status, attempts, max_attempts)
                    VALUES ($1, $2, $3::jsonb, $4, $1, $5, $6, 'done', 0, 1)
                    """,
                    correlation,
                    SYNC_OPERATION,
                    json.dumps({"targets": targets}),
                    f"{SYNC_OPERATION}:{correlation}",
                    context.account_id,
                    context.binding["id"],
                )
            else:
                await connection.execute(
                    """
                    INSERT INTO outbox_operations
                        (id, operation_type, payload, idempotency_key, correlation_id,
                         account_id, binding_id, status, attempts, max_attempts)
                    VALUES ($1, $2, $3::jsonb, $4, $1, NULL, NULL, 'done', 0, 1)
                    """,
                    correlation,
                    SYNC_OPERATION,
                    json.dumps({"targets": targets, "hour_entitlement_id": str(ident)}),
                    f"{SYNC_OPERATION}:{correlation}",
                )
            await connection.execute(
                """
                UPDATE operation_receipts
                SET result = $2::jsonb, updated_at = now()
                WHERE id = $1
                """,
                receipt_id,
                {"operation_id": str(correlation)},
            )
        else:
            account_ref = str(context.account_id) if kind == SUBJECT_BINDING else None
            existing = await connection.fetchrow(
                """
                SELECT id, business_digest, result
                FROM operation_receipts
                WHERE environment = $1 AND account_ref IS NOT DISTINCT FROM $2
                  AND installation_ref = $3
                  AND op = $4 AND idempotency_key = $5
                FOR UPDATE
                """,
                context.environment,
                account_ref,
                context.installation_ref,
                SYNC_OPERATION,
                idempotency_key,
            )
            if existing is None:
                raise ApiError("INTERNAL", http=500)
            if existing["business_digest"] != digest:
                raise ApiError(
                    "IDEMPOTENCY_CONFLICT",
                    http=409,
                    details={"reason": "business_digest_changed"},
                    request_id=request_id,
                )
            result = existing["result"]
            if isinstance(result, str):
                result = json.loads(result)
            if not isinstance(result, dict) or not result.get("operation_id"):
                raise ApiError("SERVICE_UNAVAILABLE", http=503, retryable=True)
            correlation = uuid.UUID(str(result["operation_id"]))
        return await self._sync_response(
            connection, context, subject, correlation, request_id, evaluated_at
        )

    def _revision(self, value: Any, request_id: str) -> int:
        if not isinstance(value, str) or not value.isdigit() or value.startswith("0"):
            raise ApiError("BAD_MESSAGE", request_id=request_id)
        return int(value)

    def _require_data_subject(
        self, context: SessionContext, request_id: str, subject: tuple[str, Any]
    ) -> None:
        if context.management_only:
            raise ApiError("ACCESS_DENIED", http=403, request_id=request_id)
        kind, _ident = subject
        if kind == SUBJECT_BINDING:
            if context.metadata.get("device_capacity_exceeded",False):
                raise ApiError("DEVICE_LIMIT_REACHED", http=409, request_id=request_id,
                               details={"slots_used":context.slots_used,"device_limit":context.active_entitlement["device_limit"]})
            if context.account_state not in ELIGIBLE_ACCOUNT_STATES:
                code = (
                    "SUBSCRIPTION_MISSING"
                    if context.entitlement is None
                    else "SUBSCRIPTION_EXPIRED"
                )
                raise ApiError(code, http=403, request_id=request_id)
            return
        # Hour subject is authoritative only while the installation-scoped hour is active.
        if _subject_hour(context) is None:
            raise ApiError("SUBSCRIPTION_EXPIRED", http=403, request_id=request_id)

    def _sync_digest(
        self,
        context: SessionContext,
        subject: tuple[str, Any],
        catalog_revision: int,
        binding_revision: int,
    ) -> str:
        kind, ident = subject
        material = {
            "op": SYNC_OPERATION,
            "subject_kind": kind,
            "subject_ref": str(ident),
            "account_ref": str(context.account_id) if context.account_id is not None else None,
            "installation_ref": context.installation_ref,
            "catalog_revision": catalog_revision,
            "binding_revision": binding_revision,
        }
        return hashlib.sha256(canonical_json(material)).hexdigest()

    async def _apply_sync(
        self,
        connection: asyncpg.Connection,
        context: SessionContext,
        subject: tuple[str, Any],
        correlation: uuid.UUID,
        evaluated_at: datetime,
    ) -> list[str]:
        kind, ident = subject
        # Only an expired failed apply for this binding/current generation may advance through
        # ensure_grant. Every other terminal failure remains blocked.
        _served, statuses = await _catalog_admission(
            connection,
            environment=context.environment,
            subject=subject,
            evaluated_at=evaluated_at,
            recover_expired_apply_failure=True,
        )
        if "failure" in statuses:
            raise ApiError(
                "SERVICE_UNAVAILABLE",
                http=503,
                retryable=True,
                retry_after_ms=ACCESS_FAILURE_RETRY_AFTER_MS,
                details={"reason": "access_application_failed"},
            )
        _rows, complete, _eligible = await _eligible_gateways(
            connection,
            environment=context.environment,
            subject=subject,
            evaluated_at=evaluated_at,
        )
        if not complete:
            raise ApiError(
                "SERVICE_UNAVAILABLE",
                http=503,
                retryable=True,
                details={"reason": "registry_descriptors_incomplete"},
            )
        targets: list[str] = []
        for row in complete:
            if kind == SUBJECT_BINDING:
                assert context.binding is not None and context.active_entitlement is not None
                outcome = await ensure_grant(
                    connection,
                    binding_id=context.binding["id"],
                    gateway_id=row["id"],
                    entitlement_id=context.active_entitlement["id"],
                    max_lease_seconds=self._settings.gateway_max_lease_seconds,
                    now=evaluated_at,
                    correlation_id=correlation,
                )
            else:
                outcome = await ensure_hour_grant(
                    connection,
                    installation_id=context.installation_id,
                    hour_entitlement_id=ident,
                    gateway_id=row["id"],
                    max_lease_seconds=self._settings.gateway_max_lease_seconds,
                    now=evaluated_at,
                    correlation_id=correlation,
                )
            if outcome == "device_limit_reached":
                raise ApiError("DEVICE_LIMIT_REACHED", http=409, details={"slots_used":context.slots_used,"device_limit":context.active_entitlement["device_limit"]})
            if outcome == "revoked":
                raise ApiError("DEVICE_REVOKED", http=403)
            if outcome in ("missing", "installation_revoked", "ownership_mismatch", "environment_mismatch"):
                raise ApiError("ACCESS_DENIED", http=403)
            if outcome == "no_entitlement":
                raise ApiError("SUBSCRIPTION_EXPIRED", http=403)
            if outcome in ("not_active",):
                raise ApiError("ACCESS_DENIED", http=403)
            targets.append(row["gateway_key"])
        return targets

    async def _sync_response(
        self,
        connection: asyncpg.Connection,
        context: SessionContext,
        subject: tuple[str, Any],
        correlation: uuid.UUID,
        request_id: str,
        evaluated_at: datetime,
    ) -> dict[str, Any]:
        kind, ident = subject
        parent = await connection.fetchrow(
            "SELECT * FROM outbox_operations WHERE id = $1", correlation
        )
        if parent is not None:
            if kind == SUBJECT_BINDING:
                if parent["account_id"] != context.account_id:
                    raise ApiError("SERVICE_UNAVAILABLE", http=503, retryable=True)
            else:
                payload = _operation_payload(parent)
                if (
                    parent["account_id"] is not None
                    or str(payload.get("hour_entitlement_id") or "") != str(ident)
                ):
                    raise ApiError("SERVICE_UNAVAILABLE", http=503, retryable=True)
        targets: list[str] = []
        if parent is not None:
            targets = [
                item
                for item in (_operation_payload(parent).get("targets") or [])
                if isinstance(item, str)
            ]
        child_ops = await connection.fetch(
            """
            SELECT operation.*, gateway.gateway_key
            FROM outbox_operations AS operation
            LEFT JOIN gateways AS gateway ON gateway.id = operation.gateway_id
            WHERE operation.correlation_id = $1 AND operation.id <> $1
            ORDER BY operation.created_at
            """,
            correlation,
        )
        children = {child["gateway_key"]: child for child in child_ops if child["gateway_key"]}
        if parent is None:
            # Legacy correlation-only receipt (created before the durable parent row):
            # keep resolving the same operation identity from its children instead of
            # inventing a repeat or an empty success.
            if not child_ops or any(
                child["account_id"] != context.account_id for child in child_ops
            ):
                raise ApiError("SERVICE_UNAVAILABLE", http=503, retryable=True)
            targets = [
                child["gateway_key"] for child in child_ops if child["gateway_key"]
            ]
        if kind == SUBJECT_BINDING:
            grants = await connection.fetch(
                """
                SELECT g.*, gateway.gateway_key
                FROM grants AS g
                JOIN gateways AS gateway ON gateway.id = g.gateway_id
                WHERE g.binding_id = $1
                """,
                ident,
            )
        else:
            grants = await connection.fetch(
                """
                SELECT g.*, gateway.gateway_key
                FROM grants AS g
                JOIN gateways AS gateway ON gateway.id = g.gateway_id
                WHERE g.hour_entitlement_id = $1
                """,
                ident,
            )
        grant_by_key = {grant["gateway_key"]: grant for grant in grants}
        per_state: list[str] = []
        per_node: list[dict[str, Any]] = []
        grant_entries: list[dict[str, Any]] = []
        for gateway_key in targets:
            grant = grant_by_key.get(gateway_key)
            child = children.get(gateway_key)
            if child is not None:
                state, _ = _operation_state(child, grant, _operation_payload(child))
                last_error = (child["last_error"] or "")[:64] or None
                attempts = int(child["attempts"])
            else:
                # Already-applied / noop target: readback of its own grant, never intent.
                state, _ = _grant_node_state(grant)
                last_error = None
                attempts = 0
            per_state.append(state)
            per_node.append(
                {
                    "gateway_id": gateway_key,
                    "state": state,
                    "applied_generation": (
                        str(grant["applied_generation"])
                        if grant is not None and grant["applied_generation"] is not None
                        else None
                    ),
                    "not_after": (
                        rfc3339(grant["not_after"])
                        if grant is not None and grant["not_after"] is not None
                        else None
                    ),
                    "last_error": last_error,
                    "attempts": attempts,
                }
            )
            if grant is not None:
                grant_entries.append(
                    {
                        "grant_id": str(grant["opaque_id"]),
                        "binding_ref": (
                            str(context.binding["id"])
                            if kind == SUBJECT_BINDING and context.binding is not None
                            else context.installation_ref
                        ),
                        "gateway_id": gateway_key,
                        "desired_generation": str(grant["desired_generation"]),
                        "applied_generation": (
                            str(grant["applied_generation"])
                            if grant["applied_generation"] is not None
                            else None
                        ),
                        "not_after": rfc3339(grant["not_after"]),
                        "state": _grant_contract_state(grant),
                    }
                )
        _, application_state = _aggregate_states(per_state)
        revision = await _subject_catalog_revision(connection, context, evaluated_at, subject)
        return {
            "request_id": request_id,
            "server_time": rfc3339(now_utc()),
            "schema_version": SCHEMA_VERSION,
            "status": "ok",
            "operation_id": str(correlation),
            "access_application_state": application_state,
            "grants": grant_entries,
            "revision": str(revision),
        }


CATALOG_SERVICE_KEY: web.AppKey = web.AppKey("catalog_service", CatalogService)


async def _handle_gateways(request: web.Request) -> web.Response:
    fallback_id = request_id_for(request)
    try:
        return web.json_response(await request.app[CATALOG_SERVICE_KEY].get_gateways(request))
    except AuthError as error:
        return _error_response(
            fallback_id,
            ApiError(error.code, http=error.http, retryable=error.retryable, request_id=fallback_id),
        )
    except ApiError as error:
        return _error_response(fallback_id, error)
    except (asyncpg.PostgresError, OSError):
        logger.exception("gateways failed")
        return _error_response(fallback_id, ApiError("SERVICE_UNAVAILABLE", request_id=fallback_id))


async def _handle_access_sync(request: web.Request) -> web.Response:
    fallback_id = request_id_for(request)
    try:
        return web.json_response(await request.app[CATALOG_SERVICE_KEY].sync_access(request))
    except AuthError as error:
        return _error_response(
            fallback_id,
            ApiError(error.code, http=error.http, retryable=error.retryable, request_id=fallback_id),
        )
    except ApiError as error:
        return _error_response(fallback_id, error)
    except (asyncpg.PostgresError, OSError):
        logger.exception("access sync failed")
        return _error_response(fallback_id, ApiError("SERVICE_UNAVAILABLE", request_id=fallback_id))


def register_catalog_routes(app: web.Application) -> None:
    app.router.add_get("/api/mobile/v1/gateways", _handle_gateways)
    app.router.add_post("/api/mobile/v1/access/sync", _handle_access_sync)
