#!/usr/bin/env python3
"""STEP03.3 real gateway integration: Backend worker -> existing WDTT admin wire -> readback.

Runs the installed wdtt-server binary in a throwaway mount+network namespace with its own
config dir/ports and synthetic grant (B02 harness pattern). The Backend runs its real
migration set on an isolated PostgreSQL and drives the real GatewayAdminClient. No live A/B,
no personal grants, no phone, no production.

Evidence: apply -> readback applied; lost response -> identical-bytes retry, one effect;
revoke -> revoked tombstone; delayed stale refresh rejected; gateway restart keeps the fence.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
from datetime import UTC, datetime
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
_RUNTIME = Path(tempfile.mkdtemp(prefix="terlimo-033-rt-"))
_RUNTIME.chmod(0o700)
os.environ["XDG_RUNTIME_DIR"] = str(_RUNTIME)

import asyncpg
import pgserver

from terlimo_backend.config import Settings
from terlimo_backend.db import Database
from terlimo_backend.gateway_adapter import GatewayAdminClient, GatewayError
from terlimo_backend.gateway_control import (
    GatewayControlHandlers,
    ensure_grant,
    revoke_binding_grants,
)
from terlimo_backend.migrations import runner
from terlimo_backend.pop import installation_fingerprint
from terlimo_backend.worker import OutboxWorker

SERVER = "/usr/local/bin/wdtt-server"
MAIN_PASSWORD = "<SET_TEST_GATEWAY_PASSWORD>"
NODE_ID = "terlimo-033-node"
MAX_WORKERS = 36
SPKI = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEf2GWLvpp78Vox1cAEyfhgUKaywhAfUgWYaf5JldPvQZWP_mwVeNQNWK6hBTYmLpeci1h7TWFg-mkxysKZevnHA"
FINGERPRINT = installation_fingerprint(SPKI)


def say(message: str) -> None:
    print(f"[integration] {message}", flush=True)


def evidence(name: str, value: object) -> None:
    print(f"[evidence] {name}={json.dumps(value, ensure_ascii=False, default=str)}", flush=True)


def make_tls_creds(creds_dir: str) -> None:
    from cryptography import x509
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import ec
    from cryptography.x509.oid import NameOID

    key = ec.generate_private_key(ec.SECP256R1())
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "terlimo-033-fixture")])
    now = datetime.now(UTC)
    cert = (
        x509.CertificateBuilder()
        .subject_name(name)
        .issuer_name(name)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - __import__("datetime").timedelta(days=1))
        .not_valid_after(now + __import__("datetime").timedelta(days=2))
        .sign(key, hashes.SHA256())
    )
    os.makedirs(creds_dir, mode=0o700, exist_ok=True)
    cert_path = os.path.join(creds_dir, "wl-test-dtls-cert")
    key_path = os.path.join(creds_dir, "wl-test-dtls-key")
    with open(cert_path, "wb") as fh:
        fh.write(cert.public_bytes(serialization.Encoding.PEM))
    with open(key_path, "wb") as fh:
        fh.write(
            key.private_bytes(
                serialization.Encoding.PEM,
                serialization.PrivateFormat.PKCS8,
                serialization.NoEncryption(),
            )
        )
    os.chmod(cert_path, 0o600)
    os.chmod(key_path, 0o600)


class IsolatedGateway:
    def __init__(self, work: str, listen_port: int, wg_port: int) -> None:
        self.work = work
        self.listen_port = listen_port
        self.wg_port = wg_port
        self.creds = os.path.join(work, "creds")
        self.log_path = os.path.join(work, "server.log")
        self.proc: subprocess.Popen | None = None
        self.log = None
        self._socket_ready = False

    def start(self) -> None:
        os.makedirs(self.work, mode=0o700, exist_ok=True)
        self._socket_ready = False
        make_tls_creds(self.creds)
        inner = (
            "mount -t sysfs sysfs /sys; ip link set lo up; "
            f"CREDENTIALS_DIRECTORY={self.creds} WL_TEST_ENABLED=1 WL_TEST_NODE_ID={NODE_ID} "
            f"WL_TEST_WG_ISOLATION_CONFIRMED=1 WL_TEST_BACKEND_SOCKET={self.work}/bs.sock "
            f"exec {SERVER} -listen 127.0.0.1:{self.listen_port} -wg-port {self.wg_port} "
            f"-config-dir {self.work} -max-workers-per-access {MAX_WORKERS} "
            f"-password {MAIN_PASSWORD} -admin 1 -bot-token fixture -dns 1.1.1.1 -wg-backend userspace"
        )
        self.log = Path(self.log_path).open("ab")  # noqa: SIM115 - closed explicitly in stop()
        self.proc = subprocess.Popen(
            ["sudo", "-n", "unshare", "-m", "-n", "--", "bash", "-c", inner],
            stdout=self.log,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )

    def pid(self) -> int | None:
        out = subprocess.run(
            ["pgrep", "-a", "wdtt-server"], capture_output=True, text=True, check=False
        ).stdout
        for line in out.splitlines():
            if f"-config-dir {self.work}" in line:
                return int(line.split()[0])
        return None

    def wait_ready(self, timeout: float = 25.0) -> dict:
        deadline = time.monotonic() + timeout
        last = None
        socket_path = os.path.join(self.work, "admin.sock")
        while time.monotonic() < deadline:
            if self.pid() and os.path.exists(socket_path):
                if not self._socket_ready:
                    # The isolated instance runs in a root namespace; allow this user to talk
                    # to its throwaway admin socket (fixture-only config dir).
                    subprocess.run(["sudo", "-n", "chmod", "666", socket_path], check=False)
                    self._socket_ready = True
                try:
                    return self.call({"operation": "engine_status"})
                except Exception as exc:  # noqa: BLE001
                    last = str(exc)
            time.sleep(0.1)
        raise RuntimeError(f"gateway not ready: {last}")

    def call(self, command: dict) -> dict:
        import socket as socket_module

        payload = json.dumps(command, separators=(",", ":"), ensure_ascii=False)
        envelope = json.dumps(
            {"main_password": MAIN_PASSWORD, "args": ["client-test", payload]},
            separators=(",", ":"),
            ensure_ascii=False,
        )
        sock = socket_module.socket(socket_module.AF_UNIX)
        sock.settimeout(10)
        sock.connect(os.path.join(self.work, "admin.sock"))
        sock.sendall(envelope.encode())
        data = b""
        while True:
            chunk = sock.recv(65536)
            if not chunk:
                break
            data += chunk
        sock.close()
        return json.loads(data.decode())

    def stop(self) -> None:
        pid = self.pid()
        if pid is not None:
            subprocess.run(["sudo", "-n", "kill", str(pid)], check=False)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline and self.pid() is not None:
            time.sleep(0.1)
        if self.log is not None:
            self.log.close()
            self.log = None

    def cleanup(self) -> None:
        self.stop()
        subprocess.run(["sudo", "-n", "rm", "-rf", self.work], check=False)


async def db_execute(dsn: str, query: str, *args) -> str:
    connection = await asyncpg.connect(dsn, timeout=10)
    try:
        return await connection.execute(query, *args)
    finally:
        await connection.close()


async def db_fetchval(dsn: str, query: str, *args):
    connection = await asyncpg.connect(dsn, timeout=10)
    try:
        return await connection.fetchval(query, *args)
    finally:
        await connection.close()


class DroppingClient(GatewayAdminClient):
    """Performs the real RPC once, then reports transport failure (lost response)."""

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.dropped = False

    def call_sync(self, command):
        if command.get("operation") == "refresh_lease" and not self.dropped:
            self.dropped = True
            try:
                super().call_sync(command)
            except GatewayError:
                pass
            raise GatewayError("GATEWAY_UNREACHABLE", "simulated lost response")
        return super().call_sync(command)


async def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--work", default="/tmp/terlimo-033-gateway")
    parser.add_argument("--listen-port", type=int, default=56300)
    parser.add_argument("--wg-port", type=int, default=56302)
    arguments = parser.parse_args()

    gateway = IsolatedGateway(arguments.work, arguments.listen_port, arguments.wg_port)
    pgdata = Path(tempfile.mkdtemp(prefix="terlimo-033-pg-"))
    server = None
    failures: list[str] = []
    try:
        say("starting isolated real gateway (mount+net namespace, synthetic grant only)")
        gateway.start()
        status = gateway.wait_ready()
        evidence("gateway_engine_status", status.get("client_test", status))

        server = pgserver.get_server(pgdata)
        server.ensure_pgdata_inited()
        server.ensure_postgres_running()
        admin_dsn = server.get_uri()
        name = "terlimo_033"
        connection = await asyncpg.connect(admin_dsn, timeout=10)
        await connection.execute(f'CREATE DATABASE "{name}"')
        await connection.close()
        parts = urlsplit(admin_dsn)
        database_url = urlunsplit((parts.scheme, parts.netloc, f"/{name}", parts.query, parts.fragment))
        connection = await asyncpg.connect(database_url, timeout=10)
        await runner.apply_migrations(connection)
        account = await connection.fetchval("INSERT INTO accounts (status) VALUES ('verified') RETURNING id")
        installation = await connection.fetchval(
            """
            INSERT INTO installations (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test','android',$1,$2,'technical') RETURNING id
            """,
            FINGERPRINT,
            SPKI,
        )
        binding = await connection.fetchval(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active') RETURNING id",
            account,
            installation,
        )
        entitlement = await connection.fetchval(
            """
            INSERT INTO entitlements (account_id, kind, status, ends_at, device_limit)
            VALUES ($1,'imported','active', now() + interval '600 seconds', 2) RETURNING id
            """,
            account,
        )
        gateway_id = await connection.fetchval(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, registry_state)
            VALUES ('terlimo-033', 'test', $1::jsonb, 'registered') RETURNING id
            """,
            json.dumps({"node_id": NODE_ID, "admin_socket": os.path.join(arguments.work, "admin.sock")}),
        )
        await connection.close()

        settings = Settings(
            database_url=database_url,
            environment="test",
            log_level="WARNING",
            api_host="127.0.0.1",
            api_port=0,
            db_pool_min=1,
            db_pool_max=5,
            db_command_timeout_seconds=10,
            worker_poll_interval_seconds=0.05,
            worker_lock_timeout_seconds=30,
            worker_max_attempts=12,
            db_reconnect_cooldown_seconds=0.2,
            gateway_admin_main_password=MAIN_PASSWORD,
            gateway_admin_timeout_seconds=10,
            gateway_max_lease_seconds=900,
        )
        database = Database(settings)
        await database.connect()

        def client_factory(_key, endpoints):
            return GatewayAdminClient(
                socket_path=str(endpoints["admin_socket"]),
                main_password=MAIN_PASSWORD,
                timeout_seconds=10,
            )

        handlers = GatewayControlHandlers(settings, client_factory=client_factory).as_handlers()
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="integration-033")

        say("apply: desired grant -> outbox -> real gateway -> readback")
        async with database.acquire() as pool_connection:
            await ensure_grant(
                pool_connection,
                binding_id=binding,
                gateway_id=gateway_id,
                entitlement_id=entitlement,
                max_lease_seconds=settings.gateway_max_lease_seconds,
            )
        assert await worker.drain() == 1
        row = await db_fetchval(
            database_url,
            "SELECT row_to_json(g) FROM grants AS g",
        )
        job = json.loads(row)
        readback = await client_factory(None, {"admin_socket": os.path.join(arguments.work, "admin.sock")}).grant_get(
            job["gateway_credential"], NODE_ID
        )
        evidence(
            "apply_readback",
            {
                "state": job["state"],
                "applied_generation": job["applied_generation"],
                "lease_seq": readback["lease_seq"],
                "gateway_generation": readback["generation"],
                "runtime_applied": readback["runtime_applied"],
                "expires_at": readback["expires_at"],
                "backend_not_after": job["not_after"],
            },
        )
        if job["state"] != "applied" or readback["runtime_applied"] is not True:
            failures.append("apply did not reach applied with runtime_applied true")

        say("lost response: real refresh applied once, retry with identical bytes")
        await db_execute(
            database_url,
            "UPDATE grants SET not_after = now() + interval '100 seconds' WHERE id = $1",
            job["id"],
        )
        await db_execute(
            database_url,
            "UPDATE entitlements SET ends_at = now() + interval '4 hours' WHERE id = $1",
            entitlement,
        )
        async with database.acquire() as pool_connection:
            assert (
                await ensure_grant(
                    pool_connection,
                    binding_id=binding,
                    gateway_id=gateway_id,
                    entitlement_id=entitlement,
                    max_lease_seconds=settings.gateway_max_lease_seconds,
                )
                == "enqueued"
            )

        def dropping_factory(_key, endpoints):
            return DroppingClient(
                socket_path=str(endpoints["admin_socket"]),
                main_password=MAIN_PASSWORD,
                timeout_seconds=10,
            )

        dropping_worker = OutboxWorker(
            database,
            settings,
            handlers=GatewayControlHandlers(settings, client_factory=dropping_factory).as_handlers(),
            worker_id="integration-033-drop",
        )
        assert await dropping_worker.drain() == 1
        pending = json.loads(
            await db_fetchval(
                database_url,
                "SELECT row_to_json(o) FROM outbox_operations AS o WHERE operation_type='gateway.apply_grant' ORDER BY created_at DESC LIMIT 1",
            )
        )
        after_drop = await client_factory(None, {"admin_socket": os.path.join(arguments.work, "admin.sock")}).grant_get(
            job["gateway_credential"], NODE_ID
        )
        evidence("lost_response_after_drop", {"op_status": pending["status"], "lease_seq": after_drop["lease_seq"]})

        await db_execute(
            database_url,
            "UPDATE outbox_operations SET available_at = now() WHERE id = $1",
            pending["id"],
        )
        assert await worker.drain() == 1
        after_retry = await client_factory(None, {"admin_socket": os.path.join(arguments.work, "admin.sock")}).grant_get(
            job["gateway_credential"], NODE_ID
        )
        refreshed = json.loads(await db_fetchval(database_url, "SELECT row_to_json(g) FROM grants AS g"))
        evidence(
            "lost_response_after_retry",
            {
                "lease_seq": after_retry["lease_seq"],
                "gateway_generation": after_retry["generation"],
                "state": refreshed["state"],
                "applied_generation": refreshed["applied_generation"],
            },
        )
        if int(after_retry["lease_seq"]) != 2 or int(after_retry["generation"]) != 1:
            failures.append("lost-response retry produced a second refresh effect")

        say("revoke: fence applied, delayed stale refresh rejected at the gateway")
        async with database.acquire() as pool_connection:
            assert await revoke_binding_grants(pool_connection, binding_id=binding) == 1
        assert await worker.drain() == 1
        revoked = await client_factory(None, {"admin_socket": os.path.join(arguments.work, "admin.sock")}).grant_get(
            job["gateway_credential"], NODE_ID
        )
        evidence("revoke_readback", {"revoked": revoked["revoked"], "generation": revoked["generation"]})
        if revoked["revoked"] is not True or int(revoked["generation"]) != 2:
            failures.append("revoke tombstone not applied")

        stale_refresh = {
            "operation": "refresh_lease",
            "password": job["gateway_credential"],
            "expires_at": int(time.time()) + 600,
            "expected_seq": str(revoked["lease_seq"]),
            "grant": {
                "grant_id": job["opaque_id"],
                "registration_id": FINGERPRINT,
                "node_id": NODE_ID,
                "public_key_spki": SPKI,
                "generation": str(revoked["generation"]),
                "lease_seq": str(int(revoked["lease_seq"]) + 1),
                "revoked": False,
                "operation_id": "stale-refresh-after-revoke",
            },
        }
        stale_response = gateway.call(stale_refresh)
        evidence("delayed_stale_refresh", stale_response)
        if stale_response.get("ok") is not False or stale_response.get("code") not in (
            "LEASE_CONFLICT",
            "CONFLICT",
        ):
            failures.append("delayed stale refresh was not fenced by the gateway")

        say("restart gateway: revoke fence persists")
        gateway.stop()
        gateway.start()
        gateway.wait_ready()
        after_restart = await client_factory(None, {"admin_socket": os.path.join(arguments.work, "admin.sock")}).grant_get(
            job["gateway_credential"], NODE_ID
        )
        evidence(
            "restart_fence",
            {"revoked": after_restart["revoked"], "generation": after_restart["generation"]},
        )
        if after_restart["revoked"] is not True or int(after_restart["generation"]) != 2:
            failures.append("revoke fence did not survive gateway restart")

        await database.close()
    finally:
        gateway.cleanup()
        if server is not None:
            server.cleanup()
        shutil.rmtree(pgdata, ignore_errors=True)
        shutil.rmtree(_RUNTIME, ignore_errors=True)

    if failures:
        say(f"INTEGRATION FAILED: {failures}")
        return 1
    say("INTEGRATION PASSED")
    return 0


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
