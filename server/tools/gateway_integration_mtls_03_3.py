#!/usr/bin/env python3
"""STEP03.3 correction1: real mTLS management-handler in front of the isolated gateway.

Chain: Backend worker -> ManagementTlsClient (client cert) -> management-handler (mTLS,
identity/node/field checks) -> local client-test admin.sock -> real isolated wdtt-server.
Negatives: wrong node, wrong backend identity, untrusted CA, unknown/dangerous command.
"""

from __future__ import annotations

import argparse
import asyncio
import ipaddress
import json
import os
import shutil
import sys
import tempfile
from datetime import UTC, datetime, timedelta
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import gateway_integration_03_3 as base
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID

from terlimo_backend.config import Settings
from terlimo_backend.db import Database
from terlimo_backend.gateway_adapter import GatewayError, ManagementTlsClient
from terlimo_backend.gateway_control import (
    GatewayControlHandlers,
    ensure_grant,
    revoke_binding_grants,
)
from terlimo_backend.management_handler import HandlerConfig, ManagementHandler
from terlimo_backend.migrations import runner
from terlimo_backend.worker import OutboxWorker

SERVER_NAME = "gw-node.test"
BACKEND_IDENTITY = "terlimo-backend-test"


def write_key_cert(key_path: Path, cert_path: Path, key, cert) -> None:
    key_path.write_bytes(
        key.private_bytes(
            serialization.Encoding.PEM,
            serialization.PrivateFormat.PKCS8,
            serialization.NoEncryption(),
        )
    )
    cert_path.write_bytes(cert.public_bytes(serialization.Encoding.PEM))


def make_ca(name: str):
    key = ec.generate_private_key(ec.SECP256R1())
    subject = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, name)])
    now = datetime.now(UTC)
    cert = (
        x509.CertificateBuilder()
        .subject_name(subject)
        .issuer_name(subject)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - timedelta(days=1))
        .not_valid_after(now + timedelta(days=2))
        .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
        .sign(key, hashes.SHA256())
    )
    return key, cert


def make_leaf(ca_key, ca_cert, common_name: str, *, san=None):
    key = ec.generate_private_key(ec.SECP256R1())
    now = datetime.now(UTC)
    builder = (
        x509.CertificateBuilder()
        .subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, common_name)]))
        .issuer_name(ca_cert.subject)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - timedelta(days=1))
        .not_valid_after(now + timedelta(days=2))
        .add_extension(x509.BasicConstraints(ca=False, path_length=None), critical=True)
    )
    if san:
        builder = builder.add_extension(x509.SubjectAlternativeName(san), critical=False)
    return key, builder.sign(ca_key, hashes.SHA256())


async def raw_tls_request(port: int, pki: dict, request: dict) -> dict:
    import ssl

    context = ssl.create_default_context(cafile=pki["ca_file"])
    context.load_cert_chain(pki["backend_cert"], pki["backend_key"])
    context.check_hostname = True
    reader, writer = await asyncio.open_connection(
        "127.0.0.1", port, ssl=context, server_hostname=SERVER_NAME
    )
    writer.write(json.dumps(request).encode())
    await writer.drain()
    body = await reader.read()
    writer.close()
    return json.loads(body.decode()) if body else {}


