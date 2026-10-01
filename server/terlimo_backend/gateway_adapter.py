"""Client for the existing WDTT gateway management wire (adminsocket `client-test`).

Exact reuse of the accepted gateway control surface (no new API): one JSON request
`{"main_password": ..., "args": ["client-test", "<command-json>"]}` over the admin Unix
socket; the response is `{"ok": true, "client_test": {...}}` or `{"ok": false, "code": ...}`.
The gateway idempotency digest is SHA-256 over the exact command bytes, so this client
serializes every command deterministically (compact separators, fixed key order) and a retry
of the same logical operation reproduces the identical bytes.

Reference: WDTT client_test_admin.go (grant_provision / refresh_lease / grant_revoke /
grant_get / engine_status) and admin.go handleAdminSocketConn.
"""

from __future__ import annotations

import asyncio
import json
import logging
import socket
import ssl
from typing import Any

from .operation_timing import mark_management_refresh_drained

logger = logging.getLogger(__name__)

PROVISION = "grant_provision"
REFRESH = "refresh_lease"
REVOKE = "grant_revoke"
GET = "grant_get"
ENGINE_STATUS = "engine_status"

RETRYABLE_CODES = frozenset(
    {"STORAGE_FAILED", "WG_REMOVE_UNCONFIRMED", "MANAGED_ENGINE_UNAVAILABLE"}
)
TERMINAL_CODES = frozenset(
    {
        "BAD_MESSAGE",
        "NODE_ID_MISMATCH",
        "GRANT_CONFLICT",
        "IDEMPOTENCY_CONFLICT",
        "CONFLICT",
        "LEASE_CONFLICT",
        "NOT_FOUND",
        "CAPACITY_FULL",
        "WG_ISOLATION_UNCONFIRMED",
        "WORKER_POLICY_UNCONFIRMED",
        "TEST_DISABLED",
        "BAD_OPERATION",
        "AUTH_FAILED",
    }
)


class GatewayError(RuntimeError):
    def __init__(self, code: str, message: str = "") -> None:
        super().__init__(code)
        self.code = code
        self.message = message

    @property
    def retryable(self) -> bool:
        return self.code not in TERMINAL_CODES


