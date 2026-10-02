"""Canonical installation PoP helpers for TERLIMO mobile v1.

This module is a port of the accepted contract implementation so Backend and Android
cannot diverge:

    terlimo-s1-contracts @ 336bd6dae9e9340874d6e128a360fe6cfe2f26e6
    auth/pop_canonical.py  sha256 262b9acc0ba74a02211793cb928002060aa6ead7494f0abaca5459086a4d1751
    auth/POP_PAYLOAD_V1.md sha256 2de387a7e3b85590850c41d5491653c2304b93fedb083a91b361d623d42fd122

Contract operations and validation order are retained; optional payload-free
diagnostic observer guards and the `installation_fingerprint` helper are added.
No network, no secret store, stdlib + cryptography only.

Test vectors: vectors/auth_vectors.json (TEST-ONLY synthetic key, not a secret).
"""

from __future__ import annotations

import base64
import hashlib
import json
import re
import unicodedata
from collections.abc import Iterable
from typing import Any

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec

PREFIX = b"WLBS-POP-1\x00"
HEX = set("0123456789abcdef")

# Fields that belong to the fresh proof envelope, not to the business operation.
# Their change MUST NOT change the business-operation identity.
FRESHNESS_FIELDS = frozenset({"request_id", "ts", "nonce", "critical"})

# Common business-identity fields present in every projection.
PROJECTION_COMMON = ("env", "scope", "op", "installation_id")

# Exact per-scope action fields (the only scope-specific fields that may affect the action).
PER_SCOPE_FIELDS = {
    "enrollment": ("public_key_spki_b64", "platform", "name"),
    "session": ("requested_scopes",),
    "binding": ("device_name",),
    "telegram-link": (),
    "catalog": ("known_revision",),
    "checkout": ("payment_id",),
    "payment:write": ("quote_id",),
    "access:sync": ("catalog_revision", "binding_revision"),
    "onboarding:start": ("request_key", "intent_id", "gateway_key"),
}

# Fields that MUST be present for the operation to be well-formed. A missing required
# action field is a malformed operation and must never produce a business digest.
REQUIRED_ACTION_FIELDS = {
    "enrollment": ("public_key_spki_b64", "platform"),
    "session": ("requested_scopes",),
    "binding": (),
    "telegram-link": (),
    "catalog": ("known_revision",),
    "checkout": ("payment_id",),
    "payment:write": ("quote_id",),
    "access:sync": ("catalog_revision", "binding_revision"),
    "onboarding:start": ("request_key",),
}


class PopError(ValueError):
    code = "BAD_MESSAGE"


class WrongEnvironment(PopError):
    code = "WRONG_ENVIRONMENT"


class WrongScope(PopError):
    code = "WRONG_SCOPE"


class UnknownCriticalField(PopError):
    code = "UNKNOWN_CRITICAL_FIELD"