async def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--work", default="/tmp/terlimo-033-mtls-gateway")
    parser.add_argument("--listen-port", type=int, default=56310)
    parser.add_argument("--wg-port", type=int, default=56312)
    arguments = parser.parse_args()

    gateway = base.IsolatedGateway(arguments.work, arguments.listen_port, arguments.wg_port)
    pki_dir = Path(tempfile.mkdtemp(prefix="terlimo-033-pki-"))
    pgdata = Path(tempfile.mkdtemp(prefix="terlimo-033-mtls-pg-"))
    server = None
    handler = None
    failures: list[str] = []
    try:
        # Temporary TEST PKI (never committed, removed in cleanup).
        ca_key, ca_cert = make_ca("terlimo-test-ca")
        write_key_cert(pki_dir / "ca.key", pki_dir / "ca.pem", ca_key, ca_cert)
        server_key, server_cert = make_leaf(
            ca_key,
            ca_cert,
            SERVER_NAME,
            san=[x509.DNSName(SERVER_NAME), x509.IPAddress(ipaddress.ip_address("127.0.0.1"))],
        )
        write_key_cert(pki_dir / "server.key", pki_dir / "server.pem", server_key, server_cert)
        backend_key, backend_cert = make_leaf(ca_key, ca_cert, BACKEND_IDENTITY)
        write_key_cert(pki_dir / "backend.key", pki_dir / "backend.pem", backend_key, backend_cert)
        other_key, other_cert = make_leaf(ca_key, ca_cert, "other-backend")
        write_key_cert(pki_dir / "other.key", pki_dir / "other.pem", other_key, other_cert)
        foreign_ca_key, foreign_ca_cert = make_ca("foreign-ca")
        foreign_key, foreign_cert = make_leaf(foreign_ca_key, foreign_ca_cert, BACKEND_IDENTITY)
        write_key_cert(pki_dir / "foreign.key", pki_dir / "foreign.pem", foreign_key, foreign_cert)
        pki = {
            "ca_file": str(pki_dir / "ca.pem"),
            "backend_cert": str(pki_dir / "backend.pem"),
            "backend_key": str(pki_dir / "backend.key"),
            "other_cert": str(pki_dir / "other.pem"),
            "other_key": str(pki_dir / "other.key"),
            "foreign_cert": str(pki_dir / "foreign.pem"),
            "foreign_key": str(pki_dir / "foreign.key"),
        }

        base.say("starting isolated real gateway")
        gateway.start()
        gateway.wait_ready()

        config = HandlerConfig()
        config.bind = "127.0.0.1"
        config.port = 0
        config.server_cert = str(pki_dir / "server.pem")
        config.server_key = str(pki_dir / "server.key")
        config.client_ca = pki["ca_file"]
        config.allowed_identities = frozenset({BACKEND_IDENTITY})
        config.node_id = base.NODE_ID
        config.admin_socket = os.path.join(arguments.work, "admin.sock")
        config.main_password = base.MAIN_PASSWORD
        config.max_not_after = 86400
        handler = ManagementHandler(config)
        handler_port = await handler.start()
        base.evidence("management_handler", {"port": handler_port, "node": base.NODE_ID})

        server = base.pgserver.get_server(pgdata)
        server.ensure_pgdata_inited()
        server.ensure_postgres_running()
        admin_dsn = server.get_uri()
        name = "terlimo_033_mtls"
        connection = await base.asyncpg.connect(admin_dsn, timeout=10)
        await connection.execute(f'CREATE DATABASE "{name}"')
        await connection.close()
        parts = base.urlsplit(admin_dsn)
        database_url = base.urlunsplit(
            (parts.scheme, parts.netloc, f"/{name}", parts.query, parts.fragment)
        )
        connection = await base.asyncpg.connect(database_url, timeout=10)
        await runner.apply_migrations(connection)
        account = await connection.fetchval(
            "INSERT INTO accounts (status) VALUES ('verified') RETURNING id"
        )
        installation = await connection.fetchval(
            """
            INSERT INTO installations (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test','android',$1,$2,'technical') RETURNING id
            """,
            base.FINGERPRINT,
            base.SPKI,
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
            VALUES ('terlimo-033-mtls','test',$1::jsonb,'registered') RETURNING id
            """,
            json.dumps(
                {
                    "node_id": base.NODE_ID,
                    "management": {
                        "host": "127.0.0.1",
                        "port": handler_port,
                        "server_name": SERVER_NAME,
                    },
                }
            ),
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
            gateway_management_ca_file=pki["ca_file"],
            gateway_management_cert_file=pki["backend_cert"],
            gateway_management_key_file=pki["backend_key"],
            gateway_max_lease_seconds=900,
        )
        database = Database(settings)
        await database.connect()
        handlers = GatewayControlHandlers(settings).as_handlers()
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="integration-mtls")

        base.say("apply through mTLS handler -> local admin socket -> real gateway")
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
        assert await worker.drain() == 1
        grant = json.loads(
            await base.db_fetchval(database_url, "SELECT row_to_json(g) FROM grants AS g")
        )
        credential = grant["gateway_credential"]
        mts_client = ManagementTlsClient(
            host="127.0.0.1",
            port=handler_port,
            server_name=SERVER_NAME,
            ca_file=pki["ca_file"],
            cert_file=pki["backend_cert"],
            key_file=pki["backend_key"],
            node_id=base.NODE_ID,
            timeout_seconds=5,
        )
        readback = await mts_client.grant_get(credential, base.NODE_ID)
        base.evidence(
            "mtls_apply_readback",
            {
                "state": grant["state"],
                "runtime_applied": readback["runtime_applied"],
                "generation": readback["generation"],
                "lease_seq": readback["lease_seq"],
            },
        )
        if grant["state"] != "applied" or readback["runtime_applied"] is not True:
            failures.append("mTLS apply did not reach applied")
        baseline_lease_seq = readback["lease_seq"]

        base.say("negatives: wrong node / wrong identity / untrusted CA / unknown command")
        wrong_node = ManagementTlsClient(
            host="127.0.0.1",
            port=handler_port,
            server_name=SERVER_NAME,
            ca_file=pki["ca_file"],
            cert_file=pki["backend_cert"],
            key_file=pki["backend_key"],
            node_id="other-node",
            timeout_seconds=5,
        )
        try:
            await wrong_node.grant_get(credential, "other-node")
            failures.append("wrong node id accepted")
        except GatewayError as error:
            base.evidence("negative_wrong_node", {"code": error.code})

        import ssl

        wrong_identity_context = ssl.create_default_context(cafile=pki["ca_file"])
        wrong_identity_context.load_cert_chain(pki["other_cert"], pki["other_key"])
        wrong_identity_context.check_hostname = True
        try:
            reader, writer = await asyncio.open_connection(
                "127.0.0.1", handler_port, ssl=wrong_identity_context, server_hostname=SERVER_NAME
            )
            writer.write(
                json.dumps(
                    {"v": 1, "op": "get", "node_id": base.NODE_ID, "credential": credential, "fields": {}}
                ).encode()
            )
            await writer.drain()
            body = await reader.read()
            writer.close()
            response = json.loads(body.decode()) if body else {}
            base.evidence("negative_wrong_identity", response)
            if response.get("code") != "AUTHZ_DENIED":
                failures.append("wrong backend identity was not denied")
        except (ssl.SSLError, ConnectionError, OSError) as error:
            base.evidence("negative_wrong_identity", {"tls": type(error).__name__})

        try:
            foreign = ManagementTlsClient(
                host="127.0.0.1",
                port=handler_port,
                server_name=SERVER_NAME,
                ca_file=pki["ca_file"],
                cert_file=pki["foreign_cert"],
                key_file=pki["foreign_key"],
                node_id=base.NODE_ID,
                timeout_seconds=5,
            )
            await foreign.engine_status()
            failures.append("untrusted client CA accepted")
        except (GatewayError, ssl.SSLError, ConnectionError, OSError) as error:
            base.evidence(
                "negative_untrusted_ca",
                {"code": getattr(error, "code", type(error).__name__)},
            )

        unknown = await raw_tls_request(
            handler_port, pki, {"v": 1, "op": "shell", "node_id": base.NODE_ID, "fields": {}}
        )
        base.evidence("negative_unknown_command", unknown)
        if unknown.get("code") != "BAD_MESSAGE":
            failures.append("unknown command was not refused")

        after_negatives = await mts_client.grant_get(credential, base.NODE_ID)
        if after_negatives["lease_seq"] != baseline_lease_seq:
            failures.append("a refused request touched the gateway")

        base.say("revoke through mTLS; handler restart keeps the node identity/fence view")
        async with database.acquire() as pool_connection:
            assert await revoke_binding_grants(pool_connection, binding_id=binding) == 1
        assert await worker.drain() == 1
        revoked = await mts_client.grant_get(credential, base.NODE_ID)
        base.evidence("mtls_revoke_readback", {"revoked": revoked["revoked"], "generation": revoked["generation"]})
        if revoked["revoked"] is not True or int(revoked["generation"]) != 2:
            failures.append("mTLS revoke did not apply")

        await handler.stop()
        handler2 = ManagementHandler(config)
        handler_port2 = await handler2.start()
        handler = handler2
        mts_client2 = ManagementTlsClient(
            host="127.0.0.1",
            port=handler_port2,
            server_name=SERVER_NAME,
            ca_file=pki["ca_file"],
            cert_file=pki["backend_cert"],
            key_file=pki["backend_key"],
            node_id=base.NODE_ID,
            timeout_seconds=5,
        )
        after_restart = await mts_client2.grant_get(credential, base.NODE_ID)
        base.evidence(
            "handler_restart_readback",
            {"revoked": after_restart["revoked"], "generation": after_restart["generation"]},
        )
        if after_restart["revoked"] is not True or int(after_restart["generation"]) != 2:
            failures.append("handler restart lost the gateway fence view")

        await database.close()
    finally:
        if handler is not None:
            await handler.stop()
        gateway.cleanup()
        if server is not None:
            server.cleanup()
        shutil.rmtree(pgdata, ignore_errors=True)
        shutil.rmtree(pki_dir, ignore_errors=True)
        shutil.rmtree(base._RUNTIME, ignore_errors=True)

    if failures:
        base.say(f"MTLS INTEGRATION FAILED: {failures}")
        return 1
    base.say("MTLS INTEGRATION PASSED")
    return 0


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
