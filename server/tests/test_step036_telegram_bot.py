"""S3-A product-bot runner: Telegram update -> confirm -> /me, fake transport, no network."""
from __future__ import annotations

from terlimo_backend.config import Settings
from terlimo_backend.db import Database
from terlimo_backend.telegram_binding import (
    create_registration_link,
    registration_view,
)
from terlimo_backend.telegram_bot import RegistrationBotRunner, handle_update

BOT_KEY = "test-bot-key"
BOT_USERNAME = "terlimo_reg_bot"
BOT_TOKEN = "123456:TEST-TOKEN-NOT-REAL"


class FakeTransport:
    def __init__(self, updates):
        self.updates = list(updates)
        self.sent: list[tuple[int, str]] = []

    async def get_updates(self, offset: int, timeout: int):
        pending = [u for u in self.updates if u.get("update_id", 0) >= offset]
        return pending

    async def send_message(self, chat_id: int, text: str) -> None:
        self.sent.append((chat_id, text))


def _settings(settings_factory, url) -> Settings:
    return settings_factory(
        url,
        telegram_bot_username=BOT_USERNAME,
        telegram_bot_key=BOT_KEY,
        telegram_bot_token=BOT_TOKEN,
        registration_token_ttl_seconds=600,
    )


async def _connect(url: str):
    import json

    import asyncpg

    connection = await asyncpg.connect(url, timeout=10)
    await connection.set_type_codec("jsonb", schema="pg_catalog", encoder=json.dumps, decoder=json.loads)
    return connection


async def _installation(url: str) -> object:
    connection = await _connect(url)
    try:
        return await connection.fetchval(
            """
            INSERT INTO installations (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test', 'android', $1, 'spki', 'technical') RETURNING id
            """,
            __import__("secrets").token_hex(32),
        )
    finally:
        await connection.close()


async def _hour(url: str, installation_id, *, active: bool) -> None:
    connection = await _connect(url)
    try:
        ends = "now() + interval '30 minutes'" if active else "now() - interval '1 minute'"
        await connection.execute(
            f"""
            INSERT INTO entitlements (account_id, installation_id, kind, status, starts_at, ends_at, device_limit, revision)
            VALUES (NULL, $1, 'onboarding_hour', 'active', now() - interval '1 minute', {ends}, 1, 1)
            """,
            installation_id,
        )
    finally:
        await connection.close()


def _update(update_id: int, telegram_id: int, text: str) -> dict:
    return {
        "update_id": update_id,
        "message": {"chat": {"id": telegram_id}, "from": {"username": "u"}, "text": text},
    }


async def _pending_token(migrated_url, settings, installation_id) -> str:
    connection = await _connect(migrated_url)
    try:
        link = await create_registration_link(connection, settings, installation_id=installation_id)
    finally:
        await connection.close()
    return link["token"]


async def test_bot_start_confirms_once_then_replay_is_idempotent(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    installation_id = await _installation(migrated_url)
    await _hour(migrated_url, installation_id, active=True)
    token = await _pending_token(migrated_url, settings, installation_id)

    transport = FakeTransport([
        _update(1, 900001, "/start"),
        _update(2, 900001, f"/start {token}"),
        _update(3, 900001, f"/start {token}"),  # duplicate update replay
    ])
    database = Database(settings)
    await database.connect()
    try:
        runner = RegistrationBotRunner(settings, database, transport)
        handled = await runner.handle_once()
        states = [item["state"] for item in handled]
        assert states == ["missing_token", "registered", "registered"]
        assert transport.sent and transport.sent[-1][0] == 900001
        assert "будущий пробный" in transport.sent[-1][1]

        async with database.acquire() as connection:
            no_dup = await connection.fetchval(
                "SELECT count(*) FROM account_bindings WHERE installation_id = $1 AND status='active'",
                installation_id,
            )
            view = await registration_view(connection, installation_id, settings)
        assert no_dup == 1  # replay must not create a duplicate binding
        assert view["state"] == "registered"
        assert view["telegram_id"] == 900001
        assert view["trial_available"] is True
        assert view["trial_reason"] == "within_hour_no_prior_trial"
    finally:
        await database.close()


async def test_bot_start_after_hour_and_used_trial_eligibility(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    installation_id = await _installation(migrated_url)
    await _hour(migrated_url, installation_id, active=False)
    token = await _pending_token(migrated_url, settings, installation_id)
    connection = await _connect(migrated_url)
    try:
        outcome = await handle_update(connection, settings, _update(10, 900002, f"/start {token}"))
    finally:
        await connection.close()
    assert outcome["result"]["trial_available"] is False
    assert outcome["result"]["trial_reason"] == "hour_expired"
    assert "покупка" in outcome["reply"]

    # A Telegram id with an existing trial history is never eligible again.
    settings2 = _settings(settings_factory, migrated_url)
    installation2 = await _installation(migrated_url)
    await _hour(migrated_url, installation2, active=True)
    connection = await _connect(migrated_url)
    try:
        account_id = await connection.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified', 900003) RETURNING id"
        )
        await connection.execute(
            """
            INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at, device_limit, revision)
            VALUES ($1, 'trial', 'expired', now() - interval '10 days', now() - interval '3 days', 1, 2)
            """,
            account_id,
        )
        token2 = (await create_registration_link(connection, settings2, installation_id=installation2))["token"]
        outcome2 = await handle_update(connection, settings2, _update(11, 900003, f"/start {token2}"))
    finally:
        await connection.close()
    assert outcome2["result"]["trial_available"] is False
    assert outcome2["result"]["trial_reason"] == "trial_already_used"
    assert "уже использован" in outcome2["reply"]
