"""G6 evidence transport: dedicated mTLS listener through the production entrypoint.

Tests drive the real builders (`create_app` startup/shutdown, `EvidenceListener`,
`build_endpoint_ssl_context`, `EvidenceRelay`) with ephemeral CAs/certs, local sockets and
the bundled TEST PostgreSQL. Roles are configured independently; no live material is used.
"""
from __future__ import annotations

import asyncio
import datetime as dt
import json
import os
import ssl
import uuid
from dataclasses import replace

import aiohttp
import pytest
from aiohttp import web
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID
from test_step036_onboarding_hour_storage import (
    FakeBootstrapClient,
    FakeCipher,
    _connect,
    _counts,
    _provision,
    _seed_gateway,
    _seed_installation,
)

from terlimo_backend.api import create_app
from terlimo_backend.config import ConfigError, load_settings
from terlimo_backend.db import Database
from terlimo_backend.evidence_transport import (
    EVIDENCE_LISTENER_KEY,
    EVIDENCE_PATH,
    EvidenceRelay,
    EvidenceTransportError,
    build_endpoint_ssl_context,
    require_endpoint_material,
    require_relay_material,
)
from terlimo_backend.onboarding_hour import create_intent

GATEWAY_KEY = "terlimo-035-node"


def _write(path, data: bytes) -> str:
    path.write_bytes(data)
    os.chmod(path, 0o600)
    return str(path)


def _pem_key(key) -> bytes:
    return key.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    )


def _pem_cert(cert) -> bytes:
    return cert.public_bytes(serialization.Encoding.PEM)


def _make_ca(tmp_path, name: str):
    key = ec.generate_private_key(ec.SECP256R1())
    subject = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, name)])
    moment = dt.datetime.now(dt.UTC)
    cert = (
        x509.CertificateBuilder()
        .subject_name(subject)
        .issuer_name(subject)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(moment - dt.timedelta(hours=1))
        .not_valid_after(moment + dt.timedelta(days=1))
        .add_extension(x509.BasicConstraints(ca=True, path_length=0), critical=True)
        .sign(key, hashes.SHA256())
    )
    ca_file = _write(tmp_path / f"{name}.pem", _pem_cert(cert))
    return key, cert, ca_file


def _make_leaf(tmp_path, ca_key, ca_cert, cn: str, *, san: list[str] | None = None):
    key = ec.generate_private_key(ec.SECP256R1())
    moment = dt.datetime.now(dt.UTC)
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
    return _write(tmp_path / f"{cn}.pem", _pem_cert(cert)), _write(
        tmp_path / f"{cn}.key.pem", _pem_key(key)
    )


def _client_context(ca_file: str, cert_file: str, key_file: str) -> ssl.SSLContext:
    context = ssl.create_default_context(cafile=ca_file)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.check_hostname = True
    context.verify_mode = ssl.CERT_REQUIRED
    context.load_cert_chain(cert_file, key_file)
    return context


class _Material:
    def __init__(self, tmp_path):
        self.tmp = tmp_path
        self.ca_key, self.ca_cert, self.ca_file = _make_ca(tmp_path, "node-ca")
        self.foreign_ca_key, self.foreign_ca_cert, self.foreign_ca_file = _make_ca(
            tmp_path, "foreign-ca"
        )
        self.server_cert, self.server_key = _make_leaf(
            tmp_path, self.ca_key, self.ca_cert, "localhost", san=["localhost"]
        )
        self.nodes = {
            GATEWAY_KEY: _make_leaf(tmp_path, self.ca_key, self.ca_cert, GATEWAY_KEY),
            "terlimo-other": _make_leaf(tmp_path, self.ca_key, self.ca_cert, "terlimo-other"),
            "terlimo-unknown": _make_leaf(tmp_path, self.ca_key, self.ca_cert, "terlimo-unknown"),
            "terlimo-foreign": _make_leaf(
                tmp_path, self.foreign_ca_key, self.foreign_ca_cert, "terlimo-foreign"
            ),
        }

    def node_context(self, key: str = GATEWAY_KEY) -> ssl.SSLContext:
        cert_file, key_file = self.nodes[key]
        return _client_context(self.ca_file, cert_file, key_file)