def _no_duplicates(pairs: Iterable[tuple[str, Any]]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for key, value in pairs:
        if key in out:
            raise PopError("duplicate key")
        out[key] = value
    return out


def nfc(text: str) -> str:
    """Single canonical Unicode form for all strings: NFC (POP_PAYLOAD_V1.md section 2)."""
    return unicodedata.normalize("NFC", text)


def _nfc_tree(obj: Any) -> Any:
    if isinstance(obj, str):
        return nfc(obj)
    if isinstance(obj, dict):
        out: dict[str, Any] = {}
        for key, value in obj.items():
            nkey = nfc(key)
            if nkey in out:
                raise PopError("NFC key collision")
            out[nkey] = _nfc_tree(value)
        return out
    if isinstance(obj, list):
        return [_nfc_tree(value) for value in obj]
    return obj


def canonical_json(obj: Any) -> bytes:
    """Deterministic UTF-8 JSON: NFC strings, sorted keys, compact separators, no floats/NaN."""
    _reject_non_canonical(obj)
    return json.dumps(
        _nfc_tree(obj), sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False
    ).encode("utf-8")


def _reject_non_canonical(obj: Any) -> None:
    if isinstance(obj, float):
        raise PopError("floats are not canonical")
    if isinstance(obj, dict):
        for key, value in obj.items():
            if not isinstance(key, str):
                raise PopError("non-string key")
            _reject_non_canonical(value)
    elif isinstance(obj, list):
        for value in obj:
            _reject_non_canonical(value)


def _observe(observer, phase: str, **metadata) -> None:
    """Optional diagnostics cannot affect PoP results or exceptions."""
    try:
        observer(phase, metadata=metadata or None)
    except Exception:
        pass


def parse_strict(data: bytes) -> Any:
    return json.loads(data.decode("utf-8"), object_pairs_hook=_no_duplicates)


def b64url_encode(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def b64url_decode(text: str) -> bytes:
    if not isinstance(text, str) or any(
        c not in "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_" for c in text
    ):
        raise PopError("bad base64url")
    pad = "=" * (-len(text) % 4)
    return base64.urlsafe_b64decode(text + pad)


def b64url_decode_exact(text: str, size: int) -> bytes:
    """Strict base64url: decode, require exact size, and require re-encode equality.

    Rejects padding, non-canonical trailing bits and any non-canonical input.
    """
    raw = b64url_decode(text)
    if len(raw) != size or b64url_encode(raw) != text:
        raise PopError("non-canonical base64url")
    return raw


def validate_pin(text: str) -> bytes:
    """`dtls_spki_sha256` = canonical unpadded base64url of exactly 32 bytes."""
    return b64url_decode_exact(text, 32)


def hex16(value: str) -> bytes:
    if not isinstance(value, str) or len(value) != 32 or any(c not in HEX for c in value):
        raise PopError("bad 16-byte hex")
    return bytes.fromhex(value)


def sha256_hex(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def payload_hash(payload_bytes: bytes) -> bytes:
    return hashlib.sha256(payload_bytes).digest()


def pop_message(request_id: str, challenge_id: str, nonce_b64: str, payload_bytes: bytes) -> bytes:
    return (
        PREFIX
        + hex16(request_id)
        + hex16(challenge_id)
        + b64url_decode(nonce_b64)
        + payload_hash(payload_bytes)
    )


def check_environment(payload: dict[str, Any], expected: str) -> None:
    if payload.get("env") != expected:
        raise WrongEnvironment()


def check_scope(payload: dict[str, Any], expected: str) -> None:
    if payload.get("scope") != expected:
        raise WrongScope()


def check_critical_fields(payload: dict[str, Any], known: Iterable[str]) -> None:
    critical = payload.get("critical")
    if critical is None:
        return
    if not isinstance(critical, list) or not all(isinstance(x, str) for x in critical):
        raise PopError("bad critical")
    unknown = [name for name in critical if name not in set(known)]
    if unknown:
        raise UnknownCriticalField(",".join(unknown))


def check_top_level_fields(payload: dict[str, Any], known: Iterable[str]) -> None:
    """Strict top-level: unknown fields are rejected; additive data lives in `extensions`."""
    unknown = [name for name in payload if name not in set(known)]
    if unknown:
        raise PopError("unknown top-level field: " + ",".join(sorted(unknown)))


def decode_signed_payload(signed_payload_b64: str, *, observer=None) -> tuple[bytes, Any]:
    """Decode and prove canonical: re-canonicalization must equal the received bytes."""
    raw = b64url_decode(signed_payload_b64)
    if observer is not None:
        _observe(observer, "payload_decode_end", signed_payload_bytes=len(raw))
    try:
        obj = parse_strict(raw)
    except (ValueError, UnicodeError) as exc:
        raise PopError("malformed signed payload") from exc
    if observer is not None:
        _observe(observer, "payload_jsonparse_end")
    if canonical_json(obj) != raw:
        raise PopError("non-canonical signed payload")
    if observer is not None:
        _observe(observer, "payload_canonical_end")
    return raw, obj


def bind_fields(payload: dict[str, Any], expected: dict[str, Any]) -> None:
    """Every expected semantic field must be present in the signed payload and equal."""
    for key, value in expected.items():
        if payload.get(key) != value:
            raise PopError(f"binding mismatch: {key}")


KNOWN_TOP_LEVEL = frozenset(
    {
        "env",
        "scope",
        "op",
        "installation_id",
        "ts",
        "nonce",
        "request_id",
        "idempotency_key",
        "requested_scopes",
        "device_name",
        "known_revision",
        "payment_id",
        "quote_id",
        "public_key_spki_b64",
        "platform",
        "name",
        "catalog_revision",
        "binding_revision",
        "request_key",
        "intent_id",
        "gateway_key",
        "extensions",
        "critical",
    }
)

_EXT_KEY_RE = re.compile(r"^[a-z][a-z0-9_.-]{0,63}$")


def check_extensions(payload: dict[str, Any]) -> None:
    """Explicit extensions policy: additive, signed, name-safe, and action-affecting.

    - `extensions` must be an object.
    - Keys must be lowercase identifiers (`^[a-z][a-z0-9_.-]{0,63}$`).
    - Keys must not shadow any known field/freshness/reserved name.
    - Values are covered by the signature and are part of the business projection, so a
      changed extension conflicts under the same Idempotency-Key. Extensions must not be
      used to introduce a second meaning for an existing field.
    """
    ext = payload.get("extensions")
    if ext is None:
        return
    if not isinstance(ext, dict):
        raise PopError("bad extensions")
    reserved = set(KNOWN_TOP_LEVEL) | {"extensions", "critical"}
    for key in ext:
        if not isinstance(key, str) or not _EXT_KEY_RE.match(key):
            raise PopError("bad extension key")
        if key in reserved:
            raise PopError("extension shadows reserved field: " + key)


def business_projection(payload: dict[str, Any]) -> dict[str, Any]:
    """Stable business identity of an operation: the exact fields that can affect the action.

    Excludes the fresh-proof envelope (`request_id`, `ts`, `nonce`, `critical`), the
    idempotency storage key, and the signature itself. `scope`/`op`/`env` and the
    installation identity are always included; action fields are the exact per-scope set;
    `extensions` are included verbatim (explicit policy).
    """
    scope = payload.get("scope")
    if scope not in PER_SCOPE_FIELDS:
        raise PopError("unknown scope")
    check_extensions(payload)
    for field in REQUIRED_ACTION_FIELDS[scope]:
        if field not in payload:
            raise PopError("missing required action field: " + field)
    projection: dict[str, Any] = {}
    for field in PROJECTION_COMMON:
        if field not in payload:
            raise PopError("missing " + field)
        projection[field] = payload[field]
    for field in PER_SCOPE_FIELDS[scope]:
        if field in payload:
            projection[field] = payload[field]
    if "extensions" in payload:
        projection["extensions"] = payload["extensions"]
    return projection


def business_digest(payload: dict[str, Any]) -> str:
    """SHA-256 over the canonical business projection. Independent of fresh proof fields."""
    return sha256_hex(canonical_json(business_projection(payload)))


def verify_proof(
    public_key,
    *,
    signed_payload_b64: str,
    payload_hash: str,
    signature_b64: str,
    request_id: str,
    challenge_id: str,
    nonce_b64: str,
    expected: dict[str, Any],
    known_top_level: Iterable[str],
    server_known_fields: Iterable[str],
    observer=None,
) -> dict[str, Any]:
    """Full verifier path (POP_PAYLOAD_V1.md section 3): returns the validated payload."""
    raw, payload = (decode_signed_payload(signed_payload_b64) if observer is None
                    else decode_signed_payload(signed_payload_b64, observer=observer))
    if sha256_hex(raw) != payload_hash:
        raise PopError("payload_hash mismatch")
    check_top_level_fields(payload, known_top_level)
    check_critical_fields(payload, server_known_fields)
    # inner/outer equality: the payload must name the same request_id and nonce that were
    # put into the signed message; `expected` alone (derived from the payload) cannot prove it.
    if payload.get("request_id") != request_id:
        raise PopError("request_id inner/outer mismatch")
    if payload.get("nonce") != nonce_b64:
        raise PopError("nonce inner/outer mismatch")
    message = pop_message(request_id, challenge_id, nonce_b64, raw)
    if observer is not None:
        _observe(observer, "proof_fields_message_end")
    valid = (verify(public_key, message, signature_b64) if observer is None
             else verify(public_key, message, signature_b64, observer=observer))
    if not valid:
        raise PopError("PROOF_INVALID")
    bind_fields(payload, expected)
    if observer is not None:
        _observe(observer, "proof_binding_end")
    return payload


def sign(private_key, message: bytes) -> str:
    der = private_key.sign(message, ec.ECDSA(hashes.SHA256()))
    return b64url_encode(der)


def verify(public_key, message: bytes, signature_b64: str, *, observer=None) -> bool:
    try:
        signature = b64url_decode(signature_b64)
        if observer is not None:
            _observe(observer, "ecdsa_verify_begin", signature_bytes=len(signature))
        public_key.verify(signature, message, ec.ECDSA(hashes.SHA256()))
        if observer is not None:
            _observe(observer, "ecdsa_verify_end")
        return True
    except (InvalidSignature, PopError, ValueError):
        if observer is not None:
            _observe(observer, "proof_signature_rejected")
        return False


def load_public_key(spki_b64: str, *, observer=None):
    """Load a strict P-256 SPKI public key from canonical unpadded base64url."""
    raw = b64url_decode(spki_b64)
    if observer is not None:
        _observe(observer, "key_decode_end", key_der_bytes=len(raw))
    try:
        key = serialization.load_der_public_key(raw)
    except (ValueError, TypeError) as exc:
        raise PopError("bad public key") from exc
    if observer is not None:
        _observe(observer, "key_derparse_end")
    if not isinstance(key, ec.EllipticCurvePublicKey) or not isinstance(key.curve, ec.SECP256R1):
        raise PopError("unsupported public key")
    if key.key_size != 256:
        raise PopError("unsupported public key size")
    if observer is not None:
        _observe(observer, "key_validation_end", algorithm="p256")
    return key


def installation_fingerprint(spki_b64: str) -> str:
    """`installation_id` = SHA-256(SPKI DER) lowercase hex (POP_PAYLOAD_V1.md section 1)."""
    return sha256_hex(b64url_decode(spki_b64))
