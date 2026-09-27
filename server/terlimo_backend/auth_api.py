"""Mobile API v1: installation enrollment and installation-scoped sessions (step 03.2).

Implements the accepted C01 contract (terlimo-s1-contracts @ 336bd6d):
- POST /api/mobile/v1/auth/challenge   -> single-use, key/purpose/environment-bound challenge
- POST /api/mobile/v1/installations    -> technical installation + scoped session (no entitlement)
- POST /api/mobile/v1/auth/session     -> fresh PoP proof -> scoped session

The PoP verifier is the ported contract implementation in `terlimo_backend.pop`
(see provenance there). No account, trial, onboarding hour, binding/slot or gateway
grant is created here. Secrets (session bearers) are never logged; sessions store only
the bearer's SHA-256, while the idempotency receipt keeps the exact response needed for
lost-response retries.
"""

from __future__ import annotations

import hashlib
import json
import logging
import secrets
from datetime import UTC, datetime
from typing import Any

import asyncpg
from aiohttp import web

from . import pop
from .config import Settings
from .db import Database

logger = logging.getLogger(__name__)

SCHEMA_VERSION = "1.0"
MOBILE_PREFIX = "/api/mobile/v1"
OP_ENROLLMENT = "installations.create"
OP_SESSION = "auth.session"
PURPOSE_BINDING = {
    "enrollment": ("enrollment", OP_ENROLLMENT),
    "session": ("session", OP_SESSION),
    "onboarding-start-intent": ("onboarding:start", "onboarding.intent"),
}
CHALLENGE_PURPOSES = (
    "enrollment",
    "session",
    "binding",
    "telegram-link",
    "catalog",
    "checkout",
    "onboarding-start-intent",
)
ENVIRONMENTS = ("test", "production")
PROOF_FIELDS = frozenset(
    {
        "algorithm",
        "signature_b64",
        "request_id",
        "challenge_id",
        "nonce_b64",
        "payload_hash",
        "signed_payload_b64",
        "environment",
    }
)
SESSION_SCOPES = frozenset(
    {
        "enrollment",
        "session:read",
        "session:write",
        "account:read",
        "devices:write",
        "access:sync",
        "payment:write",
        "management-only",
    }
)
UNLINKED_INSTALLATION_SCOPES = frozenset(
    {"enrollment", "session:read", "session:write", "management-only"}
)
# A proven trusted installation->account link may additionally request access:sync in 03.4.
# Future payment/admin scopes stay unavailable; a session scope is never itself a right.
LINKED_SESSION_SCOPES = UNLINKED_INSTALLATION_SCOPES | {"access:sync"}
_NOT_GIVEN = object()
MAX_REQUEST_BYTES = 64 * 1024

ERROR_HTTP = {
    "BAD_MESSAGE": 400,
    "UNSUPPORTED_VERSION": 400,
    "UNKNOWN_CRITICAL_FIELD": 400,
    "WRONG_ENVIRONMENT": 400,
    "WRONG_SCOPE": 400,
    "PROOF_INVALID": 401,
    "CHALLENGE_EXPIRED": 401,
    "CHALLENGE_REUSED": 401,
    "REPLAY_DETECTED": 401,
    "SESSION_INVALID": 401,
    "SESSION_EXPIRED": 401,
    "DEVICE_REVOKED": 403,
    "ACCESS_DENIED": 403,
    "IDEMPOTENCY_CONFLICT": 409,
    "RATE_LIMITED": 429,
    "SERVICE_UNAVAILABLE": 503,
    "INTERNAL": 500,
}
RETRYABLE_CODES = frozenset({"RATE_LIMITED", "SERVICE_UNAVAILABLE"})


class ApiError(Exception):
    def __init__(
        self,
        code: str,
        *,
        http: int | None = None,
        retryable: bool | None = None,
        retry_after_ms: int | None = None,
        details: dict[str, Any] | None = None,
        request_id: str | None = None,
    ) -> None:
        super().__init__(code)
        self.code = code
        self.http = http if http is not None else ERROR_HTTP.get(code, 400)
        self.retryable = retryable if retryable is not None else code in RETRYABLE_CODES
        self.retry_after_ms = retry_after_ms
        self.details = details
        self.request_id = request_id


def now_utc() -> datetime:
    return datetime.now(UTC)


def rfc3339(moment: datetime) -> str:
    return moment.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def random_hex(bytes_count: int) -> str:
    return secrets.token_hex(bytes_count)


def random_b64url(bytes_count: int) -> str:
    return pop.b64url_encode(secrets.token_bytes(bytes_count))