class GatewayAdminClient:
    """Admin socket client. Blocking socket IO runs in a worker thread."""

    def __init__(self, socket_path: str, main_password: str, timeout_seconds: float = 10.0) -> None:
        if not socket_path.startswith("/"):
            raise ValueError("gateway admin socket path must be absolute")
        if not main_password:
            raise ValueError("gateway admin main password is not configured")
        self._socket_path = socket_path
        self._main_password = main_password
        self._timeout_seconds = timeout_seconds

    async def call(self, command: dict[str, Any]) -> dict[str, Any]:
        return await asyncio.to_thread(self.call_sync, command)

    def call_sync(self, command: dict[str, Any]) -> dict[str, Any]:
        payload = json.dumps(command, separators=(",", ":"), ensure_ascii=False, sort_keys=False)
        envelope = json.dumps(
            {"main_password": self._main_password, "args": ["client-test", payload]},
            separators=(",", ":"),
            ensure_ascii=False,
            sort_keys=False,
        )
        try:
            with socket.socket(socket.AF_UNIX) as connection:
                connection.settimeout(self._timeout_seconds)
                connection.connect(self._socket_path)
                connection.sendall(envelope.encode("utf-8"))
                chunks: list[bytes] = []
                while True:
                    chunk = connection.recv(65536)
                    if not chunk:
                        break
                    chunks.append(chunk)
        except OSError as exc:
            raise GatewayError("GATEWAY_UNREACHABLE", str(exc)) from exc
        raw = b"".join(chunks)
        try:
            response = json.loads(raw.decode("utf-8"))
        except (ValueError, UnicodeError) as exc:
            raise GatewayError("GATEWAY_BAD_RESPONSE") from exc
        if response.get("ok") is False:
            code = str(response.get("code") or "AUTH_FAILED")
            raise GatewayError(code, str(response.get("message", ""))[:200])
        if response.get("ok") is True and "client_test" in response:
            return response["client_test"]
        # Legacy admin commands answer with an adminResponse; treat as gateways error.
        raise GatewayError("GATEWAY_BAD_RESPONSE")

    async def engine_status(self) -> dict[str, Any]:
        return await self.call({"operation": ENGINE_STATUS})

    async def bootstrap_provision(
        self, *, credential_id: str, secret: str, expires_at: int
    ) -> dict[str, Any]:
        """Node bootstrap credential for the onboarding hour (control scope, not a grant)."""
        return await self.call(
            {
                "operation": "bootstrap_provision",
                "bootstrap": {
                    "credential_id": credential_id,
                    "secret": secret,
                    "expires_at": int(expires_at),
                    "revoked": False,
                },
            }
        )

    async def bootstrap_revoke(self, *, credential_id: str) -> dict[str, Any]:
        return await self.call(
            {"operation": "bootstrap_revoke", "bootstrap": {"credential_id": credential_id}}
        )

    async def grant_get(self, password: str, node_id: str) -> dict[str, Any]:
        return await self.call(
            {"operation": GET, "password": password, "grant": {"node_id": node_id}}
        )

    async def usage(self, password: str, node_id: str) -> dict[str, Any]:
        return await self.call(
            {"operation": "usage", "password": password, "grant": {"node_id": node_id}}
        )

    async def grant_provision(
        self,
        *,
        password: str,
        grant_id: str,
        registration_id: str,
        node_id: str,
        public_key_spki: str,
        generation: str,
        lease_seq: str,
        operation_id: str,
        expires_at: int,
    ) -> dict[str, Any]:
        return await self.call(
            {
                "operation": PROVISION,
                "password": password,
                "expires_at": int(expires_at),
                "grant": {
                    "grant_id": grant_id,
                    "registration_id": registration_id,
                    "node_id": node_id,
                    "public_key_spki": public_key_spki,
                    "generation": generation,
                    "lease_seq": lease_seq,
                    "revoked": False,
                    "operation_id": operation_id,
                },
            }
        )

    async def refresh_lease(
        self,
        *,
        password: str,
        grant_id: str,
        registration_id: str,
        node_id: str,
        public_key_spki: str,
        generation: str,
        lease_seq: str,
        expected_seq: str,
        operation_id: str,
        expires_at: int,
    ) -> dict[str, Any]:
        return await self.call(
            {
                "operation": REFRESH,
                "password": password,
                "expires_at": int(expires_at),
                "expected_seq": expected_seq,
                "grant": {
                    "grant_id": grant_id,
                    "registration_id": registration_id,
                    "node_id": node_id,
                    "public_key_spki": public_key_spki,
                    "generation": generation,
                    "lease_seq": lease_seq,
                    "revoked": False,
                    "operation_id": operation_id,
                },
            }
        )

    async def grant_revoke(
        self,
        *,
        password: str,
        grant_id: str,
        registration_id: str,
        node_id: str,
        public_key_spki: str,
        generation: str,
        lease_seq: str,
        operation_id: str,
        expires_at: int,
    ) -> dict[str, Any]:
        return await self.call(
            {
                "operation": REVOKE,
                "password": password,
                "expires_at": int(expires_at),
                "grant": {
                    "grant_id": grant_id,
                    "registration_id": registration_id,
                    "node_id": node_id,
                    "public_key_spki": public_key_spki,
                    "generation": generation,
                    "lease_seq": lease_seq,
                    "revoked": True,
                    "operation_id": operation_id,
                },
            }
        )


