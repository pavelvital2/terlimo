"""mTLS management handler tests: identity checks, strict typed ops, no local call on refusal.

Uses temporary CA/server/client certificates; the "local admin socket" is the wire-faithful
fake, so a refused request can be proven to never reach the local surface.
"""

from __future__ import annotations

import asyncio
import ipaddress
import json
import ssl
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID
from fake_gateway_admin import FakeGatewayAdmin

from terlimo_backend.gateway_adapter import GatewayError, ManagementTlsClient
from terlimo_backend.management_handler import HandlerConfig, ManagementHandler

NODE_ID = "gw-node-mtls"


def _write_key_cert(path_key: Path, path_cert: Path, key, cert) -> None:
    path_key.write_bytes(
        key.private_bytes(
            serialization.Encoding.PEM,
            serialization.PrivateFormat.PKCS8,
            serialization.NoEncryption(),
        )
    )
    path_cert.write_bytes(cert.public_bytes(serialization.Encoding.PEM))


def _ca(name: str):
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


def _leaf(ca_key, ca_cert, common_name: str, *, san: list[x509.GeneralName] | None = None):
    key = ec.generate_private_key(ec.SECP256R1())
    subject = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, common_name)])
    now = datetime.now(UTC)
    builder = (
        x509.CertificateBuilder()
        .subject_name(subject)
        .issuer_name(ca_cert.subject)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - timedelta(days=1))
        .not_valid_after(now + timedelta(days=2))
        .add_extension(x509.BasicConstraints(ca=False, path_length=None), critical=True)
    )
    if san:
        builder = builder.add_extension(x509.SubjectAlternativeName(san), critical=False)
    cert = builder.sign(ca_key, hashes.SHA256())
    return key, cert


class Pki:
    def __init__(self, tmp_path: Path) -> None:
        self.dir = tmp_path
        self.ca_key, self.ca_cert = _ca("terlimo-test-ca")
        _write_key_cert(self.dir / "ca.key", self.dir / "ca.pem", self.ca_key, self.ca_cert)
        self.ca_file = str(self.dir / "ca.pem")

        server_key, server_cert = _leaf(
            self.ca_key,
            self.ca_cert,
            "gw-node.test",
            san=[x509.DNSName("gw-node.test"), x509.IPAddress(ipaddress.ip_address("127.0.0.1"))],
        )
        _write_key_cert(self.dir / "server.key", self.dir / "server.pem", server_key, server_cert)
        self.server_cert = str(self.dir / "server.pem")
        self.server_key = str(self.dir / "server.key")

        backend_key, backend_cert = _leaf(self.ca_key, self.ca_cert, "terlimo-backend-test")
        _write_key_cert(self.dir / "backend.key", self.dir / "backend.pem", backend_key, backend_cert)
        self.backend_cert = str(self.dir / "backend.pem")
        self.backend_key = str(self.dir / "backend.key")

        other_key, other_cert = _leaf(self.ca_key, self.ca_cert, "other-backend")
        _write_key_cert(self.dir / "other.key", self.dir / "other.pem", other_key, other_cert)
        self.other_cert = str(self.dir / "other.pem")
        self.other_key = str(self.dir / "other.key")

        foreign_ca_key, foreign_ca_cert = _ca("foreign-ca")
        foreign_key, foreign_cert = _leaf(foreign_ca_key, foreign_ca_cert, "terlimo-backend-test")
        _write_key_cert(self.dir / "foreign.key", self.dir / "foreign.pem", foreign_key, foreign_cert)
        self.foreign_cert = str(self.dir / "foreign.pem")
        self.foreign_key = str(self.dir / "foreign.key")


def _handler_config(pki: Pki, admin_socket: str) -> HandlerConfig:
    config = HandlerConfig()
    config.bind = "127.0.0.1"
    config.port = 0
    config.server_cert = pki.server_cert
    config.server_key = pki.server_key
    config.client_ca = pki.ca_file
    config.allowed_identities = frozenset({"terlimo-backend-test"})
    config.node_id = NODE_ID
    config.admin_socket = admin_socket
    config.main_password = "fixture-main"
    config.max_not_after = 86400
    return config


@pytest.fixture
async def mtls_env(tmp_path):
    gateway = FakeGatewayAdmin(main_password="fixture-main", node_id=NODE_ID)
    await gateway.start()
    pki = Pki(tmp_path)
    handler = ManagementHandler(_handler_config(pki, gateway.socket_path))
    port = await handler.start()
    try:
        yield gateway, pki, handler, port
    finally:
        await handler.stop()
        await gateway.stop()


