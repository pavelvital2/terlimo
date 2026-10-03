"""G6 service-only API relay: dedicated AF_UNIX hop (node -> relay) and mTLS internal
handler (relay -> backend) that faithfully replays the existing mobile API over a fixed
loopback destination.

The handler never reimplements auth or fabricates requests: it validates the strict service
frame, then performs a real HTTP request (method/path/query/body/headers preserved) against
the same backend's fixed loopback listener and returns the raw status/body. All authority
(PoP/bearer/idempotency/catalog) stays with the existing API. The handler performs no
onboarding/evidence writes; ``/api/mobile/v1/onboarding/intents`` is reached only as the
existing public route.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import signal
import ssl
import stat
import time
from contextvars import ContextVar
from datetime import UTC, datetime
from typing import Any

from aiohttp import ClientError, ClientSession, ClientTimeout, TCPConnector, TraceConfig, web

from .auth_api import AUTH_REQUEST_BUDGET_SECONDS, AUTH_DEADLINE_HEADER
from .auth_api_phase import AuthApiPhase, new_auth_api_phase
from .config import Settings
from .db import Database
from .evidence_transport import (
    EvidenceTransportError,
    _peer_credentials,
    _peer_identities,
    resolve_gateway_context,
)

logger = logging.getLogger(__name__)
_relay_phase_context: ContextVar[AuthApiPhase | None] = ContextVar(
    "relay_phase_context", default=None
)

SERVICE_PATH = "/internal/onboarding/service-http"
SERVICE_MAX_FRAME = 1536 * 1024
SERVICE_MAX_REQUEST_BODY = 64 * 1024
SERVICE_MAX_RESPONSE_BODY = 1024 * 1024
SERVICE_MAX_HEADERS = 8 * 1024
SERVICE_MAX_HEADER_VALUE = 1024
SERVICE_MAX_PATH_QUERY = 2048
SERVICE_ACTIVE_KEY: web.AppKey = web.AppKey("service_active", "list[int]")

_ALLOWED_HEADERS = ("Authorization", "Content-Type", "X-Request-ID", "Idempotency-Key")
_RESPONSE_HEADERS = ("Content-Type", "Retry-After")
_BOUNDED_PATH_ID = re.compile(r"[A-Za-z0-9._~-]{1,128}")
_REQUEST_ID = re.compile(r"^[0-9a-f]{32}$")
_OPERATION_ID = re.compile(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")


AUTH_RELAY_BUDGET_SECONDS = 21


def _auth_request(parsed: dict[str, Any]) -> bool:
    return parsed.get("method") == "POST" and parsed.get("path") in {
        "/api/mobile/v1/auth/challenge", "/api/mobile/v1/auth/session"
    }


def _request_budget(settings: Settings, parsed: dict[str, Any], auth_seconds: float) -> float:
    return auth_seconds if _auth_request(parsed) else settings.service_timeout_seconds


def _frame_auth(data: bytes) -> bool:
    # Classification only: invalid frames still reach the existing backend validator.
    try:
        return _auth_request(_parse_service_request(data))
    except EvidenceTransportError:
        return False


def _frame_budget(settings: Settings, data: bytes) -> float:
    return AUTH_RELAY_BUDGET_SECONDS if _frame_auth(data) else settings.service_timeout_seconds


def service_path_allowed(method: str, path: str) -> bool:
    """Exact existing mobile API operations only; no arbitrary URL/host/CONNECT/redirect."""
    fixed = {
        "/api/mobile/v1/devices": "GET",
        "/api/mobile/v1/announcements": "GET",
        "/api/mobile/v1/auth/challenge": "POST",
        "/api/mobile/v1/installations": "POST",
        "/api/mobile/v1/auth/session": "POST",
        "/api/mobile/v1/access/sync": "POST",
        "/api/mobile/v1/onboarding/intents": "POST",
        "/api/mobile/v1/registration/telegram/link": "POST",
        "/api/mobile/v1/trial/activate": "POST",
        "/api/mobile/v1/me": "GET",
        "/api/mobile/v1/service-seed": "GET",
        "/api/mobile/v1/gateways": "GET",
        "/api/mobile/v1/usage": "GET",
        "/api/mobile/v1/plans": "GET",
        "/api/mobile/v1/quotes": "POST",
        "/api/mobile/v1/payments": "POST",
    }
    if path == "/api/mobile/v1/referral/candidate":
        return method in {"POST", "DELETE"}
    if path == "/api/mobile/v1/referral":
        return method == "GET"
    if path in fixed:
        return method == fixed[path]
    prefix = "/api/mobile/v1/operations/"
    if method == "GET" and path.startswith(prefix):
        return bool(_OPERATION_ID.match(path[len(prefix) :]))
    payment_prefix = "/api/mobile/v1/payments/"
    if method == "GET" and path.startswith(payment_prefix):
        return bool(_OPERATION_ID.match(path[len(payment_prefix) :]))
    device_prefix = "/api/mobile/v1/devices/"
    if method == "DELETE" and path.startswith(device_prefix):
        return bool(_OPERATION_ID.match(path[len(device_prefix) :]))
    checkout_prefix = "/api/mobile/v1/payments/"
    checkout_suffix = "/checkout-session"
    if method == "POST" and path.startswith(checkout_prefix):
        rest = path[len(checkout_prefix):]
        if not rest.endswith(checkout_suffix):
            return False
        return bool(_BOUNDED_PATH_ID.fullmatch(rest[: -len(checkout_suffix)]))
    announcement_prefix = "/api/mobile/v1/announcements/"
    read_suffix = "/read"
    if method == "POST" and path.startswith(announcement_prefix) and path.endswith(read_suffix):
        return bool(_OPERATION_ID.match(path[len(announcement_prefix) : -len(read_suffix)]))
    return False


def _no_duplicates(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise EvidenceTransportError("SERVICE_BAD_FRAME", http=400)
        result[key] = value
    return result


_B64_ALPHABET = re.compile(r"^[A-Za-z0-9_-]*$")


def _decode_b64(value: Any, limit: int) -> bytes:
    """Strict canonical raw-URL base64, matching the Go client's codec."""
    import base64

    if not isinstance(value, str) or not _B64_ALPHABET.match(value):
        raise EvidenceTransportError("SERVICE_BAD_FRAME", http=400)
    if len(value) > (limit + 2) // 3 * 4 + 8:
        raise EvidenceTransportError("SERVICE_BAD_FRAME", http=400)
    try:
        raw = base64.urlsafe_b64decode(value + "=" * (-len(value) % 4))
    except (ValueError, TypeError) as exc:
        raise EvidenceTransportError("SERVICE_BAD_FRAME", http=400) from exc
    if len(raw) > limit or base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii") != value:
        raise EvidenceTransportError("SERVICE_BAD_FRAME", http=400)
    return raw


