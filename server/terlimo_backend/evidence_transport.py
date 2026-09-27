"""G6 evidence transport (open seam): dedicated backend mTLS listener + AF_UNIX relay.

The node envelope stays unchanged: ``{credential_id, connection_id, request_id, body}``
(node ``client_test_transport.go``). ``body`` is opaque client bytes and is never inspected
for identity or policy. Gateway identity is derived exclusively from the verified client
certificate mapped to a ``gateways`` registry row (``gateway_key``/``environment``); the
typed :class:`~terlimo_backend.onboarding_hour.GatewayContext` is then handed to the accepted
``admit_evidence``, which re-checks the binding under the installation lock.

The backend endpoint runs on its own listener (host/port from config) with a mandatory
client-certificate TLS context; the public API keeps running without a client-cert
requirement and never serves this internal route. The AF_UNIX relay is transport only: it
restricts the local peer by uid/gid, forwards the bounded envelope verbatim over per-node
mTLS and returns the backend bytes. Roles are configured independently; a relay host needs
no server key and a backend needs no node key.

Explicit connect-intent provisioning (``create_intent`` + worker provision) stays the
prerequisite. Known integration prerequisite: a ready intent plus any background RPC (for
example ``background.keepalive``) currently admits evidence, so an explicit
connect/response contract is still required before live; this slice neither weakens
``admit_evidence`` nor invents a body marker.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import socket
import ssl
import struct
import uuid
from typing import Any

import asyncpg
from aiohttp import ClientError, ClientSession, ClientTimeout, TCPConnector, web

from . import pop
from .auth_api import parse_utc
from .config import Settings
from .db import Database
from .management_handler import _peer_identities
from .onboarding_hour import (
    ONBOARDING_SCOPE,
    ONBOARDING_START_OP,
    GatewayContext,
    OnboardingError,
    admit_evidence,
)

logger = logging.getLogger(__name__)

EVIDENCE_PATH = "/internal/onboarding/evidence"
ENVELOPE_KEYS = frozenset({"credential_id", "connection_id", "request_id", "body"})
START_BODY_KEYS = frozenset({"v", "op", "intent_id", "request_key", "proof"})
START_PROOF_KEYS = frozenset(
    {
        "algorithm",
        "environment",
        "request_id",
        "challenge_id",
        "nonce_b64",
        "payload_hash",
        "signed_payload_b64",
        "signature_b64",
    }
)
MAX_CREDENTIAL_ID = 128
MAX_CONNECTION_ID = 64
MAX_REQUEST_ID = 128
MAX_REQUEST_KEY = 128
MAX_ENVELOPE_BYTES = 262144
HEX32 = re.compile(r"[0-9a-f]{32}")
HEX64 = re.compile(r"[0-9a-f]{64}")
B64URL = re.compile(r"[A-Za-z0-9_-]{1,16384}")
EVIDENCE_LISTENER_KEY: web.AppKey = web.AppKey("evidence_listener", "EvidenceListener")


class EvidenceTransportError(RuntimeError):
    def __init__(self, code: str, *, http: int = 403, retry_after: int | None = None) -> None:
        super().__init__(code)
        self.code = code
        self.http = http
        self.retry_after = retry_after


def require_endpoint_material(settings: Settings) -> None:
    """Fail closed on missing endpoint material; no plaintext/X-header fallback exists."""
    if not settings.evidence_endpoint_enabled:
        raise EvidenceTransportError("ONBOARDING_EVIDENCE_ENDPOINT_DISABLED", http=503)
    required = {
        "ONBOARDING_EVIDENCE_SERVER_CERT_FILE": settings.evidence_server_cert_file,
        "ONBOARDING_EVIDENCE_SERVER_KEY_FILE": settings.evidence_server_key_file,
        "ONBOARDING_EVIDENCE_CLIENT_CA_FILE": settings.evidence_client_ca_file,
    }
    for name, path in required.items():
        if not path or not os.path.isfile(path):
            raise EvidenceTransportError(f"{name} is not a file", http=503)
    if not settings.evidence_listen_host:
        raise EvidenceTransportError("evidence listen host is not configured", http=503)


def require_relay_material(settings: Settings) -> None:
    """Fail closed on missing relay material; the relay never needs the server key."""
    if not settings.evidence_relay_enabled:
        raise EvidenceTransportError("ONBOARDING_EVIDENCE_RELAY_DISABLED", http=503)
    required = {
        "ONBOARDING_EVIDENCE_NODE_CERT_FILE": settings.evidence_node_cert_file,
        "ONBOARDING_EVIDENCE_NODE_KEY_FILE": settings.evidence_node_key_file,
        "ONBOARDING_EVIDENCE_BACKEND_CA_FILE": settings.evidence_backend_ca_file,
    }
    for name, path in required.items():
        if not path or not os.path.isfile(path):
            raise EvidenceTransportError(f"{name} is not a file", http=503)
    if not settings.evidence_relay_socket or not settings.evidence_backend_host:
        raise EvidenceTransportError("evidence relay target is not configured", http=503)
    if settings.evidence_relay_allowed_uid < 0:
        raise EvidenceTransportError("evidence relay allowed uid is not configured", http=503)


def build_endpoint_ssl_context(settings: Settings) -> ssl.SSLContext:
    """Production builder for the dedicated listener: verified client certificates only."""
    require_listener_material(settings)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(settings.evidence_server_cert_file, settings.evidence_server_key_file)
    context.load_verify_locations(cafile=settings.evidence_client_ca_file)
    context.verify_mode = ssl.CERT_REQUIRED
    return context


async def resolve_gateway_context(
    connection: asyncpg.Connection, identities: set[str], environment: str
) -> GatewayContext:
    """Certificate identities -> registry row -> typed GatewayContext (never from JSON)."""
    if not identities:
        raise EvidenceTransportError("EVIDENCE_AUTH_REQUIRED")
    rows = await connection.fetch(
        """
        SELECT id, gateway_key, environment, registry_state
        FROM gateways WHERE gateway_key = ANY($1::text[])
        """,
        sorted(identities),
    )
    if not rows:
        raise EvidenceTransportError("EVIDENCE_REGISTRY_UNKNOWN")
    gateway_ids = {row["id"] for row in rows}
    if len(gateway_ids) != 1:
        raise EvidenceTransportError("EVIDENCE_REGISTRY_AMBIGUOUS")
    row = rows[0]
    if row["environment"] != environment:
        raise EvidenceTransportError("EVIDENCE_ENV_MISMATCH")
    if row["registry_state"] != "registered":
        raise EvidenceTransportError("EVIDENCE_REGISTRY_DISABLED")
    return GatewayContext(
        gateway_id=row["id"], gateway_key=row["gateway_key"], environment=row["environment"]
    )


def _validate_envelope(payload: Any) -> tuple[str, str, str]:
    if not isinstance(payload, dict) or set(payload) != ENVELOPE_KEYS:
        raise EvidenceTransportError("EVIDENCE_BAD_ENVELOPE", http=400)
    values: list[str] = []
    for name, limit in (
        ("credential_id", MAX_CREDENTIAL_ID),
        ("connection_id", MAX_CONNECTION_ID),
        ("request_id", MAX_REQUEST_ID),
    ):
        value = payload[name]
        if not isinstance(value, str) or not value or len(value) > limit:
            raise EvidenceTransportError("EVIDENCE_BAD_ENVELOPE", http=400)
        values.append(value)
    return values[0], values[1], values[2]


def _uuid_field(raw: Any) -> str:
    if not isinstance(raw, str):
        raise EvidenceTransportError("EVIDENCE_UNKNOWN_OPERATION", http=400)
    try:
        return str(uuid.UUID(raw))
    except ValueError as exc:
        raise EvidenceTransportError("EVIDENCE_UNKNOWN_OPERATION", http=400) from exc


def _validate_start_body(payload: Any) -> tuple[str, str, dict[str, Any]]:
    """Strict versioned start RPC: anything else (background, malformed) stops before DB work."""
    if not isinstance(payload, dict) or set(payload) != START_BODY_KEYS:
        raise EvidenceTransportError("EVIDENCE_UNKNOWN_OPERATION", http=400)
    if type(payload["v"]) is not int or payload["v"] != 1 or payload["op"] != ONBOARDING_START_OP:
        raise EvidenceTransportError("EVIDENCE_UNKNOWN_OPERATION", http=400)
    intent_id = _uuid_field(payload["intent_id"])
    request_key = payload["request_key"]
    if (
        not isinstance(request_key, str)
        or not request_key
        or len(request_key) > MAX_REQUEST_KEY
    ):
        raise EvidenceTransportError("EVIDENCE_UNKNOWN_OPERATION", http=400)
    proof = payload["proof"]
    if not isinstance(proof, dict) or set(proof) != START_PROOF_KEYS:
        raise EvidenceTransportError("EVIDENCE_UNKNOWN_OPERATION", http=400)
    if proof["algorithm"] != "ES256" or proof["environment"] not in ("test", "production"):
        raise EvidenceTransportError("EVIDENCE_UNKNOWN_OPERATION", http=400)
    for name, pattern in (
        ("request_id", HEX32),
        ("challenge_id", re.compile(r"[0-9a-f]{16,64}")),
        ("payload_hash", HEX64),
        ("nonce_b64", re.compile(r"[A-Za-z0-9_-]{16,64}")),
        ("signed_payload_b64", B64URL),
        ("signature_b64", B64URL),
    ):
        value = proof[name]
        if not isinstance(value, str) or not pattern.fullmatch(value):
            raise EvidenceTransportError("EVIDENCE_UNKNOWN_OPERATION", http=400)
    return intent_id, request_key, proof


def _pop_transport_error(exc: pop.PopError) -> EvidenceTransportError:
    code = str(exc)
    if code in ("WRONG_ENVIRONMENT", "WRONG_SCOPE", "UNKNOWN_CRITICAL_FIELD", "BAD_MESSAGE"):
        return EvidenceTransportError(code, http=400)
    return EvidenceTransportError("PROOF_INVALID", http=401)


def _error_response(error: EvidenceTransportError) -> web.Response:
    body: dict[str, Any] = {"status": "error", "code": error.code, "retryable": False}
    if error.retry_after is not None:
        body["retryable"] = True
        body["retry_after"] = error.retry_after
    return web.json_response(body, status=error.http)


def _evidence_handler(settings: Settings, database: Database):
    async def handler(request: web.Request) -> web.Response:
        identities = _peer_identities(request.transport.get_extra_info("ssl_object"))
        try:
            raw = await request.read()
            if len(raw) > MAX_ENVELOPE_BYTES:
                raise EvidenceTransportError("EVIDENCE_BAD_ENVELOPE", http=400)
            try:
                payload = json.loads(raw)
            except ValueError as exc:
                raise EvidenceTransportError("EVIDENCE_BAD_ENVELOPE", http=400) from exc
            credential_id, connection_id, _envelope_request_id = _validate_envelope(payload)
            intent_id, body_request_key, proof = _validate_start_body(payload["body"])
            async with database.acquire() as connection:
                caller = await resolve_gateway_context(
                    connection, identities, settings.environment
                )
                intent = await connection.fetchrow(
                    """
                    SELECT id, installation_id, gateway_id, environment, request_key
                    FROM onboarding_intents WHERE credential_id = $1
                    """,
                    credential_id,
                )
                if intent is None:
                    raise OnboardingError("ONBOARDING_CREDENTIAL_UNKNOWN", http=403)
                if str(intent["id"]) != intent_id or intent["request_key"] != body_request_key:
                    raise OnboardingError("ONBOARDING_ENV_MISMATCH", http=403)
                if (
                    caller.gateway_id != intent["gateway_id"]
                    or caller.environment != intent["environment"]
                    or intent["environment"] != settings.environment
                ):
                    raise OnboardingError("ONBOARDING_ENV_MISMATCH", http=403)
                installed = await connection.fetchrow(
                    "SELECT public_key_spki_b64, public_key_fingerprint FROM installations WHERE id = $1",
                    intent["installation_id"],
                )
                if installed is None or not installed["public_key_spki_b64"]:
                    raise EvidenceTransportError("PROOF_INVALID", http=401)
                try:
                    public_key = pop.load_public_key(installed["public_key_spki_b64"])
                    signed = pop.verify_proof(
                        public_key,
                        signed_payload_b64=proof["signed_payload_b64"],
                        payload_hash=proof["payload_hash"],
                        signature_b64=proof["signature_b64"],
                        request_id=proof["request_id"],
                        challenge_id=proof["challenge_id"],
                        nonce_b64=proof["nonce_b64"],
                        expected={},
                        known_top_level=pop.KNOWN_TOP_LEVEL,
                        server_known_fields=pop.KNOWN_TOP_LEVEL,
                    )
                except pop.PopError as exc:
                    raise _pop_transport_error(exc) from exc
                if signed.get("env") != intent["environment"] or proof["environment"] != intent["environment"]:
                    raise EvidenceTransportError("WRONG_ENVIRONMENT", http=400)
                if signed.get("scope") != ONBOARDING_SCOPE or signed.get("op") != ONBOARDING_START_OP:
                    raise EvidenceTransportError("WRONG_SCOPE", http=400)
                if (
                    signed.get("installation_id") != installed["public_key_fingerprint"]
                    or signed.get("intent_id") != str(intent["id"])
                    or signed.get("request_key") != intent["request_key"]
                    or body_request_key != intent["request_key"]
                ):
                    raise EvidenceTransportError("PROOF_INVALID", http=401)
                if signed.get("request_id") != proof["request_id"] or signed.get("nonce") != proof["nonce_b64"]:
                    raise EvidenceTransportError("PROOF_INVALID", http=401)
                try:
                    digest = pop.business_digest(signed)
                except pop.PopError as exc:
                    raise _pop_transport_error(exc) from exc
                # No mutable freshness state is read here: the admission path re-checks the
                # durable evidence binding first and only then consumes the challenge under the
                # installation lock, at the actual post-wait time.
                ts_value = signed.get("ts")
                if not isinstance(ts_value, str):
                    raise EvidenceTransportError("PROOF_INVALID", http=401)
                try:
                    proof_ts_epoch = parse_utc(ts_value)
                except (TypeError, ValueError):
                    raise EvidenceTransportError("PROOF_INVALID", http=401) from None
                result = await admit_evidence(
                    connection,
                    caller=caller,
                    credential_id=credential_id,
                    connection_id=connection_id,
                    request_id=proof["request_id"],
                    evidence_digest=digest,
                    consume_challenge_id=proof["challenge_id"],
                    proof_nonce=proof["nonce_b64"],
                    proof_ts_epoch=proof_ts_epoch,
                    proof_skew_seconds=settings.proof_skew_seconds,
                )
        except EvidenceTransportError as error:
            return _error_response(error)
        except OnboardingError as error:
            return _error_response(
                EvidenceTransportError(error.code, http=error.http, retry_after=error.retry_after)
            )
        return web.json_response({"status": "ok", **result})

    return handler


def register_evidence_routes(app: web.Application, settings: Settings, database: Database) -> None:
    """The route exists only when the endpoint role is explicitly enabled (fail closed)."""
    if not settings.evidence_endpoint_enabled:
        return
    app.router.add_post(EVIDENCE_PATH, _evidence_handler(settings, database))


class EvidenceListener:
    """Dedicated mTLS listener; the public API keeps its own plaintext loop unchanged.

    The listener hosts the evidence route and/or the service-only API relay route; it is the
    shared per-node mTLS transport for both. Startup fails closed when neither role is enabled.
    """

    def __init__(self, settings: Settings, database: Database) -> None:
        require_listener_material(settings)
        self._settings = settings
        self._database = database
        self._runner: web.AppRunner | None = None
        self.port: int = settings.evidence_listen_port

    async def start(self) -> int:
        from .service_relay import SERVICE_MAX_FRAME, register_service_routes

        app = web.Application(client_max_size=SERVICE_MAX_FRAME + 65536)
        register_evidence_routes(app, self._settings, self._database)
        register_service_routes(app, self._settings, self._database)
        # handler_cancellation makes a client disconnect actually cancel the handler and its
        # upstream replay instead of letting it run to timeout.
        self._runner = web.AppRunner(app, handler_cancellation=True)
        await self._runner.setup()
        site = web.TCPSite(
            self._runner,
            self._settings.evidence_listen_host,
            self._settings.evidence_listen_port,
            ssl_context=build_endpoint_ssl_context(self._settings),
        )
        await site.start()
        addresses = self._runner.addresses
        if addresses:
            self.port = int(addresses[0][1])
        return self.port

    async def stop(self) -> None:
        if self._runner is not None:
            await self._runner.cleanup()
            self._runner = None


def require_listener_material(settings: Settings) -> None:
    """The shared listener is enabled when either the evidence or the service role is on."""
    if not (settings.evidence_endpoint_enabled or settings.service_endpoint_enabled):
        raise EvidenceTransportError("ONBOARDING_EVIDENCE_ENDPOINT_DISABLED", http=503)
    required = {
        "ONBOARDING_EVIDENCE_SERVER_CERT_FILE": settings.evidence_server_cert_file,
        "ONBOARDING_EVIDENCE_SERVER_KEY_FILE": settings.evidence_server_key_file,
        "ONBOARDING_EVIDENCE_CLIENT_CA_FILE": settings.evidence_client_ca_file,
    }
    for name, path in required.items():
        if not path or not os.path.isfile(path):
            raise EvidenceTransportError(f"{name} is not a file", http=503)
    if not settings.evidence_listen_host:
        raise EvidenceTransportError("evidence listen host is not configured", http=503)


async def start_evidence_listener(settings: Settings, database: Database) -> EvidenceListener | None:
    """Entrypoint helper: returns a started listener or None when both roles are disabled."""
    if not (settings.evidence_endpoint_enabled or settings.service_endpoint_enabled):
        return None
    listener = EvidenceListener(settings, database)
    await listener.start()
    return listener


def _peer_credentials(writer: asyncio.StreamWriter) -> tuple[int, int]:
    transport_socket = writer.get_extra_info("socket")
    if transport_socket is None:
        return -1, -1
    raw = transport_socket.getsockopt(
        socket.SOL_SOCKET, socket.SO_PEERCRED, struct.calcsize("3i")
    )
    _pid, uid, gid = struct.unpack("3i", raw)
    return uid, gid


class EvidenceRelay:
    """AF_UNIX local hop with restricted peer credentials; verbatim mTLS forwarding."""

    def __init__(self, settings: Settings) -> None:
        require_relay_material(settings)
        self._settings = settings
        self._server: asyncio.AbstractServer | None = None
        self._session: ClientSession | None = None

    def _backend_context(self) -> ssl.SSLContext:
        context = ssl.create_default_context(cafile=self._settings.evidence_backend_ca_file)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.check_hostname = True
        context.verify_mode = ssl.CERT_REQUIRED
        context.load_cert_chain(
            self._settings.evidence_node_cert_file, self._settings.evidence_node_key_file
        )
        return context

    async def start(self) -> str:
        socket_path = self._settings.evidence_relay_socket
        parent = os.path.dirname(socket_path) or "."
        os.makedirs(parent, mode=0o700, exist_ok=True)
        if os.path.exists(socket_path):
            os.unlink(socket_path)
        connector = TCPConnector(ssl=self._backend_context())
        self._session = ClientSession(connector=connector)
        self._server = await asyncio.start_unix_server(self._handle, path=socket_path)
        os.chmod(socket_path, 0o600)
        return socket_path

    async def stop(self) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()
            self._server = None
        if self._session is not None:
            await self._session.close()
            self._session = None
        socket_path = self._settings.evidence_relay_socket
        if socket_path and os.path.exists(socket_path):
            os.unlink(socket_path)

    async def run_forever(self) -> None:
        await self.start()
        await asyncio.Event().wait()

    async def _handle(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        settings = self._settings
        timeout = settings.evidence_timeout_seconds
        try:
            uid, gid = _peer_credentials(writer)
            allowed_gid = settings.evidence_relay_allowed_gid
            if uid != settings.evidence_relay_allowed_uid or (
                allowed_gid >= 0 and gid != allowed_gid
            ):
                return
            try:
                data = await asyncio.wait_for(reader.readuntil(b"\n"), timeout=timeout)
            except (asyncio.IncompleteReadError, asyncio.LimitOverrunError, TimeoutError):
                return
            if not data or len(data) > MAX_ENVELOPE_BYTES or self._session is None:
                return
            try:
                async with self._session.post(
                    f"https://{settings.evidence_backend_host}:{settings.evidence_backend_port}{EVIDENCE_PATH}",
                    data=data,
                    headers={"Content-Type": "application/json"},
                    timeout=ClientTimeout(total=timeout),
                    server_hostname=settings.evidence_backend_server_name or None,
                ) as response:
                    payload = await response.read()
            except (ClientError, TimeoutError, ConnectionError, ssl.SSLError):
                logger.warning("evidence relay backend hop failed")
                return
            writer.write(payload)
            await writer.drain()
        finally:
            writer.close()
            try:
                await writer.wait_closed()
            except (ConnectionError, ssl.SSLError):
                pass


def main() -> None:
    from .config import load_settings

    settings = load_settings(require_database=False)
    relay = EvidenceRelay(settings)
    asyncio.run(relay.run_forever())


if __name__ == "__main__":
    main()