def _settings(settings_factory, url: str, material: _Material, **overrides):
    values = {
        "evidence_endpoint_enabled": True,
        "evidence_server_cert_file": material.server_cert,
        "evidence_server_key_file": material.server_key,
        "evidence_client_ca_file": material.ca_file,
        "evidence_listen_host": "127.0.0.1",
        "evidence_listen_port": 0,
        "evidence_relay_enabled": True,
        "evidence_node_cert_file": material.nodes[GATEWAY_KEY][0],
        "evidence_node_key_file": material.nodes[GATEWAY_KEY][1],
        "evidence_backend_ca_file": material.ca_file,
        "evidence_backend_host": "localhost",
        "evidence_backend_port": 0,
        "evidence_backend_server_name": "localhost",
        "evidence_relay_socket": str(material.tmp / "evidence.sock"),
        "evidence_relay_allowed_uid": os.getuid(),
        "evidence_relay_allowed_gid": -1,
        "evidence_timeout_seconds": 5,
    }
    values.update(overrides)
    return settings_factory(url, **values)


async def _start_app(settings, database):
    app = create_app(settings, database)
    runner = web.AppRunner(app)
    await runner.setup()
    return app, runner


async def _ready_intent(url: str, request_key: str, gateway_key: str = GATEWAY_KEY):
    installation_id = await _seed_installation(url)
    gateway_id, _ = await _seed_gateway(url, key=gateway_key)
    connection = await _connect(url)
    try:
        created = await create_intent(
            connection,
            installation_id=installation_id,
            request_key=request_key,
            environment="test",
            cipher=FakeCipher(),
        )
    finally:
        await connection.close()
    await _provision(url, created["id"], FakeBootstrapClient())
    return created, gateway_id


def _unknown_op_envelope(credential_id: str, *, connection_id: str = "c" * 32):
    # Unknown operation: rejected before any database work.
    return json.dumps(
        {
            "credential_id": credential_id,
            "connection_id": connection_id,
            "request_id": "req-unknown",
            "body": {
                "v": 1,
                "op": "background.keepalive",
                "intent_id": str(uuid.uuid4()),
                "request_key": "transport-test",
                "proof": {},
            },
        }
    ).encode() + b"\n"


def _envelope(credential_id: str, *, connection_id: str = "c" * 32, request_id: str = "req-1"):
    # Structurally valid start body with non-verifiable dummy proof material: reaches the
    # registry/credential gates so those failures can be asserted without a real signing key.
    return json.dumps(
        {
            "credential_id": credential_id,
            "connection_id": connection_id,
            "request_id": request_id,
            "body": {
                "v": 1,
                "op": "onboarding.start",
                "intent_id": str(uuid.uuid4()),
                "request_key": "transport-test",
                "proof": {
                    "algorithm": "ES256",
                    "environment": "test",
                    "request_id": "a" * 32,
                    "challenge_id": "b" * 16,
                    "nonce_b64": "c" * 32,
                    "payload_hash": "d" * 64,
                    "signed_payload_b64": "e" * 64,
                    "signature_b64": "f" * 64,
                },
            },
        }
    ).encode() + b"\n"


async def _post(session, port: int, payload: bytes):
    async with session.post(
        f"https://localhost:{port}{EVIDENCE_PATH}", data=payload
    ) as response:
        return response.status, await response.json()