def _client(pki: Pki, port: int, *, cert: str | None = None, key: str | None = None) -> ManagementTlsClient:
    return ManagementTlsClient(
        host="127.0.0.1",
        port=port,
        server_name="gw-node.test",
        ca_file=pki.ca_file,
        cert_file=cert or pki.backend_cert,
        key_file=key or pki.backend_key,
        node_id=NODE_ID,
        timeout_seconds=5,
    )


def _fields(
    generation: str = "1", lease_seq: str = "1", operation_id: str = "op-mtls-1"
) -> dict:
    return {
        "grant_id": "grant-mtls-1",
        "registration_id": "reg-mtls-1",
        "public_key_spki": "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE",
        "generation": generation,
        "lease_seq": lease_seq,
        "operation_id": operation_id,
        "expires_at": int(datetime.now(UTC).timestamp()) + 600,
    }


async def test_provision_maps_to_local_client_test_command(mtls_env):
    gateway, pki, _handler, port = mtls_env
    readback = await _client(pki, port).grant_provision(
        password="cred-1", **_fields()
    )
    assert readback["grant_id"] == "grant-mtls-1"
    assert len(gateway.commands) == 1
    command = json.loads(gateway.commands[0])
    assert command["operation"] == "grant_provision"
    assert command["password"] == "cred-1"
    assert command["grant"]["node_id"] == NODE_ID
    assert command["expires_at"] == _fields()["expires_at"]
    assert set(command) == {"operation", "password", "expires_at", "grant"}


async def test_refresh_revoke_and_get_map_and_validate_identity(mtls_env):
    gateway, pki, _handler, port = mtls_env
    client = _client(pki, port)
    await client.grant_provision(password="cred-2", **_fields())
    await client.refresh_lease(
        password="cred-2",
        expected_seq="1",
        **_fields(lease_seq="2", operation_id="op-mtls-refresh"),
    )
    await client.grant_revoke(
        password="cred-2",
        generation="2",
        lease_seq="2",
        grant_id="grant-mtls-1",
        registration_id="reg-mtls-1",
        public_key_spki="MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE",
        operation_id="op-mtls-revoke",
        expires_at=int(datetime.now(UTC).timestamp()) + 600,
    )
    operations = [json.loads(command)["operation"] for command in gateway.commands]
    assert operations == ["grant_provision", "refresh_lease", "grant_revoke"]
    readback = await client.grant_get("cred-2", NODE_ID)
    assert readback["revoked"] is True and int(readback["generation"]) == 2


async def test_missing_client_certificate_is_rejected(mtls_env):
    gateway, pki, _handler, port = mtls_env
    context = ssl.create_default_context(cafile=pki.ca_file)
    context.check_hostname = True
    # TLS-level abort or the handler's explicit identity rejection are both acceptable;
    # either way the local admin surface must stay untouched.
    try:
        reader, writer = await asyncio.wait_for(
            asyncio.open_connection("127.0.0.1", port, ssl=context, server_hostname="gw-node.test"),
            timeout=5,
        )
        writer.write(b"{}")
        await writer.drain()
        try:
            body = await asyncio.wait_for(reader.read(), timeout=5)
            # An empty body (connection dropped) or an explicit error both mean rejection.
            if body:
                assert json.loads(body.decode()).get("status") == "error"
        finally:
            writer.close()
    except (ssl.SSLError, ConnectionError, OSError, TimeoutError):
        pass
    assert gateway.commands == []


async def test_untrusted_client_certificate_is_rejected(mtls_env):
    gateway, pki, _handler, port = mtls_env
    with pytest.raises((GatewayError, ssl.SSLError, ConnectionError, OSError, TimeoutError)):
        client = _client(pki, port, cert=pki.foreign_cert, key=pki.foreign_key)
        await asyncio.wait_for(client.engine_status(), timeout=5)
    assert gateway.commands == []


async def test_certificate_from_ca_but_wrong_identity_is_denied(mtls_env):
    gateway, pki, _handler, port = mtls_env
    client = _client(pki, port, cert=pki.other_cert, key=pki.other_key)
    with pytest.raises(GatewayError) as error:
        await client.engine_status()
    assert error.value.code == "AUTHZ_DENIED"
    assert gateway.commands == []


async def test_wrong_expected_node_is_refused_without_local_call(mtls_env):
    gateway, pki, _handler, port = mtls_env
    client = ManagementTlsClient(
        host="127.0.0.1",
        port=port,
        server_name="gw-node.test",
        ca_file=pki.ca_file,
        cert_file=pki.backend_cert,
        key_file=pki.backend_key,
        node_id="other-node",
        timeout_seconds=5,
    )
    with pytest.raises(GatewayError) as error:
        await client.engine_status()
    assert error.value.code == "NODE_ID_MISMATCH"
    assert gateway.commands == []


