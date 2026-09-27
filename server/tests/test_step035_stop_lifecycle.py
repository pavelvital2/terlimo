"""Lifecycle regression for STEP035 stop/exit safety (incident: SIGTERM removed the work tree).

Runs the REAL tools/step035/run_isolated.py `main()`/`run_loop()` try/finally as a subprocess
with a fresh owned work dir and no PostgreSQL/node/network (via the test-only
`--lifecycle-selftest`). A normal stop (SIGTERM/SIGINT) never deletes persistent data; the only
removal path is the guarded `--cleanup --purge-owned-work` command.
"""

from __future__ import annotations

import importlib.util
import json
import os
import pathlib
import select
import shutil
import signal
import subprocess
import sys
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]
TOOLS = ROOT / "tools" / "step035"
SCRIPT = TOOLS / "run_isolated.py"


def _load():
    spec = importlib.util.spec_from_file_location("step035_stop_lifecycle", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


runiso = _load()
WORK_ROOT = runiso.WORK_ROOT


def _fresh_work(prefix: str) -> pathlib.Path:
    WORK_ROOT.mkdir(mode=0o700, parents=True, exist_ok=True)
    return WORK_ROOT / f"{prefix}-{os.urandom(4).hex()}"


def _spawn(work: pathlib.Path, *extra: str) -> subprocess.Popen:
    return subprocess.Popen(
        [sys.executable, str(SCRIPT), "--work", str(work), *extra],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        bufsize=1,
    )


def _wait_ready(proc: subprocess.Popen, timeout: float = 30.0) -> int:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise AssertionError(f"lifecycle selftest exited early rc={proc.returncode}")
        ready, _, _ = select.select([proc.stdout], [], [], 0.5)
        if not ready:
            continue
        line = proc.stdout.readline()
        if not line:
            break
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            doc = json.loads(line)
        except ValueError:
            continue
        if doc.get("lifecycle_selftest_ready"):
            return int(doc["child"])
        entry = doc.get("steps", {}).get("lifecycle_selftest", {})
        if entry.get("ready"):
            return int(entry["child"])
    raise AssertionError("lifecycle selftest did not become ready")


def _alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def _assert_preserved_after_signal(sig: signal.Signals) -> None:
    work = _fresh_work("lifecycle")
    proc = _spawn(work, "--lifecycle-selftest")
    try:
        child = _wait_ready(proc)
        proc.send_signal(sig)
        assert proc.wait(timeout=30) == 0, f"exit after {sig.name} was not clean"
        assert work.is_dir(), f"{sig.name} removed the persistent work tree"
        assert (work / runiso.MARKER_NAME).is_file(), f"{sig.name} lost the ownership marker"
        assert (work / "pg" / "PGDATA_DUMMY").is_file(), f"{sig.name} lost pg data"
        assert (work / "profile.dummy").is_file(), f"{sig.name} lost the profile"
        assert (work / "logs" / "serve.pid").is_file(), f"{sig.name} lost the pid record"
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline and _alive(child):
            time.sleep(0.1)
        assert not _alive(child), f"child survived {sig.name}"
    finally:
        if proc.poll() is None:
            proc.kill()
        if work.exists():
            runiso.remove_owned_work(work)


def test_sigterm_keeps_persistent_work_and_terminates_child():
    _assert_preserved_after_signal(signal.SIGTERM)


def test_sigint_keeps_persistent_work_and_terminates_child():
    _assert_preserved_after_signal(signal.SIGINT)


def test_guarded_purge_refuses_unowned_and_moved_work():
    # unowned: no ownership marker -> refuse, data kept
    unowned = _fresh_work("unowned")
    unowned.mkdir(parents=True)
    (unowned / "data.txt").write_text("must survive")
    try:
        result = subprocess.run(
            [sys.executable, str(SCRIPT), "--cleanup", "--work", str(unowned), "--purge-owned-work"],
            capture_output=True,
            text=True,
            timeout=60,
        )
        assert result.returncode != 0, "unguarded purge of unowned work succeeded"
        assert (unowned / "data.txt").is_file(), "unguarded purge removed foreign data"
    finally:
        shutil.rmtree(unowned, ignore_errors=True)

    # moved: marker points at a different work path -> refuse, data kept
    other = _fresh_work("other")
    moved = _fresh_work("moved")
    moved.mkdir(parents=True)
    marker = {
        "version": runiso.MARKER_VERSION,
        "work": str(other),
        "uid": os.getuid(),
        "run_id": "moved",
        "created_at": "2026-01-01T00:00:00Z",
    }
    (moved / runiso.MARKER_NAME).write_text(json.dumps(marker))
    (moved / "data.txt").write_text("must survive")
    try:
        result = subprocess.run(
            [sys.executable, str(SCRIPT), "--cleanup", "--work", str(moved), "--purge-owned-work"],
            capture_output=True,
            text=True,
            timeout=60,
        )
        assert result.returncode != 0, "purge of a moved work dir succeeded"
        assert (moved / "data.txt").is_file(), "moved-work purge removed data"
    finally:
        shutil.rmtree(moved, ignore_errors=True)


def test_lifecycle_selftest_refuses_existing_work():
    work = _fresh_work("existing")
    work.mkdir(parents=True)
    marker = {
        "version": runiso.MARKER_VERSION,
        "work": str(work),
        "uid": os.getuid(),
        "run_id": "live",
        "created_at": "2026-01-01T00:00:00Z",
    }
    (work / runiso.MARKER_NAME).write_text(json.dumps(marker))
    (work / "state.json").write_text("{}")
    try:
        result = subprocess.run(
            [sys.executable, str(SCRIPT), "--work", str(work), "--lifecycle-selftest"],
            capture_output=True,
            text=True,
            timeout=60,
        )
        assert result.returncode != 0, "selftest adopted an existing work dir"
        assert (work / "state.json").is_file(), "selftest touched existing work"
    finally:
        shutil.rmtree(work, ignore_errors=True)
