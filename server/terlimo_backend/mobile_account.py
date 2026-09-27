"""Account read routes (step 03.4, server side): GET /me and own operation status.

Both routes use the shared session authorizer and real PostgreSQL state. Reads never create a
trial, onboarding hour or grant; unknown state is never presented as known; foreign resources
are a neutral 404. Responses use the exact accepted C01 DTOs.
"""

from __future__ import annotations

import hashlib
import json
import logging
import uuid
from collections.abc import Mapping
from datetime import UTC, datetime, timedelta
from typing import Any

import asyncpg
from aiohttp import web

from .auth_api import ApiError, _error_response, random_hex
from .config import Settings
from .db import Database
from .session_auth import AuthError, SessionContext, authenticate_session, now_utc
from .telegram_binding import registration_view
from .trial_activation import trial_status

logger = logging.getLogger(__name__)

SCHEMA_VERSION = "1.0"
SYNC_PARENT_OPERATION = "access.sync"
BASE_LIMIT = 2
REQUEST_ID_HEADER = "X-Request-ID"
_TERMINAL_FAILURES = frozenset(
    {
        "superseded_by_newer_generation",
        "superseded_during_apply",
        "superseded_during_revoke",
        "unsupported_operation_type",
        "grant_missing",
        "gateway_state_conflict",
        "missing_gateway_credential",
    }
)



async def effective_device_limit(connection, account_id, *, now: datetime | None = None) -> int:
    """Most recent EFFECTIVE commercial right decides the slot limit.

    Same semantics as session_auth commercial selection: status=active and
    starts_at <= now < ends_at (NULL start/end allowed). A future or expired right never
    changes the limit; without one the base limit applies.
    """
    now = now or datetime.now(UTC)
    rows = await connection.fetch(
        """
        SELECT device_limit, starts_at, ends_at, status FROM entitlements
        WHERE account_id = $1 AND kind IN ('trial','paid','imported')
        ORDER BY created_at DESC
        """,
        account_id,
    )
    for item in rows:
        started = item["starts_at"] is None or item["starts_at"] <= now
        not_ended = item["ends_at"] is None or item["ends_at"] > now
        if item["status"] == "active" and started and not_ended:
            return int(item["device_limit"]) if item["device_limit"] is not None else BASE_LIMIT
    return BASE_LIMIT

def rfc3339(moment: datetime | None) -> str | None:
    if moment is None:
        return None
    return moment.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def request_id_for(request: web.Request) -> str:
    header = request.headers.get(REQUEST_ID_HEADER, "")
    if len(header) == 32 and all(character in "0123456789abcdef" for character in header):
        return header
    return random_hex(16)


def _bearer_token(request: web.Request) -> str:
    header = request.headers.get("Authorization", "")
    prefix = "Bearer "
    if not header.startswith(prefix):
        raise AuthError("SESSION_INVALID", 401)
    return header[len(prefix) :].strip()


def _entitlement_plan(entitlement: Mapping[str, Any]) -> dict[str, Any] | None:
    """Strict, issuance-time plan view; None means the plan is unknown (no fabrication)."""
    data = entitlement if isinstance(entitlement, dict) else dict(entitlement)
    raw = data.get("source_plan")
    if isinstance(raw, str):
        try:
            raw = json.loads(raw)
        except ValueError:
            raw = None
    if not isinstance(raw, dict):
        return None
    plan_id = raw.get("plan_id")
    duration_code = raw.get("duration_code")
    if not isinstance(plan_id, str) or not plan_id or not isinstance(duration_code, str) or not duration_code:
        return None
    title = raw.get("title")
    return {"id": plan_id, "title": title if isinstance(title, str) else None, "duration_code": duration_code}