def _byte_len(value: str) -> int:
    return len(value.encode("utf-8"))


def _reject_constant(_value: str) -> Any:
    raise ValueError("non-finite json constant")


def _parse_service_request(raw: bytes) -> dict[str, Any]:
    if len(raw) == 0 or len(raw) > SERVICE_MAX_FRAME:
        raise EvidenceTransportError("SERVICE_BAD_FRAME", http=400)
    try:
        payload = json.loads(
            raw.decode("utf-8"), object_pairs_hook=_no_duplicates, parse_constant=_reject_constant
        )
    except (ValueError, UnicodeError) as exc:
        raise EvidenceTransportError("SERVICE_BAD_FRAME", http=400) from exc
    if not isinstance(payload, dict) or set(payload) != {
        "v",
        "op",
        "request_id",
        "method",
        "path",
        "query",
        "headers",
        "body_b64",
    }:
        raise EvidenceTransportError("SERVICE_BAD_FRAME", http=400)
    if type(payload["v"]) is not int or payload["v"] != 1 or payload["op"] != "service.http":
        raise EvidenceTransportError("SERVICE_BAD_FRAME", http=400)
    request_id = payload["request_id"]
    if not isinstance(request_id, str) or _REQUEST_ID.fullmatch(request_id) is None:
        raise EvidenceTransportError("SERVICE_BAD_FRAME", http=400)
    method, path, query = payload["method"], payload["path"], payload["query"]
    if method not in ("GET", "POST", "DELETE"):
        raise EvidenceTransportError("SERVICE_BAD_METHOD", http=400)
    if not isinstance(path, str) or not isinstance(query, str):
        raise EvidenceTransportError("SERVICE_BAD_PATH", http=400)
    if not path or _byte_len(path) + _byte_len(query) > SERVICE_MAX_PATH_QUERY:
        raise EvidenceTransportError("SERVICE_BAD_PATH", http=400)
    if (
        any(ch in path for ch in "\\?#%")
        or ".." in path
        or "//" in path
        or _has_control(path)
        or _has_control(query)
        or "#" in query
    ):
        raise EvidenceTransportError("SERVICE_BAD_PATH", http=400)
    if not service_path_allowed(method, path):
        raise EvidenceTransportError("SERVICE_PATH_DENIED", http=403)
    headers = payload["headers"]
    if headers is None:
        headers = {}
    if not isinstance(headers, dict):
        raise EvidenceTransportError("SERVICE_BAD_HEADERS", http=400)
    total = 0
    for name, value in headers.items():
        if (
            name not in _ALLOWED_HEADERS
            or not isinstance(value, str)
            or _byte_len(value) > SERVICE_MAX_HEADER_VALUE
            or _has_control(value)
            or _has_control(name)
        ):
            raise EvidenceTransportError("SERVICE_BAD_HEADERS", http=400)
        total += _byte_len(name) + _byte_len(value)
    if total > SERVICE_MAX_HEADERS:
        raise EvidenceTransportError("SERVICE_BAD_HEADERS", http=400)
    body = _decode_b64(payload["body_b64"], SERVICE_MAX_REQUEST_BODY)
    return {
        "request_id": request_id,
        "method": method,
        "path": path,
        "query": query,
        "headers": headers,
        "body": body,
    }


