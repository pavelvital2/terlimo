#!/usr/bin/env python3
"""STEP036 proposed isolated Android TEST server package, adapted from STEP035.

Only --phone --serve is allowed for startup. Root review is required before any start.
The existing A/B and STEP035 services are separate and must remain running.
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import hashlib
import importlib.util
import ipaddress
import json
import os
import secrets
import shutil
import subprocess
import sys
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2]
# Canonical dedicated work root: every package path must live below it, with an ownership
# marker, before any destructive action. Nothing outside is ever removed or signalled.
WORK_ROOT = Path("/home/pavel/step036-device-stage")
RUN_DIR = Path(os.environ.get("XDG_RUNTIME_DIR") or f"/tmp/terlimo-036-{os.getpid()}")
RUN_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
os.environ["XDG_RUNTIME_DIR"] = str(RUN_DIR)
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "tools"))

import aiohttp
import gateway_integration_03_3 as base
import gateway_integration_mtls_03_3 as mtls
from cryptography import x509
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ec

from terlimo_backend.gateway_control import revoke_binding_grants
from terlimo_backend.config import Settings
from terlimo_backend.evidence_transport import EvidenceRelay
from terlimo_backend.service_relay import ServiceRelay
from terlimo_backend.management_handler import HandlerConfig, ManagementHandler
from terlimo_backend.migrations import runner

NODE_ID = "terlimo-036-node"
SERVER_NAME = "gw-036.test"
BACKEND_IDENTITY = "terlimo-036-backend"
MAX_WORKERS = 36
API_HOST = "127.0.0.1"
API_PORT = 18092
GW_LISTEN = 56340
GW_WG = 56342
HANDLER_PORT = 56341
MOBILE = "/api/mobile/v1"
CONTRACT_DIR = Path(
    os.environ.get("TERLIMO_CONTRACT_DIR", "/home/pavel/projects/terlimo-s1-contracts")
)
# phone-ready (external) mode: network prepared by tools/step036/device_network.py (owner)
EXTERNAL_IP = "193.5.251.217"
PHONE_NS = "terlimo-036"
PHONE_DTLS = 57500
PHONE_WG = 57501
NODE_BIN_DEFAULT = "/home/pavel/step036-device-stage/pkg/bin/wdtt-server"
# Resolved from --node-bin / STEP036_NODE_BIN in main(); the shared default is never replaced
# by the package. Identity checks below compare against this exact resolved path.
SERVER_BIN = NODE_BIN_DEFAULT


def say(message: str) -> None:
    print(f"[step036] {message}", flush=True)


MARKER_NAME = ".step036-owner.json"
MARKER_VERSION = 1


def guard_error(reason: str) -> RuntimeError:
    return RuntimeError(f"step036 refused: {reason}")


def load_marker(work: Path) -> dict | None:
    marker_path = work / MARKER_NAME
    if not marker_path.is_file() or marker_path.is_symlink():
        return None
    try:
        marker = json.loads(marker_path.read_text(encoding="utf-8"))
    except ValueError:
        return None
    return marker if isinstance(marker, dict) else None


def write_marker(work: Path, run_id: str, created_at: str) -> None:
    marker_path = work / MARKER_NAME
    data = {
        "version": MARKER_VERSION,
        "work": str(work),
        "uid": os.getuid(),
        "run_id": run_id,
        "created_at": created_at,
    }
    marker_path.write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")
    os.chmod(marker_path, 0o600)


def resolve_work(
    raw: str,
    *,
    require_owned: bool,
    for_destructive: bool = False,
    expected_uid: int | None = None,
    caller_uid: int | None = None,
) -> tuple[Path, dict | None]:
    """Resolve a package work path and enforce the ownership/symlink/root guards.

    The default (`expected_uid=None`) keeps the historical rule: marker uid must equal the
    caller uid. A *foreign* uid (e.g. root provisioning a user-owned work dir) is accepted only
    through the explicit privileged path: the caller is root, the operation is not destructive,
    and `expected_uid` - taken by the caller from the verified work dir `stat`, not from the
    marker alone - as well as the actual work dir uid and the marker uid must all agree.
    """
    caller = os.getuid() if caller_uid is None else caller_uid
    raw_path = Path(raw).expanduser()
    probe = raw_path
    while True:
        if probe.is_symlink():
            raise guard_error(f"symlink path component: {probe}")
        if probe == probe.parent:
            break
        probe = probe.parent
    work = raw_path.resolve()
    root = WORK_ROOT.resolve()
    if work == root:
        raise guard_error("work must be a dedicated subdirectory of the work root, not the root")
    if root not in work.parents:
        raise guard_error(f"work must live inside {root}")
    marker = load_marker(work)
    if require_owned or for_destructive:
        if marker is None:
            raise guard_error(f"ownership marker missing in {work}")
        if marker.get("version") != MARKER_VERSION:
            raise guard_error("ownership marker version mismatch")
        want = caller if expected_uid is None else expected_uid
        if want != caller and (caller != 0 or for_destructive):
            raise guard_error("foreign uid override requires root and is not destructive")
        owner = work.stat().st_uid
        if marker.get("work") != str(work) or marker.get("uid") != want or owner != want:
            raise guard_error("ownership marker mismatch (foreign or moved work dir)")
    return work, marker


def proc_starttime(pid: int) -> str | None:
    try:
        stat = Path(f"/proc/{pid}/stat").read_text(encoding="utf-8")
    except OSError:
        return None
    # field 22 (1-based) after the command name in parentheses
    tail = stat.rsplit(")", 1)[-1].split()
    return tail[19] if len(tail) > 19 else None


def proc_cmdline(pid: int) -> str | None:
    try:
        raw = Path(f"/proc/{pid}/cmdline").read_bytes()
    except OSError:
        return None
    return raw.replace(b"\x00", b" ").decode("utf-8", "replace").strip()


def pid_record_path(work: Path, name: str) -> Path:
    return work / "logs" / f"{name}.pid"


def write_pid_record(work: Path, name: str, pid: int, kind: str) -> None:
    record = {"pid": pid, "starttime": proc_starttime(pid), "kind": kind}
    path = pid_record_path(work, name)
    path.write_text(json.dumps(record) + "\n", encoding="utf-8")
    os.chmod(path, 0o600)


def _kind_token(work: Path, kind: str) -> str:
    if kind == "serve":
        return "device_stage.py"
    if kind == "gateway":
        return f"-config-dir {work / 'gateway'}"
    return f"terlimo-{kind}"


def record_identity_valid(record: dict) -> bool:
    """A record counts only with a usable PID and a real /proc starttime value."""
    try:
        pid = int(record["pid"])
    except (KeyError, TypeError, ValueError):
        return False
    starttime = record.get("starttime")
    return not (
        pid <= 0
        or not isinstance(starttime, str)
        or not starttime.isdigit()
        or starttime == "0"
    )


def verified_alive(record: dict) -> bool:
    """True only when the recorded PID is alive AND still has the recorded identity."""
    try:
        pid = int(record["pid"])
    except (KeyError, TypeError, ValueError):
        return False
    expected_start = record.get("starttime")
    return expected_start is not None and proc_starttime(pid) == expected_start


def _cmdline_matches(work: Path, record: dict) -> bool:
    try:
        pid = int(record["pid"])
    except (KeyError, TypeError, ValueError):
        return False
    kind = str(record.get("kind") or "")
    return _kind_token(work, kind) in (proc_cmdline(pid) or "")


def stop_one(
    work: Path, record: dict, *, term_timeout: float = 20.0, escalate: bool = True
) -> dict:
    """Terminate one recorded process with identity checks, bounded wait and escalation.

    Never signals a PID whose /proc identity does not match the record; escalation (SIGKILL)
    is limited to the same verified own process.
    """
    try:
        pid = int(record["pid"])
    except (KeyError, TypeError, ValueError):
        return {"pid": None, "action": "skipped", "reason": "unreadable record"}
    if not verified_alive(record):
        return {"pid": pid, "action": "already_gone", "verified": True}
    if not _cmdline_matches(work, record):
        return {"pid": pid, "action": "skipped", "reason": "cmdline mismatch"}
    kind = str(record.get("kind") or "")
    if kind == "gateway":
        subprocess.run(["sudo", "-n", "kill", "-TERM", str(pid)], check=False)
    else:
        try:
            os.kill(pid, 15)
        except ProcessLookupError:
            return {"pid": pid, "action": "already_gone", "verified": True}
    deadline = time.monotonic() + term_timeout
    while time.monotonic() < deadline:
        if not verified_alive(record):
            return {"pid": pid, "action": "term_exit", "verified": True}
        time.sleep(0.2)
    if not escalate:
        return {"pid": pid, "action": "term_timeout", "verified": False}
    if not verified_alive(record) or not _cmdline_matches(work, record):
        return {"pid": pid, "action": "identity_changed", "verified": False}
    if kind == "gateway":
        subprocess.run(["sudo", "-n", "kill", "-KILL", str(pid)], check=False)
    else:
        try:
            os.kill(pid, 9)
        except ProcessLookupError:
            return {"pid": pid, "action": "already_gone", "verified": True}
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if not verified_alive(record):
            return {"pid": pid, "action": "kill_exit", "verified": True}
        time.sleep(0.2)
    return {"pid": pid, "action": "kill_timeout", "verified": False}


def stop_recorded_processes(
    work: Path, *, marker: dict | None = None, term_timeout: float = 20.0
) -> dict:
    """Stop only recorded, identity-verified processes; explicit per-PID result."""
    if marker is None:
        marker = load_marker(work)
    if marker is None or marker.get("work") != str(work) or marker.get("uid") != os.getuid():
        raise guard_error("refusing to stop processes without a valid ownership marker")
    results: list[dict] = []
    present: set[str] = set()
    candidates = sorted((work / "logs").glob("*.pid")) if (work / "logs").is_dir() else []
    for record_path in candidates:
        kind = record_path.stem
        present.add(kind)
        try:
            record = json.loads(record_path.read_text(encoding="utf-8"))
            record.setdefault("kind", kind)
        except (ValueError, OSError):
            results.append(
                {"pid": None, "kind": kind, "action": "unreadable", "verified": False,
                 "reason": f"{record_path.name} unreadable"}
            )
            continue
        results.append({"kind": kind, **stop_one(work, record, term_timeout=term_timeout)})
    for kind in expected_kinds(work):
        if kind not in present:
            results.append(
                {"pid": None, "kind": kind, "action": "missing_record", "verified": False,
                 "reason": "no ownership record: cannot prove this component was stopped"}
            )
    failed = [
        item
        for item in results
        if item.get("verified") is False
        or item["action"] in ("missing_record", "unreadable", "skipped", "term_timeout", "kill_timeout", "identity_changed")
    ]
    return {
        "results": results,
        "stopped": [item["pid"] for item in results if item["action"] in ("term_exit", "kill_exit")],
        "already_gone": [item["pid"] for item in results if item["action"] == "already_gone"],
        "failed": failed,
        "verified_exit": not failed,
    }


def stop_package(work: Path) -> dict:
    """Prepared safe stop: verify ownership, stop serve first, then the remaining own records.

    Does not touch PostgreSQL, network resources or the work tree; those are separate
    explicit steps in the reviewed stop order.
    """
    work, marker = resolve_work(str(work), require_owned=True, for_destructive=True)
    report = {"work": str(work), "steps": {}}
    if not marker:
        raise guard_error("ownership marker missing")
    serve_record_path = pid_record_path(work, "serve")
    if serve_record_path.is_file():
        try:
            serve_record = json.loads(serve_record_path.read_text(encoding="utf-8"))
        except ValueError:
            serve_record = None
        if serve_record is not None:
            if not verified_alive(serve_record):
                report["steps"]["serve"] = {"pid": serve_record.get("pid"), "action": "already_gone", "verified": True}
            elif serve_record.get("pid") == os.getpid():
                raise guard_error("refusing to stop the current process")
            else:
                try:
                    report["steps"]["serve"] = stop_one(work, serve_record, term_timeout=30.0)
                except Exception as error:
                    report["steps"]["serve"] = {"action": "stop_error", "verified": False, "reason": str(error)}
    for name in ("api", "worker", "gateway"):
        record_path = pid_record_path(work, name)
        if not record_path.is_file():
            continue
        try:
            record = json.loads(record_path.read_text(encoding="utf-8"))
        except ValueError:
            continue
        try:
            report["steps"][name] = stop_one(work, record, term_timeout=15.0)
        except Exception as error:
            report["steps"][name] = {"action": "stop_error", "verified": False, "reason": str(error)}
    all_steps = list(report["steps"].values())
    report["verified_exit"] = bool(all_steps) and all(
        step.get("verified", True) for step in all_steps
    )
    return report


def pg_bin(name: str) -> Path:
    import pgserver

    return Path(pgserver.__file__).resolve().parent / "pginstall" / "bin" / name


def expected_kinds(work: Path) -> list[str]:
    kinds = ["serve", "api", "worker"]
    state_path = work / "state.json"
    phone_ready = False
    if state_path.is_file():
        try:
            phone_ready = bool(json.loads(state_path.read_text(encoding="utf-8")).get("phone_ready"))
        except ValueError:
            phone_ready = False
    if phone_ready or (work / "gateway").exists():
        kinds.append("gateway")
    return kinds


class SystemProbe:
    """Read-only /proc + kernel facts used to prove ownership of a live package."""

    def processes(self) -> list[dict]:
        rows: list[dict] = []
        for entry in Path("/proc").iterdir():
            if not entry.name.isdigit():
                continue
            pid = int(entry.name)
            try:
                cmdline = (
                    Path(f"/proc/{pid}/cmdline").read_bytes().replace(b"\x00", b" ").decode("utf-8", "replace").strip()
                )
                stat_tail = Path(f"/proc/{pid}/stat").read_text(encoding="utf-8").rsplit(")", 1)[-1].split()
                starttime = stat_tail[19]
                ppid = stat_tail[1]
            except (OSError, IndexError):
                continue
            env_work = False
            try:
                env_work = b".step036-device-runs/" in Path(f"/proc/{pid}/environ").read_bytes()
            except OSError:
                env_work = None  # unreadable (root-owned) is not proof
            exe = None
            try:
                exe = os.readlink(f"/proc/{pid}/exe")
            except OSError:
                exe = None
            rows.append(
                {
                    "pid": pid,
                    "ppid": ppid,
                    "cmdline": cmdline,
                    "starttime": starttime,
                    "env_work": env_work,
                    "exe": exe,
                }
            )
        return rows

    def env_contains(self, pid: int, token: str) -> bool | None:
        try:
            blob = Path(f"/proc/{pid}/environ").read_bytes()
        except OSError:
            return None
        return token.encode() in blob

    def cwd(self, pid: int) -> str | None:
        try:
            return os.readlink(f"/proc/{pid}/cwd")
        except OSError:
            return None

    def ns_identify(self, pid: int) -> str | None:
        result = subprocess.run(
            ["sudo", "-n", "ip", "netns", "identify", str(pid)],
            capture_output=True,
            text=True,
            check=False,
        )
        name = result.stdout.strip()
        return name or None

    def ns_pids(self, name: str) -> list[int]:
        result = subprocess.run(
            ["sudo", "-n", "ip", "netns", "pids", name],
            capture_output=True,
            text=True,
            check=False,
        )
        return [int(item) for item in result.stdout.split() if item.isdigit()]

    def root_exe(self, pid: int) -> str | None:
        result = subprocess.run(
            ["sudo", "-n", "readlink", f"/proc/{pid}/exe"],
            capture_output=True,
            text=True,
            check=False,
        )
        return result.stdout.strip() or None


def collect_live_components(work: Path, probe: Any = None) -> tuple[dict, list[str]]:
    """Prove the existing live package from work/cwd/parent/env/ns/exe/starttime.

    Returns ({kind: pid}, proof_lines) or raises guard_error listing every missing proof.
    No bare old PIDs and no broad process kill: only exact, cross-checked evidence is used.
    """
    probe = probe or SystemProbe()
    processes = probe.processes()
    proofs: list[str] = []
    found: dict[str, int] = {}
    problems: list[str] = []

    api_like = {
        row["pid"]: row
        for row in processes
        if "/terlimo-api" in row["cmdline"] or row["exe"] == str(Path("/home/pavel/projects/terlimo-backend/.venv/bin") / "terlimo-api")
    }
    worker_like = {
        row["pid"]: row
        for row in processes
        if "/terlimo-worker" in row["cmdline"] or row["exe"] == str(Path("/home/pavel/projects/terlimo-backend/.venv/bin") / "terlimo-worker")
    }
    work_token = str(work)
    api = [row for row in api_like.values() if probe.env_contains(row["pid"], work_token) is True]
    worker = [row for row in worker_like.values() if probe.env_contains(row["pid"], work_token) is True]
    if len(api) != 1:
        problems.append(f"api: expected exactly one env-linked process, got {len(api)}")
    if len(worker) != 1:
        problems.append(f"worker: expected exactly one env-linked process, got {len(worker)}")
    if problems:
        raise guard_error("adoption refused: " + "; ".join(problems))
    api_row, worker_row = api[0], worker[0]
    if api_row["ppid"] != worker_row["ppid"]:
        raise guard_error("adoption refused: api/worker parent mismatch")
    serve_pid = int(api_row["ppid"])
    serve_row = next((row for row in processes if row["pid"] == serve_pid), None)
    if (
        serve_row is None
        or "device_stage.py" not in serve_row["cmdline"]
        or "--serve" not in serve_row["cmdline"]
        or probe.cwd(serve_pid) != str(ROOT)
    ):
        raise guard_error("adoption refused: serve parent linkage/cwd unproven")
    found["serve"] = serve_pid
    found["api"] = api_row["pid"]
    found["worker"] = worker_row["pid"]
    proofs.append(f"serve {serve_pid}: cmdline run_isolated --serve, cwd {ROOT}, starttime {serve_row['starttime']}")
    proofs.append(f"api {api_row['pid']}: ppid=serve, env work root, starttime {api_row['starttime']}")
    proofs.append(f"worker {worker_row['pid']}: ppid=serve, env work root, starttime {worker_row['starttime']}")

    if "gateway" in expected_kinds(work):
        config = f"-config-dir {work / 'gateway'}"
        candidates = [
            row
            for row in processes
            if config in row["cmdline"]
            and row["cmdline"].lstrip().startswith(SERVER_BIN)
        ]
        if len(candidates) != 1:
            raise guard_error(f"adoption refused: gateway node not uniquely proven ({len(candidates)})")
        node = candidates[0]
        if probe.ns_identify(node["pid"]) != "terlimo-036":
            raise guard_error("adoption refused: node not proven inside netns terlimo-036")
        ns_pids = probe.ns_pids("terlimo-036")
        if ns_pids != [node["pid"]]:
            raise guard_error(f"adoption refused: netns pids {ns_pids} != node {node['pid']}")
        if (probe.root_exe(node["pid"]) or "") != SERVER_BIN:
            raise guard_error("adoption refused: node executable unproven")
        found["gateway"] = node["pid"]
        proofs.append(
            f"gateway {node['pid']}: config-dir work/gateway, netns terlimo-036 singleton, "
            f"exe wdtt-server, starttime {node['starttime']}"
        )
    return found, proofs


def adopt_live_package(work: Path, probe: Any = None) -> dict:
    """Write PID records for the already-running package only after full ownership proof."""
    work, marker = resolve_work(str(work), require_owned=True, for_destructive=True)
    if not marker:
        raise guard_error("ownership marker missing")
    if not (work / "state.json").is_file():
        raise guard_error("state.json missing: this work dir was not started by this package")
    found, proofs = collect_live_components(work, probe=probe)
    processes = (probe or SystemProbe()).processes()
    by_pid = {row["pid"]: row for row in processes}
    for kind, pid in found.items():
        row = by_pid[pid]
        write_pid_record(work, kind, pid, kind=kind)
        # write_pid_record reads /proc starttime; for root-owned nodes use the probe value if needed
        if row.get("starttime"):
            record_path = pid_record_path(work, kind)
            record = json.loads(record_path.read_text(encoding="utf-8"))
            if record.get("starttime") != row["starttime"]:
                record["starttime"] = row["starttime"]
                record_path.write_text(json.dumps(record) + "\n", encoding="utf-8")
    return {"work": str(work), "adopted": found, "proofs": proofs}


def remove_owned_work(work: Path) -> None:
    """Remove only an owned work tree; the root-owned gateway subdir is removed via sudo."""
    work, _marker = resolve_work(str(work), require_owned=True, for_destructive=True)
    gateway_dir = work / "gateway"
    if gateway_dir.exists():
        if gateway_dir.is_symlink() or not gateway_dir.is_dir():
            raise guard_error("gateway path is not a plain directory")
        subprocess.run(["sudo", "-n", "rm", "-rf", str(gateway_dir)], check=False)
    shutil.rmtree(work, ignore_errors=True)


def load_contract_pop():
    spec = importlib.util.spec_from_file_location(
        "step036_contract_pop", CONTRACT_DIR / "auth" / "pop_canonical.py"
    )
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


class NetnsPhoneGateway(base.IsolatedGateway):
    """Real wdtt-server inside the named netns with external DNAT (owner-applied networking)."""

    def __init__(self, work: str) -> None:
        super().__init__(work, PHONE_DTLS, PHONE_WG)

    def start(self) -> None:
        os.makedirs(self.work, mode=0o700, exist_ok=True)
        self._socket_ready = False
        cert = Path(self.creds) / "wl-test-dtls-cert"
        key = Path(self.creds) / "wl-test-dtls-key"
        if not (cert.is_file() and key.is_file()):
            raise guard_error("pre-staged STEP036 DTLS cert/key required")
        inner = (
            f"CREDENTIALS_DIRECTORY={self.creds} WL_TEST_ENABLED=1 WL_TEST_NODE_ID={NODE_ID} "
            f"WL_TEST_WG_ISOLATION_CONFIRMED=1 WL_TEST_BACKEND_SOCKET={self.work}/../evidence-relay.sock "
            f"WL_TEST_SERVICE_ENABLED=1 WL_TEST_SERVICE_SEED=\"$(cat {self.work}/../secrets/service-seed)\" "
            f"WL_TEST_SERVICE_BACKEND_SOCKET={self.work}/../service-relay.sock "
            f"exec {SERVER_BIN} -listen 0.0.0.0:{self.listen_port} -wg-port {self.wg_port} "
            f"-config-dir {self.work} -max-workers-per-access {base.MAX_WORKERS} "
            f"-password {base.MAIN_PASSWORD} -admin 1 -bot-token fixture -dns 1.1.1.1 "
            f"-wg-backend userspace"
        )
        self.log = Path(self.log_path).open("ab")  # noqa: SIM115 - closed in stop()
        self.proc = subprocess.Popen(
            ["sudo", "-n", "ip", "netns", "exec", PHONE_NS, "bash", "-c", inner],
            stdout=self.log,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )


class PopClient:
    """Minimal client-side PoP signer using the accepted contract module."""

    def __init__(self, contract_pop) -> None:
        self._pop = contract_pop
        self.private = ec.generate_private_key(ec.SECP256R1())
        self.der = self.private.public_key().public_bytes(
            serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo
        )
        self.spki_b64 = contract_pop.b64url_encode(self.der)
        self.fingerprint = hashlib.sha256(self.der).hexdigest()

    def proof(self, challenge: dict, *, op: str, scope: str, extra: dict) -> dict:
        request_id = secrets.token_hex(16)
        nonce = challenge["nonce_b64"]
        payload = {
            "env": "test",
            "scope": scope,
            "op": op,
            "installation_id": self.fingerprint,
            "ts": datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "nonce": nonce,
            "request_id": request_id,
        }
        payload.update(extra)
        payload_bytes = self._pop.canonical_json(payload)
        message = self._pop.pop_message(
            request_id, challenge["challenge_id"], nonce, payload_bytes
        )
        return {
            "algorithm": "ES256",
            "signature_b64": self._pop.sign(self.private, message),
            "request_id": request_id,
            "challenge_id": challenge["challenge_id"],
            "nonce_b64": nonce,
            "payload_hash": hashlib.sha256(payload_bytes).hexdigest(),
            "signed_payload_b64": self._pop.b64url_encode(payload_bytes),
            "environment": "test",
        }


def make_pki(pki_dir: Path) -> dict:
    pki_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    ca_key, ca_cert = mtls.make_ca("terlimo-036-test-ca")
    mtls.write_key_cert(pki_dir / "ca.key", pki_dir / "ca.pem", ca_key, ca_cert)
    server_key, server_cert = mtls.make_leaf(
        ca_key,
        ca_cert,
        SERVER_NAME,
        san=[
            x509.DNSName(SERVER_NAME),
            x509.IPAddress(ipaddress.ip_address("127.0.0.1")),
        ],
    )
    mtls.write_key_cert(pki_dir / "server.key", pki_dir / "server.pem", server_key, server_cert)
    backend_key, backend_cert = mtls.make_leaf(ca_key, ca_cert, BACKEND_IDENTITY)
    mtls.write_key_cert(pki_dir / "backend.key", pki_dir / "backend.pem", backend_key, backend_cert)
    node_key, node_cert = mtls.make_leaf(ca_key, ca_cert, NODE_ID)
    mtls.write_key_cert(pki_dir / "node.key", pki_dir / "node.pem", node_key, node_cert)
    for item in pki_dir.iterdir():
        os.chmod(item, 0o600)
    os.chmod(pki_dir, 0o700)
    return {
        "ca_file": str(pki_dir / "ca.pem"),
        "server_cert": str(pki_dir / "server.pem"),
        "server_key": str(pki_dir / "server.key"),
        "backend_cert": str(pki_dir / "backend.pem"),
        "backend_key": str(pki_dir / "backend.key"),
        "node_cert": str(pki_dir / "node.pem"),
        "node_key": str(pki_dir / "node.key"),
    }


async def db_connect(database_url: str):
    """Raw connection with the jsonb codec the Backend pool normally provides."""
    connection = await base.asyncpg.connect(database_url, timeout=10)
    await connection.set_type_codec(
        "jsonb", schema="pg_catalog", encoder=json.dumps, decoder=json.loads
    )
    return connection


def dtls_pin(creds_dir: Path) -> str:
    """Advertised pin comes from the DTLS cert the real node loads (CREDENTIALS_DIRECTORY),
    never from the management mTLS PKI."""
    return spki_pin(creds_dir / "wl-test-dtls-cert")


def spki_pin(cert_path: Path) -> str:
    cert = x509.load_pem_x509_certificate(cert_path.read_bytes())
    der = cert.public_key().public_bytes(
        serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo
    )
    return base64.urlsafe_b64encode(hashlib.sha256(der).digest()).rstrip(b"=").decode("ascii")


async def get_json(session: aiohttp.ClientSession, url: str, **kwargs):
    async with session.get(url, **kwargs) as response:
        return response.status, await response.json()


async def post_json(session: aiohttp.ClientSession, url: str, payload: dict, **kwargs):
    async with session.post(url, json=payload, **kwargs) as response:
        return response.status, await response.json()


def start_process(name: str, command: list[str], env: dict, log_path: Path) -> subprocess.Popen:
    log = log_path.open("ab")
    process = subprocess.Popen(
        command, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True
    )
    write_pid_record(log_path.parent.parent, name, process.pid, kind=name)
    return process


async def wait_ready(base_url: str, timeout: float = 30.0) -> None:
    deadline = time.monotonic() + timeout
    last = None
    async with aiohttp.ClientSession() as session:
        while time.monotonic() < deadline:
            try:
                status, body = await get_json(session, f"{base_url}/health/ready")
                if status == 200:
                    return
                last = (status, body)
            except aiohttp.ClientError as exc:
                last = str(exc)
            await asyncio.sleep(0.2)
    raise RuntimeError(f"API not ready: {last}")


async def wait_catalog(
    session: aiohttp.ClientSession, base_url: str, headers: dict, timeout: float = 45.0
) -> dict:
    deadline = time.monotonic() + timeout
    last: dict | None = None
    while time.monotonic() < deadline:
        status, body = await get_json(session, f"{base_url}{MOBILE}/gateways", headers=headers)
        if status == 200:
            return body
        last = {"http": status, "body": body}
        await asyncio.sleep(0.3)
    raise RuntimeError(f"catalog did not apply: {json.dumps(last)[:400]}")


async def wait_postgres(admin_dsn: str, timeout: float = 30.0) -> None:
    deadline = time.monotonic() + timeout
    last: Exception | None = None
    while time.monotonic() < deadline:
        try:
            connection = await base.asyncpg.connect(admin_dsn, timeout=5)
            await connection.fetchval("SELECT 1")
            await connection.close()
            return
        except Exception as exc:  # noqa: BLE001 - bounded readiness probe
            last = exc
            await asyncio.sleep(0.3)
    raise RuntimeError(f"package PostgreSQL not ready: {last}")


async def ensure_database(admin_dsn: str, name: str) -> str:
    """Create the package database only when absent; never drop/recreate on restart."""
    connection = await base.asyncpg.connect(admin_dsn, timeout=10)
    try:
        exists = await connection.fetchval("SELECT 1 FROM pg_database WHERE datname = $1", name)
        if not exists:
            await connection.execute(f'CREATE DATABASE "{name}"')
    finally:
        await connection.close()
    return "created" if not exists else "reused"


async def run_loop(arguments: argparse.Namespace) -> dict:
    work, existing_marker = resolve_work(
        arguments.work, require_owned=False, for_destructive=False
    )
    if arguments.lifecycle_selftest and work.exists():
        # Test-only harness: it must never adopt or touch real existing packages.
        raise guard_error("lifecycle selftest requires a fresh, isolated work dir")
    if work.exists() and not work.is_dir():
        raise guard_error("work path exists and is not a directory")
    if work.exists() and existing_marker is None and any(work.iterdir()):
        raise guard_error("work dir exists without an ownership marker; not adopted")
    work.mkdir(mode=0o700, parents=True, exist_ok=True)
    run_id = (existing_marker or {}).get("run_id") or secrets.token_hex(8)
    created_at = (existing_marker or {}).get("created_at") or datetime.now(UTC).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    )
    write_marker(work, run_id, created_at)
    logs = work / "logs"
    logs.mkdir(mode=0o700, exist_ok=True)
    write_pid_record(work, "serve", os.getpid(), kind="serve")
    report: dict = {
        "work": str(work),
        "profile": "STEP036 isolated device TEST; no existing customer import",
        "steps": {},
    }
    gateway = None
    handler = None
    server = None
    evidence_relay = None
    service_relay = None
    relay_settings = None
    processes: list[subprocess.Popen] = []
    gateway_main_password = secrets.token_urlsafe(24)
    base.NODE_ID = NODE_ID
    base.MAX_WORKERS = MAX_WORKERS
    base.MAIN_PASSWORD = gateway_main_password
    contract_pop = load_contract_pop()

    try:
        if arguments.lifecycle_selftest:
            # Test-only lifecycle probe: exercises the real main()/run_loop try/finally and
            # signal path with an owned work dir but no PostgreSQL, node, network or API.
            (work / "pg").mkdir(mode=0o700, exist_ok=True)
            (work / "pg" / "PGDATA_DUMMY").write_text("synthetic pgdata marker\n")
            (work / "profile.dummy").write_text(
                json.dumps({"admin_profile": {"vk_hashes": "dummy"}}) + "\n"
            )
            dummy = subprocess.Popen(
                [sys.executable, "-c", "import time; time.sleep(300)"],
                start_new_session=True,
            )
            processes.append(dummy)
            write_pid_record(work, "dummy", dummy.pid, kind="dummy")
            report["steps"]["lifecycle_selftest"] = {"ready": True, "child": dummy.pid}
            stop = asyncio.Event()
            loop = asyncio.get_running_loop()
            import signal as signal_module

            for signal_name in ("SIGINT", "SIGTERM"):
                loop.add_signal_handler(getattr(signal_module, signal_name), stop.set)
            print(
                json.dumps({"lifecycle_selftest_ready": True, "child": dummy.pid}),
                flush=True,
            )
            print(json.dumps(report, indent=2), flush=True)
            await stop.wait()
            return report
        pg_cleanup_mode = None if arguments.keep else "stop"
        server = base.pgserver.get_server(str(work / "pg"), cleanup_mode=pg_cleanup_mode)
        server.ensure_pgdata_inited()
        server.ensure_postgres_running()
        admin_dsn = server.get_uri()
        await wait_postgres(admin_dsn)
        db_state = await ensure_database(admin_dsn, "terlimo_036")
        report["steps"]["package_database"] = db_state
        parts = base.urlsplit(admin_dsn)
        database_url = base.urlunsplit(
            (parts.scheme, parts.netloc, "/terlimo_036", parts.query, parts.fragment)
        )
        connection = await base.asyncpg.connect(database_url, timeout=10)
        await runner.apply_migrations(connection)
        await connection.close()
        report["steps"]["postgres_and_migrations"] = "ok (STEP036 current migrations, db " + db_state + ")"

        env = dict(os.environ)
        onboarding_key_file = work / "secrets" / "onboarding-key"
        service_seed_file = work / "secrets" / "service-seed"
        vk_file = work / "secrets" / "vk-hashes.json"
        if not all(path.is_file() for path in (onboarding_key_file, service_seed_file, vk_file)):
            raise guard_error("pre-staged STEP036 onboarding key, service seed and VK hashes required")
        vk_hashes = json.loads(vk_file.read_text())
        if not isinstance(vk_hashes, list) or len(vk_hashes) != 4 or any(
            not isinstance(item, str) or not item for item in vk_hashes
        ):
            raise guard_error("protected STEP036 VK hash set must contain exactly four hashes")
        env.update(
            {
                "DATABASE_URL": database_url,
                "PYTHONPATH": str(ROOT) + os.pathsep + env.get("PYTHONPATH", ""),
                "STEP036_WORK_ROOT": str(work),
                "TERLIMO_ENV": "test",
                "API_HOST": API_HOST,
                "API_PORT": str(arguments.api_port),
                "GATEWAY_LOCAL_ADMIN_ENABLED": "false",
                "ONBOARDING_SECRET_KEY": onboarding_key_file.read_text().strip(),
                "LOG_LEVEL": "INFO",
            }
        )

        if arguments.phone and not arguments.no_gateway:
            namespaces_result = await asyncio.to_thread(
                subprocess.run,
                ["ip", "netns", "list"],
                capture_output=True,
                text=True,
                check=False,
            )
            namespaces = namespaces_result.stdout.split()
            if PHONE_NS not in namespaces:
                raise RuntimeError(
                    f"namespace {PHONE_NS} is absent: apply tools/step036/device_network.py "
                    "apply (owner, after review) before --phone"
                )
            gateway = NetnsPhoneGateway(str(work / "gateway"))
            gateway.start()
            status = gateway.wait_ready()
            gateway_pid = gateway.pid()
            if gateway_pid:
                write_pid_record(work, "gateway", gateway_pid, kind="gateway")
        elif not arguments.no_gateway:
            gateway = base.IsolatedGateway(str(work / "gateway"), GW_LISTEN, GW_WG)
            gateway.start()
            status = gateway.wait_ready()
            gateway_pid = gateway.pid()
            if gateway_pid:
                write_pid_record(work, "gateway", gateway_pid, kind="gateway")

        if gateway is not None:
            pki = make_pki(work / "pki")
            config = HandlerConfig()
            config.bind = "127.0.0.1"
            config.port = HANDLER_PORT
            config.server_cert = pki["server_cert"]
            config.server_key = pki["server_key"]
            config.client_ca = pki["ca_file"]
            config.allowed_identities = frozenset({BACKEND_IDENTITY})
            config.node_id = NODE_ID
            config.admin_socket = str(work / "gateway" / "admin.sock")
            config.main_password = gateway_main_password
            config.max_not_after = 86400
            handler = ManagementHandler(config)
            handler_port = await handler.start()
            # N1: the advertised pin must be the actual DTLS credential the node loads.
            dtls_cert = Path(gateway.creds) / "wl-test-dtls-cert"
            if not dtls_cert.is_file():
                raise RuntimeError("DTLS credential cert missing after node start")
            pin = dtls_pin(Path(gateway.creds))
            management_pin = spki_pin(Path(pki["server_cert"]))
            if pin == management_pin:
                raise RuntimeError("DTLS pin equals the management mTLS pin: credentials mixed up")
            report["steps"]["dtls_credential"] = {
                "cert": "wl-test-dtls-cert",
                "sha256": hashlib.sha256(dtls_cert.read_bytes()).hexdigest(),
                "pin_matches_advertised": True,
                "distinct_from_management_pki": True,
            }
            env.update(
                {
                    "GATEWAY_MANAGEMENT_CA_FILE": pki["ca_file"],
                    "GATEWAY_MANAGEMENT_CERT_FILE": pki["backend_cert"],
                    "GATEWAY_MANAGEMENT_KEY_FILE": pki["backend_key"],
                    "ONBOARDING_EVIDENCE_ENDPOINT_ENABLED": "true",
                    "ONBOARDING_SERVICE_ENDPOINT_ENABLED": "true",
                    "ONBOARDING_EVIDENCE_SERVER_CERT_FILE": pki["server_cert"],
                    "ONBOARDING_EVIDENCE_SERVER_KEY_FILE": pki["server_key"],
                    "ONBOARDING_EVIDENCE_CLIENT_CA_FILE": pki["ca_file"],
                    "ONBOARDING_EVIDENCE_LISTEN_HOST": "127.0.0.1",
                    "ONBOARDING_EVIDENCE_LISTEN_PORT": "18093",
                }
            )
            relay_settings = Settings(
                database_url=database_url, environment="test", log_level="INFO",
                api_host=API_HOST, api_port=arguments.api_port, db_pool_min=1,
                db_pool_max=5, db_command_timeout_seconds=5,
                worker_poll_interval_seconds=0.2, worker_lock_timeout_seconds=30,
                worker_max_attempts=8, evidence_relay_enabled=True,
                service_relay_enabled=True,
                evidence_node_cert_file=pki["node_cert"],
                evidence_node_key_file=pki["node_key"],
                evidence_backend_ca_file=pki["ca_file"],
                evidence_backend_host="localhost", evidence_backend_port=18093,
                evidence_backend_server_name=SERVER_NAME,
                evidence_relay_socket=str(work / "evidence-relay.sock"),
                service_relay_socket=str(work / "service-relay.sock"),
                evidence_relay_allowed_uid=0, service_relay_allowed_uid=0,
            )
            report["steps"]["real_gateway"] = {
                "node_id": NODE_ID,
                "max_workers": MAX_WORKERS,
                "handler_port": handler_port,
                "engine_status_managed_ready": status.get("managed_ready"),
            }

            connection = await base.asyncpg.connect(database_url, timeout=10)
            await connection.execute(
                """
                INSERT INTO gateways (gateway_key, environment, display_name, endpoints,
                                      capabilities, registry_state, confirmed_max_workers, vk_hashes)
                VALUES ($1, 'test', 'STEP036 device node', $2::jsonb, '["managed"]'::jsonb,
                        'registered', 36, $3::jsonb)
                ON CONFLICT (gateway_key) DO UPDATE
                SET endpoints = EXCLUDED.endpoints,
                    display_name = EXCLUDED.display_name,
                    capabilities = EXCLUDED.capabilities,
                    registry_state = 'registered',
                    confirmed_max_workers = 36,
                    vk_hashes = EXCLUDED.vk_hashes
                """,
                NODE_ID,
                json.dumps(
                    {
                        "node_id": NODE_ID,
                        "peer_ip": EXTERNAL_IP if arguments.phone else "127.0.0.1",
                        "dtls_port": PHONE_DTLS if arguments.phone else GW_LISTEN,
                        "wg_port": PHONE_WG if arguments.phone else GW_WG,
                        "dtls_spki_sha256": pin,
                        "region": "test",
                        "country_code": "XX",
                        "target_workers": MAX_WORKERS,
                        "management": {
                            "host": "127.0.0.1",
                            "port": handler_port,
                            "server_name": SERVER_NAME,
                        },
                    }
                ),
                json.dumps(vk_hashes),
            )
            await connection.close()
            report["steps"]["registry_seed"] = "ok (gateway_key == node_id, target_workers=36)"
            report["phone_ready"] = arguments.phone

        api = start_process("api", [str(Path("/home/pavel/projects/terlimo-backend/.venv/bin") / "terlimo-api")], env, logs / "api.log")
        worker = start_process(
            "worker", [str(Path("/home/pavel/projects/terlimo-backend/.venv/bin") / "terlimo-worker")], env, logs / "worker.log"
        )
        processes = [api, worker]
        base_url = f"http://{API_HOST}:{arguments.api_port}"
        await wait_ready(base_url)
        if relay_settings is not None:
            evidence_relay = EvidenceRelay(relay_settings)
            service_relay = ServiceRelay(relay_settings)
            await evidence_relay.start()
            await service_relay.start()
        report["steps"]["api_worker_readiness"] = "ok"

        if arguments.serve:
            state = {
                "work": str(work),
                "database_url": database_url,
                "node_id": NODE_ID,
                "external_ip": EXTERNAL_IP if arguments.phone else "127.0.0.1",
                "dtls_port": PHONE_DTLS if arguments.phone else GW_LISTEN,
                "wg_port": PHONE_WG if arguments.phone else GW_WG,
                "api_base_url": base_url,
                "phone_ready": bool(arguments.phone),
                "fixture": "STEP036 separate installation only; no existing identity reset",
            }
            state_path = work / "state.json"
            state_path.write_text(json.dumps(state, indent=2) + "\n")
            os.chmod(state_path, 0o600)
            report["steps"]["persistent_serve"] = {
                "state_file": str(state_path),
                "note": "API/worker/gateway stay up; no fixture session created; Ctrl+C to stop",
            }
            print(json.dumps(report, indent=2), flush=True)
            stop = asyncio.Event()
            loop = asyncio.get_running_loop()
            for signal_name in ("SIGINT", "SIGTERM"):
                import signal as signal_module

                loop.add_signal_handler(
                    getattr(signal_module, signal_name), stop.set
                )
            await stop.wait()
            return report

        async with aiohttp.ClientSession() as session:
            pop = PopClient(contract_pop)
            status, challenge = await post_json(
                session,
                f"{base_url}{MOBILE}/auth/challenge",
                {
                    "installation_fingerprint": pop.fingerprint,
                    "purpose": "enrollment",
                    "environment": "test",
                },
            )
            assert status == 200, challenge
            proof = pop.proof(
                challenge,
                op="installations.create",
                scope="enrollment",
                extra={
                    "public_key_spki_b64": pop.spki_b64,
                    "platform": "android",
                    "name": "STEP036 synthetic",
                },
            )
            status, enrollment = await post_json(
                session,
                f"{base_url}{MOBILE}/installations",
                {"public_key_spki_b64": pop.spki_b64, "proof": proof},
            )
            assert status == 200, enrollment
            report["steps"]["real_pop_enrollment"] = {
                "installation_state": enrollment["installation"]["state"]
            }

            connection = await base.asyncpg.connect(database_url, timeout=10)
            installation_id = await connection.fetchval(
                "SELECT id FROM installations WHERE public_key_fingerprint = $1", pop.fingerprint
            )
            account_id = await connection.fetchval(
                "INSERT INTO accounts (status, telegram_id) VALUES ('verified', $1) RETURNING id",
                secrets.randbelow(1 << 40),
            )
            binding_id = await connection.fetchval(
                """
                INSERT INTO account_bindings (account_id, installation_id, status)
                VALUES ($1, $2, 'active') RETURNING id
                """,
                account_id,
                installation_id,
            )
            await connection.execute(
                """
                INSERT INTO entitlements
                    (account_id, kind, status, starts_at, ends_at, device_limit, revision)
                VALUES ($1, 'paid', 'active', now() - interval '1 hour',
                        now() + interval '30 days', 2, 1)
                """,
                account_id,
            )
            await connection.close()
            report["steps"]["synthetic_fixture"] = "ok (paid synthetic entitlement, no trial)"

            status, s_challenge = await post_json(
                session,
                f"{base_url}{MOBILE}/auth/challenge",
                {
                    "installation_fingerprint": pop.fingerprint,
                    "purpose": "session",
                    "environment": "test",
                },
            )
            assert status == 200, s_challenge
            proof = pop.proof(
                s_challenge,
                op="auth.session",
                scope="session",
                extra={
                    "requested_scopes": ["session:read", "access:sync"],
                    "idempotency_key": "step036-session-0001",
                },
            )
            status, session_body = await post_json(
                session, f"{base_url}{MOBILE}/auth/session", {"proof": proof}
            )
            assert status == 200, session_body
            token = session_body["session"]["session_id"]
            assert session_body["session"]["account_ref"] == str(account_id)
            headers = {"Authorization": f"Bearer {token}"}
            report["steps"]["linked_pop_session"] = {
                "scopes": session_body["session"]["scopes"],
                "account_linked": True,
            }

            status, me = await get_json(session, f"{base_url}{MOBILE}/me", headers=headers)
            assert status == 200 and me["account_state"] == "ACTIVE_PAID", me
            report["steps"]["me"] = {
                "account_state": me["account_state"],
                "binding_revision": me["binding_revision"],
            }

            status, catalog = await get_json(
                session, f"{base_url}{MOBILE}/gateways", headers=headers
            )
            if arguments.no_gateway:
                report["steps"]["catalog_without_gateway"] = {
                    "http": status,
                    "code": catalog.get("code"),
                }
            else:
                assert status == 409 and catalog["code"] == "ACCESS_SYNC_PENDING", catalog
                tokens = catalog["details"]
                status, sync = await post_json(
                    session,
                    f"{base_url}{MOBILE}/access/sync",
                    {
                        "catalog_revision": tokens["catalog_revision"],
                        "binding_revision": tokens["binding_revision"],
                    },
                    headers={**headers, "Idempotency-Key": "step036-sync-000001"},
                )
                assert status == 200, sync
                catalog = await wait_catalog(session, base_url, headers)
                gw = catalog["gateways"][0]
                assert gw["gateway_id"] == NODE_ID, gw
                assert gw["target_workers"] == MAX_WORKERS, gw
                expected_ip = EXTERNAL_IP if arguments.phone else "127.0.0.1"
                expected_dtls = PHONE_DTLS if arguments.phone else GW_LISTEN
                expected_wg = PHONE_WG if arguments.phone else GW_WG
                assert gw["transport"]["peer_ip"] == expected_ip
                assert gw["transport"]["dtls_port"] == expected_dtls
                assert gw["transport"]["wg_port"] == expected_wg
                assert gw["access"]["device_ref"] == pop.fingerprint
                assert gw["access"]["generation"] == "1"
                assert int(gw["access"]["lease_seq"]) >= 1
                readback = gateway.call(
                    {
                        "operation": "grant_get",
                        "password": gw["access"]["password"],
                        "grant": {"node_id": NODE_ID},
                    }
                )["client_test"]
                assert readback["node_id"] == NODE_ID
                assert readback["registration_id"] == pop.fingerprint
                assert readback["max_workers"] == MAX_WORKERS
                assert readback["revoked"] is False
                assert readback["lease_seq"] == gw["access"]["lease_seq"]
                report["steps"]["catalog_and_real_readback"] = {
                    "gateway_id": gw["gateway_id"],
                    "generation": gw["access"]["generation"],
                    "lease_seq": gw["access"]["lease_seq"],
                    "target_workers": gw["target_workers"],
                    "readback_confirmed": True,
                }

                if not arguments.verify_revoke:
                    report["steps"]["fixture_left_in_place"] = (
                        "explicit cleanup required (attach_android.py --revoke / --cleanup)"
                    )
                    return report
                connection = await db_connect(database_url)
                assert await revoke_binding_grants(connection, binding_id=binding_id) == 1
                await connection.close()
                deadline = time.monotonic() + 45
                revoked_row = None
                while time.monotonic() < deadline:
                    connection = await db_connect(database_url)
                    revoked_row = await connection.fetchrow(
                        """
                        SELECT state, applied_generation, desired_generation, lease_seq,
                               last_readback, gateway_credential
                        FROM grants WHERE binding_id = $1
                        """,
                        binding_id,
                    )
                    await connection.close()
                    if (
                        revoked_row["state"] == "revoked"
                        and revoked_row["applied_generation"] == revoked_row["desired_generation"]
                        and (revoked_row["last_readback"] or {}).get("revoked") is True
                    ):
                        break
                    await asyncio.sleep(0.3)
                else:
                    raise RuntimeError("confirmed actual revoke did not complete")
                readback = gateway.call(
                    {
                        "operation": "grant_get",
                        "password": revoked_row["gateway_credential"],
                        "grant": {"node_id": NODE_ID},
                    }
                )["client_test"]
                assert readback["revoked"] is True
                assert readback["registration_id"] == pop.fingerprint
                # The binding revoke fences the session (correct product behaviour).
                status, body = await get_json(
                    session, f"{base_url}{MOBILE}/gateways", headers=headers
                )
                assert status == 401 and body["code"] == "SESSION_INVALID", body
                report["steps"]["confirmed_actual_revoke"] = {
                    "db_confirmed": True,
                    "node_readback_revoked": True,
                    "old_session_rejected": 401,
                }
    finally:
        if service_relay is not None:
            await service_relay.stop()
        if evidence_relay is not None:
            await evidence_relay.stop()
        for process in processes:
            process.terminate()
        for process in processes:
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
        if handler is not None:
            await handler.stop()
        if gateway is not None:
            gateway.stop()
        if server is not None and not arguments.keep:
            server.cleanup()
        # Persistent package data (marker/pg/profile/state/logs) is NEVER removed by a normal
        # exit, SIGINT or SIGTERM. The only removal path is the explicit, separately guarded
        # `--cleanup --purge-owned-work` command; run_loop never calls remove_owned_work.
    return report


def stop_package_postgres(work: Path, *, timeout: float = 30.0) -> dict:
    """Stop this work dir's PostgreSQL via scoped pg_ctl and prove the actual exit."""
    pgdata = work / "pg"
    pid_file = pgdata / "postmaster.pid"
    pg_ctl = pg_bin("pg_ctl")
    if not pid_file.is_file():
        # postmaster.pid absence alone proves nothing: verify by exact -D reference.
        survivors = _processes_with_token(f"-D {pgdata}")
        if survivors:
            return {
                "ok": False,
                "stopped": False,
                "verified": False,
                "reason": f"postmaster.pid absent but processes still reference {pgdata}: {survivors}",
            }
        return {"ok": True, "stopped": False, "verified": True, "reason": "no postmaster for this pgdata"}
    if not pg_ctl.is_file():
        return {"ok": False, "stopped": False, "verified": False, "reason": "pg_ctl not found"}
    result = subprocess.run(
        [str(pg_ctl), "-D", str(pgdata), "-m", "fast", "-w", "-t", str(int(timeout)), "stop"],
        capture_output=True,
        text=True,
        check=False,
    )
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not pid_file.exists():
            break
        time.sleep(0.3)
    if pid_file.exists():
        return {
            "ok": False,
            "stopped": False,
            "verified": False,
            "reason": f"postmaster.pid still present after pg_ctl stop: {result.stderr.strip()[:150]}",
        }
    # No remaining process may reference this exact pgdata.
    survivors = _processes_with_token(f"-D {pgdata}")
    if survivors:
        return {
            "ok": False,
            "stopped": False,
            "verified": False,
            "reason": f"processes still reference {pgdata}: {survivors}",
        }
    return {"ok": True, "stopped": True, "verified": True}


