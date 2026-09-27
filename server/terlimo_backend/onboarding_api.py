"""Public explicit-connect route: PoP `onboarding:start` intent (no hour start).

Creates or returns the installation-scoped connect intent and, when the worker has provisioned
it, delivers the bootstrap credential plus the single outstanding bootstrap start challenge.
This route never calls `admit_evidence` and never writes `onboarding_evidence`/`entitlements`.
"""

from __future__ import annotations

import re
from typing import Any

import asyncpg
from aiohttp import web

from . import SCHEMA_VERSION, pop
from .auth_api import (
    ERROR_HTTP,
    ApiError,
    _error_response,
    _json_body,
    now_utc,
    parse_utc,
    random_hex,
    rfc3339,
)
from .config import Settings
from .db import Database
from .onboarding_hour import (
    INTENT_CHALLENGE_PURPOSE,
    ONBOARDING_INTENT_OP,
    ONBOARDING_SCOPE,
    OnboardingError,
    build_secret_cipher,
    public_intent,
)
from .session_auth import AuthError, authenticate_session

MOBILE_PREFIX = "/api/mobile/v1"
PROOF_KEYS = frozenset(
    {
        "algorithm",
        "environment",
        "request_id",
        "challenge_id",
        "nonce_b64",
        "payload_hash",
        "signed_payload_b64",
        "signature_b64",
    }
)
ONBOARDING_SERVICE_KEY: web.AppKey = web.AppKey("onboarding_intent_service", "OnboardingIntentService")


def _require_pattern(value: Any, pattern: str) -> str:
    if not isinstance(value, str) or not re.fullmatch(pattern, value):
        raise ApiError("BAD_MESSAGE")
    return value


def _proof_object(proof: Any) -> dict[str, Any]:
    if not isinstance(proof, dict) or set(proof) != PROOF_KEYS:
        raise ApiError("BAD_MESSAGE")
    if proof.get("algorithm") != "ES256":
        raise ApiError("BAD_MESSAGE", details={"reason": "algorithm_unsupported"})
    if proof.get("environment") not in ("test", "production"):
        raise ApiError("BAD_MESSAGE", details={"reason": "unknown_environment"})
    _require_pattern(proof.get("request_id"), r"[0-9a-f]{32}")
    _require_pattern(proof.get("challenge_id"), r"[0-9a-f]{16,64}")
    _require_pattern(proof.get("payload_hash"), r"[0-9a-f]{64}")
    _require_pattern(proof.get("nonce_b64"), r"[A-Za-z0-9_-]{16,64}")
    _require_pattern(proof.get("signed_payload_b64"), r"[A-Za-z0-9_-]{1,16384}")
    _require_pattern(proof.get("signature_b64"), r"[A-Za-z0-9_-]{1,16384}")
    return proof


