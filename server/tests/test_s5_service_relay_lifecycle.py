"""Service relay socket lifecycle: systemd stop, fail-closed, foreign inode, volatile runtime dir.

Runs the real relay module as a subprocess on a private temp socket. Certificates are the TEST A
fixtures read-only; the test skips when that material is unavailable.
"""
from __future__ import annotations

import os
import shutil
import signal
import socket
import stat
import subprocess
import sys
import time
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
PKI = Path("/home/pavel/step036-device-stage/pkg/pki")


def _env(sock: Path, uid: int) -> dict:
    return {
        **os.environ,
        "PYTHONPATH": str(ROOT),
        "TERLIMO_ENV": "test",
        "DATABASE_URL": "postgresql://postgres:@/none?host=/tmp",
        "ONBOARDING_SERVICE_RELAY_ENABLED": "true",
        "ONBOARDING_SERVICE_RELAY_SOCKET": str(sock),
        "ONBOARDING_SERVICE_RELAY_UID": str(uid),
        "ONBOARDING_EVIDENCE_NODE_CERT_FILE": str(PKI / "node.pem"),
        "ONBOARDING_EVIDENCE_NODE_KEY_FILE": str(PKI / "node.key"),
        "ONBOARDING_EVIDENCE_BACKEND_CA_FILE": str(PKI / "ca.pem"),
        "ONBOARDING_EVIDENCE_BACKEND_HOST": "127.0.0.1",
        "ONBOARDING_EVIDENCE_BACKEND_PORT": "18093",
        "ONBOARDING_EVIDENCE_BACKEND_SERVER_NAME": "gw-036.test",
    }


def _require_material() -> None:
    for name in ("node.pem", "node.key", "ca.pem"):
        if not (PKI / name).is_file():
            pytest.skip(f"TEST A pki material unavailable: {PKI / name}")


def _spawn(sock: Path) -> subprocess.Popen:
    return subprocess.Popen(
        [sys.executable, "-m", "terlimo_backend.service_relay"],
        cwd=ROOT,
        env=_env(sock, os.getuid()),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )


def _wait_socket(sock: Path, timeout: float = 10.0) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            if stat.S_ISSOCK(sock.lstat().st_mode):
                return True
        except FileNotFoundError:
            pass
        time.sleep(0.05)
    return False


def _stop_clean(proc: subprocess.Popen, timeout: float = 10.0) -> int | None:
    if proc.poll() is None:
        proc.send_signal(signal.SIGTERM)
    try:
        return proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(timeout=5)
        return None


def test_sigterm_graceful_stop_unlinks_socket_and_restart(tmp_path):
    _require_material()
    runtime = tmp_path / "run"
    runtime.mkdir(mode=0o700)
    sock = runtime / "service-relay.sock"

    first = _spawn(sock)
    try:
        assert _wait_socket(sock), first.stdout.read() if first.poll() is not None else "socket not created"
        rc = _stop_clean(first)
        assert rc == 0, f"unexpected exit {rc}"
        assert not sock.exists(), "SIGTERM must unlink the relay-owned socket"
    finally:
        first.kill()

    second = _spawn(sock)
    try:
        assert _wait_socket(sock), "restart on the same path must work without manual rm"
        assert _stop_clean(second) == 0
        assert not sock.exists()
    finally:
        second.kill()


def test_second_instance_fails_closed_and_first_socket_survives(tmp_path):
    _require_material()
    runtime = tmp_path / "run"
    runtime.mkdir(mode=0o700)
    sock = runtime / "service-relay.sock"

    first = _spawn(sock)
    try:
        assert _wait_socket(sock)
        second = _spawn(sock)
        out, _ = second.communicate(timeout=10)
        assert second.returncode != 0, out
        assert "socket path already exists" in out
        assert first.poll() is None, "first instance must stay alive"
        assert sock.exists()
        assert _stop_clean(first) == 0
    finally:
        first.kill()


def test_graceful_stop_never_unlinks_foreign_replaced_inode(tmp_path):
    _require_material()
    runtime = tmp_path / "run"
    runtime.mkdir(mode=0o700)
    sock = runtime / "service-relay.sock"

    relay = _spawn(sock)
    try:
        assert _wait_socket(sock)
        os.unlink(sock)
        foreign = socket.socket(socket.AF_UNIX)
        foreign.bind(str(sock))
        foreign_inode = sock.lstat().st_ino
        assert _stop_clean(relay) == 0
        assert sock.exists(), "relay must not delete a socket it no longer owns"
        assert sock.lstat().st_ino == foreign_inode
        foreign.close()
    finally:
        relay.kill()
        if sock.exists():
            os.unlink(sock)


def test_unclean_kill_then_runtime_dir_reset_recovers(tmp_path):
    _require_material()
    runtime = tmp_path / "run"
    runtime.mkdir(mode=0o700)
    sock = runtime / "service-relay.sock"

    first = _spawn(sock)
    assert _wait_socket(sock)
    first.kill()
    first.wait(timeout=5)
    assert sock.exists(), "SIGKILL leaves the socket behind (unclean case)"

    stale = _spawn(sock)
    out, _ = stale.communicate(timeout=10)
    assert stale.returncode != 0 and "socket path already exists" in out, "start stays fail-closed on stale path"

    # Volatile RuntimeDirectory semantics: after a host reboot the runtime dir is freshly created.
    shutil.rmtree(runtime)
    runtime.mkdir(mode=0o700)
    recovered = _spawn(sock)
    try:
        assert _wait_socket(sock), "fresh runtime dir must allow an operator-free start"
        assert _stop_clean(recovered) == 0
        assert not sock.exists()
    finally:
        recovered.kill()