class ManagementTlsClient:
    """Backend client for the gateway management mTLS handler (schema v1).

    Same call surface as GatewayAdminClient, so the worker treats both transports identically.
    """

    def __init__(
        self,
        *,
        host: str,
        port: int,
        server_name: str,
        ca_file: str,
        cert_file: str,
        key_file: str,
        node_id: str,
        timeout_seconds: float = 10.0,
    ) -> None:
        for value, name in ((ca_file, "ca_file"), (cert_file, "cert_file"), (key_file, "key_file")):
            if not value:
                raise ValueError(f"management TLS {name} is not configured")
        if not server_name:
            raise ValueError("management TLS server_name is not configured")
        self._host = host
        self._port = int(port)
        self._server_name = server_name
        self._ca_file = ca_file
        self._cert_file = cert_file
        self._key_file = key_file
        self._node_id = node_id
        self._timeout_seconds = timeout_seconds

    def _context(self) -> ssl.SSLContext:
        context = ssl.create_default_context(cafile=self._ca_file)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.check_hostname = True
        context.verify_mode = ssl.CERT_REQUIRED
        context.load_cert_chain(self._cert_file, self._key_file)
        return context

    async def call(
        self, op: str, *, credential: str | None = None, fields: dict[str, Any] | None = None
    ) -> dict[str, Any]:
        request: dict[str, Any] = {"v": 1, "op": op, "node_id": self._node_id, "fields": fields or {}}
        if credential is not None:
            request["credential"] = credential
        payload = json.dumps(request, separators=(",", ":"), ensure_ascii=False, sort_keys=False)
        try:
            reader, writer = await asyncio.wait_for(
                asyncio.open_connection(
                    self._host,
                    self._port,
                    ssl=self._context(),
                    server_hostname=self._server_name,
                ),
                timeout=self._timeout_seconds,
            )
        except ssl.SSLCertVerificationError as exc:
            raise GatewayError("GATEWAY_TLS_UNTRUSTED", str(exc)) from exc
        except ssl.SSLError as exc:
            raise GatewayError("GATEWAY_TLS_ERROR", str(exc)) from exc
        except OSError as exc:
            raise GatewayError("GATEWAY_UNREACHABLE", str(exc)) from exc
        try:
            writer.write(payload.encode("utf-8"))
            await writer.drain()
            if op == "refresh":
                mark_management_refresh_drained()
            raw = await asyncio.wait_for(reader.read(), timeout=self._timeout_seconds)
        except (OSError, TimeoutError) as exc:
            raise GatewayError("GATEWAY_UNREACHABLE", str(exc)) from exc
        finally:
            writer.close()
        try:
            response = json.loads(raw.decode("utf-8"))
        except (ValueError, UnicodeError) as exc:
            raise GatewayError("GATEWAY_BAD_RESPONSE") from exc
        if response.get("status") != "ok":
            code = str(response.get("code") or "INTERNAL")
            raise GatewayError(code)
        readback = response.get("readback")
        if not isinstance(readback, dict):
            raise GatewayError("GATEWAY_BAD_RESPONSE")
        return readback

    async def engine_status(self) -> dict[str, Any]:
        return await self.call("status")

    async def bootstrap_provision(
        self, *, credential_id: str, secret: str, expires_at: int
    ) -> dict[str, Any]:
        return await self.call(
            "bootstrap_provision",
            credential="",
            fields={
                "credential_id": credential_id,
                "secret": secret,
                "expires_at": int(expires_at),
            },
        )

    async def bootstrap_revoke(self, *, credential_id: str) -> dict[str, Any]:
        return await self.call(
            "bootstrap_revoke", credential="", fields={"credential_id": credential_id}
        )

    async def grant_get(self, password: str, node_id: str) -> dict[str, Any]:
        return await self.call("get", credential=password)

    async def usage(self, password: str, node_id: str) -> dict[str, Any]:
        return await self.call("usage", credential=password)

    async def grant_provision(self, **kwargs) -> dict[str, Any]:
        return await self.call(
            "provision",
            credential=kwargs["password"],
            fields={
                "grant_id": kwargs["grant_id"],
                "registration_id": kwargs["registration_id"],
                "public_key_spki": kwargs["public_key_spki"],
                "generation": kwargs["generation"],
                "lease_seq": kwargs["lease_seq"],
                "operation_id": kwargs["operation_id"],
                "expires_at": int(kwargs["expires_at"]),
            },
        )

    async def refresh_lease(self, **kwargs) -> dict[str, Any]:
        return await self.call(
            "refresh",
            credential=kwargs["password"],
            fields={
                "grant_id": kwargs["grant_id"],
                "registration_id": kwargs["registration_id"],
                "public_key_spki": kwargs["public_key_spki"],
                "generation": kwargs["generation"],
                "lease_seq": kwargs["lease_seq"],
                "expected_seq": kwargs["expected_seq"],
                "operation_id": kwargs["operation_id"],
                "expires_at": int(kwargs["expires_at"]),
            },
        )

    async def grant_revoke(self, **kwargs) -> dict[str, Any]:
        return await self.call(
            "revoke",
            credential=kwargs["password"],
            fields={
                "grant_id": kwargs["grant_id"],
                "registration_id": kwargs["registration_id"],
                "public_key_spki": kwargs["public_key_spki"],
                "generation": kwargs["generation"],
                "lease_seq": kwargs["lease_seq"],
                "operation_id": kwargs["operation_id"],
                "expires_at": int(kwargs["expires_at"]),
            },
        )


