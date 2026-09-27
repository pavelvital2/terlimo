"""Local backend stand for the full-chain client/node bootstrap bring-up (TEST-only).

Runs the accepted backend pieces on loopback with ephemeral material:
  public API (plain HTTP, loopback) + EvidenceListener (mTLS evidence and/or service routes)
  + optionally the local evidence Unix relay. The stand never touches live hosts, never binds
  a non-loopback address and never prints secrets; readiness/manifest files are 0600.

Usage (executable):
    python tools/step036/local_stand.py --database-url <isolated TEST dsn> --root <dir>

The process stays in the foreground until SIGTERM/SIGINT, then stops the listener, the relay
and the database pool. Readiness JSON: <root>/readiness.json (started atomically, 0600).
"""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import datetime as dt
import hashlib
import json
import logging
import os
import signal
import socket
from collections.abc import Coroutine
from dataclasses import replace
from pathlib import Path
from typing import Any

from aiohttp import web
from cryptography import x509
from cryptography.fernet import Fernet
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID

from terlimo_backend.api import create_app
from terlimo_backend.config import Settings
from terlimo_backend.db import Database
from terlimo_backend.evidence_transport import EVIDENCE_LISTENER_KEY, EvidenceRelay
from terlimo_backend.service_relay import ServiceRelay

GATEWAY_KEY = "terlimo-local-stand"


def _write_private(path: Path, data: bytes) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    os.chmod(path.parent, 0o700)
    path.write_bytes(data)
    os.chmod(path, 0o600)
    return path


def _make_certs(root: Path) -> dict[str, str]:
    ca_key = ec.generate_private_key(ec.SECP256R1())
    ca_name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "local-stand-ca")])
    moment = dt.datetime.now(dt.UTC)
    ca_cert = (
        x509.CertificateBuilder()
        .subject_name(ca_name)
        .issuer_name(ca_name)
        .public_key(ca_key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(moment - dt.timedelta(hours=1))
        .not_valid_after(moment + dt.timedelta(days=1))
        .add_extension(x509.BasicConstraints(ca=True, path_length=0), critical=True)
        .sign(ca_key, hashes.SHA256())
    )

    def leaf(cn: str, *, san: list[str] | None = None) -> tuple[str, str]:
        key = ec.generate_private_key(ec.SECP256R1())
        builder = (
            x509.CertificateBuilder()
            .subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, cn)]))
            .issuer_name(ca_cert.subject)
            .public_key(key.public_key())
            .serial_number(x509.random_serial_number())
            .not_valid_before(moment - dt.timedelta(hours=1))
            .not_valid_after(moment + dt.timedelta(days=1))
            .add_extension(x509.BasicConstraints(ca=False, path_length=None), critical=True)
        )
        if san:
            builder = builder.add_extension(
                x509.SubjectAlternativeName([x509.DNSName(name) for name in san]), critical=False
            )
        cert = builder.sign(ca_key, hashes.SHA256())
        cert_path = _write_private(root / f"{cn}.pem", cert.public_bytes(serialization.Encoding.PEM))
        key_path = _write_private(
            root / f"{cn}.key.pem",
            key.private_bytes(
                serialization.Encoding.PEM,
                serialization.PrivateFormat.PKCS8,
                serialization.NoEncryption(),
            ),
        )
        return str(cert_path), str(key_path)

    ca_file = _write_private(root / "ca.pem", ca_cert.public_bytes(serialization.Encoding.PEM))
    server_cert, server_key = leaf("localhost", san=["localhost"])
    node_cert, node_key = leaf(GATEWAY_KEY)
    server_public = x509.load_pem_x509_certificate(Path(server_cert).read_bytes()).public_key()
    spki = hashlib.sha256(
        server_public.public_bytes(
            serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo
        )
    ).hexdigest()
    return {
        "ca_file": str(ca_file),
        "server_cert": server_cert,
        "server_key": server_key,
        "node_cert": node_cert,
        "node_key": node_key,
        "server_cert_spki_sha256": spki,
    }


