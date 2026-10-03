"""Minimal HTTP API for the unified backend.

Only technical endpoints exist in 03.1: health, readiness and version.
Business mobile routes arrive in 03.2/03.3 and must use the same application
model; this runtime does not fabricate successful business responses.
"""

from __future__ import annotations

import asyncio
import logging
import uuid

from aiohttp import web

from . import SCHEMA_VERSION, __version__
from .announcements import register_announcement_routes
from .auth_api import AUTH_SERVICE_KEY, InstallationSessionService, register_mobile_auth_routes
from .config import Settings, load_settings
from .db import Database
from .evidence_transport import EVIDENCE_LISTENER_KEY, start_evidence_listener
from .maintenance import run_maintenance_loop
from .migrations import runner
from .mobile_account import ACCOUNT_SERVICE_KEY, AccountService, register_account_routes
from .mobile_catalog import CATALOG_SERVICE_KEY, CatalogService, register_catalog_routes
from .mobile_devices import register_device_routes
from .onboarding_api import (
    ONBOARDING_SERVICE_KEY,
    OnboardingIntentService,
    register_onboarding_routes,
)
from .referral_api import register_referral_routes
from .recovery_api import register_recovery_routes
from .payments import register_payment_routes
from .s5_payments import register_s5_payment_routes
from .telegram_binding import register_telegram_registration_routes
from .trial_activation import register_trial_routes

logger = logging.getLogger(__name__)

REQUEST_ID_HEADER = "X-Request-ID"
REQUEST_ID_KEY: web.RequestKey = web.RequestKey("request_id", str)
SETTINGS_KEY: web.AppKey = web.AppKey("settings", Settings)
DATABASE_KEY: web.AppKey = web.AppKey("db", Database)
MAINTENANCE_TASK_KEY: web.AppKey = web.AppKey("maintenance_task", asyncio.Task)
MAINTENANCE_STOP_KEY: web.AppKey = web.AppKey("maintenance_stop", asyncio.Event)


@web.middleware
async def request_context_middleware(
    request: web.Request, handler
) -> web.StreamResponse:
    request_id = request.headers.get(REQUEST_ID_HEADER) or uuid.uuid4().hex
    request[REQUEST_ID_KEY] = request_id
    try:
        response = await handler(request)
    except web.HTTPException as exc:
        response = exc
    response.headers[REQUEST_ID_HEADER] = request_id
    return response


def _payload(request: web.Request, status: str) -> dict[str, object]:
    settings: Settings = request.app[SETTINGS_KEY]
    return {
        "status": status,
        "service": "terlimo-backend",
        "version": __version__,
        "schema_version": SCHEMA_VERSION,
        "environment": settings.environment,
        "request_id": request[REQUEST_ID_KEY],
    }


async def health_live(request: web.Request) -> web.Response:
    return web.json_response(_payload(request, "live"))


async def health_ready(request: web.Request) -> web.Response:
    settings: Settings = request.app[SETTINGS_KEY]
    database: Database = request.app[DATABASE_KEY]
    body = _payload(request, "ready")
    body["database"] = settings.redacted_database_url
    if not await database.ensure_ready():
        body["status"] = "not_ready"
        body["reason"] = "database_unavailable"
        return web.json_response(body, status=503)
    try:
        async with database.acquire() as connection:
            pending = await runner.pending_versions(connection)
    except Exception:
        logger.exception("readiness database check failed")
        body["status"] = "not_ready"
        body["reason"] = "database_unavailable"
        return web.json_response(body, status=503)
    if pending:
        body["status"] = "not_ready"
        body["reason"] = "migrations_pending"
        body["pending_migrations"] = pending
        return web.json_response(body, status=503)
    body["migrations"] = "applied"
    return web.json_response(body)


async def version(request: web.Request) -> web.Response:
    return web.json_response(_payload(request, "ok"))


def create_app(settings: Settings, database: Database) -> web.Application:
    app = web.Application(middlewares=[request_context_middleware], client_max_size=64 * 1024)
    app[SETTINGS_KEY] = settings
    app[DATABASE_KEY] = database
    app[AUTH_SERVICE_KEY] = InstallationSessionService(settings, database)
    app[ACCOUNT_SERVICE_KEY] = AccountService(settings, database)
    app[CATALOG_SERVICE_KEY] = CatalogService(settings, database)
    app.router.add_get("/health/live", health_live)
    app.router.add_get("/health/ready", health_ready)
    app.router.add_get("/version", version)
    register_mobile_auth_routes(app)
    register_account_routes(app)
    register_catalog_routes(app)
    app[ONBOARDING_SERVICE_KEY] = OnboardingIntentService(settings, database)
    register_onboarding_routes(app)
    register_telegram_registration_routes(app, settings, database)
    register_trial_routes(app, settings, database)
    register_payment_routes(app, settings, database)
    register_s5_payment_routes(app, settings, database)
    register_announcement_routes(app, settings, database)
    register_device_routes(app, settings, database)
    register_recovery_routes(app, settings, database)
    register_referral_routes(app, settings, database)
    app[MAINTENANCE_TASK_KEY] = None

    async def _startup(_: web.Application) -> None:
        # The API must start even when PostgreSQL is down; /health/ready then
        # retries the same DSN in-process (bounded reconnects).
        if not await database.ensure_ready():
            logger.warning("database is not ready at startup; /health/ready will report 503")
        app[EVIDENCE_LISTENER_KEY] = await start_evidence_listener(settings, database)
        stop_event = asyncio.Event()
        app[MAINTENANCE_STOP_KEY] = stop_event
        app[MAINTENANCE_TASK_KEY] = asyncio.create_task(
            run_maintenance_loop(database, settings, stop_event)
        )

    async def _cleanup(_: web.Application) -> None:
        listener = app.get(EVIDENCE_LISTENER_KEY)
        if listener is not None:
            await listener.stop()
        stop_event: asyncio.Event | None = app.get(MAINTENANCE_STOP_KEY)
        if stop_event is not None:
            stop_event.set()
        task: asyncio.Task | None = app.get(MAINTENANCE_TASK_KEY)
        if task is not None:
            try:
                await asyncio.wait_for(task, timeout=5)
            except (TimeoutError, asyncio.CancelledError):
                task.cancel()
        await database.close()

    app.on_startup.append(_startup)
    app.on_cleanup.append(_cleanup)
    return app


def main() -> None:
    settings = load_settings()
    logging.basicConfig(
        level=settings.log_level,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    database = Database(settings)
    app = create_app(settings, database)
    logger.info(
        "starting API on %s:%s (env=%s, db=%s)",
        settings.api_host,
        settings.api_port,
        settings.environment,
        settings.redacted_database_url,
    )
    web.run_app(app, host=settings.api_host, port=settings.api_port, handle_signals=True)