class BootstrapGatewayClientAdapter:
    """Adapter from the existing admin clients to the onboarding-hour client protocol."""

    def __init__(self, client: Any) -> None:
        self._client = client

    async def bootstrap_provision(
        self, *, credential_id: str, secret: str, expires_at, node_id: str
    ) -> dict[str, Any]:
        return await self._client.bootstrap_provision(
            credential_id=credential_id,
            secret=secret,
            expires_at=int(expires_at.timestamp()) if hasattr(expires_at, "timestamp") else int(expires_at),
        )

    async def bootstrap_revoke(self, *, credential_id: str) -> None:
        await self._client.bootstrap_revoke(credential_id=credential_id)


def build_bootstrap_client_factory(settings):
    """Factory(gateway_key, endpoints) -> BootstrapGatewayClientAdapter (existing transport)."""

    def factory(gateway_key: str, endpoints: dict[str, Any]) -> BootstrapGatewayClientAdapter:
        return BootstrapGatewayClientAdapter(build_gateway_client(settings, gateway_key, endpoints))

    return factory


def build_gateway_client(
    settings, gateway_key: str, endpoints: dict[str, Any], *, prefer_local_admin: bool = False
):
    """Admin-controlled registry choice: mTLS management endpoint, else local admin socket.

    ``prefer_local_admin`` (collector usage readback) selects the isolated-TEST local admin
    socket explicitly: environment=test plus GATEWAY_LOCAL_ADMIN_ENABLED. There is no silent
    fallback, so a disabled/forbidden local admin surfaces as a clear error instead of a
    management-protocol rejection.
    """
    socket_path = endpoints.get("admin_socket")
    if prefer_local_admin and socket_path:
        # The local admin socket is an isolated-TEST transport only: explicit opt-in plus
        # environment=test. There is no silent local fallback in non-TEST.
        if settings.environment != "test":
            raise GatewayError("GATEWAY_LOCAL_ADMIN_FORBIDDEN")
        if not settings.gateway_local_admin_enabled:
            raise GatewayError("GATEWAY_LOCAL_ADMIN_DISABLED")
        return GatewayAdminClient(
            socket_path=str(socket_path),
            main_password=settings.gateway_admin_main_password,
            timeout_seconds=settings.gateway_admin_timeout_seconds,
        )
    management = endpoints.get("management")
    if isinstance(management, dict) and management.get("host"):
        return ManagementTlsClient(
            host=str(management["host"]),
            port=int(management["port"]),
            server_name=str(management.get("server_name") or ""),
            ca_file=settings.gateway_management_ca_file,
            cert_file=settings.gateway_management_cert_file,
            key_file=settings.gateway_management_key_file,
            node_id=str(endpoints.get("node_id") or gateway_key),
            timeout_seconds=settings.gateway_admin_timeout_seconds,
        )
    socket_path = endpoints.get("admin_socket")
    if socket_path:
        # The local admin socket is an isolated-TEST transport only: explicit opt-in plus
        # environment=test. There is no silent local fallback in non-TEST.
        if settings.environment != "test":
            raise GatewayError("GATEWAY_LOCAL_ADMIN_FORBIDDEN")
        if not settings.gateway_local_admin_enabled:
            raise GatewayError("GATEWAY_LOCAL_ADMIN_DISABLED")
        return GatewayAdminClient(
            socket_path=str(socket_path),
            main_password=settings.gateway_admin_main_password,
            timeout_seconds=settings.gateway_admin_timeout_seconds,
        )
    raise GatewayError("GATEWAY_ENDPOINT_MISSING")