def build_stand_settings(
    database_url: str,
    root: Path,
    *,
    service: bool = False,
    relay: bool = False,
) -> Settings:
    material = _make_certs(root)
    secrets_dir = root / "secrets"
    secrets_dir.mkdir(parents=True, exist_ok=True)
    os.chmod(secrets_dir, 0o700)
    secret_key = Fernet.generate_key().decode()
    relay_uid = os.getuid()
    return Settings(
        # NOTE: the Fernet onboarding key lives only in this in-memory Settings object; it is
        # never written to disk by the stand.
        database_url=database_url,
        environment="test",
        log_level="WARNING",
        api_host="127.0.0.1",
        api_port=0,
        db_pool_min=1,
        db_pool_max=5,
        db_command_timeout_seconds=5,
        worker_poll_interval_seconds=0.2,
        worker_lock_timeout_seconds=30,
        worker_max_attempts=8,
        onboarding_secret_key=secret_key,
        evidence_endpoint_enabled=True,
        evidence_server_cert_file=material["server_cert"],
        evidence_server_key_file=material["server_key"],
        evidence_client_ca_file=material["ca_file"],
        evidence_listen_host="127.0.0.1",
        evidence_listen_port=0,
        evidence_node_cert_file=material["node_cert"],
        evidence_node_key_file=material["node_key"],
        evidence_backend_ca_file=material["ca_file"],
        evidence_backend_host="localhost",
        evidence_backend_port=0,
        evidence_backend_server_name="localhost",
        evidence_relay_socket=str(root / "evidence-relay.sock"),
        evidence_relay_allowed_uid=relay_uid,
        evidence_relay_allowed_gid=-1,
        evidence_timeout_seconds=5,
        evidence_relay_enabled=relay,
        service_endpoint_enabled=service,
        service_relay_enabled=service,
        service_relay_socket=str(root / "service-relay.sock"),
        service_relay_allowed_uid=relay_uid,
        service_relay_allowed_gid=-1,
        service_upstream_host="127.0.0.1",
        service_max_concurrency=8,
    )


class LocalStand:
    def __init__(self, settings: Settings, root: Path) -> None:
        self._settings = settings
        self._root = root
        self._database = Database(settings)
        self._runner: web.AppRunner | None = None
        self._relay: EvidenceRelay | None = None
        self._service_relay: ServiceRelay | None = None
        self._api_port = 0
        self._evidence_port = 0
        self._relay_socket: str | None = None
        self._service_socket: str | None = None

    async def start(self) -> dict[str, Any]:
        if not self._settings.api_port:
            with socket.socket() as probe:
                probe.bind(("127.0.0.1", 0))
                chosen = int(probe.getsockname()[1])
            self._settings = replace(self._settings, api_port=chosen)
        await self._database.ensure_ready()
        app = create_app(self._settings, self._database)
        self._runner = web.AppRunner(app)
        await self._runner.setup()
        public = web.TCPSite(self._runner, "127.0.0.1", self._settings.api_port)
        await public.start()
        self._api_port = int(public._server.sockets[0].getsockname()[1])
        listener = app.get(EVIDENCE_LISTENER_KEY)
        if listener is None:
            raise RuntimeError("evidence/service listener did not start")
        self._evidence_port = listener.port
        if self._settings.evidence_relay_enabled:
            relay_settings = replace(
                self._settings,
                evidence_backend_host="localhost",
                evidence_backend_port=self._evidence_port,
                evidence_endpoint_enabled=False,
            )
            self._relay = EvidenceRelay(relay_settings)
            self._relay_socket = await self._relay.start()
        if self._settings.service_relay_enabled:
            service_settings = replace(
                self._settings,
                api_port=self._api_port,
                evidence_backend_host="localhost",
                evidence_backend_port=self._evidence_port,
            )
            self._service_relay = ServiceRelay(service_settings)
            self._service_socket = await self._service_relay.start()
        readiness = {
            "environment": self._settings.environment,
            "gateway_key": GATEWAY_KEY,
            "api_base": f"http://127.0.0.1:{self._api_port}",
            "evidence": {
                "host": "localhost",
                "port": self._evidence_port,
                "server_cert_spki_sha256": _server_spki(self._settings),
            },
            "service_enabled": self._settings.service_endpoint_enabled,
            "relay_socket": self._relay_socket,
            "service_socket": self._service_socket,
            "created_at": dt.datetime.now(dt.UTC).isoformat(),
        }
        _write_private(self._root / "readiness.json", json.dumps(readiness, indent=2).encode())
        return readiness

    def _invalidate_readiness(self) -> None:
        readiness = self._root / "readiness.json"
        try:
            _write_private(readiness, json.dumps({"status": "stopped"}).encode())
        except OSError as exc:
            logging.getLogger(__name__).warning("readiness invalidation failed: %s", exc)

    async def _cleanup(self, name: str, resource: Any, method: str, bound: float) -> str | None:
        try:
            await asyncio.wait_for(getattr(resource, method)(), timeout=bound)
        except TimeoutError:
            logging.getLogger(__name__).warning("local stand %s stop timed out", name)
            return f"{name}: timeout"
        except (OSError, RuntimeError) as exc:
            logging.getLogger(__name__).warning("local stand %s stop failed: %s", name, exc)
            return f"{name}: {exc}"
        return None

    async def stop(self, per_resource_timeout: float = 5.0) -> list[str]:
        """Invalidate readiness first, then run every cleanup in parallel, each bounded.

        Parallel independent bounds keep an outer shutdown budget meaningful: a slow or failing
        resource can no longer starve the following cleanups.
        """
        self._invalidate_readiness()
        jobs: list[Coroutine[Any, Any, str | None]] = []
        for name, attribute, method in (
            ("service_relay", "_service_relay", "stop"),
            ("relay", "_relay", "stop"),
            ("runner", "_runner", "cleanup"),
        ):
            resource = getattr(self, attribute)
            setattr(self, attribute, None)
            if resource is not None:
                jobs.append(self._cleanup(name, resource, method, per_resource_timeout))
        database = self._database
        jobs.append(self._cleanup("database", database, "close", per_resource_timeout))
        results = await asyncio.gather(*jobs)
        return [item for item in results if item is not None]

