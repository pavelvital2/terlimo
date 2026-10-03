"""Existing product bot: registration deep link and public Recovery v1 code.

Processes `/recovery` (public signed code, no DB) and `/start <one-time token>`,
which confirms through the existing
``confirm_registration`` (no client-supplied identity is trusted), and replies with the server
result. It is an optional process (never started by ``create_app``), configurable via
TELEGRAM_REGISTRATION_BOT_TOKEN; registration still requires USERNAME/KEY. Public
recovery additionally needs RECOVERY_CODE_FILE and RECOVERY_VERIFY_KEY_B64.
The transport is injectable so local tests need no Telegram network.

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
from .db import Database, DatabaseUnavailable
from .recovery_code import PublicRecoveryCode, RecoveryUnavailable
from .telegram_binding import confirm_registration

logger = logging.getLogger(__name__)

TELEGRAM_API = "https://api.telegram.org"
START_COMMAND = "/start"
RECOVERY_BUTTON = "Восстановить подключение"
RECOVERY_INSTRUCTION = "Скопируйте сообщение с кодом целиком (удержание → Скопировать). Вернитесь в TERLIMO: Настройки → Восстановить подключение → вставьте код. Покупка и повторная регистрация не нужны. Код меняет подключение, а не аккаунт или доступ."
RECOVERY_MENU = {"keyboard": [[{"text": RECOVERY_BUTTON}]], "resize_keyboard": True}


def bot_ready(settings: Settings) -> bool:
    return bool(settings.telegram_bot_token)


class TelegramTransport(Protocol):
    async def get_updates(self, offset: int, timeout: int) -> list[dict[str, Any]]: ...
    async def send_message(self, chat_id: int, text: str, *, reply_markup: dict | None = None) -> None: ...


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

    async def send_message(self, chat_id: int, text: str, *, reply_markup: dict | None = None) -> None:
        session = await self._client()
        payload = {"chat_id": chat_id, "text": text}
        if reply_markup is not None:
            payload["reply_markup"] = reply_markup
        async with session.post(
            f"{TELEGRAM_API}/bot{self._token}/sendMessage",
            json=payload,
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


def _recovery_chat(update: dict[str, Any], settings: Settings) -> int | None:
    message = update.get("message") if isinstance(update, dict) else None
    if not isinstance(message, dict):
        return None
    chat, text = message.get("chat"), message.get("text")
    if not isinstance(chat, dict) or type(chat.get("id")) is not int or not isinstance(text, str):
        return None
    accepted = {"/recovery", "/start recovery", RECOVERY_BUTTON}
    if settings.telegram_bot_username:
        accepted.add("/recovery@" + settings.telegram_bot_username.lstrip("@"))
        accepted.add("/start@" + settings.telegram_bot_username.lstrip("@") + " recovery")
    return chat["id"] if text.strip() in accepted else None


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
    connection: asyncpg.Connection | None, settings: Settings, update: dict[str, Any],
    *, public_code: PublicRecoveryCode | None = None,
) -> dict[str, Any] | None:
    chat_id = _recovery_chat(update, settings)
    if chat_id is not None:
        try:
            code = (public_code or PublicRecoveryCode(settings)).get()
            # Whole code only: no prefix/markup that users might accidentally paste.
            return {"telegram_id": chat_id, "state": "recovery", "reply": code,
                    "instruction": RECOVERY_INSTRUCTION}
        except RecoveryUnavailable:
            return {"telegram_id": chat_id, "state": "recovery_unavailable",
                    "reply": "Код восстановления сейчас недоступен. Попробуйте позже."}
    parsed = _start_token(update)
    if parsed is None:
        return None
    telegram_id, username, token = parsed
    if not token:
        return {"telegram_id": telegram_id, "state": "missing_token", "reply": "Откройте ссылку регистрации заново.", "reply_markup": RECOVERY_MENU}
    try:
        result = await confirm_registration(
            connection, settings, token=token, telegram_id=telegram_id, telegram_username=username
        )
    except (ApiError, asyncpg.PostgresError, OSError) as error:  # bounded reply, no crash loop
        code = getattr(error, "code", type(error).__name__)
        return {"telegram_id": telegram_id, "state": str(code), "reply": "Ссылка регистрации недействительна.", "reply_markup": RECOVERY_MENU}
    return {"telegram_id": telegram_id, "state": "registered", "result": result, "reply": _reply_text(result), "reply_markup": RECOVERY_MENU}


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
        self._public_code = PublicRecoveryCode(settings)

    async def handle_once(self) -> list[dict[str, Any]]:
        updates = await self._transport.get_updates(self._offset, self._poll_timeout)
        handled: list[dict[str, Any]] = []
        for update in updates:
            update_id = update.get("update_id")
            if type(update_id) is int:
                self._offset = max(self._offset, update_id + 1)
            parsed = _start_token(update)
            if _recovery_chat(update, self._settings) is not None or (parsed is not None and not parsed[2]):
                outcome = await handle_update(None, self._settings, update, public_code=self._public_code)
            elif parsed is not None:
                try:
                    if not self._settings.database_url or not await self._database.ensure_ready():
                        raise DatabaseUnavailable("registration database unavailable")
                    async with self._database.acquire() as connection:
                        outcome = await handle_update(connection, self._settings, update)
                except (DatabaseUnavailable, asyncpg.PostgresError, OSError):
                    outcome = {"telegram_id": parsed[0], "state": "registration_unavailable",
                               "reply": "Регистрация сейчас недоступна. Попробуйте позже.",
                               "reply_markup": RECOVERY_MENU}
            else:
                outcome = None
            if outcome is not None:
                handled.append(outcome)
        for outcome in handled:
            if outcome.get("reply"):
                try:
                    if outcome.get("reply_markup"):
                        await self._transport.send_message(outcome["telegram_id"], outcome["reply"],
                                                           reply_markup=outcome["reply_markup"])
                    else:
                        await self._transport.send_message(outcome["telegram_id"], outcome["reply"])
                    if outcome.get("instruction"):
                        await self._transport.send_message(outcome["telegram_id"], outcome["instruction"])
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
    # Lazy registration acquisition: recovery remains available while application DB is down.
    transport = AiohttpTelegramTransport(settings.telegram_bot_token)
    runner = RegistrationBotRunner(settings, database, transport)
    try:
        await runner.run_forever()
    finally:
        await transport.close()
        await database.close()


def main() -> None:
    settings = load_settings(require_database=False)
    if not bot_ready(settings):
        raise SystemExit(
            "product bot not configured: set TELEGRAM_REGISTRATION_BOT_TOKEN"
        )
    logging.basicConfig(level=settings.log_level)
    try:
        asyncio.run(_amain(settings))
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
