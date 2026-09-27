"""S3-B trial activation: server-side eligibility, one 7-day interval, replay, paid priority."""
from __future__ import annotations

import hashlib
import json
import secrets
from datetime import UTC, datetime, timedelta

import asyncpg
from aiohttp.test_utils import TestClient, TestServer
from test_auth_flow import MOBILE, KeyMaterial, PoPClient, _challenge, _enroll, _session

from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.trial_activation import ACTIVATE_PATH, TRIAL_CHANNEL_KEY

BOT_KEY = "trial-test-key"
BOT_USERNAME = "trial_test_bot"


class FakeChecker:
    def __init__(self, member: bool = True) -> None:
        self.member = member
        self.calls = 0

    async def is_member(self, telegram_id: int) -> bool:
        self.calls += 1
        return self.member


async def _connect(url: str) -> asyncpg.Connection:
    connection = await asyncpg.connect(url, timeout=10)
    await connection.set_type_codec("jsonb", schema="pg_catalog", encoder=json.dumps, decoder=json.loads)
    return connection


def _settings(settings_factory, url):
    return settings_factory(
        url,
        telegram_bot_username=BOT_USERNAME,
        telegram_bot_key=BOT_KEY,
        telegram_bot_token="123:token",
        telegram_trial_channel_id="@terlimo_news",
    )


async def _app(migrated_url, settings_factory, member=True):
    settings = _settings(settings_factory, migrated_url)
    database = Database(settings)
    app = create_app(settings, database)
    checker = FakeChecker(member)
    app[TRIAL_CHANNEL_KEY] = checker
    client = TestClient(TestServer(app))
    await client.start_server()
    return client, settings, database, checker


async def _session_token(client) -> tuple[PoPClient, str]:
    key = KeyMaterial()
    pop_client = PoPClient(key)
    enrollment = await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))
    assert enrollment.status == 200, await enrollment.text()
    session = await _session(
        client, pop_client, await _challenge(client, key, "session"),
        ["session:read", "session:write"], f"idem-{secrets.token_hex(8)}",
    )
    assert session.status == 200, await session.text()
    return pop_client, (await session.json())["session"]["session_id"]


async def _register(
    migrated_url: str, fingerprint: str, telegram_id: int, *,
    within_hour: bool, existing_trial: str | None = None, paid: bool = False,
):
    connection = await _connect(migrated_url)
    try:
        installation_id = await connection.fetchval(
            "SELECT id FROM installations WHERE public_key_fingerprint = $1", fingerprint
        )
        account_id = await connection.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified', $1) RETURNING id", telegram_id
        )
        await connection.execute(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1, $2, 'active')",
            account_id, installation_id,
        )
        reason = "within_hour_no_prior_trial" if within_hour else "hour_expired"
        await connection.execute(
            """
            INSERT INTO registration_links
                (token_sha256, installation_id, environment, status, expires_at, confirmed_at,
                 telegram_id, within_hour, trial_available, trial_reason)
            VALUES ($1, $2, 'test', 'confirmed', now() + interval '10 minutes', now(),
                    $3, $4, $4, $5)
            """,
            hashlib.sha256(secrets.token_bytes(16)).hexdigest(), installation_id, telegram_id,
            within_hour, reason,
        )
        if existing_trial is not None:
            starts = datetime.now(UTC) + (timedelta(days=-10) if existing_trial == "expired" else timedelta(0))
            ends = starts + timedelta(days=7)
            await connection.execute(
                """
                INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at, device_limit, revision)
                VALUES ($1, 'trial', $2, $3, $4, 2, 1)
                """,
                account_id, existing_trial, starts, ends,
            )
        if paid:
            await connection.execute(
                """
                INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at, device_limit, revision)
                VALUES ($1, 'paid', 'active', now() - interval '1 day', now() + interval '30 days', 2, 1)
                """,
                account_id,
            )
        return account_id, installation_id
    finally:
        await connection.close()


async def _activate(client, token):
    return await client.post(ACTIVATE_PATH, headers={"Authorization": f"Bearer {token}"}, json={})


async def test_unregistered_and_late_registered_are_rejected(migrated_url, settings_factory):
    client, _settings_obj, database, checker = await _app(migrated_url, settings_factory)
    try:
        _pop, token = await _session_token(client)
        body = await (await _activate(client, token)).json()
        assert body["code"] == "REGISTRATION_REQUIRED"
        assert checker.calls == 0
    finally:
        await database.close()
        await client.close()


async def test_timely_registration_after_hour_activates_exactly_seven_days(migrated_url, settings_factory):
    client, _settings_obj, database, checker = await _app(migrated_url, settings_factory)
    try:
        pop_client, token = await _session_token(client)
        await _register(migrated_url, pop_client.key.fingerprint, 700001, within_hour=True)
        first = await _activate(client, token)
        assert first.status == 200, await first.text()
        payload = (await first.json())["trial"]
        assert payload["state"] == "active" and payload["replay"] is False
        starts = datetime.fromisoformat(payload["starts_at"])
        ends = datetime.fromisoformat(payload["ends_at"])
        assert ends - starts == timedelta(days=7)
        assert checker.calls == 1

        # Retry/replay returns the original interval, never a new one.
        replay = await _activate(client, token)
        assert replay.status == 200
        replay_payload = (await replay.json())["trial"]
        assert replay_payload["replay"] is True
        assert replay_payload["starts_at"] == payload["starts_at"]
        assert replay_payload["ends_at"] == payload["ends_at"]
        connection = await _connect(migrated_url)
        try:
            assert await connection.fetchval("SELECT count(*) FROM entitlements WHERE kind='trial'") == 1
        finally:
            await connection.close()

        me = await client.get(f"{MOBILE}/me", headers={"Authorization": f"Bearer {token}"})
        trial = (await me.json())["trial"]
        assert trial["state"] == "active"
        assert trial["can_activate"] is False
    finally:
        await database.close()
        await client.close()