def _server_spki(settings: Settings) -> str:
    public = x509.load_pem_x509_certificate(
        Path(settings.evidence_server_cert_file).read_bytes()
    ).public_key()
    return hashlib.sha256(
        public.public_bytes(
            serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo
        )
    ).hexdigest()


async def run_bounded(stand: Any, stop_event: asyncio.Event) -> dict[str, Any] | None:
    """Race startup against the stop signal; never publish readiness after a stop signal.

    The startup task and the stop waiter are both settled in the finally block for every
    outcome (ready, signal, startup exception), so no task is left pending, and the cleanup
    runs exactly once for whatever was partially started.
    """
    startup = asyncio.ensure_future(stand.start())
    stop_wait = asyncio.ensure_future(stop_event.wait())
    result: dict[str, Any] | None = None
    try:
        done, _pending = await asyncio.wait(
            {startup, stop_wait}, return_when=asyncio.FIRST_COMPLETED
        )
        if startup in done:
            result = startup.result()
            if not stop_event.is_set():
                print(
                    json.dumps(
                        {
                            "status": "ready",
                            "api_base": result["api_base"],
                            "evidence_port": result["evidence"]["port"],
                        }
                    )
                )
            else:
                result = None
        if not stop_event.is_set() and result is not None:
            await stop_event.wait()
    finally:
        if not stop_wait.done():
            stop_wait.cancel()
        with contextlib.suppress(BaseException):
            await stop_wait
        if not startup.done():
            startup.cancel()
            with contextlib.suppress(BaseException):
                await asyncio.wait_for(startup, timeout=10)
        try:
            await asyncio.wait_for(stand.stop(), timeout=15)
        except TimeoutError:
            logging.getLogger(__name__).warning("local stand shutdown exceeded the bound")
        except (OSError, RuntimeError) as exc:
            logging.getLogger(__name__).warning("local stand shutdown error: %s", exc)
    return result


async def _run(args: argparse.Namespace) -> None:
    root = Path(args.root).resolve()
    settings = build_stand_settings(
        args.database_url, root, service=args.service, relay=not args.no_relay
    )
    stand = LocalStand(settings, root)
    stop_event = asyncio.Event()
    loop = asyncio.get_event_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop_event.set)
    await run_bounded(stand, stop_event)


def main() -> None:
    parser = argparse.ArgumentParser(description="TEST-only local backend stand")
    parser.add_argument("--database-url", required=True)
    parser.add_argument("--root", required=True)
    parser.add_argument("--service", action="store_true")
    parser.add_argument("--no-relay", action="store_true")
    asyncio.run(_run(parser.parse_args()))


if __name__ == "__main__":
    main()
