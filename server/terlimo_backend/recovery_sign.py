"""Offline operator tool. Private key only from an explicit PEM path, never bot config.

python -m terlimo_backend.recovery_sign --seed-file public-seed.json \
    --private-key-file /operator/private/ed25519.pem --environment test \
    --output recovery-code.txt --public-key-output recovery-verify-key.txt

Only signed public output is written. Do not use the test vector key for deployment.
"""
import argparse
from pathlib import Path

from cryptography.exceptions import UnsupportedAlgorithm
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from .recovery_code import MAX_SEED, RecoveryInvalid, encode_b64, sign_seed


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--seed-file", required=True)
    parser.add_argument("--private-key-file", required=True)
    parser.add_argument("--environment", choices=("test", "production"), required=True)
    parser.add_argument("--output")
    parser.add_argument("--public-key-output")
    args = parser.parse_args()
    try:
        with Path(args.seed_file).open("rb") as stream:
            seed = stream.read(MAX_SEED + 1)
        with Path(args.private_key_file).open("rb") as stream:
            key_bytes = stream.read(16385)
        if len(key_bytes) > 16384:
            raise RecoveryInvalid("key_size")
        key = serialization.load_pem_private_key(key_bytes, password=None)
        if not isinstance(key, Ed25519PrivateKey):
            raise RecoveryInvalid("key_type")
        code = sign_seed(seed, key, args.environment)
        if args.output:
            Path(args.output).write_text(code + "\n", encoding="ascii")
        else:
            print(code)
        if args.public_key_output:
            raw = key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
            Path(args.public_key_output).write_text(encode_b64(raw) + "\n", encoding="ascii")
    except (OSError, ValueError, TypeError, UnsupportedAlgorithm):
        parser.exit(2, "Invalid seed/key or unavailable input/output.\n")


if __name__ == "__main__":
    main()