async def test_hour_expired_before_registration_and_missing_channel(migrated_url, settings_factory):
    client, _settings_obj, database, checker = await _app(migrated_url, settings_factory, member=True)
    try:
        pop_a, token_a = await _session_token(client)
        await _register(migrated_url, pop_a.key.fingerprint, 700002, within_hour=False)
        body = await (await _activate(client, token_a)).json()
        assert body["code"] == "TRIAL_NOT_ELIGIBLE"
        assert body["details"]["reason"] == "hour_expired_before_registration"

        pop_b, token_b = await _session_token(client)
        await _register(migrated_url, pop_b.key.fingerprint, 700003, within_hour=True)
        checker.member = False
        body_b = await (await _activate(client, token_b)).json()
        assert body_b["code"] == "CHANNEL_MEMBERSHIP_REQUIRED"
        connection = await _connect(migrated_url)
        try:
            assert await connection.fetchval("SELECT count(*) FROM entitlements WHERE kind='trial'") == 0
        finally:
            await connection.close()
    finally:
        await database.close()
        await client.close()


async def test_paid_priority_and_prior_trial_replay(migrated_url, settings_factory):
    client, _settings_obj, database, _checker = await _app(migrated_url, settings_factory)
    try:
        pop_paid, token_paid = await _session_token(client)
        await _register(migrated_url, pop_paid.key.fingerprint, 700004, within_hour=True, paid=True)
        body = await (await _activate(client, token_paid)).json()
        assert body["code"] == "SUBSCRIPTION_ACTIVE"

        pop_old, token_old = await _session_token(client)
        await _register(migrated_url, pop_old.key.fingerprint, 700005, within_hour=True, existing_trial="expired")
        replay = await _activate(client, token_old)
        assert replay.status == 200
        payload = (await replay.json())["trial"]
        assert payload["state"] == "used" and payload["replay"] is True
        me = await client.get(f"{MOBILE}/me", headers={"Authorization": f"Bearer {token_old}"})
        trial = (await me.json())["trial"]
        assert trial["state"] == "used" and trial["can_activate"] is False
    finally:
        await database.close()
        await client.close()


async def test_second_installation_same_account_replays_original_interval(migrated_url, settings_factory):
    client, _settings_obj, database, _checker = await _app(migrated_url, settings_factory)
    try:
        pop_first, token_first = await _session_token(client)
        account_id, _installation_first = await _register(
            migrated_url, pop_first.key.fingerprint, 700006, within_hour=True
        )
        first = (await (await _activate(client, token_first)).json())["trial"]
        assert first["state"] == "active"

        # A second installation bound to the same account/Telegram id gets the same right,
        # never a second interval.
        pop_second, token_second = await _session_token(client)
        connection = await _connect(migrated_url)
        try:
            second_installation = await connection.fetchval(
                "SELECT id FROM installations WHERE public_key_fingerprint = $1",
                pop_second.key.fingerprint,
            )
            await connection.execute(
                "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1, $2, 'active')",
                account_id, second_installation,
            )
            await connection.execute(
                """
                INSERT INTO registration_links
                    (token_sha256, installation_id, environment, status, expires_at, confirmed_at,
                     telegram_id, within_hour, trial_available, trial_reason)
                VALUES ($1, $2, 'test', 'confirmed', now() + interval '10 minutes', now(),
                        700006, true, true, 'within_hour_no_prior_trial')
                """,
                hashlib.sha256(secrets.token_bytes(16)).hexdigest(), second_installation,
            )
        finally:
            await connection.close()
        replay = (await (await _activate(client, token_second)).json())["trial"]
        assert replay["replay"] is True
        assert replay["starts_at"] == first["starts_at"] and replay["ends_at"] == first["ends_at"]
        connection = await _connect(migrated_url)
        try:
            assert await connection.fetchval("SELECT count(*) FROM entitlements WHERE kind='trial'") == 1
        finally:
            await connection.close()
    finally:
        await database.close()
        await client.close()


async def test_missing_channel_credential_is_fail_closed(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, telegram_bot_username=BOT_USERNAME, telegram_bot_key=BOT_KEY)
    database = Database(settings)
    from aiohttp.test_utils import TestClient, TestServer

    from terlimo_backend.api import create_app

    app = create_app(settings, database)  # default real BotApi checker; channel/token unset
    client = TestClient(TestServer(app))
    await client.start_server()
    try:
        pop, token = await _session_token(client)
        await _register(migrated_url, pop.key.fingerprint, 700007, within_hour=True)
        me = await client.get(f"{MOBILE}/me", headers={"Authorization": f"Bearer {token}"})
        trial = (await me.json())["trial"]
        assert trial["state"] == "ineligible"
        assert trial["reason"] == "channel_check_unavailable"
        assert trial["can_activate"] is False
        activated = await _activate(client, token)
        assert activated.status == 503
        assert (await activated.json())["code"] == "TRIAL_CHECK_UNAVAILABLE"
    finally:
        await database.close()
        await client.close()