def _has_control(value: str) -> bool:
    return any(ord(ch) < 0x20 or ord(ch) == 0x7F for ch in value)


def _service_error_body(request_id: str, code: str, retryable: bool) -> dict[str, Any]:
    return {"v": 1, "request_id": request_id, "error": {"code": code, "retryable": retryable}}


def _request_id_from_raw(raw: bytes) -> str | None:
    """Best-effort bounded echo of a well-formed ``request_id`` from a rejected frame.

    Node-side reply validation requires the error frame to carry the same request_id as the
    request; without it the peer rejects the transport error as SERVICE_BAD_RESPONSE and the
    real reason code is lost. Only a strict 32-hex id from a duplicate-free JSON object is
    echoed; anything else keeps the zero placeholder.
    """
    try:
        value = json.loads(raw.decode("utf-8"), object_pairs_hook=_no_duplicates)
    except (ValueError, UnicodeError, EvidenceTransportError):
        return None
    candidate = value.get("request_id") if isinstance(value, dict) else None
    if isinstance(candidate, str) and _REQUEST_ID.fullmatch(candidate):
        return candidate
    return None


def _service_upstream_url(settings: Settings, path: str, query: str):
    """Fixed loopback target with a raw (pre-encoded) query, IPv6-safe."""
    from yarl import URL

    host = settings.service_upstream_host
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    raw = f"http://{host}:{settings.api_port}{path}"
    if query:
        raw = f"{raw}?{query}"
    return URL(raw, encoded=True)


async def _read_capped(response: Any, limit: int) -> bytes:
    """Read a response body with a hard cap before/while allocating."""
    if response.content_length is not None and response.content_length > limit:
        raise EvidenceTransportError("SERVICE_BAD_RESPONSE", http=502)
    chunks: list[bytes] = []
    total = 0
    async for chunk in response.content.iter_chunked(65536):
        total += len(chunk)
        if total > limit:
            raise EvidenceTransportError("SERVICE_BAD_RESPONSE", http=502)
        chunks.append(chunk)
    return b"".join(chunks)


