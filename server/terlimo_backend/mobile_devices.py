"""S5 §05.4–05.5 account devices: list and authorized targeted removal.

Management authorization (root decision 2026-09-27): a bound session manages the account's
bindings; an unbound installation session may manage devices ONLY through a fresh, server-confirmed
Telegram proof for the same installation/environment (latest registration_link, status confirmed,
owned by that Telegram account). No client-supplied account/telegram ids are ever trusted.
Removing a device revokes its binding, sessions and grants and invalidates every pending/confirmed
registration link of the removed installation (only when an active binding is actually revoked);
it does NOT set a permanent installation state.

Contract (09_CONTRACTS_AND_DATA §3/§5.5, 05_STEP §05.4–05.5):
  GET    /api/mobile/v1/devices           own bindings: id/name/platform/status/is_current/bound_at
  DELETE /api/mobile/v1/devices/{id}      revoke intent -> operation_id, slot freed; apply progress
                                          is reported separately (outbox/gateway readback)

Semantics implemented here on top of the existing account_bindings/sessions/grants/outbox models:
* deleting one binding is atomic: the binding row is locked and revoked, its sessions are revoked,
  every pending/confirmed registration link of that installation is expired, and a durable
  `gateway.revoke_grant` intent is enqueued for every non-revoked grant (no new source of business
  truth on the gateway); the installation keeps its current state (only an explicit admin action
  may set `revoked`);
* the other devices, the subscription end, trial history and payments are untouched;
* a repeat DELETE is idempotent (no new intents); a binding of another account is a neutral 404,
  so existence is never leaked;
* a late/delayed grant cannot resurrect access because the binding/grant generation is fenced; the
  commercial end_at is never changed.
"""

from __future__ import annotations

import json
import uuid
from datetime import UTC, datetime
from typing import Any

import asyncpg
from aiohttp import web

from .auth_api import SCHEMA_VERSION, ApiError, _error_response, random_hex, rfc3339
from .config import Settings
from .db import Database
from .gateway_control import REVOKE_OPERATION, revoke_binding_grants
from .mobile_account import _TERMINAL_FAILURES, effective_device_limit
from .session_auth import AuthError, authenticate_session

MOBILE_PREFIX = "/api/mobile/v1"
DEVICES_PATH = f"{MOBILE_PREFIX}/devices"
DEVICE_PATH = f"{MOBILE_PREFIX}/devices/{{binding_id}}"


def _envelope(payload: dict[str, Any]) -> web.Response:
    body = {
        "request_id": random_hex(16),
        "server_time": rfc3339(datetime.now(UTC)),
        "schema_version": SCHEMA_VERSION,
        "status": "ok",
    }
    body.update(payload)
    return web.json_response(body)


def _bearer(request: web.Request) -> str:
    header = request.headers.get("Authorization", "")
    if not header.startswith("Bearer "):
        raise AuthError("SESSION_INVALID", 401)
    return header[len("Bearer ") :].strip()


def _device_view(row: asyncpg.Record, current_binding_id) -> dict[str, Any]:
    """Canonical devices.json Device (additionalProperties=false).

    The platform value is passed through unchanged: enrollment (auth_api) already validates the
    only supported input model (platform == "android"), so no truth is substituted here.
    """
    return {
        "device_id": str(row["id"]),
        "name": row["name"],
        "platform": row["platform"],
        "is_current": current_binding_id is not None and str(row["id"]) == str(current_binding_id),
        "status": row["status"],
        "bound_at": rfc3339(row["bound_at"]),
        "revoked_at": rfc3339(row["revoked_at"]) if row["revoked_at"] is not None else None,
    }


async def _management_scope(connection, context, settings: Settings) -> dict[str, Any]:
    """Resolve the account a management request is allowed to operate on.

    Bound session -> its own account. Unbound installation -> only with the latest confirmed
    Telegram registration proof for that exact installation/environment; account is derived
    server-side from accounts.telegram_id, never from the request.
    """
    if context.account_id is not None:
        return {
            "account_id": context.account_id,
            "current_binding_id": context.binding["id"] if context.binding is not None else None,
            "proof": "binding",
        }
    link = await connection.fetchrow(
        """
        SELECT telegram_id, status FROM registration_links
        WHERE installation_id = $1 AND environment = $2
        ORDER BY created_at DESC, id DESC
        LIMIT 1
        """,
        context.installation_id,
        settings.environment,
    )
    account_id = None
    if link is not None and link["status"] == "confirmed" and link["telegram_id"] is not None:
        account_id = await connection.fetchval(
            "SELECT id FROM accounts WHERE telegram_id = $1", link["telegram_id"]
        )
    if account_id is None:
        # Explicit machine-readable removal status for the removed installation itself
        # (revoked binding history, no fresh confirmed proof); never leaks another account.
        history = await connection.fetchval(
            "SELECT 1 FROM account_bindings WHERE installation_id = $1 AND status = 'revoked' LIMIT 1",
            context.installation_id,
        )
        raise ApiError(
            "DEVICE_REMOVED" if history is not None else "DEVICE_MANAGEMENT_FORBIDDEN", http=403
        )
    return {"account_id": account_id, "current_binding_id": None, "proof": "registration_link"}


