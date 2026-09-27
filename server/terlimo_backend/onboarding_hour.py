"""Durable one-time onboarding hour storage and admission state machine (slice A).

Design basis: docs/proposals/STEP036_ONBOARDING_HOUR_DELTA_V1.md (revision 4). This module
contains storage/service logic only: no public routes, no listeners, no node/phone calls. The
authenticated transport boundary (gateway certificate -> registry -> GatewayContext) is a future
slice; callers pass an already-verified typed context, never a caller-declared trust flag, and
the context is still re-validated against the gateway registry here.

Contract notes for the route slice: `create_intent` repeat returns the stored record (a `ready`
record is delivered with the secret by `intent_response`, not by `create_intent`); a provisioning
failure keeps the intent `pending` (retriable until the bounded TTL) - the terminal `failed`
transition and its reconcile belong to the worker slice and are not claimed here.

Residual prerequisite (not closed): DB-side credential locks serialize terminal transitions with
in-flight handler RPCs, but they cannot cancel an RPC already sent to the node. If the process
dies with an unknown remote outcome, the credential may exist until its own node TTL and a later
revoke may observe NOT_FOUND. TEST compensation for unknown/absent revoke tombstones (and remote
compensation before live) is required separately and is not implemented here.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import secrets
import uuid
from contextlib import asynccontextmanager
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from typing import Any, Protocol

import asyncpg
from cryptography.fernet import Fernet, InvalidToken

from terlimo_backend.gateway_control import _enqueue

HOUR_SECONDS = 3600
INTENT_TTL_MAX_SECONDS = 900
HOUR_KIND = "onboarding_hour"
BOOTSTRAP_PROVISION_OPERATION = "gateway.bootstrap_provision"
BOOTSTRAP_REVOKE_OPERATION = "gateway.bootstrap_revoke"
ONBOARDING_SCOPE = "onboarding:start"
ONBOARDING_INTENT_OP = "onboarding.intent"
ONBOARDING_START_OP = "onboarding.start"
INTENT_CHALLENGE_PURPOSE = "onboarding-start-intent"
START_CHALLENGE_PURPOSE = "onboarding-start"
logger = logging.getLogger(__name__)

_CREDENTIAL_NAMESPACE = uuid.uuid5(uuid.NAMESPACE_URL, "terlimo/onboarding/hour")
_ACTIVE_STATES = ("pending", "ready")
_TERMINAL_STATES = ("failed", "expired", "revoked")


class OnboardingError(RuntimeError):
    """Bounded service error; carries only a code and optional retry hint."""

    def __init__(self, code: str, *, http: int = 403, retry_after: int | None = None) -> None:
        super().__init__(code)
        self.code = code
        self.http = http
        self.retry_after = retry_after


@dataclass(frozen=True)
class GatewayContext:
    """Transport-authenticated gateway identity (future mTLS adapter produces this)."""

    gateway_id: uuid.UUID
    gateway_key: str
    environment: str


class SecretCipher(Protocol):
    def encrypt(self, plaintext: str) -> bytes: ...

    def decrypt(self, ciphertext: bytes) -> str: ...


class UnavailableSecretCipher:
    """Fail-closed placeholder when no protected runtime key is configured."""

    def encrypt(self, plaintext: str) -> bytes:
        raise RuntimeError("secret cipher provider is not configured")

    def decrypt(self, ciphertext: bytes) -> str:
        raise RuntimeError("secret cipher provider is not configured")


class FernetSecretCipher:
    """Symmetric cipher over the protected runtime key (cryptography library, no new deps)."""

    def __init__(self, key: str) -> None:
        try:
            self._fernet = Fernet(key.encode("utf-8") if isinstance(key, str) else key)
        except (ValueError, TypeError) as exc:
            raise ValueError("invalid onboarding secret key") from exc

    def encrypt(self, plaintext: str) -> bytes:
        return self._fernet.encrypt(plaintext.encode("utf-8"))

    def decrypt(self, ciphertext: bytes) -> str:
        try:
            return self._fernet.decrypt(bytes(ciphertext)).decode("utf-8")
        except InvalidToken as exc:
            raise RuntimeError("secret ciphertext is invalid") from exc


def build_secret_cipher(settings: Any) -> SecretCipher:
    """Fernet from the protected runtime config; missing/blank key -> fail-closed placeholder."""
    key = getattr(settings, "onboarding_secret_key", "") or ""
    if not str(key).strip():
        return UnavailableSecretCipher()
    return FernetSecretCipher(str(key).strip())


class BootstrapGatewayClient(Protocol):
    async def bootstrap_provision(
        self, *, credential_id: str, secret: str, expires_at: datetime, node_id: str
    ) -> dict[str, Any]: ...

    async def bootstrap_revoke(self, *, credential_id: str) -> None: ...


def _now(now: datetime | None) -> datetime:
    return now or datetime.now(UTC)


def _utc(value: datetime | None) -> datetime | None:
    if value is None:
        return None
    return value.replace(tzinfo=UTC) if value.tzinfo is None else value.astimezone(UTC)


def _secret_hash(secret: str) -> str:
    return hashlib.sha256(secret.encode("utf-8")).hexdigest()


def _connection_hash(connection_id: str) -> str:
    return hashlib.sha256(connection_id.encode("utf-8")).hexdigest()


def _endpoints(row: asyncpg.Record) -> dict[str, Any]:
    value = row["endpoints"]
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except ValueError:
            return {}
    return value if isinstance(value, dict) else {}


def _gateway_ready(row: asyncpg.Record | None, environment: str) -> bool:
    """Minimal registry gate shared by intent creation and evidence admission."""
    if row is None or row["registry_state"] != "registered" or row["environment"] != environment:
        return False
    endpoints = _endpoints(row)
    if endpoints.get("node_id") != row["gateway_key"]:
        return False
    target = endpoints.get("target_workers")
    if type(target) is not int or row["confirmed_max_workers"] != target:
        return False
    if not isinstance(endpoints.get("peer_ip"), str) or not endpoints["peer_ip"]:
        return False
    if type(endpoints.get("dtls_port")) is not int or type(endpoints.get("wg_port")) is not int:
        return False
    return isinstance(endpoints.get("dtls_spki_sha256"), str) and bool(endpoints["dtls_spki_sha256"])


async def select_gateway(
    connection: asyncpg.Connection, environment: str, gateway_id: uuid.UUID | None = None
) -> asyncpg.Record:
    rows = await connection.fetch(
        "SELECT * FROM gateways WHERE environment = $1 ORDER BY gateway_key", environment
    )
    for row in rows:
        if gateway_id is not None and row["id"] != gateway_id:
            continue
        if _gateway_ready(row, environment):
            return row
    raise OnboardingError("ONBOARDING_GATEWAY_NOT_READY", http=503, retry_after=30)


def intent_payload(row: asyncpg.Record) -> dict[str, Any]:
    """State and deadlines only; the secret is added by :func:`intent_response` when ready."""
    return {
        "id": str(row["id"]),
        "installation_id": str(row["installation_id"]),
        "gateway_id": str(row["gateway_id"]),
        "environment": row["environment"],
        "state": row["state"],
        "unit_epoch": int(row["unit_epoch"]),
        "credential_id": row["credential_id"],
        "expires_at": _utc(row["expires_at"]).isoformat(),
        "started_at": _utc(row["started_at"]).isoformat() if row["started_at"] else None,
        "not_after": _utc(row["hour_not_after"]).isoformat() if row["hour_not_after"] else None,
        "failed_reason": row["failed_reason"],
    }


async def _active_commercial(connection: asyncpg.Connection, installation_id: uuid.UUID) -> bool:
    """Active paid/trial/imported right for the installation's active binding (paid is king).

    Unified lock protocol (single primitive): commercial writers and hour start/create both take
    the installation row FOR UPDATE first (callers of this helper already hold it via
    `_lock_installation`). The entitlement rows are read live (indefinite ends allowed).

    Integration prerequisite (not claimed closed here): the activation/payment route writer does
    not exist in this backend yet (`/trial`, checkout are open seams); until it adopts the same
    installation-row lock for its writes, a concurrent activation is not serialized by this
    module and the race stays open by design.
    """
    binding = await connection.fetchrow(
        "SELECT account_id FROM account_bindings WHERE installation_id = $1 AND status = 'active'",
        installation_id,
    )
    if binding is None or binding["account_id"] is None:
        return False
    found = await connection.fetchval(
        """
        SELECT 1 FROM entitlements
        WHERE account_id = $1 AND kind IN ('trial', 'paid', 'imported') AND status = 'active'
          AND (starts_at IS NULL OR starts_at <= now())
          AND (ends_at IS NULL OR ends_at > now())
        LIMIT 1
        """,
        binding["account_id"],
    )
    return found is not None


def _credential_lock_key(credential_id: str) -> str:
    return f"onboarding-credential:{credential_id}"


CREDENTIAL_LOCK_WAIT_SECONDS = 5.0
CREDENTIAL_LOCK_CLEANUP_SECONDS = 2.0


async def _poison_connection(connection: asyncpg.Connection) -> None:
    """Never return a possibly-locked connection to the pool: hard-terminate it."""
    terminate = getattr(connection, "terminate", None)
    try:
        if callable(terminate):
            terminate()
        else:  # pragma: no cover - defensive for non-proxy connections
            await connection.close()
    except Exception:
        logger.debug("failed to terminate a possibly locked connection", exc_info=True)


async def _await_cleanup_bounded(cleanup_task: asyncio.Task[None]) -> bool:
    """Await cleanup while preserving cancellation; bounded, then poison the connection."""
    try:
        await asyncio.shield(cleanup_task)
        return True
    except asyncio.CancelledError:
        for _ in range(4):
            try:
                await asyncio.wait_for(asyncio.shield(cleanup_task), timeout=CREDENTIAL_LOCK_CLEANUP_SECONDS)
                return True
            except asyncio.CancelledError:
                continue
            except (TimeoutError, Exception):  # noqa: BLE001
                return False
        return False
    except Exception:  # noqa: BLE001
        return False


@asynccontextmanager
async def _credential_lock(
    connection: asyncpg.Connection,
    credential_id: str,
    *,
    wait_seconds: float = CREDENTIAL_LOCK_WAIT_SECONDS,
):
    """Ownership-aware advisory lock held across the actual RPC lifecycle.

    Acquisition uses bounded ``pg_try_advisory_lock`` retries; a busy credential fails with a
    retryable error instead of blocking a pooled connection indefinitely. Cancellation at any
    point (including ambiguous acquire/unlock outcomes) poisons the connection so a possibly
    locked session never returns to the pool; the original error/cancellation is preserved.

    Honest limit: this serializes DB-side terminal transitions with the handler's RPC; it does
    NOT cancel an already-sent remote RPC. Session loss with an unknown remote outcome remains a
    residual prerequisite (see module notes) and is not claimed closed.
    """
    key = _credential_lock_key(credential_id)
    acquired = False
    uncertain = False
    loop = asyncio.get_running_loop()
    deadline = loop.time() + wait_seconds
    try:
        while True:
            remaining = deadline - loop.time()
            if remaining <= 0:
                raise OnboardingError("ONBOARDING_CREDENTIAL_BUSY", http=503, retry_after=5)
            acquire_task = asyncio.ensure_future(
                connection.fetchval(
                    "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", key
                )
            )
            try:
                got = await asyncio.wait_for(
                    asyncio.shield(acquire_task), timeout=remaining
                )
            except TimeoutError:
                uncertain = True
                await _await_cleanup_bounded(acquire_task)
                raise OnboardingError("ONBOARDING_CREDENTIAL_BUSY", http=503, retry_after=5)
            except asyncio.CancelledError:
                # outcome unknown: wait bounded for the attempt, remember success if it landed
                completed = await _await_cleanup_bounded(acquire_task)
                if completed and not acquire_task.cancelled() and acquire_task.exception() is None:
                    acquired = bool(acquire_task.result())
                else:
                    uncertain = True
                raise
            if got:
                acquired = True
                break
            await asyncio.sleep(0.05)
        yield
    finally:
        async def _cleanup() -> None:
            nonlocal uncertain
            if acquired:
                try:
                    released = await asyncio.wait_for(
                        connection.fetchval(
                            "SELECT pg_advisory_unlock(hashtextextended($1, 0))", key
                        ),
                        timeout=CREDENTIAL_LOCK_CLEANUP_SECONDS,
                    )
                    if released is not True:
                        uncertain = True
                except BaseException:  # noqa: BLE001 - unlock failed or unknown
                    uncertain = True
            if uncertain:
                await _poison_connection(connection)

        cleanup_task = asyncio.ensure_future(_cleanup())
        completed = await _await_cleanup_bounded(cleanup_task)
        if not completed:
            uncertain = True
            await _poison_connection(connection)


async def _expire_locked(connection: asyncpg.Connection, row: asyncpg.Record) -> None:
    """Active -> expired transition: wipe ciphertext and enqueue the exact targeted revoke."""
    await connection.execute(
        "UPDATE onboarding_intents SET state = 'expired', secret_enc = NULL WHERE id = $1",
        row["id"],
    )
    await _enqueue_revoke(connection, row)


async def _lock_installation(connection: asyncpg.Connection, installation_id: uuid.UUID) -> asyncpg.Record:
    row = await connection.fetchrow(
        "SELECT id, state FROM installations WHERE id = $1 FOR UPDATE", installation_id
    )
    if row is None:
        raise OnboardingError("INSTALLATION_UNKNOWN", http=403)
    if row["state"] == "revoked":
        raise OnboardingError("INSTALLATION_REVOKED", http=403)
    return row


async def create_intent(
    connection: asyncpg.Connection,
    *,
    installation_id: uuid.UUID,
    request_key: str,
    environment: str,
    cipher: SecretCipher,
    gateway_id: uuid.UUID | None = None,
    ttl_seconds: int = 600,
    now: datetime | None = None,
    correlation_id: Any = None,
) -> dict[str, Any]:
    """Create or return the installation-scoped intent for ``request_key`` (idempotent)."""
    if not isinstance(request_key, str) or not 1 <= len(request_key) <= 128:
        raise OnboardingError("BAD_MESSAGE", http=400)
    if type(ttl_seconds) is not int or not 1 <= ttl_seconds <= INTENT_TTL_MAX_SECONDS:
        raise OnboardingError("BAD_MESSAGE", http=400)
    moment = _now(now)
    async with connection.transaction():
        await _lock_installation(connection, installation_id)
        existing = await connection.fetchrow(
            "SELECT * FROM onboarding_intents WHERE installation_id = $1 AND request_key = $2",
            installation_id,
            request_key,
        )
        if existing is not None:
            if gateway_id is not None and existing["gateway_id"] != gateway_id:
                # Immutable binding under the same lock as read/create: a repeated request_key
                # can never silently return another gateway's intent.
                raise OnboardingError("ONBOARDING_INTENT_CONFLICT", http=409)
            return intent_payload(existing)
        unit_exists = await connection.fetchval(
            "SELECT 1 FROM entitlements WHERE kind = $1 AND installation_id = $2",
            HOUR_KIND,
            installation_id,
        )
        if unit_exists is not None:
            # The one-time installation unit is already consumed (any status): a new request key
            # never creates another intent/outbox/provision.
            raise OnboardingError("ONBOARDING_UNIT_STARTED", http=409)
        if await _active_commercial(connection, installation_id):
            # An active paid/trial right is authoritative: no hour is created or consumed.
            raise OnboardingError("ONBOARDING_SUBSCRIPTION_ACTIVE", http=409)
        active = await connection.fetchval(
            "SELECT id FROM onboarding_intents WHERE installation_id = $1 AND state = ANY($2::text[])",
            installation_id,
            list(_ACTIVE_STATES),
        )
        if active is not None:
            raise OnboardingError("ONBOARDING_UNIT_ACTIVE", http=409)
        gateway = await select_gateway(connection, environment, gateway_id)
        epoch = int(
            await connection.fetchval(
                "SELECT COALESCE(max(unit_epoch), 0) + 1 FROM onboarding_intents WHERE installation_id = $1",
                installation_id,
            )
        )
        intent_id = uuid.uuid4()
        credential_id = str(uuid.uuid5(_CREDENTIAL_NAMESPACE, f"hour:{installation_id}:{epoch}"))
        secret = secrets.token_urlsafe(32)
        expires_at = moment + timedelta(seconds=ttl_seconds)
        await connection.execute(
            """
            INSERT INTO onboarding_intents
                (id, installation_id, gateway_id, environment, unit_epoch, credential_id,
                 request_key, secret_hash, secret_enc, state, expires_at)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending', $10)
            """,
            intent_id,
            installation_id,
            gateway["id"],
            environment,
            epoch,
            credential_id,
            request_key,
            _secret_hash(secret),
            cipher.encrypt(secret),
            expires_at,
        )
        await _enqueue(
            connection,
            operation_type=BOOTSTRAP_PROVISION_OPERATION,
            idempotency_key=f"bootstrap:{credential_id}",
            payload={"intent_id": str(intent_id), "credential_id": credential_id},
            gateway_id=gateway["id"],
            target_revision=1,
            correlation_id=correlation_id,
        )
        row = await connection.fetchrow(
            "SELECT * FROM onboarding_intents WHERE id = $1", intent_id
        )
        return intent_payload(row)


async def intent_response(
    connection: asyncpg.Connection,
    *,
    installation_id: uuid.UUID,
    request_key: str,
    cipher: SecretCipher,
    expected_gateway_id: uuid.UUID | None = None,
    now: datetime | None = None,
) -> dict[str, Any]:
    """Bounded state contract for a repeated intent request (no new epoch ever)."""
    moment = _now(now)
    pending_error: OnboardingError | None = None
    result: dict[str, Any] | None = None
    async with connection.transaction():
        await _lock_installation(connection, installation_id)
        row = await connection.fetchrow(
            "SELECT * FROM onboarding_intents WHERE installation_id = $1 AND request_key = $2 FOR UPDATE",
            installation_id,
            request_key,
        )
        if row is None:
            raise OnboardingError("ONBOARDING_INTENT_UNKNOWN", http=404)
        if expected_gateway_id is not None and row["gateway_id"] != expected_gateway_id:
            # Compared under the same FOR UPDATE lock, before state/expiry reconcile or any
            # secret decryption: a repeated request_key never returns another gateway's intent.
            raise OnboardingError("ONBOARDING_INTENT_CONFLICT", http=409)
        state = row["state"]
        if state in _ACTIVE_STATES and _utc(row["expires_at"]) <= moment:
            # Expiry reconcile (wipe + targeted revoke) must COMMIT before the 410 is raised.
            await _expire_locked(connection, row)
            pending_error = OnboardingError("ONBOARDING_INTENT_EXPIRED", http=410)
            result = None
        else:
            payload = intent_payload(row)
            payload["state"] = state
            if state == "pending":
                # Explicit contract: the client retry cadence for a pending intent is 1s
                # (matching the worker provision poll); TTL/Connect/hour remain unchanged.
                payload["retry_after"] = 1
                result = payload
            elif state == "ready":
                payload["secret"] = cipher.decrypt(row["secret_enc"])
                result = payload
            elif state == "started":
                result = payload
            elif state == "failed":
                payload["reason"] = row["failed_reason"] or "provision_failed"
                result = payload
            elif state == "expired":
                pending_error = OnboardingError("ONBOARDING_INTENT_EXPIRED", http=410)
            else:
                raise OnboardingError("ONBOARDING_INTENT_REVOKED", http=403)
    if pending_error is not None:
        raise pending_error
    if result is None:
        raise OnboardingError("ONBOARDING_STATE_INVALID", http=500)
    return result


async def ensure_start_challenge(
    connection: asyncpg.Connection,
    *,
    intent_row: asyncpg.Record,
    ttl_seconds: int,
    now: datetime | None = None,
) -> dict[str, Any]:
    """One outstanding start challenge per intent; refresh supersedes the previous one.

    The unique index covers unused rows regardless of expiry, so an expired-but-unused row is
    taken out of the outstanding state atomically before a new one is inserted. Caller runs
    this inside a transaction.
    """
    moment = _now(now)
    existing = await connection.fetchrow(
        """
        SELECT challenge_id, nonce_b64, expires_at FROM auth_challenges
        WHERE purpose = $3 AND intent_id = $1 AND used_at IS NULL AND superseded_at IS NULL
          AND expires_at > $2
        ORDER BY created_at DESC LIMIT 1
        FOR UPDATE
        """,
        intent_row["id"],
        moment,
        START_CHALLENGE_PURPOSE,
    )
    if existing is not None:
        return {
            "challenge_id": existing["challenge_id"],
            "nonce_b64": existing["nonce_b64"],
            "expires_at": _utc(existing["expires_at"]).isoformat(),
        }
    await connection.execute(
        """
        UPDATE auth_challenges SET superseded_at = $2
        WHERE purpose = $3 AND intent_id = $1 AND used_at IS NULL AND superseded_at IS NULL
        """,
        intent_row["id"],
        moment,
        START_CHALLENGE_PURPOSE,
    )
    fingerprint = await connection.fetchval(
        "SELECT public_key_fingerprint FROM installations WHERE id = $1",
        intent_row["installation_id"],
    )
    expires_at = min(_utc(intent_row["expires_at"]), moment + timedelta(seconds=ttl_seconds))
    challenge_id = secrets.token_hex(16)
    nonce_b64 = secrets.token_urlsafe(32)
    await connection.execute(
        """
        INSERT INTO auth_challenges
            (challenge_id, nonce_b64, installation_fingerprint, purpose, op, environment,
             expires_at, intent_id)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
        """,
        challenge_id,
        nonce_b64,
        fingerprint,
        START_CHALLENGE_PURPOSE,
        ONBOARDING_START_OP,
        intent_row["environment"],
        expires_at,
        intent_row["id"],
    )
    return {
        "challenge_id": challenge_id,
        "nonce_b64": nonce_b64,
        "expires_at": _utc(expires_at).isoformat(),
    }


def _public_gateway(gateway_key: str | None, endpoints: Any) -> dict[str, Any]:
    if isinstance(endpoints, str):
        try:
            endpoints = json.loads(endpoints)
        except ValueError:
            endpoints = {}
    if not isinstance(endpoints, dict):
        endpoints = {}
    return {
        "node_id": gateway_key,
        "endpoint": {
            "peer_ip": endpoints.get("peer_ip"),
            "dtls_port": endpoints.get("dtls_port"),
            "dtls_spki_sha256": endpoints.get("dtls_spki_sha256"),
        },
    }


async def public_intent(
    connection: asyncpg.Connection,
    *,
    installation_id: uuid.UUID,
    request_key: str,
    environment: str,
    cipher: SecretCipher,
    challenge_ttl_seconds: int,
    gateway_key: str | None = None,
    now: datetime | None = None,
) -> dict[str, Any]:
    """Public connect-intent contract: create-or-return, never starts the hour.

    The final snapshot and the start-challenge issuance run under the canonical
    installation -> intent lock order (same as admission and the sweeps), and the row is
    re-read inside that transaction. A concurrent start/revoke/expiry between the first
    lookup and the lock can therefore never deliver a stale ready state or a stale decrypted
    secret, and concurrent first polls cannot both insert an outstanding challenge.
    """
    moment = _now(now)
    preferred_gateway_id: uuid.UUID | None = None
    if gateway_key is not None:
        if not isinstance(gateway_key, str) or not 1 <= len(gateway_key) <= 128:
            raise OnboardingError("ONBOARDING_GATEWAY_NOT_READY", http=503, retry_after=30)
        selected = await connection.fetchrow(
            "SELECT id FROM gateways WHERE gateway_key = $1 AND environment = $2",
            gateway_key,
            environment,
        )
        if selected is None:
            raise OnboardingError("ONBOARDING_GATEWAY_NOT_READY", http=503, retry_after=30)
        preferred_gateway_id = selected["id"]
    try:
        await intent_response(
            connection,
            installation_id=installation_id,
            request_key=request_key,
            cipher=cipher,
            expected_gateway_id=preferred_gateway_id,
            now=moment,
        )
    except OnboardingError as exc:
        if exc.code != "ONBOARDING_INTENT_UNKNOWN":
            raise
        await create_intent(
            connection,
            installation_id=installation_id,
            request_key=request_key,
            environment=environment,
            cipher=cipher,
            gateway_id=preferred_gateway_id,
            now=moment,
        )
    pending_error: OnboardingError | None = None
    result: dict[str, Any] | None = None
    async with connection.transaction():
        await _lock_installation(connection, installation_id)
        row = await connection.fetchrow(
            """
            SELECT * FROM onboarding_intents
            WHERE installation_id = $1 AND request_key = $2
            FOR UPDATE
            """,
            installation_id,
            request_key,
        )
        if row is None:
            raise OnboardingError("ONBOARDING_INTENT_UNKNOWN", http=404)
        if preferred_gateway_id is not None and row["gateway_id"] != preferred_gateway_id:
            # Defensive final check under the same installation/intent lock: never build a
            # result, decrypt a secret or reconcile expiry for another gateway.
            raise OnboardingError("ONBOARDING_INTENT_CONFLICT", http=409)
        current = _now(now)
        state = row["state"]
        if state in _ACTIVE_STATES and _utc(row["expires_at"]) <= current:
            await _expire_locked(connection, row)
            pending_error = OnboardingError("ONBOARDING_INTENT_EXPIRED", http=410)
        else:
            gateway = await connection.fetchrow(
                "SELECT gateway_key, endpoints FROM gateways WHERE id = $1", row["gateway_id"]
            )
            base: dict[str, Any] = {
                "status": state,
                "state": state,
                "intent_id": str(row["id"]),
                "request_key": request_key,
                "expires_at": _utc(row["expires_at"]).isoformat(),
                "gateway": _public_gateway(
                    gateway["gateway_key"] if gateway is not None else None,
                    gateway["endpoints"] if gateway is not None else None,
                ),
            }
            if state == "pending":
                # Explicit contract: pending retry cadence is 1s regardless of the remaining
                # TTL; the bootstrap budget on the client side is never squeezed by this hint.
                base["retry_after"] = 1
            elif state == "ready":
                base["credential_id"] = row["credential_id"]
                base["bootstrap"] = {
                    "credential_id": row["credential_id"],
                    "secret": cipher.decrypt(row["secret_enc"]),
                }
                base["start_challenge"] = await ensure_start_challenge(
                    connection, intent_row=row, ttl_seconds=challenge_ttl_seconds, now=current
                )
            elif state == "started":
                base["started_at"] = (
                    _utc(row["started_at"]).isoformat() if row["started_at"] else None
                )
                base["not_after"] = (
                    _utc(row["hour_not_after"]).isoformat() if row["hour_not_after"] else None
                )
            elif state == "failed":
                base["reason"] = row["failed_reason"] or "provision_failed"
            else:
                pending_error = OnboardingError("ONBOARDING_INTENT_REVOKED", http=403)
            result = base
    if pending_error is not None:
        raise pending_error
    if result is None:
        raise OnboardingError("ONBOARDING_STATE_INVALID", http=500)
    return result


async def _recorded_evidence(connection: asyncpg.Connection, credential_id: str) -> asyncpg.Record | None:
    return await connection.fetchrow(
        "SELECT * FROM onboarding_evidence WHERE credential_id = $1", credential_id
    )


async def admit_evidence(
    connection: asyncpg.Connection,
    *,
    caller: GatewayContext,
    credential_id: str,
    connection_id: str,
    request_id: str,
    evidence_digest: str | None = None,
    consume_challenge_id: str | None = None,
    proof_nonce: str | None = None,
    proof_ts_epoch: float | None = None,
    proof_skew_seconds: int | None = None,
    now: datetime | None = None,
) -> dict[str, Any]:
    """Authenticated-bound evidence admission; starts the hour exactly once.

    Order: (0) caller/registry validation (the typed context is re-checked against the gateway
    row, so a forged dataclass cannot reach the credential lookup), (1) credential lookup,
    (2) installation lock + intent binding, (3) authenticated replay, (4) under-lock start
    checks. An expired intent commits its expiry reconcile (wipe + targeted revoke) before the
    410 is raised, so a rejected start never rolls back the needed reconcile.
    """
    gateway_pre = await connection.fetchrow(
        "SELECT id, gateway_key, environment FROM gateways WHERE id = $1", caller.gateway_id
    )
    if (
        gateway_pre is None
        or gateway_pre["gateway_key"] != caller.gateway_key
        or gateway_pre["environment"] != caller.environment
    ):
        raise OnboardingError("ONBOARDING_ENV_MISMATCH", http=403)
    pre = await connection.fetchrow(
        "SELECT id, installation_id FROM onboarding_intents WHERE credential_id = $1", credential_id
    )
    if pre is None:
        raise OnboardingError("ONBOARDING_CREDENTIAL_UNKNOWN", http=403)
    pending_error: OnboardingError | None = None
    result: dict[str, Any] | None = None
    async with connection.transaction():
        await _lock_installation(connection, pre["installation_id"])
        row = await connection.fetchrow(
            "SELECT * FROM onboarding_intents WHERE id = $1 FOR UPDATE", pre["id"]
        )
        if row is None:
            raise OnboardingError("ONBOARDING_CREDENTIAL_UNKNOWN", http=403)
        gateway = await connection.fetchrow("SELECT * FROM gateways WHERE id = $1", row["gateway_id"])
        if (
            caller.gateway_id != row["gateway_id"]
            or caller.environment != row["environment"]
            or gateway is None
            or gateway["gateway_key"] != caller.gateway_key
        ):
            raise OnboardingError("ONBOARDING_ENV_MISMATCH", http=403)
        # Single post-lock admission moment for a NEW start: expiry checks, start timestamps
        # and the challenge freshness all use the time after the canonical locks were acquired.
        # An injected `now` keeps deterministic tests unchanged.
        moment = _now(now)
        message = json.dumps(
            {
                "op": "onboarding.evidence",
                "intent_id": str(row["id"]),
                "connection_id": connection_id,
                "request_id": request_id,
            },
            sort_keys=True,
            separators=(",", ":"),
        )
        recorded = await _recorded_evidence(connection, credential_id)
        if recorded is not None:
            # Authenticated replay of the started unit: historical start only, never new rights.
            # The durable admission identity is the stored business digest; a digest was never
            # invented for legacy rows, so replay for them fails closed.
            if evidence_digest is not None:
                if recorded["start_digest"] is None:
                    raise OnboardingError("ONBOARDING_REPLAY_UNAVAILABLE", http=409)
                if recorded["start_digest"] != evidence_digest:
                    raise OnboardingError("ONBOARDING_START_CONFLICT", http=409)
            return {
                "state": "started",
                "replay": True,
                "intent_id": str(row["id"]),
                "credential_id": credential_id,
                "started_at": _utc(row["started_at"]).isoformat(),
                "not_after": _utc(row["hour_not_after"]).isoformat(),
                "connection_id_hash": recorded["connection_id_hash"],
                "message_hash": hashlib.sha256(message.encode("utf-8")).hexdigest(),
            }
        state = row["state"]
        if state == "revoked":
            raise OnboardingError("ONBOARDING_INTENT_REVOKED", http=403)
        if state == "failed":
            raise OnboardingError("ONBOARDING_INTENT_FAILED", http=409)
        if state == "expired" or _utc(row["expires_at"]) <= moment:
            await _expire_locked(connection, row)
            pending_error = OnboardingError("ONBOARDING_INTENT_EXPIRED", http=410)
        elif state != "ready":
            raise OnboardingError("ONBOARDING_GATEWAY_NOT_READY", http=503, retry_after=10)
        else:
            started_unit = await connection.fetchval(
                "SELECT id FROM entitlements WHERE kind = $1 AND installation_id = $2",
                HOUR_KIND,
                row["installation_id"],
            )
            if started_unit is not None:
                raise OnboardingError("ONBOARDING_UNIT_STARTED", http=409)
            if not _gateway_ready(gateway, row["environment"]):
                raise OnboardingError("ONBOARDING_GATEWAY_NOT_READY", http=503, retry_after=30)
            if await _active_commercial(connection, row["installation_id"]):
                # A paid/trial right activated before the first start is authoritative; the hour
                # is not consumed and is not inserted.
                raise OnboardingError("ONBOARDING_SUBSCRIPTION_ACTIVE", http=409)
            hour_not_after = moment + timedelta(seconds=HOUR_SECONDS)
            if consume_challenge_id is not None:
                # Freshness for a NEW start only, evaluated under the same lock and at the
                # actual post-wait time; consumption is part of this transaction, so a rejected
                # start rolls the consumption back and nothing is partially consumed.
                if (
                    proof_ts_epoch is not None
                    and proof_skew_seconds is not None
                    and abs(proof_ts_epoch - moment.timestamp()) > proof_skew_seconds
                ):
                    raise OnboardingError("PROOF_INVALID", http=401)
                consumed = await connection.fetchval(
                    """
                    UPDATE auth_challenges SET used_at = $2
                    WHERE challenge_id = $1 AND purpose = $3 AND op = $4
                      AND intent_id = $5 AND environment = $6
                      AND nonce_b64 = $7
                      AND used_at IS NULL AND superseded_at IS NULL
                      AND expires_at > $2
                    RETURNING 1
                    """,
                    consume_challenge_id,
                    moment,
                    START_CHALLENGE_PURPOSE,
                    ONBOARDING_START_OP,
                    row["id"],
                    row["environment"],
                    proof_nonce,
                )
                if consumed is None:
                    raise OnboardingError("CHALLENGE_EXPIRED", http=401)
            await connection.execute(
                """
                INSERT INTO onboarding_evidence
                    (intent_id, credential_id, installation_id, gateway_id, environment,
                     connection_id_hash, request_id, first_seen_at, start_digest)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
                """,
                row["id"],
                credential_id,
                row["installation_id"],
                row["gateway_id"],
                row["environment"],
                _connection_hash(connection_id),
                request_id,
                moment,
                evidence_digest,
            )
            await connection.execute(
                """
                INSERT INTO entitlements
                    (account_id, installation_id, kind, status, starts_at, ends_at, device_limit,
                     revision, source_legacy_id)
                VALUES (NULL, $1, $2, 'active', $3, $4, 1, 1, $5)
                """,
                row["installation_id"],
                HOUR_KIND,
                moment,
                hour_not_after,
                f"onboarding-hour:{row['installation_id']}",
            )
            await connection.execute(
                """
                UPDATE onboarding_intents
                SET state = 'started', started_at = $2, hour_not_after = $3, secret_enc = NULL
                WHERE id = $1
                """,
                row["id"],
                moment,
                hour_not_after,
            )
            result = {
                "state": "started",
                "replay": False,
                "intent_id": str(row["id"]),
                "credential_id": credential_id,
                "started_at": moment.isoformat(),
                "not_after": hour_not_after.isoformat(),
                "message_hash": hashlib.sha256(message.encode("utf-8")).hexdigest(),
            }
    if pending_error is not None:
        raise pending_error
    if result is None:
        raise OnboardingError("ONBOARDING_STATE_INVALID", http=500)
    return result


async def revoke_intent(
    connection: asyncpg.Connection, *, intent_id: uuid.UUID, now: datetime | None = None
) -> dict[str, Any]:
    """Revoke an intent. Before start the unit is terminated; after start the historical hour and
    any data grant are preserved and only the bootstrap credential is revoked (node reconcile)."""
    moment = _now(now)
    async with connection.transaction():
        row = await connection.fetchrow(
            "SELECT * FROM onboarding_intents WHERE id = $1", intent_id
        )
        if row is None:
            raise OnboardingError("ONBOARDING_INTENT_UNKNOWN", http=404)
        await _lock_installation(connection, row["installation_id"])
        row = await connection.fetchrow(
            "SELECT * FROM onboarding_intents WHERE id = $1 FOR UPDATE", intent_id
        )
        if row["state"] == "started":
            # Accepted contract: after start the historical hour and any data grant are
            # preserved and only the bootstrap credential is revoked. Hour data-grant revocation
            # is a separate expiry/owner decision, never a silent change here.
            await _enqueue_revoke(connection, row)
            return {"revoked": False, "reason": "hour_started_credential_only"}
        if row["state"] in _TERMINAL_STATES:
            return {"revoked": False, "reason": f"already_{row['state']}"}
        await connection.execute(
            "UPDATE onboarding_intents SET state = 'revoked', revoked_at = $2, secret_enc = NULL WHERE id = $1",
            intent_id,
            moment,
        )
        await _enqueue_revoke(connection, row)
        return {"revoked": True, "reason": "intent_revoked"}


async def _enqueue_revoke(connection: asyncpg.Connection, row: asyncpg.Record) -> None:
    await _enqueue(
        connection,
        operation_type=BOOTSTRAP_REVOKE_OPERATION,
        idempotency_key=f"bootstrap-revoke:{row['credential_id']}",
        payload={"intent_id": str(row["id"]), "credential_id": row["credential_id"]},
        gateway_id=row["gateway_id"],
        target_revision=1,
    )


async def expire_hour_units(
    connection: asyncpg.Connection, *, now: datetime | None = None, limit: int = 50
) -> int:
    """Bounded recurring expiry writer for started hours: mark the entitlement expired and
    revoke exactly its own data grants (never a commercial subject). Idempotent: a repeated
    sweep sees status != active and enqueues nothing new; the hour deadline is never extended.
    """
    from .gateway_control import revoke_hour_grants

    moment = _now(now)
    if type(limit) is not int or limit < 1:
        raise OnboardingError("BAD_MESSAGE", http=400)
    async with connection.transaction():
        rows = await connection.fetch(
            """
            SELECT id, installation_id FROM entitlements
            WHERE kind = $1 AND status = 'active' AND ends_at IS NOT NULL AND ends_at <= $2
            ORDER BY ends_at, id
            LIMIT $3
            FOR UPDATE SKIP LOCKED
            """,
            HOUR_KIND,
            moment,
            limit,
        )
        expired = 0
        for row in rows:
            updated = await connection.execute(
                """
                UPDATE entitlements SET status = 'expired'
                WHERE id = $1 AND status = 'active'
                """,
                row["id"],
            )
            if updated != "UPDATE 1":
                continue
            await revoke_hour_grants(
                connection,
                installation_id=row["installation_id"],
                hour_entitlement_id=row["id"],
                now=moment,
            )
            expired += 1
        return expired


async def revoke_hour_unit(
    connection: asyncpg.Connection,
    *,
    installation_id: uuid.UUID,
    entitlement_id: uuid.UUID,
    now: datetime | None = None,
) -> bool:
    """Real entitlement revoke (not bootstrap-intent cleanup): mark the hour unit revoked and
    revoke exactly its own data grants. Idempotent; never touches a commercial subject.
    """
    from .gateway_control import revoke_hour_grants

    moment = _now(now)
    async with connection.transaction():
        updated = await connection.execute(
            """
            UPDATE entitlements SET status = 'revoked'
            WHERE id = $1 AND installation_id = $2 AND kind = $3 AND status = 'active'
            """,
            entitlement_id,
            installation_id,
            HOUR_KIND,
        )
        if updated != "UPDATE 1":
            return False
        await revoke_hour_grants(
            connection,
            installation_id=installation_id,
            hour_entitlement_id=entitlement_id,
            now=moment,
        )
        return True


SWEEP_EXPIRE = "expire_intents"
SWEEP_FAIL_EXHAUSTED = "fail_exhausted_intents"


async def _sweep_bind(connection: asyncpg.Connection, sweep: str) -> bool:
    """Try to own this sweep (bounded, multi-instance safe) and ensure its cursor row exists."""
    got = await connection.fetchval(
        "SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))", f"onboarding-sweep:{sweep}"
    )
    if got is not True:
        return False
    await connection.execute(
        "INSERT INTO onboarding_sweep_cursors (sweep) VALUES ($1) ON CONFLICT (sweep) DO NOTHING",
        sweep,
    )
    return True


async def _sweep_window(
    connection: asyncpg.Connection,
    sweep: str,
    *,
    base_query: str,
    params: tuple[Any, ...],
    window: int,
) -> tuple[list[asyncpg.Record], bool]:
    """Durable keyset window ordered by ``(expires_at, id)`` after the persisted cursor.

    Wraps to the beginning when nothing follows the cursor, so newly eligible rows with smaller
    keys are never skipped forever; the caller advances the cursor for every examined candidate
    (including busy/skipped ones). Bounded: at most ``window`` rows are read per call (constant
    factor of the batch limit), which is enough to step over a fully busy prefix in one sweep
    without turning the scan unbounded.
    """
    cursor = await connection.fetchrow(
        "SELECT last_expires_at, last_id FROM onboarding_sweep_cursors WHERE sweep = $1", sweep
    )
    last_expires = cursor["last_expires_at"] if cursor else None
    last_id = cursor["last_id"] if cursor else None
    base_params = len(params)
    p_expires = f"${base_params + 1}"
    p_id = f"${base_params + 2}"
    p_limit = f"${base_params + 3}"
    rows = await connection.fetch(
        base_query
        + f" AND ({p_expires}::timestamptz IS NULL"
        f" OR (i.expires_at, i.id) > ({p_expires}::timestamptz, {p_id}::uuid))"
        f" ORDER BY i.expires_at, i.id LIMIT {p_limit}",
        *params,
        last_expires,
        last_id,
        window,
    )
    if rows:
        return rows, False
    rows = await connection.fetch(
        base_query + f" ORDER BY i.expires_at, i.id LIMIT ${base_params + 1}",
        *params,
        window,
    )
    return rows, True


async def _sweep_cursor_advance(
    connection: asyncpg.Connection, sweep: str, row: asyncpg.Record
) -> None:
    await connection.execute(
        """
        UPDATE onboarding_sweep_cursors
        SET last_expires_at = $2, last_id = $3, updated_at = now()
        WHERE sweep = $1
        """,
        sweep,
        row["expires_at"],
        row["id"],
    )


async def expire_intents(
    connection: asyncpg.Connection, *, now: datetime | None = None, limit: int = 100
) -> int:
    """Bounded batch: stale active intents -> expired with wipe + targeted revoke.

    Fairness: a durable keyset cursor rotates through candidates, so a busy prefix cannot hide
    later free candidates; the cursor advances even for busy/skipped rows. Pure scheduling
    anchor: no rights are deleted.
    """
    moment = _now(now)
    if type(limit) is not int or limit < 1:
        raise OnboardingError("BAD_MESSAGE", http=400)
    window = limit * 8 + 1
    expired = 0
    async with connection.transaction():
        if not await _sweep_bind(connection, SWEEP_EXPIRE):
            return 0
        rows, _wrapped = await _sweep_window(
            connection,
            SWEEP_EXPIRE,
            base_query=(
                "SELECT i.id, i.installation_id, i.credential_id, i.expires_at"
                " FROM onboarding_intents AS i"
                " WHERE i.state = ANY('{pending,ready}') AND i.expires_at <= $1"
            ),
            params=(moment,),
            window=window,
        )
        if not rows:
            return 0
        examined: asyncpg.Record | None = None
        for row in rows:
            examined = row
            try:
                async with connection.transaction():
                    await connection.execute("SET LOCAL lock_timeout = '2000ms'")
                    got = await connection.fetchval(
                        "SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))",
                        _credential_lock_key(row["credential_id"]),
                    )
                    if got is not True:
                        continue  # busy with an in-flight RPC: skip, cursor still advances
                    await _lock_installation(connection, row["installation_id"])
                    current = await connection.fetchrow(
                        "SELECT * FROM onboarding_intents WHERE id = $1 FOR UPDATE", row["id"]
                    )
                    if current is None or current["state"] not in _ACTIVE_STATES:
                        continue
                    if _utc(current["expires_at"]) > moment:
                        continue
                    await _expire_locked(connection, current)
                    expired += 1
            except asyncpg.LockNotAvailableError:
                continue  # bounded installation/row wait: skip candidate
            if expired >= limit:
                break
        if examined is not None:
            await _sweep_cursor_advance(connection, SWEEP_EXPIRE, examined)
    return expired


async def fail_exhausted_intents(
    connection: asyncpg.Connection, *, limit: int = 100
) -> int:
    """Bounded batch: pending intents whose provision op is dead -> terminal failed + revoke.

    Candidate selection filters the real dead op rows; every candidate is re-checked under the
    transaction before any write. Same durable keyset fairness as :func:`expire_intents`.
    """
    moment = _now(None)
    if type(limit) is not int or limit < 1:
        raise OnboardingError("BAD_MESSAGE", http=400)
    window = limit * 8 + 1
    failed = 0
    async with connection.transaction():
        if not await _sweep_bind(connection, SWEEP_FAIL_EXHAUSTED):
            return 0
        rows, _wrapped = await _sweep_window(
            connection,
            SWEEP_FAIL_EXHAUSTED,
            base_query=(
                "SELECT i.id, i.installation_id, i.credential_id, i.expires_at"
                " FROM onboarding_intents AS i"
                " JOIN outbox_operations AS o"
                "   ON o.operation_type = $2 AND o.idempotency_key = 'bootstrap:' || i.credential_id"
                " WHERE i.state = 'pending' AND o.status = 'dead' AND $1::timestamptz IS NOT NULL"
            ),
            params=(moment, BOOTSTRAP_PROVISION_OPERATION),
            window=window,
        )
        if not rows:
            return 0
        examined: asyncpg.Record | None = None
        for row in rows:
            examined = row
            try:
                async with connection.transaction():
                    await connection.execute("SET LOCAL lock_timeout = '2000ms'")
                    got = await connection.fetchval(
                        "SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))",
                        _credential_lock_key(row["credential_id"]),
                    )
                    if got is not True:
                        continue
                    await _lock_installation(connection, row["installation_id"])
                    current = await connection.fetchrow(
                        "SELECT * FROM onboarding_intents WHERE id = $1 FOR UPDATE", row["id"]
                    )
                    if current is None or current["state"] != "pending":
                        continue
                    still_dead = await connection.fetchval(
                        """
                        SELECT status FROM outbox_operations
                        WHERE operation_type = $1 AND idempotency_key = 'bootstrap:' || $2
                        """,
                        BOOTSTRAP_PROVISION_OPERATION,
                        current["credential_id"],
                    )
                    if still_dead != "dead":
                        continue
                    await connection.execute(
                        """
                        UPDATE onboarding_intents
                        SET state = 'failed', failed_reason = 'provision_dead', secret_enc = NULL
                        WHERE id = $1
                        """,
                        current["id"],
                    )
                    await _enqueue_revoke(connection, current)
                    failed += 1
            except asyncpg.LockNotAvailableError:
                continue
            if failed >= limit:
                break
        if examined is not None:
            await _sweep_cursor_advance(connection, SWEEP_FAIL_EXHAUSTED, examined)
    return failed


async def revoke_backlog_stats(connection: asyncpg.Connection, *, limit: int = 100) -> dict[str, Any]:
    """Bounded, secret-free backlog signal for the sweep result/log.

    Counts at most ``limit`` dead revokes and flags truncation; the value is explicitly not a
    full count. The node-side credential TTL stays the last-resort guard.
    """
    if type(limit) is not int or limit < 1:
        raise OnboardingError("BAD_MESSAGE", http=400)
    rows = await connection.fetch(
        """
        SELECT id FROM outbox_operations
        WHERE operation_type = $1 AND status = 'dead'
        ORDER BY updated_at, id
        LIMIT $2
        """,
        BOOTSTRAP_REVOKE_OPERATION,
        limit + 1,
    )
    truncated = len(rows) > limit
    return {"count": min(len(rows), limit), "truncated": truncated, "limit": limit}


async def revoke_backlog(connection: asyncpg.Connection, *, limit: int = 100) -> list[dict[str, Any]]:
    """Observable remediation surface: dead revoke operations (no infinite requeue).

    A dead revoke stays dead (idempotent enqueue does not create a new attempt); operators see
    it here and the node-side credential TTL remains the last-resort guard.
    """
    rows = await connection.fetch(
        """
        SELECT o.idempotency_key, o.gateway_id, o.status, o.attempts, o.last_error,
               o.payload->>'credential_id' AS credential_id,
               o.payload->>'intent_id' AS intent_id
        FROM outbox_operations AS o
        WHERE o.operation_type = $1 AND o.status = 'dead'
        ORDER BY o.updated_at, o.id
        LIMIT $2
        """,
        BOOTSTRAP_REVOKE_OPERATION,
        limit,
    )
    return [dict(row) for row in rows]


class OnboardingHourHandlers:
    """Worker handlers for the bootstrap provision/revoke operations (no routes, no listeners)."""

    def __init__(self, *, cipher: SecretCipher, client_factory: Any) -> None:
        self._cipher = cipher
        self._client_factory = client_factory

    def as_handlers(self) -> dict[str, Any]:
        return {
            BOOTSTRAP_PROVISION_OPERATION: self.bootstrap_provision,
            BOOTSTRAP_REVOKE_OPERATION: self.bootstrap_revoke,
        }

    @staticmethod
    def _payload(operation: asyncpg.Record) -> dict[str, Any]:
        payload = operation["payload"]
        if isinstance(payload, str):
            payload = json.loads(payload)
        return payload if isinstance(payload, dict) else {}

    async def bootstrap_provision(self, connection: asyncpg.Connection, operation: asyncpg.Record) -> None:
        payload = self._payload(operation)
        intent_id = payload.get("intent_id")
        credential_id = payload.get("credential_id")
        if not isinstance(credential_id, str) or not credential_id:
            pre = await connection.fetchrow(
                "SELECT credential_id FROM onboarding_intents WHERE id = $1", intent_id
            )
            credential_id = pre["credential_id"] if pre is not None else None
        if not isinstance(credential_id, str) or not credential_id:
            return
        # Serialization: terminal transitions (sweeps) take the same credential lock and therefore
        # settle only after this RPC finishes. Honest limit: it does NOT cancel an already-sent
        # remote RPC; session loss with an unknown remote outcome stays a residual prerequisite
        # (see module docstring) and is not claimed closed.
        try:
            async with _credential_lock(connection, credential_id):
                async with connection.transaction():
                    await connection.execute("SET LOCAL lock_timeout = '2000ms'")
                    pre = await connection.fetchrow(
                        "SELECT id, installation_id FROM onboarding_intents WHERE id = $1", intent_id
                    )
                    if pre is None:
                        return
                    await _lock_installation(connection, pre["installation_id"])
                    row = await connection.fetchrow(
                        "SELECT * FROM onboarding_intents WHERE id = $1 FOR UPDATE", intent_id
                    )
                    if row["state"] != "pending":
                        if row["state"] in _TERMINAL_STATES:
                            # Late provisioning on a terminal intent never revives it; request a
                            # durable, targeted revoke of our own credential only (never delete
                            # foreign entries).
                            await _enqueue_revoke(connection, row)
                        return
                    snapshot = {
                        "id": row["id"],
                        "gateway_id": row["gateway_id"],
                        "unit_epoch": row["unit_epoch"],
                        "credential_id": row["credential_id"],
                        "secret": self._cipher.decrypt(row["secret_enc"]),
                        "expires_at": _utc(row["expires_at"]),
                    }
                gateway = await connection.fetchrow(
                    "SELECT * FROM gateways WHERE id = $1", snapshot["gateway_id"]
                )
                endpoints = _endpoints(gateway) if gateway is not None else {}
                node_id = endpoints.get("node_id")
                gateway_key = gateway["gateway_key"] if gateway is not None else ""
                client = self._client_factory(gateway_key, endpoints)
                await client.bootstrap_provision(
                    credential_id=snapshot["credential_id"],
                    secret=snapshot["secret"],
                    expires_at=snapshot["expires_at"],
                    node_id=str(node_id),
                )
                async with connection.transaction():
                    result = await connection.execute(
                        """
                        UPDATE onboarding_intents SET state = 'ready'
                        WHERE id = $1 AND state = 'pending' AND gateway_id = $2 AND unit_epoch = $3
                        """,
                        snapshot["id"],
                        snapshot["gateway_id"],
                        snapshot["unit_epoch"],
                    )
                    if result != "UPDATE 1":
                        current = await connection.fetchrow(
                            "SELECT * FROM onboarding_intents WHERE id = $1", snapshot["id"]
                        )
                        if current is not None and current["state"] in _TERMINAL_STATES:
                            await _enqueue_revoke(connection, current)
        except asyncpg.LockNotAvailableError as error:
            raise OnboardingError("ONBOARDING_BUSY", http=503, retry_after=5) from error

    async def bootstrap_revoke(self, connection: asyncpg.Connection, operation: asyncpg.Record) -> None:
        payload = self._payload(operation)
        credential_id = payload.get("credential_id")
        if not isinstance(credential_id, str) or not credential_id:
            return
        row = await connection.fetchrow(
            "SELECT gateway_id FROM onboarding_intents WHERE credential_id = $1", credential_id
        )
        gateway_id = row["gateway_id"] if row is not None else operation["gateway_id"]
        gateway = await connection.fetchrow("SELECT * FROM gateways WHERE id = $1", gateway_id)
        endpoints = _endpoints(gateway) if gateway is not None else {}
        gateway_key = gateway["gateway_key"] if gateway is not None else ""
        client = self._client_factory(gateway_key, endpoints)
        # Serialize with provision (and its retries) for the same credential.
        async with _credential_lock(connection, credential_id):
            await client.bootstrap_revoke(credential_id=credential_id)
