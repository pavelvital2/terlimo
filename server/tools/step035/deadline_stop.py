#!/usr/bin/env python3
"""One-shot deadline stop (work-preserving). Triggered by the transient systemd timer.

Sequence (manager decision): fresh protected checkpoint -> verified --stop (parent/children) ->
--cleanup WITHOUT purge (scoped PG stop, work kept) -> independent absence verification (owned
processes + package PG by exact -D) -> owned network rollback -> surgical Caddy removal ->
A/B and /help checks -> protected JSON report.

Never runs rm/purge, never broad-kills, never stops A/B or production. Any unproven step stops
the destructive chain and leaves the state plus a diagnostic report.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from datetime import UTC, datetime
from pathlib import Path

ROOT = Path("/home/pavel/projects/terlimo-backend")
PY = ROOT / ".venv" / "bin" / "python"
WORK = ROOT / ".step035-runs" / "pkg"
CHECKPOINT = Path("/home/pavel/step035-checkpoint")
sys.path.insert(0, str(ROOT / "tools"))


def _load(name: str, path: Path):
    import importlib.util

    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


run_module = _load("step035_deadline_run", ROOT / "tools" / "step035" / "run_isolated.py")
network_module = _load("step035_deadline_network", ROOT / "tools" / "step035" / "phone_network.py")


def _run(command: list[str], timeout: int = 300) -> dict:
    started = time.monotonic()
    try:
        result = subprocess.run(command, capture_output=True, text=True, timeout=timeout, check=False)
        stdout = result.stdout.strip()
        try:
            payload = json.loads(stdout)
        except ValueError:
            payload = {"raw": stdout[-400:]}
        return {
            "command": [str(item) for item in command],
            "exit": result.returncode,
            "seconds": round(time.monotonic() - started, 1),
            "result": payload,
        }
    except subprocess.TimeoutExpired:
        return {
            "command": [str(item) for item in command],
            "exit": "timeout",
            "seconds": round(time.monotonic() - started, 1),
        }


def verify_absence(work: Path = WORK) -> dict:
    """Independent check: no owned process and no package PG may remain."""
    checks: list[dict] = []
    failed: list[str] = []
    marker = run_module.load_marker(work)
    checks.append(
        {
            "ownership_marker": {
                "present": marker is not None,
                "work_matches": bool(marker and marker.get("work") == str(work)),
            }
        }
    )
    if marker is None or marker.get("work") != str(work):
        failed.append("ownership_marker")
    for kind in ("serve", "api", "worker", "gateway"):
        record_path = run_module.pid_record_path(work, kind)
        entry: dict = {"kind": kind}
        if not record_path.is_file():
            entry["record"] = "missing"
            entry["gone"] = False
        else:
            try:
                record = json.loads(record_path.read_text(encoding="utf-8"))
            except ValueError:
                entry["record"] = "unreadable"
                entry["gone"] = False
            else:
                entry["record"] = "present"
                if not run_module.record_identity_valid(record):
                    entry["identity"] = "invalid"
                    entry["gone"] = False
                else:
                    entry["gone"] = not run_module.verified_alive(record)
        if not entry["gone"]:
            failed.append(kind)
        checks.append(entry)
    pg_token = f"-D {work / 'pg'}"
    pg_pids: list[tuple[int, str]] = []
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        try:
            cmdline = Path(f"/proc/{entry.name}/cmdline").read_bytes().replace(b"\x00", b" ").decode("utf-8", "replace")
        except OSError:
            continue
        if pg_token in cmdline:
            pg_pids.append((int(entry.name), cmdline.strip()[:120]))
    checks.append({"postgres": {"processes": pg_pids, "gone": not pg_pids}})
    if pg_pids:
        failed.append("postgres")
    return {"ok": not failed, "failed": failed, "checks": checks, "work": str(work)}


def stop_and_clean() -> dict:
    out_dir = CHECKPOINT
    out_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    report: dict = {
        "action": "deadline-stop",
        "started_at": datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "steps": {},
    }
    report["steps"]["checkpoint"] = _run(
        [str(PY), str(ROOT / "tools" / "step035" / "run_isolated.py"),
         "--checkpoint", "--work", str(WORK), "--checkpoint-dir", str(out_dir)]
    )
    report["steps"]["stop"] = _run(
        [str(PY), str(ROOT / "tools" / "step035" / "run_isolated.py"), "--stop", "--work", str(WORK)]
    )
    stop_ok = bool(report["steps"]["stop"]["result"].get("verified_exit"))
    if not stop_ok:
        report["ok"] = False
        report["reason"] = "verified stop failed; nothing else was changed"
        return report
    report["steps"]["cleanup"] = _run(
        [str(PY), str(ROOT / "tools" / "step035" / "run_isolated.py"), "--cleanup", "--work", str(WORK)]
    )
    if not report["steps"]["cleanup"]["result"].get("ok"):
        report["ok"] = False
        report["reason"] = "cleanup (scoped PG stop, work kept) failed; nothing else was changed"
        return report
    absence = verify_absence()
    report["steps"]["verify_absence"] = {"command": ["internal", "verify_absence"], "exit": 0 if absence["ok"] else 1, "result": absence}
    if not absence["ok"]:
        report["ok"] = False
        report["reason"] = "owned processes/PG still present after stop; state kept for diagnostics"
        return report
    rollback = _run(
        [str(PY), str(ROOT / "tools" / "step035" / "phone_network.py"), "rollback", "--work", str(WORK)]
    )
    if rollback["exit"] != 0:
        report["steps"]["network_rollback"] = rollback
        report["ok"] = False
        report["reason"] = "owned network rollback unproven; Caddy untouched, state kept"
        return report
    report["steps"]["network_rollback"] = rollback
    caddy = _run([str(PY), str(ROOT / "tools" / "step035" / "caddy_mobile_api.py"), "remove"])
    report["steps"]["caddy_remove"] = caddy
    if caddy["exit"] != 0:
        report["ok"] = False
        report["reason"] = "surgical Caddy removal failed; other resources already stopped"
        return report
    ab_ports = subprocess.run(
        ["bash", "-c", "ss -lnuH | awk '{print $4}' | grep -E ':(56000|56002)$' || true"],
        capture_output=True,
        text=True,
        check=False,
    ).stdout.split()
    help_code = subprocess.run(
        ["curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
         "https://terlimo.193-5-251-217.sslip.io/help"],
        capture_output=True, text=True, check=False,
    ).stdout.strip()
    report["steps"]["final_checks"] = {"ab_ports_live": ab_ports, "help": help_code}
    report["ok"] = bool(ab_ports) and help_code == "200"
    report["finished_at"] = datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")
    return report


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=["run", "verify-only"])
    arguments = parser.parse_args()
    if arguments.action == "verify-only":
        result = verify_absence()
        print(json.dumps(result, indent=2))
        return 0 if result["ok"] else 2
    report = stop_and_clean()
    out_dir = CHECKPOINT
    out_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    report_path = out_dir / "deadline-stop-report.json"
    report_path.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    os.chmod(report_path, 0o600)
    print(json.dumps({k: report[k] for k in ("action", "ok", "reason") if k in report}, indent=2))
    return 0 if report.get("ok") else 2


if __name__ == "__main__":
    raise SystemExit(main())