def _entitlement_snapshot(ctx: SessionContext) -> dict[str, Any]:
    entitlement = ctx.entitlement
    if entitlement is None or entitlement["kind"] not in ("trial", "paid", "imported"):
        return {
            "type": "none",
            "status": "none",
            "valid_from": None,
            "valid_until": None,
            "effective_device_limit": BASE_LIMIT,
            "slots_used": ctx.slots_used,
            "revision": "0",
            "perpetual_commercial": False,
            "plan": None,
        }
    if ctx.active_entitlement is not None:
        status = "active"
    elif entitlement["status"] == "revoked":
        status = "revoked"
    elif entitlement["status"] != "active":
        status = "expired" if entitlement["status"] == "expired" else "none"
    elif entitlement["starts_at"] is not None and entitlement["starts_at"] > ctx.evaluated_at:
        # A scheduled future right is neither active nor expired.
        status = "none"
    elif entitlement["ends_at"] is not None and entitlement["ends_at"] <= ctx.evaluated_at:
        status = "expired"
    else:
        status = "none"
    return {
        "type": entitlement["kind"],
        "status": status,
        "valid_from": rfc3339(entitlement["starts_at"]),
        "valid_until": rfc3339(entitlement["ends_at"]),
        "effective_device_limit": (
            entitlement["device_limit"] if entitlement["device_limit"] is not None else BASE_LIMIT
        ),
        "slots_used": ctx.slots_used,
        "revision": str(entitlement["revision"] or 0),
        "perpetual_commercial": entitlement["ends_at"] is None,
        "plan": _entitlement_plan(entitlement),
    }


def _onboarding(ctx: SessionContext) -> dict[str, Any]:
    hour = ctx.onboarding_hour
    base = {
        "started_by": "server_confirmed_first_connection",
        "duration_seconds": 3600,
        "one_time": True,
        "extends_on_refresh": False,
        "extends_on_restart": False,
        "creates_trial": False,
        "requires_hardware_id": False,
        "unit": "installation_fingerprint",
        "post_telegram_identity": "account_history_correlation",
        "pre_telegram_reinstall": "may_be_indistinguishable_new_key_separate_unit",
    }
    if hour is None or hour["starts_at"] is None:
        return {**base, "state": "not_started", "started_at": None, "not_after": None}
    state = (
        "active"
        if hour["status"] == "active"
        and hour["ends_at"] is not None
        and hour["ends_at"] > ctx.evaluated_at
        else "expired"
    )
    return {
        **base,
        "state": state,
        "started_at": rfc3339(hour["starts_at"]),
        "not_after": rfc3339(hour["ends_at"]),
        # Stable hour incarnation/fence revision: bumped on revoke/incarnation, never the
        # current time and never the catalog revision.
        "revision": int(hour["revision"]),
    }


def _grant_resolution(ctx: SessionContext) -> dict[str, Any]:
    if ctx.management_only:
        return {
            "control_available": True,
            "restricted_checkout_available": True,
            "data_access": "none",
            "effective_deadline": None,
        }
    if ctx.active_entitlement is not None and ctx.binding_status == "active":
        return {
            "control_available": True,
            "restricted_checkout_available": True,
            "data_access": "subscription_data",
            "effective_deadline": rfc3339(ctx.active_entitlement["ends_at"]),
        }
    hour = ctx.onboarding_hour
    if (
        hour is not None
        # The hour unit is the installation fingerprint: an installation-bound active hour is
        # valid without an account binding (pre-Telegram onboarding).
        and hour["status"] == "active"
        and hour["ends_at"] is not None
        and hour["ends_at"] > ctx.evaluated_at
    ):
        return {
            "control_available": True,
            "restricted_checkout_available": True,
            "data_access": "onboarding_hour",
            "effective_deadline": rfc3339(hour["ends_at"]),
        }
    return {
        "control_available": True,
        "restricted_checkout_available": True,
        "data_access": "none",
        "effective_deadline": None,
    }


def _operation_payload(operation: asyncpg.Record) -> dict[str, Any]:
    payload = operation["payload"]
    if isinstance(payload, str):
        try:
            payload = json.loads(payload)
        except ValueError:
            return {}
    return payload if isinstance(payload, dict) else {}


def _active_hour_id(context: SessionContext) -> Any | None:
    hour = context.onboarding_hour
    if (
        hour is not None
        and hour["installation_id"] == context.installation_id
        and hour["status"] == "active"
        and hour["starts_at"] <= context.evaluated_at
        and hour["ends_at"] is not None
        and context.evaluated_at < hour["ends_at"]
    ):
        return hour["id"]
    return None