def sha256_hex_text(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def parse_utc(ts: str) -> float:
    if not isinstance(ts, str) or not ts.endswith("Z"):
        raise ApiError("PROOF_INVALID")
    try:
        return datetime.fromisoformat(ts).timestamp()
    except ValueError as exc:
        raise ApiError("PROOF_INVALID") from exc


def _require_object(body: Any, allowed: frozenset[str]) -> dict[str, Any]:
    if not isinstance(body, dict) or set(body) != set(allowed):
        raise ApiError("BAD_MESSAGE")
    return body


def _require_pattern(value: Any, pattern: str, code: str = "BAD_MESSAGE") -> str:
    if not isinstance(value, str) or not value:
        raise ApiError(code)
    import re

    if not re.fullmatch(pattern, value):
        raise ApiError(code)
    return value


def _require_int_bounded(value: Any, minimum: int, maximum: int) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or not minimum <= value <= maximum:
        raise ApiError("BAD_MESSAGE")
    return value


class InstallationSessionService:
    def __init__(self, settings: Settings, database: Database) -> None:
        self._settings = settings
        self._db = database

    # ---------------------------------------------------------------- challenge

    async def create_challenge(self, body: dict[str, Any]) -> dict[str, Any]:
        body = _require_object(
            body, frozenset({"installation_fingerprint", "purpose", "environment"})
        )
        fingerprint = _require_pattern(
            body.get("installation_fingerprint"), r"[0-9a-f]{64}"
        )
        purpose = _require_pattern(body.get("purpose"), r"[a-z-]+")
        environment = _require_pattern(body.get("environment"), r"[a-z]+")
        if purpose not in CHALLENGE_PURPOSES:
            raise ApiError("BAD_MESSAGE", details={"reason": "unknown_purpose"})
        if environment not in ENVIRONMENTS:
            raise ApiError("BAD_MESSAGE", details={"reason": "unknown_environment"})
        if purpose not in PURPOSE_BINDING:
            # Only purposes with a route in this step are issued; others arrive with them.
            raise ApiError("BAD_MESSAGE", details={"reason": "purpose_not_enabled"})
        op = PURPOSE_BINDING[purpose][1]
        request_id = random_hex(16)
        challenge_id = random_hex(16)
        nonce_b64 = random_b64url(32)
        expires_at = now_utc().timestamp() + self._settings.challenge_ttl_seconds
        expires_moment = datetime.fromtimestamp(expires_at, UTC)
        async with self._db.acquire() as connection:
            recent = await connection.fetchval(
                """
                SELECT count(*)
                FROM auth_challenges
                WHERE installation_fingerprint = $1
                  AND created_at > now() - make_interval(secs => 60)
                """,
                fingerprint,
            )
            if recent >= self._settings.challenge_rate_limit_per_minute:
                raise ApiError("RATE_LIMITED", retry_after_ms=60000)
            # Atomic public cap on the shared PostgreSQL: correct across API processes and
            # not bypassable with fresh random fingerprints.
            window_count = await connection.fetchval(
                """
                INSERT INTO public_endpoint_counters (scope, window_start, count)
                VALUES ('mobile_auth_challenge', date_trunc('minute', now()), 1)
                ON CONFLICT (scope, window_start)
                DO UPDATE SET count = public_endpoint_counters.count + 1
                RETURNING count
                """
            )
            if window_count > self._settings.public_challenge_limit_per_minute:
                raise ApiError("RATE_LIMITED", retry_after_ms=60000)
            await connection.execute(
                """
                INSERT INTO auth_challenges
                    (challenge_id, nonce_b64, installation_fingerprint, purpose, op,
                     environment, expires_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7)
                """,
                challenge_id,
                nonce_b64,
                fingerprint,
                purpose,
                op,
                environment,
                expires_moment,
            )
        return {
            "request_id": request_id,
            "server_time": rfc3339(now_utc()),
            "schema_version": SCHEMA_VERSION,
            "status": "ok",
            "challenge_id": challenge_id,
            "nonce_b64": nonce_b64,
            "purpose": purpose,
            "environment": environment,
            "expires_at": rfc3339(expires_moment),
            "single_use": True,
        }

    # --------------------------------------------------------------- enrollment

    async def enroll(
        self, body: dict[str, Any], *, idempotency_header: str | None = None
    ) -> dict[str, Any]:
        body = _require_object(body, frozenset({"public_key_spki_b64", "proof"}))
        spki = _require_pattern(body.get("public_key_spki_b64"), r"[A-Za-z0-9_-]+")
        try:
            public_key = pop.load_public_key(spki)
            fingerprint = pop.installation_fingerprint(spki)
        except pop.PopError as exc:
            raise ApiError("BAD_MESSAGE") from exc
        proof = self._proof_object(body.get("proof"))
        request_id = self._proof_request_id(proof)
        challenge, payload = await self._verified_payload(
            proof,
            public_key,
            expected_scope="enrollment",
            expected_op=OP_ENROLLMENT,
            request_id=request_id,
        )
        if payload.get("public_key_spki_b64") != spki:
            raise ApiError("BAD_MESSAGE", details={"reason": "public_key_mismatch"}, request_id=request_id)
        if payload.get("platform") != "android":
            raise ApiError("BAD_MESSAGE", details={"reason": "platform_unsupported"}, request_id=request_id)
        if payload.get("installation_id") != fingerprint:
            raise ApiError("PROOF_INVALID", request_id=request_id)
        name = payload.get("name")
        if name is not None and (not isinstance(name, str) or len(name) > 64):
            raise ApiError("BAD_MESSAGE", details={"reason": "bad_name"}, request_id=request_id)
        idempotency_key = self._optional_idempotency_key(payload, idempotency_header)

        async with self._db.acquire() as connection:
            decision = "created"
            stored_result: dict[str, Any] | None = None
            async with connection.transaction():
                await self._consume_challenge(connection, challenge["challenge_id"], request_id)
                if idempotency_key is not None:
                    digest = self._business_digest(payload, request_id)
                    stored_result, decision = await self._idempotent_apply(
                        connection,
                        environment=challenge["environment"],
                        installation_ref=fingerprint,
                        op=OP_ENROLLMENT,
                        idempotency_key=idempotency_key,
                        business_digest=digest,
                        request_id=request_id,
                        apply=lambda: self._apply_enrollment(
                            connection,
                            fingerprint,
                            spki,
                            name,
                            challenge["environment"],
                        ),
                    )
                else:
                    stored_result = await self._apply_enrollment(
                        connection, fingerprint, spki, name, challenge["environment"]
                    )
            if decision == "conflict":
                raise ApiError(
                    "IDEMPOTENCY_CONFLICT",
                    details={"reason": "business_digest_changed"},
                    request_id=request_id,
                )
            if decision == "expired_result":
                raise ApiError(
                    "SESSION_EXPIRED",
                    details={"reason": "idempotent_result_window_elapsed"},
                    request_id=request_id,
                )
        stored_result["request_id"] = request_id
        stored_result["server_time"] = rfc3339(now_utc())
        stored_result["schema_version"] = SCHEMA_VERSION
        stored_result["status"] = "ok"
        return stored_result

    async def _apply_enrollment(
        self,
        connection: asyncpg.Connection,
        fingerprint: str,
        spki: str,
        name: str | None,
        environment: str,
    ) -> dict[str, Any]:
        installation = await connection.fetchrow(
            """
            INSERT INTO installations
                (environment, platform, name, public_key_fingerprint, public_key_spki_b64,
                 state, last_seen_at)
            VALUES ($3, 'android', $1, $2, $4, 'technical', now())
            ON CONFLICT (public_key_fingerprint, environment)
            DO UPDATE SET last_seen_at = now(),
                          public_key_spki_b64 = EXCLUDED.public_key_spki_b64,
                          name = COALESCE(EXCLUDED.name, installations.name)
            WHERE installations.state <> 'revoked'
            RETURNING id, environment, platform, name, created_at, last_seen_at, state
            """,
            name,
            fingerprint,
            environment,
            spki,
        )
        if installation is None:
            # A revoked installation must not be resurrected by a new enrollment.
            state = await connection.fetchval(
                """
                SELECT state FROM installations
                WHERE public_key_fingerprint = $1 AND environment = $2
                """,
                fingerprint,
                environment,
            )
            if state == "revoked":
                raise ApiError("DEVICE_REVOKED")
            raise ApiError("INTERNAL", http=500)
        token, session = await self._create_session_row(
            connection, installation["id"], scopes=["enrollment"]
        )
        return {
            "installation": {
                "installation_id": fingerprint,
                "public_key_spki_b64": spki,
                "fingerprint": fingerprint,
                "platform": installation["platform"],
                "name": installation["name"],
                "environment": installation["environment"],
                "state": installation["state"],
                "created_at": rfc3339(installation["created_at"]),
                "last_seen_at": rfc3339(installation["last_seen_at"]),
            },
            "session": self._session_object(token, session, fingerprint),
            "entitlement_created": False,
        }

    # ------------------------------------------------------------------ session

    async def create_session(
        self, body: dict[str, Any], *, idempotency_header: str | None = None
    ) -> dict[str, Any]:
        body = _require_object(body, frozenset({"proof"}))
        proof = self._proof_object(body.get("proof"))
        request_id = self._proof_request_id(proof)
        challenge, payload = await self._verified_payload(
            proof,
            public_key=None,
            expected_scope="session",
            expected_op=OP_SESSION,
            request_id=request_id,
        )
        fingerprint = payload.get("installation_id")
        if not isinstance(fingerprint, str) or not fingerprint:
            raise ApiError("PROOF_INVALID", request_id=request_id)
        scopes = self._requested_scopes(payload, request_id)
        idempotency_key = self._required_idempotency_key(payload, idempotency_header)

        async with self._db.acquire() as connection:
            decision = "created"
            stored_result: dict[str, Any] | None = None
            async with connection.transaction():
                installation = await connection.fetchrow(
                    """
                    SELECT id, state, environment
                    FROM installations
                    WHERE public_key_fingerprint = $1 AND environment = $2
                    FOR UPDATE
                    """,
                    fingerprint,
                    challenge["environment"],
                )
                if installation is None:
                    raise ApiError(
                        "PROOF_INVALID",
                        details={"reason": "installation_unknown"},
                        request_id=request_id,
                    )
                if installation["state"] == "revoked":
                    raise ApiError("DEVICE_REVOKED", request_id=request_id)
                # The only trusted link is an existing server-side active binding whose account
                # proves a trusted login; it is resolved under a row lock, never from the client.
                link = await self._resolve_trusted_link(connection, installation["id"])
                await self._require_scope_availability(
                    scopes,
                    linked=link is not None,
                    request_id=request_id,
                    connection=connection,
                    installation_id=installation["id"],
                )
                await self._consume_challenge(connection, challenge["challenge_id"], request_id)
                digest = self._business_digest(payload, request_id)
                stored_result, decision = await self._idempotent_apply(
                    connection,
                    environment=challenge["environment"],
                    installation_ref=fingerprint,
                    op=OP_SESSION,
                    idempotency_key=idempotency_key,
                    business_digest=digest,
                    request_id=request_id,
                    expected_link=link,
                    apply=lambda: self._apply_session(
                        connection, installation["id"], fingerprint, scopes, link
                    ),
                )
            if decision == "conflict":
                raise ApiError(
                    "IDEMPOTENCY_CONFLICT",
                    details={"reason": "business_digest_changed"},
                    request_id=request_id,
                )
            if decision == "expired_result":
                raise ApiError(
                    "SESSION_EXPIRED",
                    details={"reason": "idempotent_result_window_elapsed"},
                    request_id=request_id,
                )
        stored_result["request_id"] = request_id
        stored_result["server_time"] = rfc3339(now_utc())
        stored_result["schema_version"] = SCHEMA_VERSION
        stored_result["status"] = "ok"
        return stored_result

    async def _apply_session(
        self,
        connection: asyncpg.Connection,
        installation_uuid: Any,
        fingerprint: str,
        scopes: list[str],
        link: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        account_id = link["account_id"] if link is not None else None
        token, session = await self._create_session_row(
            connection,
            installation_uuid,
            scopes,
            account_id,
            binding_id=link["binding_id"] if link is not None else None,
            binding_generation=link["generation"] if link is not None else None,
        )
        return {
            "session": self._session_object(
                token, session, fingerprint, str(account_id) if account_id is not None else None
            )
        }

    # ------------------------------------------------------------------ helpers

    def _business_digest(self, payload: dict[str, Any], request_id: str) -> str:
        try:
            return pop.business_digest(payload)
        except pop.PopError as exc:
            raise ApiError("BAD_MESSAGE", request_id=request_id) from exc

    def _proof_object(self, proof: Any) -> dict[str, Any]:
        if not isinstance(proof, dict) or set(proof) != set(PROOF_FIELDS):
            raise ApiError("BAD_MESSAGE")
        if proof.get("algorithm") != "ES256":
            raise ApiError("BAD_MESSAGE", details={"reason": "algorithm_unsupported"})
        _require_pattern(proof.get("signature_b64"), r"[A-Za-z0-9_-]+")
        _require_pattern(proof.get("request_id"), r"[0-9a-f]{32}")
        _require_pattern(proof.get("challenge_id"), r"[0-9a-f]{32}")
        _require_pattern(proof.get("nonce_b64"), r"[A-Za-z0-9_-]+")
        _require_pattern(proof.get("payload_hash"), r"[0-9a-f]{64}")
        _require_pattern(proof.get("signed_payload_b64"), r"[A-Za-z0-9_-]+")
        if proof.get("environment") not in ENVIRONMENTS:
            raise ApiError("BAD_MESSAGE", details={"reason": "unknown_environment"})
        return proof

    def _proof_request_id(self, proof: dict[str, Any]) -> str:
        value = proof.get("request_id")
        if not isinstance(value, str) or len(value) != 32:
            raise ApiError("BAD_MESSAGE")
        return value

    async def _verified_payload(
        self,
        proof: dict[str, Any],
        public_key: Any,
        *,
        expected_scope: str,
        expected_op: str,
        request_id: str,
    ) -> tuple[asyncpg.Record, dict[str, Any]]:
        challenge_id = proof["challenge_id"]
        async with self._db.acquire() as connection:
            challenge = await connection.fetchrow(
                "SELECT * FROM auth_challenges WHERE challenge_id = $1", challenge_id
            )
        if challenge is None:
            raise ApiError("PROOF_INVALID", details={"reason": "unknown_challenge"}, request_id=request_id)
        if challenge["used_at"] is not None:
            raise ApiError("REPLAY_DETECTED", request_id=request_id)
        if challenge["purpose"] != expected_scope or challenge["op"] != expected_op:
            raise ApiError("WRONG_SCOPE", request_id=request_id)
        if proof["environment"] != challenge["environment"]:
            raise ApiError("WRONG_ENVIRONMENT", request_id=request_id)
        if proof["nonce_b64"] != challenge["nonce_b64"]:
            raise ApiError("PROOF_INVALID", details={"reason": "nonce_mismatch"}, request_id=request_id)
        if now_utc() > challenge["expires_at"]:
            raise ApiError("CHALLENGE_EXPIRED", request_id=request_id)

        if public_key is None:
            # The session route has no unsigned key material: the signed payload names the
            # installation and the verification key is resolved from the stored installation.
            _, preview = self._decode_payload(proof, request_id)
            fingerprint = preview.get("installation_id")
            if not isinstance(fingerprint, str) or len(fingerprint) != 64:
                raise ApiError("PROOF_INVALID", request_id=request_id)
            async with self._db.acquire() as connection:
                stored = await connection.fetchrow(
                    """
                    SELECT public_key_spki_b64
                    FROM installations
                    WHERE public_key_fingerprint = $1 AND environment = $2
                    """,
                    fingerprint,
                    challenge["environment"],
                )
            if stored is None or not stored["public_key_spki_b64"]:
                raise ApiError(
                    "PROOF_INVALID",
                    details={"reason": "installation_unknown"},
                    request_id=request_id,
                )
            try:
                public_key = pop.load_public_key(stored["public_key_spki_b64"])
            except pop.PopError as exc:
                raise ApiError("PROOF_INVALID", request_id=request_id) from exc

        try:
            payload = pop.verify_proof(
                public_key,
                signed_payload_b64=proof["signed_payload_b64"],
                payload_hash=proof["payload_hash"],
                signature_b64=proof["signature_b64"],
                request_id=request_id,
                challenge_id=challenge_id,
                nonce_b64=proof["nonce_b64"],
                expected={},
                known_top_level=pop.KNOWN_TOP_LEVEL,
                server_known_fields=pop.KNOWN_TOP_LEVEL,
            )
        except pop.PopError as exc:
            # The contract raises PopError("PROOF_INVALID") as a message (class code BAD_MESSAGE);
            # map known bounded codes explicitly, everything else stays BAD_MESSAGE.
            code = str(exc)
            if code not in ERROR_HTTP:
                code = getattr(exc, "code", "BAD_MESSAGE")
            raise ApiError(code, request_id=request_id) from exc

        if payload.get("env") != challenge["environment"]:
            raise ApiError("WRONG_ENVIRONMENT", request_id=request_id)
        if payload.get("scope") != expected_scope:
            raise ApiError("WRONG_SCOPE", request_id=request_id)
        if payload.get("op") != expected_op:
            raise ApiError("WRONG_SCOPE", request_id=request_id)
        if payload.get("installation_id") != challenge["installation_fingerprint"]:
            raise ApiError("PROOF_INVALID", details={"reason": "installation_mismatch"}, request_id=request_id)
        if abs(parse_utc(payload.get("ts")) - now_utc().timestamp()) > self._settings.proof_skew_seconds:
            raise ApiError("PROOF_INVALID", details={"reason": "ts_outside_window"}, request_id=request_id)
        return challenge, payload

    def _decode_payload(self, proof: dict[str, Any], request_id: str) -> tuple[bytes, dict[str, Any]]:
        try:
            return pop.decode_signed_payload(proof["signed_payload_b64"])
        except pop.PopError as exc:
            raise ApiError("BAD_MESSAGE", request_id=request_id) from exc

    async def _consume_challenge(
        self, connection: asyncpg.Connection, challenge_id: str, request_id: str
    ) -> None:
        consumed = await connection.fetchval(
            """
            UPDATE auth_challenges
            SET used_at = now()
            WHERE challenge_id = $1 AND used_at IS NULL AND expires_at > now()
            RETURNING id
            """,
            challenge_id,
        )
        if consumed is not None:
            return
        current = await connection.fetchrow(
            "SELECT used_at, expires_at FROM auth_challenges WHERE challenge_id = $1",
            challenge_id,
        )
        if current is not None and current["used_at"] is not None:
            raise ApiError("REPLAY_DETECTED", request_id=request_id)
        raise ApiError("CHALLENGE_EXPIRED", request_id=request_id)

    async def _idempotent_apply(
        self,
        connection: asyncpg.Connection,
        *,
        environment: str,
        installation_ref: str,
        op: str,
        idempotency_key: str,
        business_digest: str,
        request_id: str,
        apply,
        expected_link: Any = _NOT_GIVEN,
    ) -> tuple[dict[str, Any] | None, str]:
        # B2a: an ambiguous legacy receipt (environment IS NULL) must never be bypassed by a
        # new environment-aware effect while its retention window is active. Any digest under
        # the same key is refused; the environment is never guessed and no legacy result or
        # bearer is returned. A legacy row whose explicitly verified retain_until has passed
        # no longer blocks (the documented post-window policy); cleanup removes it.
        legacy = await connection.fetchrow(
            """
            SELECT id
            FROM operation_receipts
            WHERE environment IS NULL
              AND installation_ref = $1 AND op = $2 AND idempotency_key = $3
              AND (retain_until IS NULL OR retain_until > now())
            FOR UPDATE
            """,
            installation_ref,
            op,
            idempotency_key,
        )
        if legacy is not None:
            raise ApiError(
                "IDEMPOTENCY_CONFLICT",
                details={"reason": "legacy_receipt_environment_unknown"},
                request_id=request_id,
            )
        receipt_id = await connection.fetchval(
            """
            INSERT INTO operation_receipts
                (environment, installation_ref, op, idempotency_key, business_digest,
                 result_expires_at, retain_until)
            VALUES ($1, $2, $3, $4, $5,
                    now() + make_interval(secs => $6),
                    now() + make_interval(secs => $7))
            ON CONFLICT (environment, installation_ref, op, idempotency_key)
                WHERE environment IS NOT NULL
            DO NOTHING
            RETURNING id
            """,
            environment,
            installation_ref,
            op,
            idempotency_key,
            business_digest,
            float(self._settings.receipt_result_ttl_seconds),
            float(self._settings.idempotency_window_seconds),
        )
        if receipt_id is None:
            existing = await connection.fetchrow(
                """
                SELECT business_digest, result, result_expires_at, retain_until
                FROM operation_receipts
                WHERE environment = $1 AND installation_ref = $2 AND op = $3
                  AND idempotency_key = $4
                FOR UPDATE
                """,
                environment,
                installation_ref,
                op,
                idempotency_key,
            )
            if existing is None:
                raise ApiError("INTERNAL", http=500)
            if existing["business_digest"] != business_digest:
                return None, "conflict"
            if existing["result"] is None:
                # Bounded raw-result retention elapsed: a stable error, never a new effect
                # for the same key while the tombstone is retained.
                return None, "expired_result"
            stored = existing["result"]
            if isinstance(stored, str):
                # asyncpg returns jsonb as text unless a codec is configured.
                stored = json.loads(stored)
            await self._authorize_stored_result(
                connection,
                environment,
                installation_ref,
                stored,
                expected_link=expected_link,
            )
            return stored, "stored"
        result = await apply()
        session_expires_at = self._result_session_expiry(result)
        if session_expires_at is not None:
            await connection.execute(
                """
                UPDATE operation_receipts
                SET result = $2::jsonb,
                    result_expires_at = LEAST(result_expires_at, $3),
                    updated_at = now()
                WHERE id = $1
                """,
                receipt_id,
                result,
                session_expires_at,
            )
        else:
            await connection.execute(
                "UPDATE operation_receipts SET result = $2::jsonb, updated_at = now() WHERE id = $1",
                receipt_id,
                result,
            )
        return result, "created"

    @staticmethod
    def _result_session_expiry(result: dict[str, Any]) -> datetime | None:
        session = result.get("session") if isinstance(result, dict) else None
        if not isinstance(session, dict):
            return None
        expires_at = session.get("expires_at")
        if not isinstance(expires_at, str):
            return None
        try:
            return datetime.fromisoformat(expires_at)
        except ValueError:
            return None

    async def _authorize_stored_result(
        self,
        connection: asyncpg.Connection,
        environment: str,
        installation_ref: str,
        stored: dict[str, Any],
        *,
        expected_link: Any = _NOT_GIVEN,
    ) -> None:
        """Fresh server authorization before any stored result is returned.

        The installation row is locked FOR UPDATE so a concurrent revoke serializes with the
        retry; the stored bearer is only returned when its live session row is unexpired,
        unrevoked, bound to this installation/environment and generation-consistent. A
        revoked/expired bearer is never presented as a live success.
        """
        installation = await connection.fetchrow(
            """
            SELECT id, state, environment
            FROM installations
            WHERE public_key_fingerprint = $1 AND environment = $2
            FOR UPDATE
            """,
            installation_ref,
            environment,
        )
        if installation is None:
            raise ApiError("PROOF_INVALID", details={"reason": "installation_unknown"})
        if installation["state"] == "revoked":
            raise ApiError("DEVICE_REVOKED")

        session = stored.get("session") if isinstance(stored, dict) else None
        if not isinstance(session, dict) or not isinstance(session.get("session_id"), str):
            raise ApiError("SESSION_INVALID", details={"reason": "receipt_without_session"})
        if session.get("installation_ref") != installation_ref:
            raise ApiError("SESSION_INVALID", details={"reason": "receipt_installation_mismatch"})
        row = await connection.fetchrow(
            """
            SELECT id, installation_id, account_id, binding_id, binding_generation,
                   revoked_at, expires_at, generation
            FROM sessions
            WHERE token_sha256 = $1
            FOR UPDATE
            """,
            sha256_hex_text(session["session_id"]),
        )
        if row is None:
            raise ApiError("SESSION_INVALID", details={"reason": "session_missing"})
        if str(row["installation_id"]) != str(installation["id"]):
            raise ApiError("SESSION_INVALID", details={"reason": "session_installation_mismatch"})
        if row["revoked_at"] is not None:
            raise ApiError("SESSION_INVALID", details={"reason": "session_revoked"})
        if row["expires_at"] <= now_utc():
            raise ApiError("SESSION_EXPIRED")
        if expected_link is _NOT_GIVEN:
            if row["account_id"] is not None or row["binding_id"] is not None:
                # 03.2 enrollment receipts are installation-only; a linked session is not
                # returned through the enrollment route.
                raise ApiError("SESSION_INVALID", details={"reason": "unexpected_account_binding"})
        elif expected_link is None:
            if row["account_id"] is not None or row["binding_id"] is not None:
                # The authoritative link changed (created/revoked/deleted) since the receipt
                # was written: the old bearer is never returned against a different state.
                raise ApiError("SESSION_INVALID", details={"reason": "binding_changed"})
        elif (
            str(row["account_id"]) != str(expected_link["account_id"])
            or row["binding_id"] is None
            or str(row["binding_id"]) != str(expected_link["binding_id"])
            or row["binding_generation"] is None
            or int(row["binding_generation"]) != int(expected_link["generation"])
        ):
            # Same account is not enough: a replaced binding row or a bumped generation is a
            # different fence and the old subject's bearer is never returned.
            raise ApiError("SESSION_INVALID", details={"reason": "binding_changed"})
        if str(session.get("generation")) != str(row["generation"]):
            raise ApiError("SESSION_INVALID", details={"reason": "generation_mismatch"})

    def _optional_idempotency_key(
        self, payload: dict[str, Any], idempotency_header: str | None
    ) -> str | None:
        key = payload.get("idempotency_key")
        if key is None:
            if idempotency_header is not None:
                raise ApiError("BAD_MESSAGE", details={"reason": "unsigned_idempotency_header"})
            return None
        return self._check_idempotency_key(key, idempotency_header)

    def _required_idempotency_key(
        self, payload: dict[str, Any], idempotency_header: str | None
    ) -> str:
        key = payload.get("idempotency_key")
        if key is None:
            raise ApiError("BAD_MESSAGE", details={"reason": "missing_idempotency_key"})
        return self._check_idempotency_key(key, idempotency_header)

    def _check_idempotency_key(self, key: Any, header: str | None) -> str:
        if not isinstance(key, str) or not 16 <= len(key) <= 128:
            raise ApiError("BAD_MESSAGE", details={"reason": "bad_idempotency_key"})
        if header is not None and header != key:
            raise ApiError("IDEMPOTENCY_CONFLICT", details={"reason": "header_mismatch"})
        return key

    def _requested_scopes(self, payload: dict[str, Any], request_id: str) -> list[str]:
        requested = payload.get("requested_scopes")
        if not isinstance(requested, list) or not requested:
            raise ApiError("BAD_MESSAGE", details={"reason": "missing_requested_scopes"}, request_id=request_id)
        if len(requested) > 8 or not all(isinstance(item, str) for item in requested):
            raise ApiError("BAD_MESSAGE", details={"reason": "bad_requested_scopes"}, request_id=request_id)
        if len(set(requested)) != len(requested):
            raise ApiError("BAD_MESSAGE", details={"reason": "duplicate_scopes"}, request_id=request_id)
        unknown = [scope for scope in requested if scope not in SESSION_SCOPES]
        if unknown:
            raise ApiError("WRONG_SCOPE", request_id=request_id)
        return list(requested)

    async def _require_scope_availability(
        self,
        scopes: list[str],
        *,
        linked: bool,
        request_id: str,
        connection: asyncpg.Connection | None = None,
        installation_id: Any = None,
    ) -> None:
        # An enum-valid scope is not by itself an authorization: the requested scope set must be
        # covered by the proven server-side link state. A valid session is never a VPN right.
        allowed = LINKED_SESSION_SCOPES if linked else UNLINKED_INSTALLATION_SCOPES
        not_available = [scope for scope in scopes if scope not in allowed]
        if not_available == ["access:sync"] and connection is not None and installation_id is not None:
            # Keep the existing scope name: an unlinked installation may request access:sync only
            # while an authoritative installation-scoped onboarding_hour is active. The check runs
            # in the same session transaction; a pre-hour/management-only bearer is never elevated.
            hour_active = await connection.fetchval(
                """
                SELECT 1 FROM entitlements
                WHERE kind = 'onboarding_hour' AND installation_id = $1
                  AND status = 'active' AND ends_at IS NOT NULL AND ends_at > now()
                LIMIT 1
                """,
                installation_id,
            )
            if hour_active is not None:
                not_available = []
        if not_available:
            raise ApiError(
                "ACCESS_DENIED",
                details={"reason": "scope_not_available_for_account_state"},
                request_id=request_id,
            )

    async def _resolve_trusted_link(
        self, connection: asyncpg.Connection, installation_id: Any
    ) -> dict[str, Any] | None:
        """Resolve an existing trusted installation->account link under a row lock.

        Only server-side rows are trusted: exactly one active binding whose account proves a
        trusted login (status verified + linked Telegram identity). No link, a revoked link, an
        ambiguous link (two active bindings) or an unproven account never elevates scopes. No
        account/binding/entitlement is created here and Telegram login is never simulated.
        """
        rows = await connection.fetch(
            """
            SELECT binding.id AS binding_id, binding.account_id, binding.status AS binding_status,
                   binding.generation, account.status AS account_status, account.telegram_id
            FROM account_bindings AS binding
            JOIN accounts AS account ON account.id = binding.account_id
            WHERE binding.installation_id = $1
            ORDER BY binding.bound_at DESC, binding.id
            FOR UPDATE OF binding
            """,
            installation_id,
        )
        active = [row for row in rows if row["binding_status"] == "active"]
        if len(active) != 1:
            return None
        row = active[0]
        if row["account_status"] != "verified" or row["telegram_id"] is None:
            return None
        return {
            "account_id": row["account_id"],
            "binding_id": row["binding_id"],
            "generation": int(row["generation"]),
        }

    async def _create_session_row(
        self,
        connection: asyncpg.Connection,
        installation_uuid: Any,
        scopes: list[str],
        account_id: Any = None,
        *,
        binding_id: Any = None,
        binding_generation: int | None = None,
    ) -> tuple[str, asyncpg.Record]:
        token = secrets.token_urlsafe(32)
        expires_at = now_utc().timestamp() + self._settings.session_ttl_seconds
        session = await connection.fetchrow(
            """
            INSERT INTO sessions
                (account_id, installation_id, scopes, generation, expires_at, token_sha256,
                 binding_id, binding_generation)
            VALUES ($5, $1, $2::text[], 1, $3, $4, $6, $7)
            RETURNING id, scopes, generation, issued_at, expires_at
            """,
            installation_uuid,
            scopes,
            datetime.fromtimestamp(expires_at, UTC),
            sha256_hex_text(token),
            account_id,
            binding_id,
            binding_generation,
        )
        return token, session

    @staticmethod
    def _session_object(
        token: str,
        session: asyncpg.Record,
        installation_ref: str,
        account_ref: str | None = None,
    ) -> dict[str, Any]:
        return {
            "session_id": token,
            "account_ref": account_ref,
            "installation_ref": installation_ref,
            "scopes": list(session["scopes"]),
            "generation": str(session["generation"]),
            "issued_at": rfc3339(session["issued_at"]),
            "expires_at": rfc3339(session["expires_at"]),
        }


AUTH_SERVICE_KEY: web.AppKey = web.AppKey("installation_session_service", InstallationSessionService)


async def _json_body(request: web.Request) -> dict[str, Any]:
    content_type = request.headers.get("Content-Type", "")
    if not content_type.startswith("application/json"):
        raise ApiError("BAD_MESSAGE")
    raw = await request.read()
    if len(raw) > MAX_REQUEST_BYTES:
        raise ApiError("BAD_MESSAGE", details={"reason": "body_too_large"})
    try:
        body = json.loads(raw)
    except (ValueError, UnicodeError) as exc:
        raise ApiError("BAD_MESSAGE") from exc
    if not isinstance(body, dict):
        raise ApiError("BAD_MESSAGE")
    return body


def _error_response(request_id: str, error: ApiError) -> web.Response:
    body: dict[str, Any] = {
        "request_id": error.request_id or request_id,
        "server_time": rfc3339(now_utc()),
        "schema_version": SCHEMA_VERSION,
        "status": "error",
        "code": error.code,
        "retryable": error.retryable,
    }
    headers = {}
    if error.retry_after_ms is not None:
        body["retry_after_ms"] = error.retry_after_ms
        headers["Retry-After"] = str(max(1, (error.retry_after_ms + 999) // 1000))
    if error.details:
        body["details"] = error.details
    return web.json_response(body, status=error.http, headers=headers)


def _service(request: web.Request) -> InstallationSessionService:
    return request.app[AUTH_SERVICE_KEY]


async def _handle_challenge(request: web.Request) -> web.Response:
    fallback_id = random_hex(16)
    try:
        body = await _json_body(request)
        return web.json_response(await _service(request).create_challenge(body))
    except ApiError as error:
        return _error_response(fallback_id, error)
    except (asyncpg.PostgresError, OSError):
        logger.exception("challenge failed")
        return _error_response(fallback_id, ApiError("SERVICE_UNAVAILABLE"))


async def _handle_enrollment(request: web.Request) -> web.Response:
    fallback_id = random_hex(16)
    try:
        body = await _json_body(request)
        result = await _service(request).enroll(
            body, idempotency_header=request.headers.get("Idempotency-Key")
        )
        return web.json_response(result)
    except ApiError as error:
        return _error_response(fallback_id, error)
    except (asyncpg.PostgresError, OSError):
        logger.exception("enrollment failed")
        return _error_response(fallback_id, ApiError("SERVICE_UNAVAILABLE"))


async def _handle_session(request: web.Request) -> web.Response:
    fallback_id = random_hex(16)
    try:
        body = await _json_body(request)
        result = await _service(request).create_session(
            body, idempotency_header=request.headers.get("Idempotency-Key")
        )
        return web.json_response(result)
    except ApiError as error:
        return _error_response(fallback_id, error)
    except (asyncpg.PostgresError, OSError):
        logger.exception("session failed")
        return _error_response(fallback_id, ApiError("SERVICE_UNAVAILABLE"))


def register_mobile_auth_routes(app: web.Application) -> None:
    app.router.add_post(f"{MOBILE_PREFIX}/auth/challenge", _handle_challenge)
    app.router.add_post(f"{MOBILE_PREFIX}/installations", _handle_enrollment)
    app.router.add_post(f"{MOBILE_PREFIX}/auth/session", _handle_session)
