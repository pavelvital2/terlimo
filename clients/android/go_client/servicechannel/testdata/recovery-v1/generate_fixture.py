#!/usr/bin/env python3
"""Deterministic Recovery v1 TEST fixture; no operator/production key is used.

Uses Python standard JSON/base64/hashlib and cryptography's standard Ed25519.
The public test key must never be packaged as a deployment verification key.
Run locally: python3 generate_fixture.py
"""

import base64
import hashlib
import json
from pathlib import Path

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey


DOMAIN = b"TERLIMO-RECOVERY-V1\x00"
TEST_SEED = hashlib.sha256(b"TERLIMO RECOVERY V1 TEST ONLY").digest()
PRIVATE_KEY = Ed25519PrivateKey.from_private_bytes(TEST_SEED)
PUBLIC_KEY = PRIVATE_KEY.public_key().public_bytes(
    serialization.Encoding.Raw, serialization.PublicFormat.Raw
)


def b64(raw):
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def compact(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


def code(payload, private_key=PRIVATE_KEY):
    return "TR1." + b64(payload) + "." + b64(private_key.sign(DOMAIN + payload))


def fixture():
    seed = {
        "version": 1,
        "revision": "2",
        "environment": "test",
        "peer_ip": "192.0.2.99",
        "dtls_port": 56001,
        "dtls_spki_sha256": b64(hashlib.sha256(b"RECOVERY V1 TEST PIN").digest()),
        "service_classifier": "synthetic-public-classifier-recovered",
        "vk_hashes": ["test-only-vk-new-a", "test-only-vk-new-b"],
        "stream_id": 3,
    }
    raw = compact(seed)
    unknown = dict(seed, subscription="not-a-grant")
    missing = dict(seed)
    del missing["stream_id"]
    null = dict(seed, stream_id=None)
    future = dict(seed, version=2)
    foreign = dict(seed, environment="production")
    foreign_key = Ed25519PrivateKey.from_private_bytes(
        hashlib.sha256(b"TERLIMO FOREIGN TEST KEY ONLY").digest()
    )
    return {
        "test_only": True,
        "signing_seed_derivation": "SHA256(TERLIMO RECOVERY V1 TEST ONLY); never use for deployments",
        "public_key_b64": b64(PUBLIC_KEY),
        "expected_seed": seed,
        "payload_utf8": raw.decode("utf-8"),
        "code": code(raw),
        "whitespace_payload_code": code(
            json.dumps(dict(reversed(list(seed.items()))), indent=2).encode("utf-8")
        ),
        "duplicate_key_code": code(raw.replace(b'"version":1', b'"version":1,"version":1', 1)),
        "unknown_key_code": code(compact(unknown)),
        "missing_stream_code": code(compact(missing)),
        "null_stream_code": code(compact(null)),
        "future_version_code": code(compact(future)),
        "foreign_environment_code": code(compact(foreign)),
        "foreign_signature_code": code(raw, foreign_key),
    }


if __name__ == "__main__":
    output = Path(__file__).with_name("fixture.json")
    output.write_text(json.dumps(fixture(), ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"Wrote deterministic TEST-only fixture: {output.name}")