class OnboardingIntentService:
    def __init__(self, settings: Settings, database: Database) -> None:
        self._settings = settings
        self._db = database

    async def create_intent(self, body: dict[str, Any], token: str) -> dict[str, Any]:
        if set(body) != {"proof"}:
            raise ApiError("BAD_MESSAGE")
        proof = _proof_object(body.get("proof"))
        request_id = proof["request_id"]
        async with self._db.acquire() as connection:
            context = await authenticate_session(
                connection, self._settings, token, required_scope="session:write"
            )
            challenge = await connection.fetchrow(
                "SELECT * FROM auth_challenges WHERE challenge_id = $1", proof["challenge_id"]
            )
            if challenge is None:
                raise ApiError("PROOF_INVALID", details={"reason": "unknown_challenge"}, request_id=request_id)
            if challenge["used_at"] is not None:
                raise ApiError("REPLAY_DETECTED", request_id=request_id)
            if (
                challenge["purpose"] != INTENT_CHALLENGE_PURPOSE
                or challenge["op"] != ONBOARDING_INTENT_OP
            ):
                raise ApiError("WRONG_SCOPE", request_id=request_id)
            if proof["environment"] != challenge["environment"]:
                raise ApiError("WRONG_ENVIRONMENT", request_id=request_id)
            if challenge["installation_fingerprint"] != context.installation_ref:
                raise ApiError("PROOF_INVALID", details={"reason": "installation_mismatch"}, request_id=request_id)
            if proof["nonce_b64"] != challenge["nonce_b64"]:
                raise ApiError("PROOF_INVALID", details={"reason": "nonce_mismatch"}, request_id=request_id)
            if now_utc() > challenge["expires_at"]:
                raise ApiError("CHALLENGE_EXPIRED", request_id=request_id)
            stored = await connection.fetchrow(
                """
                SELECT public_key_spki_b64 FROM installations
                WHERE public_key_fingerprint = $1 AND environment = $2
                """,
                context.installation_ref,
                challenge["environment"],
            )
            if stored is None or not stored["public_key_spki_b64"]:
                raise ApiError("PROOF_INVALID", details={"reason": "installation_unknown"}, request_id=request_id)
            try:
                public_key = pop.load_public_key(stored["public_key_spki_b64"])
                payload = pop.verify_proof(
                    public_key,
                    signed_payload_b64=proof["signed_payload_b64"],
                    payload_hash=proof["payload_hash"],
                    signature_b64=proof["signature_b64"],
                    request_id=request_id,
                    challenge_id=proof["challenge_id"],
                    nonce_b64=proof["nonce_b64"],
                    expected={},
                    known_top_level=pop.KNOWN_TOP_LEVEL,
                    server_known_fields=pop.KNOWN_TOP_LEVEL,
                )
            except pop.PopError as exc:
                code = str(exc)
                if code not in ERROR_HTTP:
                    code = getattr(exc, "code", "BAD_MESSAGE")
                raise ApiError(code, request_id=request_id) from exc
            if payload.get("env") != challenge["environment"]:
                raise ApiError("WRONG_ENVIRONMENT", request_id=request_id)
            if payload.get("scope") != ONBOARDING_SCOPE:
                raise ApiError("WRONG_SCOPE", request_id=request_id)
            if payload.get("op") != ONBOARDING_INTENT_OP:
                raise ApiError("WRONG_SCOPE", request_id=request_id)
            if payload.get("installation_id") != context.installation_ref:
                raise ApiError("PROOF_INVALID", details={"reason": "installation_mismatch"}, request_id=request_id)
            ts_value = payload.get("ts")
            if not isinstance(ts_value, str):
                raise ApiError("PROOF_INVALID", details={"reason": "ts_missing"}, request_id=request_id)
            try:
                ts_epoch = parse_utc(ts_value)
            except (TypeError, ValueError):
                raise ApiError("PROOF_INVALID", details={"reason": "ts_invalid"}, request_id=request_id) from None
            if abs(ts_epoch - now_utc().timestamp()) > self._settings.proof_skew_seconds:
                raise ApiError("PROOF_INVALID", details={"reason": "ts_outside_window"}, request_id=request_id)
            request_key = payload.get("request_key")
            if not isinstance(request_key, str) or not 1 <= len(request_key) <= 128:
                raise ApiError("BAD_MESSAGE", details={"reason": "bad_request_key"}, request_id=request_id)
            gateway_key = payload.get("gateway_key")
            if gateway_key is not None and (
                not isinstance(gateway_key, str) or not 1 <= len(gateway_key) <= 128
            ):
                raise ApiError("BAD_MESSAGE", details={"reason": "bad_gateway_key"}, request_id=request_id)

            # Single use of the intent challenge is its own committed step: a lost response is
            # retried with a fresh challenge, and the operation itself is idempotent by
            # request_key. Nothing else is written before the intent call.
            consumed = await connection.fetchval(
                """
                UPDATE auth_challenges SET used_at = now()
                WHERE challenge_id = $1 AND used_at IS NULL AND superseded_at IS NULL
                RETURNING 1
                """,
                proof["challenge_id"],
            )
            if consumed is None:
                raise ApiError("REPLAY_DETECTED", request_id=request_id)
            try:
                result = await public_intent(
                    connection,
                    installation_id=context.installation_id,
                    request_key=request_key,
                    environment=self._settings.environment,
                    cipher=build_secret_cipher(self._settings),
                    challenge_ttl_seconds=self._settings.challenge_ttl_seconds,
                    gateway_key=gateway_key,
                )
            except OnboardingError as exc:
                retry_after_ms = (exc.retry_after or 0) * 1000 or None
                raise ApiError(
                    exc.code,
                    http=exc.http,
                    retry_after_ms=retry_after_ms,
                    request_id=request_id,
                ) from exc
        result["request_id"] = request_id
        result["server_time"] = rfc3339(now_utc())
        result["schema_version"] = SCHEMA_VERSION
        return result


def _bearer_token(request: web.Request) -> str:
    header = request.headers.get("Authorization", "")
    if not header.startswith("Bearer "):
        raise ApiError("SESSION_INVALID", http=401)
    return header[len("Bearer ") :].strip()


def register_onboarding_routes(app: web.Application) -> None:
    app.router.add_post(f"{MOBILE_PREFIX}/onboarding/intents", _handle_intent)


async def _handle_intent(request: web.Request) -> web.Response:
    fallback_id = random_hex(16)
    try:
        body = await _json_body(request)
        result = await request.app[ONBOARDING_SERVICE_KEY].create_intent(body, _bearer_token(request))
        return web.json_response(result)
    except ApiError as error:
        return _error_response(fallback_id, error)
    except AuthError as error:
        return _error_response(fallback_id, ApiError(error.code, http=error.http))
    except (asyncpg.PostgresError, OSError):
        return _error_response(fallback_id, ApiError("SERVICE_UNAVAILABLE"))