async def test_evidence_unknown_operation_rejected_without_writes(
    migrated_url, settings_factory, tmp_path
):
    material = _Material(tmp_path)
    settings = _settings(settings_factory, migrated_url, material)
    database = Database(settings)
    created, _ = await _ready_intent(migrated_url, "ev-key-1")
    app, runner = await _start_app(settings, database)
    session = aiohttp.ClientSession(
        connector=aiohttp.TCPConnector(ssl=material.node_context())
    )
    try:
        port = app[EVIDENCE_LISTENER_KEY].port
        assert port > 0
        status, body = await _post(session, port, _unknown_op_envelope(created["credential_id"]))
        assert status == 400 and body["code"] == "EVIDENCE_UNKNOWN_OPERATION"
        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0
        connection = await _connect(migrated_url)
        try:
            assert await connection.fetchval(
                "SELECT state FROM onboarding_intents WHERE id = $1", uuid.UUID(created["id"])
            ) == "ready"
        finally:
            await connection.close()
    finally:
        await session.close()
        await runner.cleanup()


async def test_evidence_listener_identity_comes_only_from_certificate(
    migrated_url, settings_factory, tmp_path
):
    material = _Material(tmp_path)
    settings = _settings(settings_factory, migrated_url, material)
    database = Database(settings)
    created, _ = await _ready_intent(migrated_url, "ev-key-2")
    await _seed_gateway(migrated_url, key="terlimo-other")
    app, runner = await _start_app(settings, database)
    try:
        port = app[EVIDENCE_LISTENER_KEY].port
        session = aiohttp.ClientSession(
            connector=aiohttp.TCPConnector(ssl=material.node_context())
        )
        try:
            bad = json.loads(_envelope(created["credential_id"]))
            bad["body"]["environment"] = "prod"
            status, body = await _post(session, port, json.dumps(bad).encode() + b"\n")
            assert status == 400 and body["code"] == "EVIDENCE_UNKNOWN_OPERATION"
        finally:
            await session.close()

        other = aiohttp.ClientSession(
            connector=aiohttp.TCPConnector(ssl=material.node_context("terlimo-other"))
        )
        try:
            status, body = await _post(other, port, _envelope(created["credential_id"]))
            assert status == 403 and body["code"] == "ONBOARDING_ENV_MISMATCH"
        finally:
            await other.close()

        unknown = aiohttp.ClientSession(
            connector=aiohttp.TCPConnector(ssl=material.node_context("terlimo-unknown"))
        )
        try:
            status, body = await _post(unknown, port, _envelope(created["credential_id"]))
            assert status == 403 and body["code"] == "EVIDENCE_REGISTRY_UNKNOWN"
        finally:
            await unknown.close()

        counts = await _counts(migrated_url)
        assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0
    finally:
        await runner.cleanup()


async def test_evidence_listener_requires_verified_client_certificate(
    migrated_url, settings_factory, tmp_path
):
    material = _Material(tmp_path)
    settings = _settings(settings_factory, migrated_url, material)
    database = Database(settings)
    created, _ = await _ready_intent(migrated_url, "ev-key-3")
    app, runner = await _start_app(settings, database)
    try:
        port = app[EVIDENCE_LISTENER_KEY].port
        assert not any(
            getattr(route.resource, "canonical", None) == EVIDENCE_PATH
            for route in app.router.routes()
        )
        with pytest.raises(aiohttp.ClientError):
            anonymous = aiohttp.ClientSession()
            try:
                await _post(anonymous, port, _envelope(created["credential_id"]))
            finally:
                await anonymous.close()
        with pytest.raises(aiohttp.ClientError):
            async with aiohttp.ClientSession() as plain:
                await plain.post(
                    f"http://127.0.0.1:{port}{EVIDENCE_PATH}",
                    data=_envelope(created["credential_id"]),
                )
        with pytest.raises(aiohttp.ClientError):
            untrusted = aiohttp.ClientSession(
                connector=aiohttp.TCPConnector(ssl=material.node_context("terlimo-foreign"))
            )
            try:
                await _post(untrusted, port, _envelope(created["credential_id"]))
            finally:
                await untrusted.close()
    finally:
        await runner.cleanup()
    counts = await _counts(migrated_url)
    assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0
    connection = await _connect(migrated_url)
    try:
        assert await connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", uuid.UUID(created["id"])
        ) == "ready"
    finally:
        await connection.close()