async def _revalidate_authorized(
    connection,
    settings: Settings,
    token: str,
    context,
    scope: dict[str, Any],
) -> None:
    """Common in-lock revalidation for both management scopes (root correction 2).

    Re-runs the full authenticate_session under the serialization lock and requires the same
    session subject (session id, installation, environment, account subject) plus, for a bound
    session, the same live binding identity/generation with status active.
    """
    fresh = await authenticate_session(
        connection, settings, token, required_scope="session:write"
    )
    if (
        fresh.session_id != context.session_id
        or fresh.installation_id != context.installation_id
        or fresh.environment != settings.environment
        or fresh.account_id != context.account_id
    ):
        raise AuthError("SESSION_INVALID", 401)
    if scope["proof"] == "binding":
        binding = fresh.binding
        if (
            binding is None
            or str(binding["id"]) != str(scope["current_binding_id"])
            or binding["status"] != "active"
            or int(binding["generation"]) != int(context.binding["generation"])
        ):
            raise AuthError("SESSION_INVALID", 401)


async def _locked_links(connection, installation_id, environment):
    """Lock the installation's pending/confirmed proof rows (order: installation -> links)."""
    return await connection.fetch(
        """
        SELECT id, status, telegram_id, installation_id FROM registration_links
        WHERE installation_id = $1 AND environment = $2 AND status IN ('pending', 'confirmed')
        FOR UPDATE
        """,
        installation_id,
        environment,
    )


async def _revalidate_link_proof(
    connection, installation_id, account_id, links
) -> None:
    """Re-derive the account from the locked confirmed proof and compare with the advisory key."""
    confirmed = [row for row in links if row["status"] == "confirmed" and row["telegram_id"] is not None]
    if not confirmed:
        history = await connection.fetchval(
            "SELECT 1 FROM account_bindings WHERE installation_id = $1 AND status = 'revoked' LIMIT 1",
            installation_id,
        )
        raise ApiError(
            "DEVICE_REMOVED" if history is not None else "DEVICE_MANAGEMENT_FORBIDDEN", http=403
        )
    telegram_ids = {row["telegram_id"] for row in confirmed}
    proof_accounts = await connection.fetch(
        "SELECT id FROM accounts WHERE telegram_id = ANY($1::bigint[])", list(telegram_ids)
    )
    if account_id not in {row["id"] for row in proof_accounts}:
        raise ApiError("DEVICE_MANAGEMENT_FORBIDDEN", http=403)


async def _revoke_summary(connection, binding_id: Any) -> tuple[str, str | None]:
    """Access application state of the CURRENT revoke for every current revoked grant.

    Workers finalize successful revoke operations as status='done' only after the gateway
    readback confirmed the revoke generation; 'applied' additionally requires the grant readback
    fence (applied_generation == desired_generation of the current revoke). Operation rows are
    matched to the CURRENT revocation epoch of each grant (target_revision == the grant's current
    desired_generation), so history from earlier delete/rebind cycles cannot pollute the summary.
    Returns (access_application_state, gateway_operation_id | None).
    """
    rows = await connection.fetch(
        """
        SELECT o.id, o.status, o.last_error, o.target_revision,
               g.applied_generation, g.desired_generation
        FROM grants AS g
        LEFT JOIN outbox_operations AS o
          ON o.binding_id = g.binding_id
         AND o.operation_type = $2
         AND o.target_revision = g.desired_generation
         AND o.gateway_id = g.gateway_id
         AND o.payload ->> 'grant_id' = g.opaque_id::text
        WHERE g.binding_id = $1 AND g.state = 'revoked'
        ORDER BY g.created_at, g.id
        """,
        binding_id,
        REVOKE_OPERATION,
    )
    states: list[str] = []
    current_ops: list[asyncpg.Record] = []
    for row in rows:
        if row["id"] is None:
            # A revoked grant without its revoke intent is not confirmed; stay pending.
            states.append("pending")
            continue
        current_ops.append(row)
        status = row["status"]
        if status == "done":
            applied = (
                row["applied_generation"] is not None
                and row["desired_generation"] is not None
                and int(row["applied_generation"]) == int(row["desired_generation"])
            )
            states.append("applied" if applied else "pending")
        elif status == "dead":
            states.append("rejected")
        elif status == "failed":
            states.append(
                "rejected" if (row["last_error"] or "") in _TERMINAL_FAILURES else "retryable_failure"
            )
        else:
            states.append("pending")
    if not rows:
        state = "not_requested"
    elif all(item == "applied" for item in states):
        state = "applied"
    elif any(item == "rejected" for item in states):
        state = "rejected"
    elif any(item == "retryable_failure" for item in states):
        state = "retryable_failure"
    else:
        state = "pending"
    operation_id = None
    if current_ops:
        chosen = max(current_ops, key=lambda row: (int(row["target_revision"]), str(row["id"])))
        operation_id = str(chosen["id"])
    return state, operation_id