def _processes_with_token(token: str) -> list[int]:
    found: list[int] = []
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        try:
            cmdline = Path(f"/proc/{entry.name}/cmdline").read_bytes().replace(b"\x00", b" ").decode("utf-8", "replace")
        except OSError:
            continue
        if token in cmdline:
            found.append(int(entry.name))
    return found


def cleanup(work: Path, *, purge_work: bool = False) -> dict:
    """Fail-closed final stop: verified processes + verified PostgreSQL exit.

    Work data is KEPT by default (safe stop); `purge_work=True` removes the owned tree only
    after every process record and the postmaster are verified stopped.
    """
    work, _marker = resolve_work(str(work), require_owned=True, for_destructive=True)
    payload: dict = {"work": str(work), "purge_requested": purge_work}
    # Cleanup never signals anything: it requires the verified stop to have happened first.
    unproven: list[dict] = []
    verified_gone: list[str] = []
    for kind in expected_kinds(work):
        record_path = pid_record_path(work, kind)
        if not record_path.is_file():
            unproven.append({"kind": kind, "action": "missing_record"})
            continue
        try:
            record = json.loads(record_path.read_text(encoding="utf-8"))
        except ValueError:
            unproven.append({"kind": kind, "action": "unreadable"})
            continue
        if not record_identity_valid(record):
            unproven.append({"kind": kind, "action": "identity_missing"})
            continue
        if verified_alive(record):
            unproven.append({"kind": kind, "action": "still_alive"})
        else:
            verified_gone.append(kind)
    payload["verified_gone"] = verified_gone
    payload["unproven"] = unproven
    if unproven:
        payload.update(
            ok=False,
            reason="unverified/absent ownership records; run --adopt (live) or --stop first",
        )
        return payload
    pg = stop_package_postgres(work)
    payload["postgres"] = pg
    if not pg.get("verified"):
        payload.update(ok=False, reason="PostgreSQL exit unproven; data kept")
        return payload
    if purge_work:
        remove_owned_work(work)
        payload["removed"] = not work.exists()
    else:
        payload["removed"] = False
        payload["data_kept"] = True
    payload["ok"] = True
    return payload