async def test_evidence_unknown_credential_writes_nothing(migrated_url, settings_factory, tmp_path):
    material = _Material(tmp_path)
    settings = _settings(settings_factory, migrated_url, material)
    database = Database(settings)
    await database.ensure_ready()
    await _seed_gateway(migrated_url, key=GATEWAY_KEY)
    app, runner = await _start_app(settings, database)
    session = aiohttp.ClientSession(connector=aiohttp.TCPConnector(ssl=material.node_context()))
    try:
        port = app[EVIDENCE_LISTENER_KEY].port
        status, body = await _post(session, port, _envelope("unknown-credential"))
        assert status == 403 and body["code"] == "ONBOARDING_CREDENTIAL_UNKNOWN"
    finally:
        await session.close()
        await runner.cleanup()
    counts = await _counts(migrated_url)
    assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0


async def test_evidence_roles_are_independent_and_relay_restricts_peer(
    migrated_url, settings_factory, tmp_path
):
    material = _Material(tmp_path)
    settings = _settings(settings_factory, migrated_url, material)
    database = Database(settings)
    created, _ = await _ready_intent(migrated_url, "ev-key-5")
    app, runner = await _start_app(settings, database)
    relay_settings = replace(
        settings,
        evidence_endpoint_enabled=False,
        evidence_server_cert_file="",
        evidence_server_key_file="",
        evidence_client_ca_file="",
        evidence_backend_port=app[EVIDENCE_LISTENER_KEY].port,
    )
    relay = EvidenceRelay(relay_settings)
    try:
        socket_path = await relay.start()
        assert oct(os.stat(socket_path).st_mode & 0o777) == "0o600"
        reader, writer = await asyncio.open_unix_connection(socket_path)
        writer.write(_envelope(created["credential_id"]))
        await writer.drain()
        response = await asyncio.wait_for(reader.read(), timeout=10)
        writer.close()
        await writer.wait_closed()
        payload = json.loads(response)
        assert payload["status"] == "error", payload

        wrong = replace(
            relay_settings,
            evidence_relay_allowed_uid=os.getuid() + 1,
            evidence_relay_socket=str(material.tmp / "evidence-wrong.sock"),
        )
        relay_wrong = EvidenceRelay(wrong)
        wrong_path = await relay_wrong.start()
        reader, writer = await asyncio.open_unix_connection(wrong_path)
        writer.write(_envelope("unknown-credential"))
        await writer.drain()
        try:
            rejected = await asyncio.wait_for(reader.read(), timeout=10)
        except ConnectionResetError:
            rejected = b""
        assert rejected == b""
        writer.close()
        try:
            await writer.wait_closed()
        except ConnectionResetError:
            pass
        await relay_wrong.stop()
    finally:
        await relay.stop()
        await runner.cleanup()
    counts = await _counts(migrated_url)
    assert counts["onboarding_evidence"] == 0 and counts["entitlements"] == 0

    endpoint_only = replace(
        settings,
        evidence_node_cert_file="",
        evidence_node_key_file="",
        evidence_backend_ca_file="",
        evidence_backend_host="",
        evidence_backend_server_name="",
        evidence_relay_socket="",
        evidence_relay_allowed_uid=-1,
    )
    context = build_endpoint_ssl_context(endpoint_only)
    assert context.verify_mode == ssl.CERT_REQUIRED
    with pytest.raises(EvidenceTransportError):
        EvidenceRelay(endpoint_only)


