"""Recovery v1 public codes. Signing is an operator-only, offline operation.

The API/bot read the same bounded signed public file and never load a private key.
A code carries no application identity or access rights. Client revision ordering,
connection provenance and persistence remain client responsibilities.
"""
from __future__ import annotations

import base64
import binascii
import ipaddress
import json
import re
from pathlib import Path
from typing import Any

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey

from .config import Settings

DOMAIN = b"TERLIMO-RECOVERY-V1\x00"
MAX_CODE = 3500
MAX_SEED = 8192
FIELDS = frozenset({
    "version", "revision", "environment", "peer_ip", "dtls_port", "dtls_spki_sha256",
    "service_classifier", "vk_hashes", "stream_id",
})


class RecoveryInvalid(ValueError):
    """Bounded public error; do not include input/key/file contents in exceptions."""


class RecoveryUnavailable(RuntimeError):
    pass


def encode_b64(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def decode_b64(text: str, size: int | None = None) -> bytes:
    if not isinstance(text, str) or not re.fullmatch(r"[A-Za-z0-9_-]+", text):
        raise RecoveryInvalid("base64url")
    try:
        raw = base64.b64decode(text + "=" * (-len(text) % 4), altchars=b"-_", validate=True)
    except (ValueError, binascii.Error):
        raise RecoveryInvalid("base64url") from None
    if encode_b64(raw) != text or (size is not None and len(raw) != size):
        raise RecoveryInvalid("base64url")
    return raw


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result = {}
    for key, value in pairs:
        if key in result:
            raise RecoveryInvalid("duplicate_field")
        result[key] = value
    return result


def parse_seed(raw: bytes, environment: str) -> dict[str, Any]:
    if not 0 < len(raw) <= MAX_SEED:
        raise RecoveryInvalid("seed_size")
    try:
        seed = json.loads(raw.decode("utf-8"), object_pairs_hook=_unique_object)
    except (ValueError, UnicodeError, RecursionError):
        raise RecoveryInvalid("seed_json") from None
    if not isinstance(seed, dict) or seed.keys() != FIELDS:
        raise RecoveryInvalid("seed_fields")
    if type(seed["version"]) is not int or seed["version"] != 1:
        raise RecoveryInvalid("seed_version")
    revision = seed["revision"]
    if not isinstance(revision, str) or not re.fullmatch(r"0|[1-9][0-9]{0,18}", revision):
        raise RecoveryInvalid("seed_revision")
    if environment not in ("test", "production") or seed["environment"] != environment:
        raise RecoveryInvalid("seed_environment")
    peer = seed["peer_ip"]
    try:
        if not isinstance(peer, str) or "%" in peer:
            raise ValueError
        ipaddress.ip_address(peer)
    except ValueError:
        raise RecoveryInvalid("seed_peer") from None
    port = seed["dtls_port"]
    if type(port) is not int or not 1 <= port <= 65535:
        raise RecoveryInvalid("seed_port")
    decode_b64(seed["dtls_spki_sha256"], 32)
    classifier = seed["service_classifier"]
    if not isinstance(classifier, str) or not re.fullmatch(r"[!-~]{1,128}", classifier):
        raise RecoveryInvalid("seed_classifier")
    hashes = seed["vk_hashes"]
    if not isinstance(hashes, list) or not 1 <= len(hashes) <= 4:
        raise RecoveryInvalid("seed_hashes")
    for value in hashes:
        if not isinstance(value, str) or not value or any(c in value for c in " \t\r\n"):
            raise RecoveryInvalid("seed_hash")
        try:
            if len(value.encode("utf-8")) > 128:
                raise RecoveryInvalid("seed_hash")
        except UnicodeError:
            raise RecoveryInvalid("seed_hash") from None
    # The accepted Android build is arm64: retain its existing signed Go int bound.
    stream = seed["stream_id"]
    if type(stream) is not int or not 0 <= stream <= 9223372036854775807:
        raise RecoveryInvalid("seed_stream")
    return seed


def verify_code(code: str, public_key_b64: str, environment: str) -> dict[str, Any]:
    if not isinstance(code, str):
        raise RecoveryInvalid("code_type")
    code = code.strip()
    if not 0 < len(code) <= MAX_CODE or not code.isascii() or any(c.isspace() for c in code):
        raise RecoveryInvalid("code_size_or_whitespace")
    parts = code.split(".")
    if len(parts) != 3 or parts[0] != "TR1":
        raise RecoveryInvalid("code_version")
    payload = decode_b64(parts[1])
    if len(payload) > MAX_SEED:
        raise RecoveryInvalid("seed_size")
    signature = decode_b64(parts[2], 64)
    key = decode_b64(public_key_b64, 32)
    try:
        Ed25519PublicKey.from_public_bytes(key).verify(signature, DOMAIN + payload)
    except InvalidSignature:
        raise RecoveryInvalid("signature") from None
    return parse_seed(payload, environment)


def sign_seed(raw: bytes, private_key: Ed25519PrivateKey, environment: str) -> str:
    seed = parse_seed(raw, environment)
    # Fixed field order/compact UTF8; verification always uses original decoded bytes.
    payload = json.dumps({k: seed[k] for k in (
        "version", "revision", "environment", "peer_ip", "dtls_port", "dtls_spki_sha256",
        "service_classifier", "vk_hashes", "stream_id",
    )}, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    code = "TR1." + encode_b64(payload) + "." + encode_b64(private_key.sign(DOMAIN + payload))
    if len(code) > MAX_CODE:
        raise RecoveryInvalid("code_size")
    return code


class PublicRecoveryCode:
    """One small per-process cache, refreshed on file metadata change; invalid -> unavailable."""

    def __init__(self, settings: Settings):
        self._path = settings.recovery_code_file
        self._key = settings.recovery_verify_key_b64
        self._environment = settings.environment
        self._stamp: tuple[int, ...] | None = None
        self._code: str | None = None

    def get(self) -> str:
        try:
            if not self._path or not self._key:
                raise RecoveryInvalid("not_configured")
            path = Path(self._path)
            stat = path.stat()
            stamp = (stat.st_dev, stat.st_ino, stat.st_size, stat.st_mtime_ns, stat.st_ctime_ns)
            if stamp != self._stamp:
                self._code = None
                self._stamp = None
                if not 0 < stat.st_size <= MAX_CODE + 2:
                    raise RecoveryInvalid("file_size")
                with path.open("rb") as stream:
                    raw = stream.read(MAX_CODE + 3)
                if len(raw) > MAX_CODE + 2:
                    raise RecoveryInvalid("file_size")
                code = raw.decode("ascii").strip()
                verify_code(code, self._key, self._environment)
                self._code, self._stamp = code, stamp
            if self._code is None:
                raise RecoveryInvalid("not_configured")
            return self._code
        except (OSError, ValueError, UnicodeError):
            self._code, self._stamp = None, None
            raise RecoveryUnavailable("RECOVERY_UNAVAILABLE") from None
