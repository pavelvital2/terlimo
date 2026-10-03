"""Authenticated public service-seed update; no grant/registration/rights mutation."""
from datetime import UTC, datetime

import asyncpg
from aiohttp import web

from .auth_api import SCHEMA_VERSION, ApiError, _error_response, random_hex, rfc3339
from .config import Settings
from .db import Database, DatabaseUnavailable
from .recovery_code import PublicRecoveryCode, RecoveryUnavailable
from .session_auth import AuthError, authenticate_session

SERVICE_SEED_PATH = "/api/mobile/v1/service-seed"


def register_recovery_routes(app: web.Application, settings: Settings, database: Database) -> None:
    public_code = PublicRecoveryCode(settings)

    async def service_seed(request: web.Request) -> web.Response:
        # Match the existing mobile response envelope; middleware also echoes X-Request-ID.
        request_id = random_hex(16)
        try:
            header = request.headers.get("Authorization", "")
            if not header.startswith("Bearer "):
                raise AuthError("SESSION_INVALID", 401)
            token = header[7:].strip()
            async with database.acquire() as connection:
                await authenticate_session(connection, settings, token)
            code = public_code.get()
            return web.json_response({
                "request_id": request_id,
                "server_time": rfc3339(datetime.now(UTC)),
                "schema_version": SCHEMA_VERSION,
                "status": "ok",
                "recovery_code": code,
            })
        except AuthError as error:
            return _error_response(request_id, ApiError(error.code, http=error.http))
        except RecoveryUnavailable:
            return _error_response(request_id, ApiError("RECOVERY_UNAVAILABLE", http=503, retryable=True))
        except (DatabaseUnavailable, asyncpg.PostgresError, OSError):
            return _error_response(request_id, ApiError("TEMPORARILY_UNAVAILABLE", http=503, retryable=True))

    app.router.add_get(SERVICE_SEED_PATH, service_seed, allow_head=False)