async def _replay_service_request(
    settings: Settings, parsed: dict[str, Any], phase: AuthApiPhase | None = None,
    *, deadline: float | None = None,
) -> dict[str, Any]:
    seconds = _request_budget(settings, parsed, AUTH_REQUEST_BUDGET_SECONDS)
    headers = parsed["headers"]
    if _auth_request(parsed):
        cap = asyncio.get_running_loop().time() + seconds
        deadline = min(deadline, cap) if deadline is not None else cap
        seconds = max(0, deadline - asyncio.get_running_loop().time())
        if seconds <= 0:
            raise TimeoutError
        headers = {**headers, AUTH_DEADLINE_HEADER: str(deadline)}
    timeout = ClientTimeout(
        total=seconds, ceil_threshold=float("inf") if _auth_request(parsed) else 5
    )
    connector = TCPConnector(limit=settings.service_max_concurrency)
    session = ClientSession(connector=connector, trust_env=False, timeout=timeout)
    try:
        if phase is not None:
            phase.mark("replay_request_begin")
        async with session.request(
            parsed["method"],
            _service_upstream_url(settings, parsed["path"], parsed["query"]),
            data=parsed["body"],
            headers=headers,
            allow_redirects=False,
        ) as response:
            if phase is not None:
                phase.mark("replay_response_headers")
            try:
                body = await _read_capped(response, SERVICE_MAX_RESPONSE_BODY)
                if phase is not None:
                    phase.mark("replay_cappedbody_end")
            except EvidenceTransportError:
                if phase is not None:
                    phase.mark("replay_error", "capped_body")
                return _service_error_body(parsed["request_id"], "SERVICE_BAD_RESPONSE", True)
            headers = {}
            for name in _RESPONSE_HEADERS:
                if name in response.headers:
                    value = response.headers[name]
                    if len(value) > SERVICE_MAX_HEADER_VALUE or any(ch in value for ch in "\r\n"):
                        return _service_error_body(parsed["request_id"], "SERVICE_BAD_RESPONSE", True)
                    headers[name] = value
            import base64

            result = {
                "v": 1,
                "request_id": parsed["request_id"],
                "status": response.status,
                "headers": headers,
                "body_b64": base64.urlsafe_b64encode(body).rstrip(b"=").decode("ascii"),
            }
            if len(json.dumps(result, separators=(",", ":"))) > SERVICE_MAX_FRAME:
                return _service_error_body(parsed["request_id"], "SERVICE_BAD_RESPONSE", True)
            return result
    except (ClientError, TimeoutError, ConnectionError, OSError, ssl.SSLError):
        if phase is not None:
            phase.mark("replay_error", "unavailable")
        return _service_error_body(parsed["request_id"], "SERVICE_UNAVAILABLE", True)
    finally:
        if phase is not None:
            phase.mark("replay_session_close_begin")
        await session.close()
        if phase is not None:
            phase.mark("replay_session_close_end")


async def _read_request_capped(request: web.Request, limit: int) -> bytes:
    """Capped stream read for the admitted request body (never unbounded)."""
    if request.content_length is not None and request.content_length > limit:
        raise EvidenceTransportError("SERVICE_BAD_FRAME", http=413)
    chunks: list[bytes] = []
    total = 0
    async for chunk in request.content.iter_chunked(65536):
        total += len(chunk)
        if total > limit:
            raise EvidenceTransportError("SERVICE_BAD_FRAME", http=413)
        chunks.append(chunk)
    return b"".join(chunks)


