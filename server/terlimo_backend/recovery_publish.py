"""Operator-only atomic installation of an ALREADY signed public recovery code.

No private signing key, DB or API is opened. Use recovery_sign for offline signing
from the latest approved endpoint/VK configuration; never sign inside bot/site.
"""
import argparse
import hashlib
import os
import tempfile
from pathlib import Path

from .recovery_code import MAX_CODE, RecoveryInvalid, verify_code


def publish_signed_code(candidate: Path, public_key_file: Path, destination: Path,
                        *, environment: str, minimum_revision: int,
                        expected_current_sha256: str, uid: int, gid: int) -> dict:
    raw = candidate.read_bytes()
    if not 0 < len(raw) <= MAX_CODE + 2:
        raise RecoveryInvalid("file_size")
    code = raw.decode("ascii").strip()
    key = public_key_file.read_text(encoding="ascii").strip()
    seed = verify_code(code, key, environment)
    if int(seed["revision"]) < minimum_revision:
        raise RecoveryInvalid("revision_below_approved_floor")
    if destination.is_symlink():
        raise RecoveryInvalid("destination_symlink")
    before = destination.read_bytes() if destination.exists() else None
    actual_sha = hashlib.sha256(before).hexdigest() if before is not None else "absent"
    if actual_sha != expected_current_sha256:
        raise RecoveryInvalid("preimage_mismatch")
    if before is not None:
        old = before.decode("ascii").strip()
        old_seed = verify_code(old, key, environment)
        if int(seed["revision"]) < int(old_seed["revision"]):
            raise RecoveryInvalid("revision_regression")
        if seed["revision"] == old_seed["revision"] and code != old:
            raise RecoveryInvalid("revision_conflict")
    data = (code + "\n").encode("ascii")
    fd, name = tempfile.mkstemp(prefix=".recovery-", dir=destination.parent)
    temp = Path(name)
    try:
        with os.fdopen(fd, "wb") as stream:
            os.fchmod(stream.fileno(), 0o600)
            os.fchown(stream.fileno(), uid, gid)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        # Sole operator writer; refuse a changed preimage immediately before replacement.
        current = destination.read_bytes() if destination.exists() else None
        if current != before or destination.is_symlink():
            raise RecoveryInvalid("preimage_changed")
        os.replace(temp, destination)
        directory = os.open(destination.parent, os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        temp.unlink(missing_ok=True)
    return {"environment": environment, "revision": seed["revision"],
            "SHA256": hashlib.sha256(data).hexdigest(), "size": len(data),
            "uid": uid, "gid": gid, "mode": "0600", "atomic": True}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--code-file", required=True, type=Path)
    parser.add_argument("--verify-key-file", required=True, type=Path)
    parser.add_argument("--destination", required=True, type=Path)
    parser.add_argument("--environment", required=True, choices=("test", "production"))
    parser.add_argument("--minimum-revision", required=True, type=int)
    parser.add_argument("--expected-current-sha256", required=True)
    parser.add_argument("--uid", required=True, type=int)
    parser.add_argument("--gid", required=True, type=int)
    args = parser.parse_args()
    try:
        publish_signed_code(args.code_file, args.verify_key_file, args.destination,
                            environment=args.environment, minimum_revision=args.minimum_revision,
                            expected_current_sha256=args.expected_current_sha256,
                            uid=args.uid, gid=args.gid)
    except (OSError, ValueError):
        parser.exit(2, "Recovery publication refused: invalid code, revision, preimage or output.\n")


if __name__ == "__main__":
    main()
