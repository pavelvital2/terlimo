"""Gateway-deployment management handler: private mTLS in front of the local admin socket.

Fixed typed technical operations only (provision/refresh/revoke/get/status). Validates the
Backend client identity (certificate from the configured CA AND an explicit CN/SAN allowlist),
the expected node id, strict field sets, sizes and the request expiry bound, then translates
1:1 into the existing local `client-test` admin command. It never accepts the node main
password from the network, never forwards arbitrary args and never opens a plaintext listener.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
import os
import socket
import ssl
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

logger = logging.getLogger(__name__)

PROTOCOL_VERSION = 1
MAX_REQUEST_BYTES = 16 * 1024
OPS = (
    "provision",
    "refresh",
    "revoke",
    "get",
    "status",
    "usage",
    "bootstrap_provision",
    "bootstrap_revoke",
)

FIELD_RULES: dict[str, frozenset[str]] = {
    "provision": frozenset(
        {
            "grant_id",
            "registration_id",
            "public_key_spki",
            "generation",
            "lease_seq",
            "operation_id",
            "expires_at",
        }
    ),
    "refresh": frozenset(
        {
            "grant_id",
            "registration_id",
            "public_key_spki",
            "generation",
            "lease_seq",
            "expected_seq",
            "operation_id",
            "expires_at",
        }
    ),
    "revoke": frozenset(
        {
            "grant_id",
            "registration_id",
            "public_key_spki",
            "generation",
            "lease_seq",
            "operation_id",
            "expires_at",
        }
    ),
    "get": frozenset(),
    "status": frozenset(),
    "usage": frozenset(),
    "bootstrap_provision": frozenset({"credential_id", "secret", "expires_at"}),
    "bootstrap_revoke": frozenset({"credential_id"}),
}

LOCAL_COMMAND = {
    "provision": "grant_provision",
    "refresh": "refresh_lease",
    "revoke": "grant_revoke",
    "get": "grant_get",
    "status": "engine_status",
    "usage": "usage",
    "bootstrap_provision": "bootstrap_provision",
    "bootstrap_revoke": "bootstrap_revoke",
}


class HandlerError(RuntimeError):
    def __init__(self, code: str, message: str = "") -> None:
        super().__init__(code)
        self.code = code
        self.message = message


class HandlerConfig:
    def __init__(self) -> None:
        self.bind = os.environ.get("GATEWAY_MANAGEMENT_BIND", "127.0.0.1")
        self.port = int(os.environ.get("GATEWAY_MANAGEMENT_PORT", "56400"))
        self.server_cert = os.environ.get("GATEWAY_MANAGEMENT_TLS_CERT", "")
        self.server_key = os.environ.get("GATEWAY_MANAGEMENT_TLS_KEY", "")
        self.client_ca = os.environ.get("GATEWAY_MANAGEMENT_CLIENT_CA", "")
        identities = os.environ.get("GATEWAY_MANAGEMENT_ALLOWED_BACKEND_IDENTITIES", "")
        self.allowed_identities = frozenset(
            identity.strip() for identity in identities.split(",") if identity.strip()
        )
        self.node_id = os.environ.get("GATEWAY_MANAGEMENT_NODE_ID", "")
        self.admin_socket = os.environ.get("GATEWAY_MANAGEMENT_ADMIN_SOCKET", "")
        self.main_password = os.environ.get("GATEWAY_MANAGEMENT_MAIN_PASSWORD", "")
        self.max_not_after = int(os.environ.get("GATEWAY_MANAGEMENT_MAX_NOT_AFTER", "86400"))

    def validate(self) -> None:
        missing = [
            name
            for name, value in (
                ("GATEWAY_MANAGEMENT_TLS_CERT", self.server_cert),
                ("GATEWAY_MANAGEMENT_TLS_KEY", self.server_key),
                ("GATEWAY_MANAGEMENT_CLIENT_CA", self.client_ca),
                ("GATEWAY_MANAGEMENT_NODE_ID", self.node_id),
                ("GATEWAY_MANAGEMENT_ADMIN_SOCKET", self.admin_socket),
                ("GATEWAY_MANAGEMENT_MAIN_PASSWORD", self.main_password),
            )
            if not value
        ]
        if missing:
            raise SystemExit(f"management handler refused to start; missing: {', '.join(missing)}")
        if not self.allowed_identities:
            raise SystemExit(
                "management handler refused to start: no allowed Backend identities configured"
            )
        for path in (self.server_cert, self.server_key, self.client_ca):
            if not Path(path).is_file():
                raise SystemExit(f"management handler refused to start; not a file: {path}")
        if not self.bind.startswith(("127.", "::1", "localhost")):
            raise SystemExit("management handler refuses a non-private bind without explicit policy")


def _peer_identities(ssl_object: ssl.SSLObject | None) -> set[str]:
    if ssl_object is None:
        return set()
    certificate = ssl_object.getpeercert() or {}
    identities: set[str] = set()
    for rdn in certificate.get("subject", ()):
        for key, value in rdn:
            if key == "commonName":
                identities.add(value)
    for kind, value in certificate.get("subjectAltName", ()):
        if kind in ("DNS", "IP Address"):
            identities.add(value)
    return identities


def _require_string(fields: dict[str, Any], name: str, *, max_len: int) -> str:
    value = fields.get(name)
    if not isinstance(value, str) or not value or len(value) > max_len:
        raise HandlerError("BAD_MESSAGE", name)
    return value


def _require_decimal(fields: dict[str, Any], name: str) -> str:
    value = _require_string(fields, name, max_len=19)
    if not value.isdigit() or value.startswith("0"):
        raise HandlerError("BAD_MESSAGE", name)
    return value


def _validate_command(request: dict[str, Any], config: HandlerConfig) -> dict[str, Any]:
    if set(request) - {"v", "op", "node_id", "credential", "fields"}:
        raise HandlerError("BAD_MESSAGE", "unknown top-level key")
    if request.get("v") != PROTOCOL_VERSION:
        raise HandlerError("BAD_MESSAGE", "unsupported version")
    op = request.get("op")
    if op not in OPS:
        raise HandlerError("BAD_MESSAGE", "unknown operation")
    if request.get("node_id") != config.node_id:
        raise HandlerError("NODE_ID_MISMATCH")
    if op not in ("status", "bootstrap_provision", "bootstrap_revoke") and not isinstance(
        request.get("credential"), str
    ):
        raise HandlerError("BAD_MESSAGE", "credential required")
    credential = request.get("credential", "")
    if len(credential) > 256:
        raise HandlerError("BAD_MESSAGE", "credential too long")
    fields = request.get("fields") or {}
    if not isinstance(fields, dict) or set(fields) != set(FIELD_RULES[op]):
        raise HandlerError("BAD_MESSAGE", "field set mismatch")

    command: dict[str, Any] = {"operation": LOCAL_COMMAND[op]}
    if op == "status":
        return command
    if op in ("bootstrap_provision", "bootstrap_revoke"):
        # Onboarding bootstrap credentials are control-scope: no node password/credential is
        # accepted from the network, and the secret never leaves the typed field.
        credential_id = _require_string(fields, "credential_id", max_len=128)
        bootstrap: dict[str, Any] = {"credential_id": credential_id}
        if op == "bootstrap_provision":
            secret = _require_string(fields, "secret", max_len=256)
            if len(secret) < 32:
                raise HandlerError("BAD_MESSAGE", "secret too short")
            expires_at = fields.get("expires_at")
            if not isinstance(expires_at, int) or isinstance(expires_at, bool):
                raise HandlerError("BAD_MESSAGE", "expires_at")
            now = int(datetime.now(UTC).timestamp())
            if expires_at < now - 120 or expires_at > now + config.max_not_after:
                raise HandlerError("BAD_MESSAGE", "expires_at out of bounds")
            bootstrap["secret"] = secret
            bootstrap["expires_at"] = expires_at
        command["bootstrap"] = bootstrap
        return command
    command["password"] = credential
    if op == "get":
        command["grant"] = {"node_id": config.node_id}
        return command

    grant = {
        "grant_id": _require_string(fields, "grant_id", max_len=128),
        "registration_id": _require_string(fields, "registration_id", max_len=128),
        "node_id": config.node_id,
        "public_key_spki": _require_string(fields, "public_key_spki", max_len=512),
        "generation": _require_decimal(fields, "generation"),
        "lease_seq": _require_decimal(fields, "lease_seq"),
        "revoked": op == "revoke",
        "operation_id": _require_string(fields, "operation_id", max_len=128),
    }
    if any(character not in "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_" for character in grant["public_key_spki"]):
        raise HandlerError("BAD_MESSAGE", "public_key_spki")
    expires_at = fields.get("expires_at")
    if not isinstance(expires_at, int) or isinstance(expires_at, bool):
        raise HandlerError("BAD_MESSAGE", "expires_at")
    now = int(datetime.now(UTC).timestamp())
    if expires_at > now + config.max_not_after:
        raise HandlerError("BAD_MESSAGE", "expires_at out of bounds")
    if op != "revoke" and expires_at < now - 120:
        # provision/refresh must carry a future lease. A revoke is the terminal op for an
        # already-applied grant and legitimately carries the original lease expiry, which may
        # be in the past by the time the durable outbox retries or the hour has expired.
        raise HandlerError("BAD_MESSAGE", "expires_at out of bounds")
    command["expires_at"] = expires_at
    if op == "refresh":
        command["expected_seq"] = _require_decimal(fields, "expected_seq")
    command["grant"] = grant
    return command


class ManagementHandler:
    def __init__(self, config: HandlerConfig) -> None:
        self._config = config
        self._server: asyncio.AbstractServer | None = None

    async def start(self) -> int:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(self._config.server_cert, self._config.server_key)
        context.load_verify_locations(cafile=self._config.client_ca)
        context.verify_mode = ssl.CERT_REQUIRED
        self._server = await asyncio.start_server(
            self._handle, self._config.bind, self._config.port, ssl=context
        )
        sockets = self._server.sockets or ()
        return int(sockets[0].getsockname()[1]) if sockets else self._config.port

    async def stop(self) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()
            self._server = None

    async def _handle(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        ssl_object = writer.get_extra_info("ssl_object")
        peer = _peer_identities(ssl_object)
        if not peer or not (peer & self._config.allowed_identities):
            await self._respond(writer, {"v": 1, "status": "error", "code": "AUTHZ_DENIED"})
            return
        try:
            buffer = b""
            while len(buffer) < MAX_REQUEST_BYTES:
                chunk = await asyncio.wait_for(reader.read(4096), timeout=10)
                if not chunk:
                    break
                buffer += chunk
                try:
                    request = json.loads(buffer.decode("utf-8"))
                    break
                except (ValueError, UnicodeError):
                    continue
            else:
                raise HandlerError("BAD_MESSAGE", "request too large")
            if not isinstance(request, dict):
                raise HandlerError("BAD_MESSAGE")
            command = _validate_command(request, self._config)
            readback = await asyncio.to_thread(self._local_call, command)
            await self._respond(writer, {"v": 1, "status": "ok", "readback": readback})
        except HandlerError as error:
            await self._respond(writer, {"v": 1, "status": "error", "code": error.code})
        except Exception:
            logger.exception("management handler failure")
            await self._respond(writer, {"v": 1, "status": "error", "code": "INTERNAL"})

    def _local_call(self, command: dict[str, Any]) -> dict[str, Any]:
        payload = json.dumps(command, separators=(",", ":"), ensure_ascii=False, sort_keys=False)
        envelope = json.dumps(
            {"main_password": self._config.main_password, "args": ["client-test", payload]},
            separators=(",", ":"),
            ensure_ascii=False,
            sort_keys=False,
        )
        with socket.socket(socket.AF_UNIX) as connection:
            connection.settimeout(10)
            connection.connect(self._config.admin_socket)
            connection.sendall(envelope.encode("utf-8"))
            chunks: list[bytes] = []
            while True:
                chunk = connection.recv(65536)
                if not chunk:
                    break
                chunks.append(chunk)
        response = json.loads(b"".join(chunks).decode("utf-8"))
        if response.get("ok") is True and "client_test" in response:
            return response["client_test"]
        code = str(response.get("code") or "INTERNAL")
        raise HandlerError(code)

    @staticmethod
    async def _respond(writer: asyncio.StreamWriter, body: dict[str, Any]) -> None:
        writer.write(json.dumps(body, separators=(",", ":")).encode("utf-8"))
        try:
            await writer.drain()
        finally:
            writer.close()


def main() -> None:
    parser = argparse.ArgumentParser(description="TERLIMO gateway management mTLS handler")
    parser.parse_args()
    logging.basicConfig(level=os.environ.get("LOG_LEVEL", "INFO"))
    config = HandlerConfig()
    config.validate()
    handler = ManagementHandler(config)

    async def run() -> None:
        port = await handler.start()
        logger.info(
            "management handler on %s:%d (node=%s, identities=%d)",
            config.bind,
            port,
            config.node_id,
            len(config.allowed_identities),
        )
        try:
            await asyncio.Event().wait()
        finally:
            await handler.stop()

    asyncio.run(run())


if __name__ == "__main__":
    main()