def _operation_state(
    operation: asyncpg.Record,
    grant: asyncpg.Record | None,
    payload: dict[str, Any],
) -> tuple[str, str]:
    """Project the real outbox row plus the target-operation readback.

    A done gateway operation is `applied` only when the readback confirms *this* operation's
    target generation and action (apply -> applied, revoke -> revoked). A missing readback,
    a superseded target or a generation/action mismatch is rejected - never credited.
    """
    status = operation["status"]
    if status == "pending":
        return "queued", "pending"
    if status == "processing":
        return "applying", "pending"
    if status == "done":
        if operation["operation_type"] not in ("gateway.apply_grant", "gateway.revoke_grant"):
            return "applied", "applied"
        target = operation["target_revision"]
        generation = payload.get("generation")
        action = payload.get("action")
        if (
            target is None
            or generation is None
            or action not in ("apply", "revoke")
            or str(generation) != str(target)
        ):
            return "rejected", "rejected"
        if grant is None or operation["gateway_id"] is None:
            return "rejected", "rejected"
        if operation["binding_id"] is not None:
            if grant["binding_id"] != operation["binding_id"]:
                return "rejected", "rejected"
        elif (
            grant["hour_entitlement_id"] is None
            or str(grant["hour_entitlement_id"]) != str(payload.get("hour_entitlement_id"))
            or str(grant["opaque_id"]) != str(payload.get("grant_id"))
            or grant["gateway_id"] != operation["gateway_id"]
        ):
            return "rejected", "rejected"
        if int(grant["desired_generation"]) != int(target):
            return "rejected", "rejected"
        confirmed = (
            grant["applied_generation"] is not None
            and int(grant["applied_generation"]) == int(target)
            and (
                (action == "apply" and grant["state"] == "applied")
                or (action == "revoke" and grant["state"] == "revoked")
            )
        )
        return ("applied", "applied") if confirmed else ("applying", "pending")
    if status == "failed" and operation["last_error"] not in _TERMINAL_FAILURES:
        return "retryable_failure", "retryable_failure"
    if status == "dead":
        return "retryable_failure", "retryable_failure"
    return "rejected", "rejected"