async def test_unknown_command_and_dangerous_fields_are_refused_without_local_call(mtls_env):
    gateway, pki, _handler, port = mtls_env
    context = ssl.create_default_context(cafile=pki.ca_file)
    context.load_cert_chain(pki.backend_cert, pki.backend_key)
    context.check_hostname = True

    async def raw(request: dict) -> dict:
        reader, writer = await asyncio.open_connection(
            "127.0.0.1", port, ssl=context, server_hostname="gw-node.test"
        )
        writer.write(json.dumps(request).encode())
        await writer.drain()
        body = await reader.read()
        writer.close()
        return json.loads(body.decode())

    unknown = await raw({"v": 1, "op": "shell", "node_id": NODE_ID, "fields": {}})
    assert unknown == {"v": 1, "status": "error", "code": "BAD_MESSAGE"}
    extra = await raw(
        {
            "v": 1,
            "op": "provision",
            "node_id": NODE_ID,
            "credential": "cred-x",
            "fields": {**_fields(), "args": ["sh", "-c", "x"]},
        }
    )
    assert extra == {"v": 1, "status": "error", "code": "BAD_MESSAGE"}
    nested = await raw(
        {
            "v": 1,
            "op": "provision",
            "node_id": NODE_ID,
            "credential": "cred-x",
            "fields": _fields(),
            "path": "/etc/wdtt",
        }
    )
    assert nested == {"v": 1, "status": "error", "code": "BAD_MESSAGE"}
    assert gateway.commands == []


async def test_handler_restart_keeps_config_and_does_not_touch_gateway(tmp_path):
    gateway = FakeGatewayAdmin(main_password="fixture-main", node_id=NODE_ID)
    await gateway.start()
    try:
        pki = Pki(tmp_path)
        handler = ManagementHandler(_handler_config(pki, gateway.socket_path))
        port = await handler.start()
        active_port = port
        await handler.stop()
        handler2 = ManagementHandler(_handler_config(pki, gateway.socket_path))
        port2 = await handler2.start()
        try:
            client = _client(pki, port2)
            status = await client.engine_status()
            assert status["node_id"] == NODE_ID
            # Only the explicit status call reached the local socket; the restart itself did not.
            assert len(gateway.commands) == 1
            assert json.loads(gateway.commands[0])["operation"] == "engine_status"
        finally:
            await handler2.stop()
        assert active_port != 0
    finally:
        await gateway.stop()


async def test_expired_revoke_is_accepted_but_provision_refresh_reject(mtls_env):
    """Durable expiry revoke carries the original (possibly past) lease expiry.

    provision/refresh keep the strict future bound; revoke may carry an already-expired
    original lease so the terminal operation still reaches the node.
    """
    gateway, pki, _handler, port = mtls_env
    client = _client(pki, port)
    expired = int(datetime.now(UTC).timestamp()) - 300

    with pytest.raises(GatewayError) as provision_err:
        await client.grant_provision(password="cred-x", **{**_fields(), "expires_at": expired})
    assert provision_err.value.code == "BAD_MESSAGE"

    with pytest.raises(GatewayError) as refresh_err:
        await client.refresh_lease(
            password="cred-x", expected_seq="1", **{**_fields(lease_seq="2"), "expires_at": expired}
        )
    assert refresh_err.value.code == "BAD_MESSAGE"
    assert gateway.commands == []

    await client.grant_revoke(
        password="cred-x",
        generation="2",
        lease_seq="2",
        grant_id="grant-x",
        registration_id="reg-x",
        public_key_spki="MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE",
        operation_id="op-x",
        expires_at=expired,
    )
    commands = [json.loads(raw) for raw in gateway.commands]
    assert [c["operation"] for c in commands] == ["grant_revoke"]
    assert commands[0]["expires_at"] == expired
    assert commands[0]["grant"]["revoked"] is True

    # A revoke far beyond the upper bound is still refused.
    with pytest.raises(GatewayError) as upper_err:
        await client.grant_revoke(
            password="cred-x",
            generation="2",
            lease_seq="2",
            grant_id="grant-y",
            registration_id="reg-y",
            public_key_spki="MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE",
            operation_id="op-y",
            expires_at=int(datetime.now(UTC).timestamp()) + 86400 + 600,
        )
    assert upper_err.value.code == "BAD_MESSAGE"