def _service_handler(settings: Settings, database: Database):
    async def handler(request: web.Request) -> web.Response:
        phase = new_auth_api_phase("service")
        if phase is not None:
            phase.mark("handler_entry")
        # Bounded admission: no unbounded wait queue. The counter is incremented without an
        # await between the check and the claim, so excess requests get an immediate busy.
        active = request.app[SERVICE_ACTIVE_KEY]
        if active[0] >= settings.service_max_concurrency:
            return web.json_response(_service_error_body("0" * 32, "SERVICE_BUSY", True), status=429)
        active[0] += 1
        request_id = "0" * 32
        raw: bytes | None = None
        try:
            # The whole admitted handler shares the existing per-request service timeout
            # budget, so a slow/authenticated body cannot hold a slot forever.
            entered = asyncio.get_running_loop().time()
            async with asyncio.timeout(settings.service_timeout_seconds) as budget:
                raw = await _read_request_capped(request, SERVICE_MAX_FRAME)
                if phase is not None:
                    phase.mark("body_read_end")
                try:
                    parsed = _parse_service_request(raw)
                except EvidenceTransportError:
                    # Parse-stage rejections must still echo the frame's request id, otherwise
                    # the node peer reports SERVICE_BAD_RESPONSE instead of the real code.
                    recovered = _request_id_from_raw(raw)
                    if recovered is not None:
                        request_id = recovered
                    raise
                request_id = parsed["request_id"]
                if _auth_request(parsed):
                    budget.reschedule(entered + AUTH_REQUEST_BUDGET_SECONDS)
                if phase is not None:
                    phase.mark("parse_end")
                    if parsed["path"] in {"/api/mobile/v1/auth/challenge", "/api/mobile/v1/auth/session"}:
                        phase.activate(request_id)
                    else:
                        phase = None
                identities = _peer_identities(request.transport.get_extra_info("ssl_object"))
                if phase is not None:
                    phase.mark("peer_identity_end")
                    phase.mark("context_pool_acquire_begin")
                async with database.acquire() as connection:
                    if phase is not None:
                        phase.mark("context_pool_acquire_end")
                        phase.mark("resolve_gateway_context_begin")
                    await resolve_gateway_context(connection, identities, settings.environment)
                    if phase is not None:
                        phase.mark("resolve_gateway_context_end")
                if phase is not None:
                    phase.mark("context_pool_release_end")
                    phase.mark("replay_begin")
                    result = await _replay_service_request(settings, parsed, phase, **({"deadline": budget.when()} if _auth_request(parsed) else {}))
                    phase.mark("replay_end")
                else:
                    result = await _replay_service_request(settings, parsed, **({"deadline": budget.when()} if _auth_request(parsed) else {}))
            response = web.json_response(result, dumps=lambda value: json.dumps(value, separators=(",", ":")))
            if phase is not None:
                phase.mark("response_ready")
            return response
        except EvidenceTransportError as error:
            if phase is not None:
                phase.mark("handler_error", "rejected")
            return web.json_response(_service_error_body(request_id, error.code, False))
        except asyncio.CancelledError:
            if phase is not None:
                phase.mark("handler_error", "cancelled")
            raise
        except TimeoutError:
            if phase is not None:
                phase.mark("handler_error", "timeout")
            if raw is not None and request_id == "0" * 32:
                recovered = _request_id_from_raw(raw)
                if recovered is not None:
                    request_id = recovered
            return web.json_response(_service_error_body(request_id, "SERVICE_UNAVAILABLE", True), status=503)
        finally:
            active[0] -= 1
            if phase is not None:
                phase.finish()

    return handler


def register_service_routes(app: web.Application, settings: Settings, database: Database) -> None:
    if not settings.service_endpoint_enabled:
        return
    app[SERVICE_ACTIVE_KEY] = [0]
    app.router.add_post(SERVICE_PATH, _service_handler(settings, database))


def _prepare_relay_socket(socket_path: str) -> None:
    """B6: never unlink any existing path (even same-uid); operator cleans stale sockets.

    Only the parent directory is created/validated; an existing socket path fails closed so a
    second instance cannot take over or silently remove another instance's socket."""
    parent = os.path.dirname(socket_path) or "."
    if os.path.islink(parent):
        raise EvidenceTransportError("service relay parent must not be a symlink", http=503)
    if os.path.exists(parent):
        st = os.lstat(parent)
        if not stat.S_ISDIR(st.st_mode) or st.st_uid != os.getuid() or (st.st_mode & 0o077):
            raise EvidenceTransportError("service relay parent directory is not private and owned", http=503)
    else:
        os.makedirs(parent, mode=0o700)
    if os.path.lexists(socket_path):
        raise EvidenceTransportError("service relay socket path already exists", http=503)