async def _device_list_revision(connection, installation_id, account_id, fingerprint) -> int:
    """Stable devices-list revision in its own table, updated in one explicit transaction.

    Keyed by the authenticated installation; the stored fingerprint is the previous device-list
    snapshot, so one real bind/delete bumps the revision exactly once and concurrent reads cannot
    lose an increment (row lock inside the transaction, not an autocommit FOR UPDATE).
    """
    async with connection.transaction():
        row = await connection.fetchrow(
            "SELECT revision, fingerprint, account_id FROM device_list_revisions WHERE installation_id = $1 FOR UPDATE",
            installation_id,
        )
        if row is None:
            created = await connection.fetchval(
                """
                INSERT INTO device_list_revisions (installation_id, account_id, revision, fingerprint)
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
                "SELECT revision, fingerprint, account_id FROM device_list_revisions WHERE installation_id = $1 FOR UPDATE",
                installation_id,
            )
        if row["fingerprint"] != fingerprint or row["account_id"] != account_id:
            revision = int(row["revision"]) + 1
            await connection.execute(
                """
                UPDATE device_list_revisions
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


async def _device_list(connection, settings: Settings, account_id, current_binding_id):
    rows = await connection.fetch(
        """
        SELECT b.id, b.status, b.bound_at, b.revoked_at,
               i.name, i.platform
        FROM account_bindings AS b
        JOIN installations AS i ON i.id = b.installation_id
        WHERE b.account_id = $1
        ORDER BY b.bound_at DESC, b.id
        """,
        account_id,
    )
    slots_used = await _slots_used(connection, account_id)
    device_limit = await effective_device_limit(connection, account_id)
    return rows, slots_used, device_limit


async def _slots_used(connection, account_id) -> int:
    from .common_capacity import occupied_count
    return await occupied_count(connection, account_id)


def register_device_routes(app: web.Application, settings: Settings, database: Database) -> None:
    async def list_devices(request: web.Request) -> web.Response:
        fallback = random_hex(16)
        try:
            token = _bearer(request)
            async with database.acquire() as connection:
                context = await authenticate_session(
                    connection, settings, token, required_scope="session:read"
                )
                pre_scope = await _management_scope(connection, context, settings)
                async with connection.transaction():
                    # Same serialization point as bind/delete, taken BEFORE the snapshot is
                    # read: a stale GET that waited here can only observe the post-change state,
                    # so it can never write back an older fingerprint with a newer revision.
                    await connection.execute(
                        "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
                        f"bind-account:{pre_scope['account_id']}",
                    )
                    fresh = await authenticate_session(
                        connection, settings, token, required_scope="session:read"
                    )
                    if (
                        fresh.session_id != context.session_id
                        or fresh.installation_id != context.installation_id
                        or fresh.account_id != context.account_id
                    ):
                        raise AuthError("SESSION_INVALID", 401)
                    scope = await _management_scope(connection, fresh, settings)
                    if scope["account_id"] != pre_scope["account_id"]:
                        # The account subject changed while waiting: we hold the wrong advisory
                        # key, so report a retryable conflict instead of mixing metadata.
                        raise ApiError("REVISION_CONFLICT", http=409, retryable=True)
                    rows, slots_used, device_limit = await _device_list(
                        connection, settings, scope["account_id"], scope["current_binding_id"]
                    )
                    devices = [_device_view(row, scope["current_binding_id"]) for row in rows]
                    fingerprint = json.dumps(
                        {
                            "devices": devices,
                            "slots_used": slots_used,
                            "device_limit": device_limit,
                        },
                        sort_keys=True,
                        separators=(",", ":"),
                        ensure_ascii=False,
                    )
                    revision = await _device_list_revision(
                        connection,
                        fresh.installation_id,
                        scope["account_id"],
                        fingerprint,
                    )
            return _envelope({
                "devices": devices,
                "device_limit": device_limit,
                "slots_used": slots_used,
                "revision": str(revision),
            })
        except AuthError as error:
            return _error_response(fallback, ApiError(error.code, http=error.http,
                                                      retryable=error.retryable, request_id=fallback))
        except ApiError as error:
            return _error_response(fallback, error)

    async def delete_device(request: web.Request) -> web.Response:
        fallback = random_hex(16)
        try:
            token = _bearer(request)
            try:
                binding_id = uuid.UUID(request.match_info["binding_id"])
            except (ValueError, AttributeError):
                raise ApiError("DEVICE_NOT_FOUND", http=404) from None
            async with database.acquire() as connection:
                context = await authenticate_session(
                    connection, settings, token, required_scope="session:write"
                )
                scope = await _management_scope(connection, context, settings)
                target0 = await connection.fetchrow(
                    """
                    SELECT id, account_id, installation_id, status FROM account_bindings
                    WHERE id = $1 AND account_id = $2
                    """,
                    binding_id,
                    scope["account_id"],
                )
                # Foreign or unknown binding: the same neutral 404, never an existence leak.
                if target0 is None:
                    raise ApiError("DEVICE_NOT_FOUND", http=404)
                async with connection.transaction():
                    # Global lock order (root correction 2): account advisory -> target
                    # installation -> registration_links -> account_bindings. The same order
                    # is used by confirm (advisory -> installation -> link -> binding) and by
                    # link rotation (advisory -> installation -> links), so a cross-account
                    # confirm/delete on one installation can never invert the locks.
                    await connection.execute(
                        "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
                        f"bind-account:{scope['account_id']}",
                    )
                    await _revalidate_authorized(connection, settings, token, context, scope)
                    # Two installations may be involved (caller proof + target). They are always
                    # locked in ascending id order so two cross-deletes cannot invert them; the
                    # links of both are locked before any account_bindings row (global order:
                    # advisory -> installation(s) -> links -> bindings).
                    involved = {str(context.installation_id): context.installation_id,
                                str(target0["installation_id"]): target0["installation_id"]}
                    for key in sorted(involved):
                        installation = await connection.fetchrow(
                            "SELECT id, state FROM installations WHERE id = $1 FOR UPDATE",
                            involved[key],
                        )
                        if installation is not None and installation["state"] == "revoked":
                            raise AuthError("DEVICE_REVOKED", 403)
                    target_links = await _locked_links(
                        connection, target0["installation_id"], settings.environment
                    )
                    if scope["proof"] == "registration_link":
                        if str(target0["installation_id"]) == str(context.installation_id):
                            caller_links = target_links
                        else:
                            caller_links = await _locked_links(
                                connection, context.installation_id, settings.environment
                            )
                        await _revalidate_link_proof(
                            connection,
                            context.installation_id,
                            scope["account_id"],
                            caller_links,
                        )
                    binding = await connection.fetchrow(
                        """
                        SELECT id, account_id, installation_id, status
                        FROM account_bindings
                        WHERE id = $1 AND account_id = $2
                        FOR UPDATE
                        """,
                        binding_id,
                        scope["account_id"],
                    )
                    if binding is None:
                        raise ApiError("DEVICE_NOT_FOUND", http=404)
                    already_revoked = binding["status"] == "revoked"
                    if not already_revoked:
                        await revoke_binding_grants(connection, binding_id=binding_id)
                        await connection.execute(
                            """
                            UPDATE sessions
                            SET revoked_at = now(), generation = generation + 1
                            WHERE binding_id = $1 AND revoked_at IS NULL
                            """,
                            binding_id,
                        )
                        # A removed device must never manage or rebind other devices from a stale
                        # Telegram proof: invalidate every pending/confirmed link of the target.
                        # The rows are already locked above, so no new lock is taken here.
                        await connection.execute(
                            """
                            UPDATE registration_links SET status = 'expired'
                            WHERE installation_id = $1 AND status IN ('pending', 'confirmed')
                            """,
                            binding["installation_id"],
                        )
                    # already_revoked replay has no side effects at all: a historical binding must
                    # not cancel a newer pending proof of the same installation (possibly bound
                    # later by another account).
                    application_state, gateway_operation_id = await _revoke_summary(
                        connection, binding_id
                    )
                    generation = int(
                        await connection.fetchval(
                            "SELECT generation FROM account_bindings WHERE id = $1", binding_id
                        )
                    )
                operation_id = gateway_operation_id or f"local-removal:{binding_id}:g{generation}"
                return _envelope({
                    "status": "ok" if application_state in {"applied", "not_requested"} else "pending",
                    "operation_id": operation_id,
                    "slot_released": True,
                    "access_application_state": application_state,
                    "residual_access_lease_seconds": None,
                })
        except AuthError as error:
            return _error_response(fallback, ApiError(error.code, http=error.http,
                                                      retryable=error.retryable, request_id=fallback))
        except ApiError as error:
            return _error_response(fallback, error)

    app.router.add_get(DEVICES_PATH, list_devices)
    app.router.add_delete(DEVICE_PATH, delete_device)