def _snapshot_fingerprint(fields: dict[str, Any]) -> str:
    canonical = json.dumps(fields, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


async def _subject_revision(
    connection: asyncpg.Connection,
    installation_id: Any,
    account_id: Any,
    fingerprint: str,
) -> int:
    """Stable monotonic subject revision.

    Only metadata is persisted on read; entitlements/trial/hour/grants are never created or
    extended. The row lock serializes concurrent GETs and changes so the increment cannot be
    lost and a torn snapshot revision cannot be observed.
    """
    created = await connection.fetchval(
        """
        INSERT INTO subject_revisions (installation_id, account_id, revision, fingerprint)
        VALUES ($1, $2, 1, $3)
        ON CONFLICT (installation_id) DO NOTHING
        RETURNING revision
        """,
        installation_id,
        account_id,
        fingerprint,
    )
    if created is not None:
        return int(created)
    row = await connection.fetchrow(
        "SELECT revision, fingerprint FROM subject_revisions WHERE installation_id = $1 FOR UPDATE",
        installation_id,
    )
    if row["fingerprint"] != fingerprint:
        revision = int(row["revision"]) + 1
        await connection.execute(
            """
            UPDATE subject_revisions
            SET revision = $2, fingerprint = $3, account_id = $4, updated_at = now()
            WHERE installation_id = $1
            """,
            installation_id,
            revision,
            fingerprint,
            account_id,
        )
        return revision
    return int(row["revision"])


class AccountService:
    def __init__(self, settings: Settings, database: Database) -> None:
        self._settings = settings
        self._db = database

    async def _context(self, request: web.Request, *, required_scope: str | None = None) -> SessionContext:
        token = _bearer_token(request)
        async with self._db.acquire() as connection:
            return await authenticate_session(
                connection, self._settings, token, required_scope=required_scope
            )

    async def _build_me(
        self, connection: asyncpg.Connection, token: str, request_id: str
    ) -> dict[str, Any]:
        """Snapshot + revision from one REPEATABLE READ snapshot and one timestamp."""
        evaluated_at = now_utc()
        context = await authenticate_session(
            connection, self._settings, token, now=evaluated_at
        )
        telegram_linked = False
        if context.account_id is not None:
            telegram_id = await connection.fetchval(
                "SELECT telegram_id FROM accounts WHERE id = $1", context.account_id
            )
            telegram_linked = telegram_id is not None
        entitlement = _entitlement_snapshot(context)
        onboarding = _onboarding(context)
        resolution = _grant_resolution(context)
        registration = await registration_view(connection, context.installation_id, self._settings)
        trial = await trial_status(connection, self._settings, context)
        # Binding generation fence from the same snapshot; reading never creates a slot/right.
        # For an installation-scoped active hour (no commercial binding) the same backward
        # compatible numeric slot carries the stable hour fence (entitlement revision), so the
        # existing access.sync request shape works unchanged.
        if context.binding is not None:
            binding_revision = str(int(context.binding["generation"]))
        elif (
            context.onboarding_hour is not None
            and context.onboarding_hour["status"] == "active"
            and context.onboarding_hour["ends_at"] is not None
            and context.onboarding_hour["ends_at"] > context.evaluated_at
        ):
            binding_revision = str(int(context.onboarding_hour["revision"]))
        else:
            binding_revision = None
        fingerprint = _snapshot_fingerprint(
            {
                "account_ref": (
                    str(context.account_id) if context.account_id is not None else None
                ),
                "account_state": context.account_state,
                "telegram_linked": telegram_linked,
                "entitlement": entitlement,
                "binding_status": context.binding_status,
                "binding_generation": binding_revision,
                "management_only": context.management_only,
                "onboarding": onboarding,
                "grant_resolution": resolution,
                "registration": registration,
                "trial": trial,
            }
        )
        revision = await _subject_revision(
            connection, context.installation_id, context.account_id, fingerprint
        )
        body: dict[str, Any] = {
            "request_id": request_id,
            "server_time": rfc3339(now_utc()),
            "schema_version": SCHEMA_VERSION,
            "status": "ok",
            "account_state": context.account_state,
            "telegram_linked": telegram_linked,
            "entitlement": entitlement,
            "binding_status": context.binding_status,
            "binding_revision": binding_revision,
            "management_only": context.management_only,
            "onboarding": onboarding,
            "grant_resolution": resolution,
            "registration": registration,
            "trial": trial,
            "revision": str(revision),
        }
        if context.account_id is not None:
            body["account_ref"] = str(context.account_id)
        return body

    async def get_usage(self, request: web.Request) -> dict[str, Any]:
        """Account historical usage (contract 09 §11): MSK calendar buckets, explicit coverage."""
        from .mobile_catalog import ELIGIBLE_ACCOUNT_STATES
        from .usage_pipeline import MSK, REPORTING_TIMEZONE, USAGE_FRESHNESS_SECONDS

        request_id = request_id_for(request)
        token = _bearer_token(request)
        async with self._db.acquire() as connection:
            context = await authenticate_session(
                connection, self._settings, token, required_scope="session:read"
            )
            if (
                context.management_only
                or context.binding is None
                or context.binding["status"] != "active"
            ):
                raise ApiError("ACCESS_DENIED", http=403, request_id=request_id)
            if context.account_state not in ELIGIBLE_ACCOUNT_STATES:
                code = (
                    "SUBSCRIPTION_MISSING"
                    if context.entitlement is None
                    else "SUBSCRIPTION_EXPIRED"
                )
                raise ApiError(code, http=403, request_id=request_id)
            moment = now_utc()
            today_start = moment.astimezone(MSK).replace(
                hour=0, minute=0, second=0, microsecond=0
            )
            window_start = today_start - timedelta(days=29)
            rows = await connection.fetch(
                """
                SELECT (observed_at AT TIME ZONE $2)::date AS day,
                       sum(rx_delta) AS rx, sum(tx_delta) AS tx
                FROM usage_ticks
                WHERE installation_id = $1 AND observed_at >= $3
                GROUP BY day
                """,
                context.installation_id,
                REPORTING_TIMEZONE,
                window_start.astimezone(UTC),
            )
            day_totals = {
                row["day"]: (int(row["rx"] or 0), int(row["tx"] or 0)) for row in rows
            }

            coverage = await connection.fetchrow(
                "SELECT segment_start, last_trusted_at FROM usage_coverage WHERE installation_id = $1",
                context.installation_id,
            )
        coverage_start = coverage["segment_start"] if coverage else None
        last_trusted_at = coverage["last_trusted_at"] if coverage else None
        fresh = bool(
            last_trusted_at is not None
            and last_trusted_at >= moment - timedelta(seconds=USAGE_FRESHNESS_SECONDS)
        )
        # Canonical contract usage.json: exactly today/7d/30d buckets, each complete only
        # when the current contiguous trusted segment covers that whole window AND trusted
        # observations are fresh. Stale or gapped history is never presented as complete.
        buckets: list[dict[str, Any]] = []
        for period, days in (("today", 1), ("7d", 7), ("30d", 30)):
            rx = tx = 0
            for offset in range(days):
                day = (today_start - timedelta(days=offset)).date()
                bucket = day_totals.get(day, (0, 0))
                rx += bucket[0]
                tx += bucket[1]
            period_start = today_start - timedelta(days=days - 1)
            complete = bool(
                coverage_start is not None
                and coverage_start <= period_start.astimezone(UTC)
                and fresh
            )
            buckets.append(
                {
                    "period": period,
                    "rx_bytes": rx,
                    "tx_bytes": tx,
                    "complete": complete,
                }
            )
        return {
            "request_id": request_id,
            "server_time": rfc3339(now_utc()),
            "schema_version": SCHEMA_VERSION,
            "status": "ok",
            "timezone": REPORTING_TIMEZONE,
            "as_of": rfc3339(last_trusted_at) if last_trusted_at else None,
            "coverage_start": rfc3339(coverage_start) if coverage_start else None,
            "buckets": buckets,
        }

    async def get_me(self, request: web.Request) -> dict[str, Any]:
        request_id = request_id_for(request)
        token = _bearer_token(request)
        # Whole snapshot + revision in REPEATABLE READ, bounded retry on serialization
        # conflicts; each attempt re-authenticates and re-reads from one snapshot. Locks are
        # released with the transaction before the next attempt.
        last_attempts = 0
        for _ in range(3):
            last_attempts += 1
            try:
                async with (
                    self._db.acquire() as connection,
                    connection.transaction(isolation="repeatable_read"),
                ):
                    return await self._build_me(connection, token, request_id)
            except (asyncpg.SerializationError, asyncpg.DeadlockDetectedError):
                logger.warning("me snapshot retry %d after serialization conflict", last_attempts)
                continue
        raise ApiError(
            "SERVICE_UNAVAILABLE",
            http=503,
            retryable=True,
            details={"reason": "snapshot_conflict"},
            request_id=request_id,
        )

    async def get_operation(self, request: web.Request, operation_id: str) -> dict[str, Any]:
        from .mobile_catalog import _data_subject

        observe = request.query.get("observe") == OBSERVE_SYNC_RECOVERY
        request_id = request_id_for(request)
        context = await self._context(request)
        try:
            parsed_id = uuid.UUID(operation_id)
        except (ValueError, AttributeError):
            raise ApiError("NOT_FOUND", http=404, request_id=request_id) from None
        async with self._db.acquire() as connection:
            operation = await connection.fetchrow(
                """
                SELECT operation.*, gateway.gateway_key
                FROM outbox_operations AS operation
                LEFT JOIN gateways AS gateway ON gateway.id = operation.gateway_id
                WHERE operation.id = $1
                """,
                parsed_id,
            )
            owned_account = (
                operation is not None
                and operation["account_id"] is not None
                and context.account_id is not None
                and operation["account_id"] == context.account_id
            )
            hour_id = _active_hour_id(context)
            owned_hour_parent = (
                operation is not None
                and operation["operation_type"] == SYNC_PARENT_OPERATION
                and operation["account_id"] is None
                and hour_id is not None
                and str(_operation_payload(operation).get("hour_entitlement_id"))
                == str(hour_id)
            )
            owned_hour_child = False
            if (
                operation is not None
                and operation["operation_type"] in ("gateway.apply_grant", "gateway.revoke_grant")
                and operation["account_id"] is None
                and operation["binding_id"] is None
                and operation["correlation_id"] is not None
                and hour_id is not None
                and str(_operation_payload(operation).get("hour_entitlement_id")) == str(hour_id)
            ):
                parent = await connection.fetchrow(
                    "SELECT * FROM outbox_operations WHERE id = $1",
                    operation["correlation_id"],
                )
                owned_hour_child = (
                    parent is not None
                    and parent["operation_type"] == SYNC_PARENT_OPERATION
                    and parent["account_id"] is None
                    and str(_operation_payload(parent).get("hour_entitlement_id")) == str(hour_id)
                )
            if owned_account or owned_hour_parent or owned_hour_child:
                if operation["operation_type"] == SYNC_PARENT_OPERATION:
                    return await _sync_parent_progress(
                        connection, operation, request_id, context, observe=observe
                    )
                grant = None
                if operation["binding_id"] is not None and operation["gateway_id"] is not None:
                    grant = await connection.fetchrow(
                        """
                        SELECT * FROM grants
                        WHERE binding_id = $1 AND gateway_id = $2
                        """,
                        operation["binding_id"],
                        operation["gateway_id"],
                    )
                elif owned_hour_child and operation["gateway_id"] is not None:
                    grant = await connection.fetchrow(
                        """
                        SELECT * FROM grants
                        WHERE hour_entitlement_id = $1 AND installation_id = $2 AND gateway_id = $3
                        """,
                        hour_id,
                        context.installation_id,
                        operation["gateway_id"],
                    )
                state, application_state = _operation_state(
                    operation, grant, _operation_payload(operation)
                )
                per_node: list[dict[str, Any]] = []
                if grant is not None and operation["gateway_key"]:
                    per_node.append(_node_entry(operation, grant, state))
                body = {
                    "request_id": request_id,
                    "server_time": rfc3339(now_utc()),
                    "schema_version": SCHEMA_VERSION,
                    "status": "ok",
                    "operation_id": str(operation["id"]),
                    "operation_type": operation["operation_type"],
                    "state": state,
                    "access_application_state": application_state,
                    "per_node": per_node,
                }
                if observe:
                    payload = _operation_payload(operation)
                    pairs = (
                        [(operation, grant, payload)] if grant is not None else []
                    )
                    body["sync_recovery"] = await _sync_recovery_observation(
                        connection,
                        context,
                        _data_subject(context),
                        pairs,
                        now_utc(),
                    )
                return body
            children = await connection.fetch(
                """
                SELECT operation.*, gateway.gateway_key
                FROM outbox_operations AS operation
                LEFT JOIN gateways AS gateway ON gateway.id = operation.gateway_id
                WHERE operation.correlation_id = $1
                ORDER BY operation.created_at
                """,
                parsed_id,
            )
            # Neutral 404 for unknown or foreign resources; the client's account_id is never trusted.
            if (
                not children
                or context.account_id is None
                or any(
                    child["account_id"] is None or child["account_id"] != context.account_id
                    for child in children
                )
            ):
                raise ApiError("NOT_FOUND", http=404, request_id=request_id)
            per_node = []
            states: list[str] = []
            observation_pairs: list[tuple[asyncpg.Record, asyncpg.Record, dict[str, Any]]] = []
            for child in children:
                grant = None
                if child["binding_id"] is not None and child["gateway_id"] is not None:
                    grant = await connection.fetchrow(
                        "SELECT * FROM grants WHERE binding_id = $1 AND gateway_id = $2",
                        child["binding_id"],
                        child["gateway_id"],
                    )
                payload = _operation_payload(child)
                state, _ = _operation_state(child, grant, payload)
                states.append(state)
                if grant is not None and child["gateway_key"]:
                    per_node.append(_node_entry(child, grant, state))
                if grant is not None:
                    observation_pairs.append((child, grant, payload))
        state, application_state = _aggregate_states(states)
        body = {
            "request_id": request_id,
            "server_time": rfc3339(now_utc()),
            "schema_version": SCHEMA_VERSION,
            "status": "ok",
            "operation_id": str(parsed_id),
            "operation_type": "access.sync",
            "state": state,
            "access_application_state": application_state,
            "per_node": per_node,
        }
        if observe:
            body["sync_recovery"] = await _sync_recovery_observation(
                connection,
                context,
                _data_subject(context),
                observation_pairs,
                now_utc(),
            )
        return body

OBSERVE_SYNC_RECOVERY = "sync_recovery"


def _recovery_required(
    subject: tuple[str, Any] | None,
    operation: asyncpg.Record | None,
    grant: asyncpg.Record | None,
    payload: dict[str, Any],
    evaluated_at: datetime,
) -> bool:
    """True only for the deployed recoverable expired failed apply (no blanket dead rule)."""
    from .mobile_catalog import _recoverable_expired_apply_failure

    if subject is None or operation is None or grant is None:
        return False
    return _recoverable_expired_apply_failure(
        {
            "gateway_row_id": grant["gateway_id"],
            "grant_binding_id": grant["binding_id"],
            "grant_state": grant["state"],
            "not_after": grant["not_after"],
            "desired_generation": grant["desired_generation"],
            "op_type": operation["operation_type"],
            "op_status": operation["status"],
            "op_gateway_id": operation["gateway_id"],
            "op_binding_id": operation["binding_id"],
            "op_target": operation["target_revision"],
            "op_grant_id": (
                str(payload.get("grant_id")) if payload.get("grant_id") is not None else None
            ),
            "op_generation": (
                str(payload.get("generation"))
                if payload.get("generation") is not None
                else None
            ),
            "op_action": payload.get("action"),
            "opaque_id": grant["opaque_id"],
        },
        subject,
        evaluated_at,
    )


async def _sync_recovery_observation(
    connection: asyncpg.Connection,
    context: SessionContext,
    subject: tuple[str, Any] | None,
    pairs: list[tuple[asyncpg.Record, asyncpg.Record, dict[str, Any]]],
    evaluated_at: datetime,
) -> dict[str, Any]:
    """Opt-in additive marker with trustworthy revisions; never a blanket dead signal."""
    from .mobile_catalog import _admission_fence, _subject_catalog_revision

    required = any(
        _recovery_required(subject, operation, grant, payload, evaluated_at)
        for operation, grant, payload in pairs
    )
    revisions = None
    if subject is not None:
        revisions = {
            "catalog_revision": str(
                await _subject_catalog_revision(connection, context, evaluated_at, subject)
            ),
            "binding_revision": str(_admission_fence(context, subject)),
        }
    return {"required": required, **(revisions or {})}


def _node_entry(operation: asyncpg.Record, grant: asyncpg.Record, state: str) -> dict[str, Any]:
    return {
        "gateway_id": operation["gateway_key"],
        "state": state,
        "applied_generation": (
            str(grant["applied_generation"]) if grant["applied_generation"] is not None else None
        ),
        "not_after": rfc3339(grant["not_after"]),
        "last_error": ((operation["last_error"] or "")[:64] or None),
        "attempts": int(operation["attempts"]),
    }


def _grant_node_state(grant: asyncpg.Record | None) -> tuple[str, str]:
    """Progress of a target whose execution row is absent: readback of its own grant."""
    if grant is None:
        return "queued", "pending"
    if (
        grant["state"] == "applied"
        and grant["applied_generation"] is not None
        and grant["desired_generation"] is not None
        and int(grant["applied_generation"]) == int(grant["desired_generation"])
    ):
        return "applied", "applied"
    if grant["state"] == "revoked":
        readback = grant["last_readback"]
        if isinstance(readback, str):
            try:
                readback = json.loads(readback)
            except ValueError:
                readback = None
        confirmed = (
            grant["applied_generation"] is not None
            and grant["desired_generation"] is not None
            and int(grant["applied_generation"]) == int(grant["desired_generation"])
            and isinstance(readback, dict)
            and readback.get("revoked") is True
        )
        # A desired revoke is not an actual revoke: never report it as confirmed/removed.
        return ("rejected", "rejected") if confirmed else ("retryable_failure", "retryable_failure")
    if grant["state"] == "failed":
        return "retryable_failure", "retryable_failure"
    if grant["state"] == "applying":
        return "applying", "pending"
    return "queued", "pending"


async def _sync_parent_progress(
    connection: asyncpg.Connection,
    parent: asyncpg.Record,
    request_id: str,
    context: SessionContext,
    *,
    observe: bool = False,
) -> dict[str, Any]:
    """Resolve an access.sync parent operation from its target snapshot + live grants.

    The target snapshot is durable even when the sync created zero new gateway children
    (all targets already applied); progress is never credited from an empty aggregate.
    """
    payload = _operation_payload(parent)
    targets = [item for item in (payload.get("targets") or []) if isinstance(item, str)]
    children = await connection.fetch(
        """
        SELECT operation.*, gateway.gateway_key
        FROM outbox_operations AS operation
        LEFT JOIN gateways AS gateway ON gateway.id = operation.gateway_id
        WHERE operation.correlation_id = $1 AND operation.id <> $1
        ORDER BY operation.created_at
        """,
        parent["id"],
    )
    hour_id = None
    if parent["binding_id"] is None:
        hour_id = _active_hour_id(context)
    children_by_key = {
        child["gateway_key"]: child
        for child in children
        if child["gateway_key"]
        and (
            hour_id is None
            or (
                child["account_id"] is None
                and child["binding_id"] is None
                and str(_operation_payload(child).get("hour_entitlement_id")) == str(hour_id)
            )
        )
    }
    if hour_id is None:
        grants = await connection.fetch(
            """
            SELECT g.*, gateway.gateway_key FROM grants AS g
            LEFT JOIN gateways AS gateway ON gateway.id = g.gateway_id
            WHERE g.binding_id = $1
            """,
            parent["binding_id"],
        )
    else:
        grants = await connection.fetch(
            """
            SELECT g.*, gateway.gateway_key FROM grants AS g
            LEFT JOIN gateways AS gateway ON gateway.id = g.gateway_id
            WHERE g.hour_entitlement_id = $1 AND g.installation_id = $2
            """,
            hour_id,
            context.installation_id,
        )
    grants_by_key = {grant["gateway_key"]: grant for grant in grants}
    states: list[str] = []
    per_node: list[dict[str, Any]] = []
    observation_pairs: list[tuple[asyncpg.Record, asyncpg.Record, dict[str, Any]]] = []
    for gateway_key in targets:
        grant = grants_by_key.get(gateway_key)
        child = children_by_key.get(gateway_key)
        if child is not None:
            state, _ = _operation_state(child, grant, _operation_payload(child))
            last_error = (child["last_error"] or "")[:64] or None
            attempts = int(child["attempts"])
        else:
            state, _ = _grant_node_state(grant)
            last_error = None
            attempts = 0
        states.append(state)
        if child is not None and grant is not None:
            observation_pairs.append((child, grant, _operation_payload(child)))
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
    state, application_state = _aggregate_states(states)
    body = {
        "request_id": request_id,
        "server_time": rfc3339(now_utc()),
        "schema_version": SCHEMA_VERSION,
        "status": "ok",
        "operation_id": str(parent["id"]),
        "operation_type": parent["operation_type"],
        "state": state,
        "access_application_state": application_state,
        "per_node": per_node,
    }
    if observe:
        from .mobile_catalog import _data_subject

        body["sync_recovery"] = await _sync_recovery_observation(
            connection, context, _data_subject(context), observation_pairs, now_utc()
        )
    return body


def _aggregate_states(states: list[str]) -> tuple[str, str]:
    if not states:
        return "queued", "pending"
    if "rejected" in states:
        return "rejected", "rejected"
    if all(item == "applied" for item in states):
        return "applied", "applied"
    if "retryable_failure" in states:
        return "retryable_failure", "retryable_failure"
    if "applying" in states:
        return "applying", "pending"
    return "queued", "pending"


ACCOUNT_SERVICE_KEY: web.AppKey = web.AppKey("account_service", AccountService)


async def _handle_me(request: web.Request) -> web.Response:
    fallback_id = request_id_for(request)
    try:
        return web.json_response(await request.app[ACCOUNT_SERVICE_KEY].get_me(request))
    except AuthError as error:
        return _error_response(
            fallback_id,
            ApiError(error.code, http=error.http, retryable=error.retryable, request_id=fallback_id),
        )
    except ApiError as error:
        return _error_response(fallback_id, error)
    except (asyncpg.PostgresError, OSError):
        logger.exception("me failed")
        return _error_response(fallback_id, ApiError("SERVICE_UNAVAILABLE", request_id=fallback_id))


async def _handle_operation(request: web.Request) -> web.Response:
    fallback_id = request_id_for(request)
    try:
        operation_id = request.match_info.get("id", "")
        return web.json_response(
            await request.app[ACCOUNT_SERVICE_KEY].get_operation(request, operation_id)
        )
    except AuthError as error:
        return _error_response(
            fallback_id,
            ApiError(error.code, http=error.http, retryable=error.retryable, request_id=fallback_id),
        )
    except ApiError as error:
        return _error_response(fallback_id, error)
    except (asyncpg.PostgresError, OSError):
        logger.exception("operation status failed")
        return _error_response(fallback_id, ApiError("SERVICE_UNAVAILABLE", request_id=fallback_id))


async def _handle_usage(request: web.Request) -> web.Response:
    fallback_id = request_id_for(request)
    try:
        return web.json_response(await request.app[ACCOUNT_SERVICE_KEY].get_usage(request))
    except AuthError as error:
        return _error_response(
            fallback_id,
            ApiError(error.code, http=error.http, retryable=error.retryable, request_id=fallback_id),
        )
    except ApiError as error:
        return _error_response(fallback_id, error)
    except (asyncpg.PostgresError, OSError):
        logger.exception("usage failed")
        return _error_response(fallback_id, ApiError("SERVICE_UNAVAILABLE", request_id=fallback_id))


def register_account_routes(app: web.Application) -> None:
    app.router.add_get("/api/mobile/v1/me", _handle_me)
    app.router.add_get("/api/mobile/v1/usage", _handle_usage)
    app.router.add_get("/api/mobile/v1/operations/{id}", _handle_operation)