async def test_evidence_disabled_and_role_config_fail_closed(
    migrated_url, settings_factory, monkeypatch
):
    settings = settings_factory(migrated_url)
    app, runner = await _start_app(settings, Database(settings))
    try:
        assert app.get(EVIDENCE_LISTENER_KEY) is None
        assert not any(
            getattr(route.resource, "canonical", None) == EVIDENCE_PATH
            for route in app.router.routes()
        )
    finally:
        await runner.cleanup()
    with pytest.raises(EvidenceTransportError) as disabled:
        require_endpoint_material(settings)
    assert disabled.value.code == "ONBOARDING_EVIDENCE_ENDPOINT_DISABLED"
    with pytest.raises(EvidenceTransportError) as relay_disabled:
        require_relay_material(settings)
    assert relay_disabled.value.code == "ONBOARDING_EVIDENCE_RELAY_DISABLED"

    for name in (
        "ONBOARDING_EVIDENCE_SERVER_CERT_FILE",
        "ONBOARDING_EVIDENCE_CLIENT_CA_FILE",
        "ONBOARDING_EVIDENCE_NODE_CERT_FILE",
        "ONBOARDING_EVIDENCE_BACKEND_PORT",
        "ONBOARDING_EVIDENCE_RELAY_UID",
    ):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("ONBOARDING_EVIDENCE_ENDPOINT_ENABLED", "1")
    monkeypatch.delenv("ONBOARDING_EVIDENCE_RELAY_ENABLED", raising=False)
    with pytest.raises(ConfigError):
        load_settings(require_database=False)
    monkeypatch.setenv("ONBOARDING_EVIDENCE_SERVER_CERT_FILE", "/nonexistent-server.pem")
    monkeypatch.setenv("ONBOARDING_EVIDENCE_SERVER_KEY_FILE", "/nonexistent-server.key")
    monkeypatch.setenv("ONBOARDING_EVIDENCE_CLIENT_CA_FILE", "/nonexistent-ca.pem")
    monkeypatch.setenv("ONBOARDING_EVIDENCE_NODE_CERT_FILE", "/nonexistent-node.pem")
    monkeypatch.setenv("ONBOARDING_EVIDENCE_NODE_KEY_FILE", "/nonexistent-node.key")
    monkeypatch.setenv("ONBOARDING_EVIDENCE_BACKEND_CA_FILE", "/nonexistent-backend-ca.pem")
    monkeypatch.setenv("ONBOARDING_EVIDENCE_BACKEND_HOST", "localhost")
    monkeypatch.setenv("ONBOARDING_EVIDENCE_BACKEND_SERVER_NAME", "localhost")
    monkeypatch.setenv("ONBOARDING_EVIDENCE_RELAY_SOCKET", "/tmp/evidence.sock")
    monkeypatch.setenv("ONBOARDING_EVIDENCE_RELAY_UID", str(os.getuid()))
    monkeypatch.setenv("ONBOARDING_EVIDENCE_BACKEND_PORT", "443")
    endpoint_only = load_settings(require_database=False)
    assert endpoint_only.evidence_endpoint_enabled and not endpoint_only.evidence_relay_enabled
    with pytest.raises(EvidenceTransportError):
        require_relay_material(endpoint_only)
    monkeypatch.setenv("ONBOARDING_EVIDENCE_RELAY_ENABLED", "1")
    monkeypatch.delenv("ONBOARDING_EVIDENCE_ENDPOINT_ENABLED", raising=False)
    monkeypatch.delenv("ONBOARDING_EVIDENCE_SERVER_CERT_FILE", raising=False)
    monkeypatch.delenv("ONBOARDING_EVIDENCE_SERVER_KEY_FILE", raising=False)
    monkeypatch.delenv("ONBOARDING_EVIDENCE_CLIENT_CA_FILE", raising=False)
    relay_only = load_settings(require_database=False)
    assert relay_only.evidence_relay_enabled and not relay_only.evidence_endpoint_enabled
    with pytest.raises(EvidenceTransportError):
        require_endpoint_material(relay_only)
