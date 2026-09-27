#!/usr/bin/env python3
"""Protected TEST-only provisioning of trusted VK hashes into the STEP03.5 isolated node.

Run as root (sudo). The trusted source is the protected host config
(/etc/terlimo/client-test/backend/config.json, 0600): exactly one node is selected by BOTH
node_id and peer_ip, so the two node hash sets can never be mixed. The node must be stopped.

Secrets never travel through argv: the hashes are written with a protected, atomic offline
update of the node's own profile file (passwords.json): same-file temp + fsync + os.replace,
mode 0600 preserved, all other JSON fields semantically preserved (reserialization, not byte
copy), and a 0600 backup kept next to it. The profile stays 0600 and readable by the package
node, which runs as root in the throwaway network namespace (the helper itself is run via
sudo for the write). The shared /usr/local/bin/wdtt-server is never used or replaced: the
package binary must live inside the owned work dir (`<work>/bin/wdtt-server`).

Never prints hash values: only identity, count, set fingerprint and backup name.
--dry-run performs every check (ownership marker, selection, count/format, node liveness,
binary linkage) without writing.

Usage:
  sudo .venv/bin/python tools/step035/provision_trusted_vk.py --work .step035-runs/pkg \
      --node-id terlimo-test-193-5-251-217 --peer-ip 193.5.251.217 --expected-count 4
  sudo ... --dry-run
  sudo ... --clear
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import sys
import tempfile
from datetime import UTC, datetime
from pathlib import Path

CONFIG_DEFAULT = "/etc/terlimo/client-test/backend/config.json"
SHARED_BIN = "/usr/local/bin/wdtt-server"


def _load_run_module():
    spec = importlib.util.spec_from_file_location(
        "step035_run_isolated", Path(__file__).resolve().parent / "run_isolated.py"
    )
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


def _report(payload: dict) -> int:
    print(json.dumps(payload, sort_keys=True))
    return 0 if payload.get("ok") else 1


def _fail(error: str, **extra) -> int:
    return _report({"ok": False, "error": error, **extra})


def _set_fingerprint(hashes: list[str]) -> str:
    joined = "\n".join(sorted(hashes))
    return hashlib.sha256(joined.encode()).hexdigest()[:16]


def _normalize(raw: str) -> list[str]:
    return [item.strip() for item in raw.split(",") if item.strip()]


def _validate_node_bin(work: Path, node_bin: str) -> Path:
    """The package binary must live inside the owned work dir; the shared install is refused."""
    binary = Path(node_bin)
    if not binary.is_absolute():
        raise ValueError("node binary path must be absolute")
    resolved = binary.resolve()
    try:
        resolved.relative_to(work)
    except ValueError as error:
        raise ValueError("node binary must live inside the owned package work directory") from error
    if str(resolved) == SHARED_BIN:
        raise ValueError("refusing to use the shared /usr/local/bin/wdtt-server")
    if not (resolved.is_file() and os.access(resolved, os.X_OK)):
        raise ValueError("package node binary is missing or not executable")
    return resolved


def _node_running(gateway_dir: Path) -> bool:
    marker = str(gateway_dir).encode()
    for proc in Path("/proc").iterdir():
        if not proc.name.isdigit():
            continue
        try:
            cmdline = (proc / "cmdline").read_bytes()
        except OSError:
            continue
        if b"-config-dir" in cmdline and marker in cmdline:
            return True
    return False


def _select_node(config_path: Path, node_id: str, peer_ip: str) -> tuple[dict, dict]:
    config = json.loads(config_path.read_text())
    available = []
    matches = []
    for entry in config.get("nodes") or []:
        node = entry.get("node") or {}
        available.append({"node_id": node.get("node_id"), "peer_ip": node.get("peer_ip")})
        if node.get("node_id") == node_id and node.get("peer_ip") == peer_ip:
            matches.append(node)
    if len(matches) != 1:
        raise ValueError(f"node selection must match exactly one (got {len(matches)})")
    return matches[0], {"available": available}


def _validate_hashes(node: dict, expected_count: int) -> list[str]:
    hashes = node.get("vk_hashes")
    if not isinstance(hashes, list) or len(hashes) != expected_count:
        raise ValueError("vk_hashes count mismatch")
    for item in hashes:
        if (
            not isinstance(item, str)
            or not item
            or item != item.strip()
            or "," in item
            or any(character.isspace() for character in item)
        ):
            raise ValueError("vk_hashes item format invalid")
    return hashes


def update_profile_file(path: Path, hashes: list[str]) -> dict:
    """Atomic protected offline profile update: preserves every other field and the mode.

    Writes the normalized comma-joined list into admin_profile.vk_hashes, keeps a timestamped
    0600 backup next to the file, then replaces the file via same-directory temp + fsync.
    """
    original = path.read_text()
    document = json.loads(original)
    profile = document.get("admin_profile")
    if not isinstance(profile, dict):
        profile = {}
        document["admin_profile"] = profile
    profile["vk_hashes"] = ",".join(hashes)
    mode = path.stat().st_mode & 0o777
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    backup = path.with_name(f"{path.name}.pre-vk-{stamp}.bak")
    backup.write_text(original)
    os.chmod(backup, 0o600)
    descriptor, temp_name = tempfile.mkstemp(dir=str(path.parent), prefix=f".{path.name}.tmp-")
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(document, ensure_ascii=False, indent=2))
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temp_name, mode or 0o600)
        os.replace(temp_name, path)
    except BaseException:
        try:
            os.unlink(temp_name)
        except OSError:
            pass
        raise
    directory = os.open(str(path.parent), os.O_RDONLY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)
    return {"backup": backup.name, "hashes": len(hashes)}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--work", required=True)
    parser.add_argument("--config", default=CONFIG_DEFAULT)
    parser.add_argument("--node-id", required=True)
    parser.add_argument("--peer-ip", required=True)
    parser.add_argument("--expected-count", type=int, default=4)
    parser.add_argument("--node-bin", default="")
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--clear", action="store_true", help="set an empty vk hash list")
    arguments = parser.parse_args()

    run_module = _load_run_module()
    try:
        # Expected uid comes from the verified work dir stat (not the marker alone); the
        # privileged foreign-uid path is only accepted for root, non-destructive callers.
        probe, _ = run_module.resolve_work(arguments.work, require_owned=False)
        expected_uid = probe.stat().st_uid
        work, _marker = run_module.resolve_work(
            arguments.work, require_owned=True, expected_uid=expected_uid
        )
    except (RuntimeError, OSError) as error:
        return _fail(f"ownership guard refused the work dir: {error}")
    gateway_dir = work / "gateway"
    passwords = gateway_dir / "passwords.json"
    if not passwords.is_file():
        return _fail("passwords.json missing (start the package once before provisioning)")
    node_bin = arguments.node_bin or str(work / "bin" / "wdtt-server")
    try:
        binary = _validate_node_bin(work, node_bin)
    except ValueError as error:
        return _fail(f"package node binary unusable: {error}")
    if not arguments.dry_run and os.geteuid() != 0:
        return _fail("provisioning writes the node profile and must run as root")

    try:
        node, extra = _select_node(Path(arguments.config), arguments.node_id, arguments.peer_ip)
        hashes = _validate_hashes(node, arguments.expected_count)
    except (OSError, ValueError, json.JSONDecodeError) as error:
        return _fail(f"trusted config unusable: {error}")

    if _node_running(gateway_dir):
        return _fail("isolated node is running; stop the package before provisioning")

    try:
        stored = json.loads(passwords.read_text())
    except (OSError, ValueError) as error:
        return _fail(f"cannot read node profile: {error}")
    current = _normalize(str((stored.get("admin_profile") or {}).get("vk_hashes") or ""))
    target = [] if arguments.clear else hashes
    if current == target:
        return _report(
            {
                "ok": True,
                "action": "already-provisioned",
                "node_id": arguments.node_id,
                "peer_ip": arguments.peer_ip,
                "hashes": len(target),
                "set_sha256_16": _set_fingerprint(target),
                "node_bin_sha256_16": hashlib.sha256(binary.read_bytes()).hexdigest()[:16],
            }
        )
    if arguments.dry_run:
        return _report(
            {
                "ok": True,
                "action": "dry-run",
                "node_id": arguments.node_id,
                "peer_ip": arguments.peer_ip,
                "hashes": len(target),
                "set_sha256_16": _set_fingerprint(target),
                "node_bin_sha256_16": hashlib.sha256(binary.read_bytes()).hexdigest()[:16],
                "available_nodes": extra["available"],
            }
        )
    try:
        result = update_profile_file(passwords, target)
        stored = json.loads(passwords.read_text())
    except (OSError, ValueError) as error:
        return _fail(f"provisioning failed: {error}")
    written = _normalize(str((stored.get("admin_profile") or {}).get("vk_hashes") or ""))
    if sorted(written) != sorted(target):
        return _fail("readback mismatch after provisioning", written_count=len(written))
    return _report(
        {
            "ok": True,
            "action": "cleared" if arguments.clear else "provisioned",
            "node_id": arguments.node_id,
            "peer_ip": arguments.peer_ip,
            "hashes": len(target),
            "set_sha256_16": _set_fingerprint(target),
            "backup": result["backup"],
        }
    )


if __name__ == "__main__":
    sys.exit(main())
