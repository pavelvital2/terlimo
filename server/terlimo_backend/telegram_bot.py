"""S3-A minimal product-bot runner for the one-time registration deep link.

Processes only `/start <one-time token>` updates, confirms through the existing
``confirm_registration`` (no client-supplied identity is trusted), and replies with the server
result. It is an optional process (never started by ``create_app``), configurable via
TELEGRAM_REGISTRATION_BOT_{TOKEN,USERNAME,KEY}; without a token it cannot run and the backend
stays fail-closed. The transport is injectable so local tests need no network.

Run on TEST (after the owner provides a product bot token/username):
    TELEGRAM_REGISTRATION_BOT_TOKEN=... TELEGRAM_REGISTRATION_BOT_USERNAME=... \
    TELEGRAM_REGISTRATION_BOT_KEY=... \
    PYTHONPATH=/tmp/opencode/hour-data:. \
    /home/pavel/projects/terlimo-backend/.venv/bin/python -m terlimo_backend.telegram_bot
"""

from __future__ import annotations

import asyncio
import logging
from typing import Any, Protocol

import asyncpg
from aiohttp import ClientError, ClientSession, ClientTimeout

from .auth_api import ApiError
from .config import Settings, load_settings
from .db import Database
from .telegram_binding import confirm_registration, registration_enabled

logger = logging.getLogger(__name__)

TELEGRAM_API = "https://api.telegram.org"
START_COMMAND = "/start"


def bot_ready(settings: Settings) -> bool:
    return registration_enabled(settings) and bool(settings.telegram_bot_token)


class TelegramTransport(Protocol):
    async def get_updates(self, offset: int, timeout: int) -> list[dict[str, Any]]: ...
    async def send_message(self, chat_id: int, text: str) -> None: ...


class AiohttpTelegramTransport:
    def __init__(self, token: str, *, timeout_seconds: int = 30) -> None:
        self._token = token
        self._timeout = timeout_seconds
        self._session: ClientSession | None = None

    async def _client(self) -> ClientSession:
        if self._session is None:
            self._session = ClientSession(timeout=ClientTimeout(total=self._timeout + 10))
        return self._session

    async def close(self) -> None:
        if self._session is not None:
            await self._session.close()
            self._session = None

    async def get_updates(self, offset: int, timeout: int) -> list[dict[str, Any]]:
        session = await self._client()
        async with session.get(
            f"{TELEGRAM_API}/bot{self._token}/getUpdates",
            params={"offset": offset, "timeout": timeout, "allowed_updates": '["message"]'},
        ) as response:
            payload = await response.json()
        if not payload.get("ok"):
            logger.warning("telegram getUpdates not ok")
            return []
        return list(payload.get("result") or [])

    async def send_message(self, chat_id: int, text: str) -> None:
        session = await self._client()
        async with session.post(
            f"{TELEGRAM_API}/bot{self._token}/sendMessage",
            json={"chat_id": chat_id, "text": text},
        ) as response:
            await response.read()


def _start_token(update: dict[str, Any]) -> tuple[int, str | None, str] | None:
    message = update.get("message") if isinstance(update, dict) else None
    if not isinstance(message, dict):
        return None
    chat = message.get("chat") if isinstance(message.get("chat"), dict) else None
    text = message.get("text")
    if not isinstance(chat, dict) or not isinstance(text, str):
        return None
    telegram_id = chat.get("id")
    if type(telegram_id) is not int:
        return None
    parts = text.strip().split()
    if not parts or parts[0] != START_COMMAND:
        return None
    token = parts[1] if len(parts) > 1 else None
    username = message.get("from", {}).get("username") if isinstance(message.get("from"), dict) else None
    return telegram_id, username if isinstance(username, str) else None, token or ""


def _reply_text(result: dict[str, Any]) -> str:
    if result.get("trial_available"):
        return "Регистрация подтверждена. Доступен будущий пробный период 7 дней."
    reason = result.get("trial_reason")
    if reason == "trial_already_used":
        return "Регистрация подтверждена. Пробный период уже использован; доступна покупка."
    if reason == "hour_expired":
        return "Регистрация подтверждена. Час доступа завершён; доступна покупка."
    return "Регистрация подтверждена."


async def handle_update(
    connection: asyncpg.Connection, settings: Settings, update: dict[str, Any]
) -> dict[str, Any] | None:
    parsed = _start_token(update)
    if parsed is None:
        return None
    telegram_id, username, token = parsed
    if not token:
        return {"telegram_id": telegram_id, "state": "missing_token", "reply": "Откройте ссылку регистрации заново."}
    try:
        result = await confirm_registration(
            connection, settings, token=token, telegram_id=telegram_id, telegram_username=username
        )
    except (ApiError, asyncpg.PostgresError, OSError) as error:  # bounded reply, no crash loop
        code = getattr(error, "code", type(error).__name__)
        return {"telegram_id": telegram_id, "state": str(code), "reply": "Ссылка регистрации недействительна."}
    return {"telegram_id": telegram_id, "state": "registered", "result": result, "reply": _reply_text(result)}


class RegistrationBotRunner:
    def __init__(
        self,
        settings: Settings,
        database: Database,
        transport: TelegramTransport,
        *,
        poll_timeout: int = 25,
    ) -> None:
        self._settings = settings
        self._database = database
        self._transport = transport
        self._poll_timeout = poll_timeout
        self._offset = 0

    async def handle_once(self) -> list[dict[str, Any]]:
        updates = await self._transport.get_updates(self._offset, self._poll_timeout)
        handled: list[dict[str, Any]] = []
        async with self._database.acquire() as connection:
            for update in updates:
                update_id = update.get("update_id")
                if type(update_id) is int:
                    self._offset = max(self._offset, update_id + 1)
                outcome = await handle_update(connection, self._settings, update)
                if outcome is not None:
                    handled.append(outcome)
        for outcome in handled:
            if outcome.get("reply"):
                try:
                    await self._transport.send_message(outcome["telegram_id"], outcome["reply"])
                except (ClientError, OSError):
                    logger.warning("telegram reply failed")
        return handled

    async def run_forever(self) -> None:
        while True:
            try:
                await self.handle_once()
            except (ClientError, OSError, asyncpg.PostgresError):
                logger.warning("registration bot poll failed")
                await asyncio.sleep(3)


async def _amain(settings: Settings) -> None:
    database = Database(settings)
    await database.connect()
    transport = AiohttpTelegramTransport(settings.telegram_bot_token)
    runner = RegistrationBotRunner(settings, database, transport)
    try:
        await runner.run_forever()
    finally:
        await transport.close()
        await database.close()


def main() -> None:
    settings = load_settings()
    if not bot_ready(settings):
        raise SystemExit(
            "registration bot not configured: set TELEGRAM_REGISTRATION_BOT_TOKEN, "
            "TELEGRAM_REGISTRATION_BOT_USERNAME and TELEGRAM_REGISTRATION_BOT_KEY"
        )
    logging.basicConfig(level=settings.log_level)
    try:
        asyncio.run(_amain(settings))
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