def checkpoint_package(work: Path, out_dir: Path) -> dict:
    """Refresh the owner-only checkpoint (final snapshot = the last run before stop)."""
    import hashlib

    work, _marker = resolve_work(str(work), require_owned=True)
    out = Path(out_dir).expanduser().resolve()
    if out == work or work in out.parents or out in work.parents:
        raise guard_error("checkpoint dir must be outside the work tree")
    out.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(out, 0o700)
    entries: dict[str, str] = {}

    def put(name: str, data: bytes) -> None:
        path = out / name
        path.write_bytes(data)
        path.chmod(0o600)
        entries[name] = hashlib.sha256(data).hexdigest()

    state_path = work / "state.json"
    if not state_path.is_file():
        raise guard_error("state.json missing")
    state = json.loads(state_path.read_text(encoding="utf-8"))
    dump = subprocess.run(
        [str(pg_bin("pg_dump")), state["database_url"]], capture_output=True, check=False
    )
    if dump.returncode != 0:
        raise RuntimeError(f"pg_dump failed: {dump.stderr.decode('utf-8', 'replace')[:200]}")
    put("pkg.sql", dump.stdout)
    for name in ("state.json", "network.json", MARKER_NAME):
        source = work / name
        if source.is_file():
            put(name, source.read_bytes())
    for log in sorted((work / "logs").glob("*")):
        if log.is_file():
            put(f"logs/{log.name}", log.read_bytes())
    iptables = subprocess.run(
        ["sudo", "-n", "iptables-save"], capture_output=True, check=False
    )
    put("iptables.current", iptables.stdout)
    caddy = subprocess.run(
        ["sudo", "-n", "cat", "/etc/terlimo-minishop-edge/Caddyfile"],
        capture_output=True,
        check=False,
    )
    put("Caddyfile.current", caddy.stdout)
    runbook = (
        "# STEP03.5 checkpoint (owner-only)\n\n"
        f"Created: {datetime.now(UTC).strftime('%Y-%m-%dT%H:%M:%SZ')} from {work}.\n"
        "Re-run `device_stage.py --checkpoint` immediately before the stop: the LAST run is the\n"
        "final snapshot (the database changes during the phone run). Treat as secret (gateway\n"
        "credentials in pkg.sql): keep 0700/0600, never commit or relay.\n\n"
        "Stop order (fail-closed: never continue past a step that is not ok): --checkpoint\n"
        "(while PostgreSQL is live) -> --adopt (if records missing) -> --stop -> --cleanup\n"
        "(scoped PostgreSQL exit verified, data kept; no --purge-owned-work) -> verify absence\n"
        "of owned processes/PostgreSQL/ports -> device_network.py rollback -> caddy_mobile_api.py\n"
        "remove. Binary rollback is replacing <work>/bin/wdtt-server after the package is\n"
        "stopped, or running again with a previous --node-bin; never remove the work tree\n"
        "(pg/profile/state/network/logs are kept).\n"
    ).encode()
    put("RUNBOOK-restore-stop.md", runbook)
    code_commit = subprocess.run(
        ["git", "-C", str(ROOT), "rev-parse", "HEAD"], capture_output=True, text=True, check=False
    ).stdout.strip()
    code_tree = subprocess.run(
        ["git", "-C", str(ROOT), "rev-parse", "HEAD^{tree}"], capture_output=True, text=True, check=False
    ).stdout.strip()
    info = {
        "created_at": datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "work": str(work),
        "code_commit": code_commit,
        "code_tree": code_tree,
        "files": entries,
        "final_note": "re-run this command immediately before the stop; the last run is final",
    }
    (out / "CHECKPOINT-INFO.json").write_text(json.dumps(info, indent=2) + "\n", encoding="utf-8")
    os.chmod(out / "CHECKPOINT-INFO.json", 0o600)
    return {"ok": True, "dir": str(out), "created_at": info["created_at"], "files": entries}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--work", default=str(WORK_ROOT / "pkg"))
    parser.add_argument(
        "--node-bin",
        default=os.environ.get("STEP036_NODE_BIN", NODE_BIN_DEFAULT),
        help="wdtt-server binary used by this package (default: shared install)",
    )
    parser.add_argument("--api-port", type=int, default=API_PORT)
    parser.add_argument("--no-gateway", action="store_true")
    parser.add_argument("--phone", action="store_true", help="external netns node (owner-applied network)")
    parser.add_argument("--serve", action="store_true", help="persistent mode; no fixture/revoke")
    parser.add_argument("--verify-revoke", action="store_true", help="loopback self-test only")
    parser.add_argument("--keep", action="store_true")
    parser.add_argument("--stop", action="store_true", help="verified stop of own processes only")
    parser.add_argument("--stop-postgres", action="store_true", help="stop only this package's PostgreSQL; keep its data")
    parser.add_argument("--adopt", action="store_true", help="prove and record the existing live package")
    parser.add_argument("--checkpoint", action="store_true", help="refresh the owner-only checkpoint")
    parser.add_argument("--checkpoint-dir", default="/home/pavel/step036-checkpoint")
    parser.add_argument("--cleanup", action="store_true")
    parser.add_argument("--purge-owned-work", action="store_true")
    parser.add_argument("--lifecycle-selftest", action="store_true", help=argparse.SUPPRESS)
    arguments = parser.parse_args()
    if not (arguments.stop or arguments.stop_postgres or arguments.adopt or arguments.checkpoint or arguments.cleanup):
        if not (arguments.phone and arguments.serve and arguments.keep):
            print(json.dumps({"ok": False, "reason": "STEP036 device stage requires --phone --serve --keep"}))
            return 2
    global SERVER_BIN
    node_bin = Path(arguments.node_bin)
    if not node_bin.is_absolute():
        print(json.dumps({"ok": False, "reason": "node binary path must be absolute"}))
        return 2
    SERVER_BIN = str(node_bin)
    needs_binary = not (
        arguments.stop
        or arguments.stop_postgres
        or arguments.checkpoint
        or arguments.cleanup
        or arguments.lifecycle_selftest
    )
    if needs_binary and not (node_bin.is_file() and os.access(node_bin, os.X_OK)):
        print(json.dumps({"ok": False, "reason": f"node binary is not executable: {node_bin}"}))
        return 2
    if arguments.stop:
        try:
            result = stop_package(Path(arguments.work))
            print(json.dumps(result, indent=2))
        except RuntimeError as error:
            print(json.dumps({"ok": False, "reason": str(error)}, indent=2))
            return 2
        return 0 if result.get("verified_exit") else 2
    if arguments.stop_postgres:
        try:
            work, _marker = resolve_work(arguments.work, require_owned=True, for_destructive=True)
            result = stop_package_postgres(work)
        except RuntimeError as error:
            result = {"ok": False, "reason": str(error)}
        print(json.dumps(result, indent=2))
        return 0 if result.get("ok") else 2
    if arguments.adopt:
        try:
            print(json.dumps(adopt_live_package(Path(arguments.work)), indent=2))
        except RuntimeError as error:
            print(json.dumps({"ok": False, "reason": str(error)}, indent=2))
            return 2
        return 0
    if arguments.checkpoint:
        try:
            print(json.dumps(checkpoint_package(Path(arguments.work), Path(arguments.checkpoint_dir)), indent=2))
        except RuntimeError as error:
            print(json.dumps({"ok": False, "reason": str(error)}, indent=2))
            return 2
        return 0
    if arguments.cleanup:
        try:
            result = cleanup(Path(arguments.work), purge_work=arguments.purge_owned_work)
        except RuntimeError as error:
            print(json.dumps({"ok": False, "reason": str(error)}, indent=2))
            return 2
        print(json.dumps(result, indent=2))
        return 0 if result.get("ok") else 2
    report = asyncio.run(run_loop(arguments))
    print(json.dumps(report, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