class ServiceRelay:
    """AF_UNIX hop with restricted peer credentials; verbatim mTLS forwarding to the
    backend internal service handler. One bounded newline frame per request, no retries,
    bounded admission and a tracked task/cleanup lifecycle."""

    def __init__(self, settings: Settings) -> None:
        require_service_relay_material(settings)
        self._settings = settings
        self._server: asyncio.AbstractServer | None = None
        self._session: ClientSession | None = None
        self._tasks: set[asyncio.Task[Any]] = set()
        self._socket_path: str | None = None
        self._socket_identity: tuple[int, int] | None = None
        self._started = False
        self._phase_probe_enabled = os.environ.get("TERLIMO_SERVICE_RELAY_PHASE_PROBE") == "1"

    def _phase(self, phase: str, outcome: str = "ok") -> None:
        if not self._phase_probe_enabled:
            return
        context = _relay_phase_context.get()
        if context is None:
            return
        context.mark(phase, outcome)

    def _phase_trace(self) -> TraceConfig:
        trace = TraceConfig()

        def callback(phase: str, outcome: str = "ok"):
            async def mark(_session, _context, _params):
                self._phase(phase, outcome)
            return mark

        for signal_name, phase in (
            ("on_connection_queued_start", "queue_begin"),
            ("on_connection_queued_end", "queue_end"),
            ("on_dns_resolvehost_start", "dns_begin"),
            ("on_dns_resolvehost_end", "dns_end"),
            ("on_connection_create_start", "connect_begin"),
            ("on_connection_create_end", "connect_end"),
            ("on_connection_reuseconn", "connection_reuse"),
            ("on_request_headers_sent", "headers_sent"),
            ("on_request_end", "response_headers"),
        ):
            getattr(trace, signal_name).append(callback(phase))
        trace.on_request_exception.append(callback("request_exception", "error"))
        return trace

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
        if self._started or self._server is not None:
            raise EvidenceTransportError("service relay already started", http=503)
        socket_path = self._settings.service_relay_socket
        _prepare_relay_socket(socket_path)
        self._started = True
        try:
            connector = TCPConnector(ssl=self._backend_context(), limit=self._settings.service_max_concurrency)
            trace_options = {"trace_configs": [self._phase_trace()]} if self._phase_probe_enabled else {}
            self._session = ClientSession(connector=connector, **trace_options)
            self._server = await asyncio.start_unix_server(
                self._handle, path=socket_path, limit=SERVICE_MAX_FRAME + 1
            )
            st = os.lstat(socket_path)
            self._socket_path = socket_path
            self._socket_identity = (st.st_dev, st.st_ino)
            os.chmod(socket_path, 0o600)
        except BaseException:
            await self.stop()
            raise
        return socket_path

    async def stop(self) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()
            self._server = None
        tasks = list(self._tasks)
        for task in tasks:
            task.cancel()
        if tasks:
            await asyncio.gather(*tasks, return_exceptions=True)
        self._tasks.clear()
        if self._session is not None:
            await self._session.close()
            self._session = None
        path, identity = self._socket_path, self._socket_identity
        self._socket_path, self._socket_identity, self._started = None, None, False
        if path and identity and os.path.lexists(path):
            st = os.lstat(path)
            if stat.S_ISSOCK(st.st_mode) and st.st_uid == os.getuid() and (st.st_dev, st.st_ino) == identity:
                os.unlink(path)

    async def run_forever(self) -> None:
        await self.start()
        try:
            await asyncio.Event().wait()
        finally:
            await self.stop()

    async def _forward(self, data: bytes) -> bytes | None:
        settings = self._settings
        if self._session is None:
            return None
        self._phase("forward_entry")
        try:
            async with self._session.post(
                f"https://{settings.evidence_backend_host}:{settings.evidence_backend_port}{SERVICE_PATH}",
                data=data,
                headers={"Content-Type": "application/json"},
                timeout=ClientTimeout(
                    total=_frame_budget(settings, data),
                    ceil_threshold=float("inf") if _frame_auth(data) else 5,
                ),
                server_hostname=settings.evidence_backend_server_name or None,
                allow_redirects=False,
            ) as response:
                self._phase("body_begin")
                try:
                    payload = await _read_capped(response, SERVICE_MAX_FRAME)
                except asyncio.CancelledError:
                    self._phase("body_complete", "cancelled")
                    raise
                except Exception:
                    self._phase("body_complete", "error")
                    raise
                self._phase("body_complete")
                return payload
        except asyncio.CancelledError:
            self._phase("forward_cancelled", "cancelled")
            raise
        except (ClientError, TimeoutError, ConnectionError, ssl.SSLError, EvidenceTransportError):
            self._phase("forward_error", "error")
            logger.warning("service relay backend hop failed")
            return None

    async def _handle(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        settings = self._settings
        timeout = settings.service_timeout_seconds
        entered = asyncio.get_running_loop().time()
        auth = False
        task = asyncio.current_task()
        phase_token = None
        if self._phase_probe_enabled:
            try:
                phase_token = _relay_phase_context.set(
                    AuthApiPhase("relay", task_id=id(task), sink=logger)
                )
            except Exception:
                phase_token = None
            self._phase("unix_callback")
        self._tasks.add(task) if task is not None else None
        upstream: asyncio.Task[Any] | None = None
        watch: asyncio.Task[Any] | None = None
        try:
            uid, gid = _peer_credentials(writer)
            if uid != settings.service_relay_allowed_uid or (
                settings.service_relay_allowed_gid >= 0 and gid != settings.service_relay_allowed_gid
            ):
                return
            # Bounded admission: an excess connection is closed immediately, never queued.
            if len(self._tasks) > settings.service_max_concurrency:
                return
            try:
                data = await asyncio.wait_for(reader.readuntil(b"\n"), timeout=timeout)
            except (asyncio.IncompleteReadError, asyncio.LimitOverrunError, TimeoutError):
                return
            if not data or len(data) - 1 > SERVICE_MAX_FRAME:
                return
            self._phase("frame_complete")
            request_budget = _frame_budget(settings, data)
            auth = _frame_auth(data)
            deadline = entered + request_budget
            def remaining() -> float:
                return max(0, deadline - asyncio.get_running_loop().time()) if auth else timeout
            upstream = asyncio.ensure_future(self._forward(data))
            watch = asyncio.ensure_future(reader.read(1))
            done, _pending = await asyncio.wait(
                {upstream, watch}, return_when=asyncio.FIRST_COMPLETED, timeout=remaining()
            )
            if watch in done and upstream not in done:
                # Node closed the Unix socket: abort the upstream call rather than wait.
                upstream.cancel()
                return
            if upstream not in done:
                upstream.cancel()
                return
            watch.cancel()
            try:
                payload = upstream.result()
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                return
            if not payload or len(payload) > SERVICE_MAX_FRAME or b"\n" in payload:
                return
            writer.write(payload + b"\n")
            await asyncio.wait_for(writer.drain(), timeout=remaining())
            self._phase("drain_complete")
        finally:
            try:
                pending = [p for p in (upstream, watch) if p is not None and not p.done()]
                for child in pending:
                    child.cancel()
                if pending:
                    await asyncio.gather(*pending, return_exceptions=True)
            finally:
                if task is not None:
                    self._tasks.discard(task)
                writer.close()
                try:
                    await asyncio.wait_for(
                        writer.wait_closed(), timeout=min(timeout, 1) if auth else timeout
                    )
                except (TimeoutError, ConnectionError, ssl.SSLError, OSError):
                    pass
                finally:
                    if phase_token is not None:
                        context = _relay_phase_context.get()
                        if context is not None:
                            context.finish()
                        _relay_phase_context.reset(phase_token)


def require_service_relay_material(settings: Settings) -> None:
    if not settings.service_relay_enabled:
        raise EvidenceTransportError("ONBOARDING_SERVICE_RELAY_DISABLED", http=503)
    required = {
        "ONBOARDING_EVIDENCE_NODE_CERT_FILE": settings.evidence_node_cert_file,
        "ONBOARDING_EVIDENCE_NODE_KEY_FILE": settings.evidence_node_key_file,
        "ONBOARDING_EVIDENCE_BACKEND_CA_FILE": settings.evidence_backend_ca_file,
    }
    for name, path in required.items():
        if not path or not os.path.isfile(path):
            raise EvidenceTransportError(f"{name} is not a file", http=503)
    if not settings.service_relay_socket or not settings.evidence_backend_host:
        raise EvidenceTransportError("service relay target is not configured", http=503)
    if settings.service_relay_allowed_uid < 0:
        raise EvidenceTransportError("service relay allowed uid is not configured", http=503)


def main() -> None:
    from .config import load_settings

    settings = load_settings(require_database=False)
    relay = ServiceRelay(settings)
    if relay._phase_probe_enabled:
        logging.basicConfig(level=logging.INFO)

    async def _serve_until_signal() -> None:
        loop = asyncio.get_running_loop()
        shutdown = asyncio.Event()
        for sig in (signal.SIGTERM, signal.SIGINT):
            loop.add_signal_handler(sig, shutdown.set)
        await relay.start()
        try:
            await shutdown.wait()
        finally:
            await relay.stop()

    asyncio.run(_serve_until_signal())


if __name__ == "__main__":
    main()
