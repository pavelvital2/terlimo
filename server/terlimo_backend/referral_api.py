"""Referral routes over the authenticated existing service channel."""

import asyncpg
from aiohttp import web

from .auth_api import (
    ApiError,
    _error_response,
    _json_body,
    random_hex,
)
from .referral import change_candidate, referral_info
from .session_auth import AuthError, authenticate_session
from .telegram_binding import _bearer_token

PREFIX = "/api/mobile/v1/referral"


def register_referral_routes(app, settings, database):
    def endpoint(kind):
        async def handler(request):
            request_id = random_hex(16)
            candidate_lookup_started = False
            try:
                body = None
                if kind == "set":
                    body = await _json_body(request)
                elif kind == "clear" and request.can_read_body:
                    raise ApiError("BAD_MESSAGE", http=400)
                token = _bearer_token(request)
                async with database.acquire() as connection:
                    context = await authenticate_session(connection, settings, token)
                    if kind == "get":
                        result = {"referral": await referral_info(connection, context)}
                    else:
                        candidate_lookup_started = True
                        result = await change_candidate(
                            connection,
                            installation_id=context.installation_id,
                            code=None,
                            body=body,
                            idempotency_key=request.headers.get("Idempotency-Key"),
                            clear=kind == "clear",
                        )
                return web.json_response({"request_id": request_id, **result})
            except AuthError as error:
                return _error_response(
                    request_id, ApiError(error.code, http=error.http, retryable=error.retryable)
                )
            except ApiError as error:
                if error.code == "BAD_MESSAGE" and kind != "get" and not candidate_lookup_started:
                    # Parsing failed before the original K could be inspected. It is
                    # not proof that an earlier committed operation never mutated.
                    error = ApiError("BAD_MESSAGE", http=400, retryable=True)
                return _error_response(request_id, error)
            except (asyncpg.PostgresError, OSError):
                return _error_response(request_id, ApiError("SERVICE_UNAVAILABLE", http=503, retryable=True))

        return handler

    app.router.add_get(PREFIX, endpoint("get"))
    app.router.add_post(PREFIX + "/candidate", endpoint("set"))
    app.router.add_delete(PREFIX + "/candidate", endpoint("clear"))
