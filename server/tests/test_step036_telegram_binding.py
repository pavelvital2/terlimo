"""S3-A Telegram registration binding: one-time link, bot confirmation, /me eligibility.

Real HTTP (aiohttp TestClient) with real enrollment/challenge/session and the bundled TEST
PostgreSQL. Registration creates no trial/entitlement and never trusts client identity.
"""
from __future__ import annotations

import secrets

from aiohttp.test_utils import TestClient, TestServer
from test_auth_flow import MOBILE, KeyMaterial, PoPClient, _challenge, _enroll, _session

from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.telegram_binding import BOT_KEY_HEADER, CONFIRM_PATH, LINK_PATH

BOT_KEY = "test-bot-shared-key"
BOT_USERNAME = "terlimo_reg_bot"


async def _connect(migrated_url: str):
    import asyncpg

    connection = await asyncpg.connect(migrated_url, timeout=10)
    await connection.set_type_codec(
        "jsonb", schema="pg_catalog", encoder=__import__("json").dumps, decoder=__import__("json").loads
    )
    return connection


async def _session_token(client) -> tuple[PoPClient, str]:
    key = KeyMaterial()
    pop_client = PoPClient(key)
    enrollment = await _enroll(client, pop_client, await _challenge(client, key, "enrollment"))
    assert enrollment.status == 200, await enrollment.text()
    session = await _session(
        client,
        pop_client,
        await _challenge(client, key, "session"),
        ["session:read", "session:write"],
        f"idem-{secrets.token_hex(8)}",
    )
    assert session.status == 200, await session.text()
    return pop_client, (await session.json())["session"]["session_id"]


async def _installation_id(migrated_url: str, fingerprint: str):
    connection = await _connect(migrated_url)
    try:
        return await connection.fetchval(
            "SELECT id FROM installations WHERE public_key_fingerprint = $1", fingerprint
        )
    finally:
        await connection.close()


async def _seed_hour(migrated_url: str, installation_id, *, active: bool) -> None:
    connection = await _connect(migrated_url)
    try:
        ends = "now() + interval '30 minutes'" if active else "now() - interval '1 minute'"
        await connection.execute(
            f"""
            INSERT INTO entitlements (account_id, installation_id, kind, status, starts_at, ends_at,
                                      device_limit, revision)
            VALUES (NULL, $1, 'onboarding_hour', 'active', now() - interval '1 minute',
                    {ends}, 1, 1)
            """,
            installation_id,
        )
    finally:
        await connection.close()


async def _confirm(client, token: str, telegram_id: int, key: str = BOT_KEY):
    return await client.post(
        CONFIRM_PATH,
        json={"token": token, "telegram_id": telegram_id},
        headers={BOT_KEY_HEADER: key},
    )


def _settings(settings_factory, url):
    return settings_factory(
        url,
        telegram_bot_username=BOT_USERNAME,
        telegram_bot_key=BOT_KEY,
        registration_token_ttl_seconds=600,
    )


async def _app(migrated_url, settings_factory):
    settings = _settings(settings_factory, migrated_url)
    database = Database(settings)
    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    return client, settings, database


async def test_registration_within_hour_marks_future_trial_right(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(migrated_url, settings_factory)
    try:
        pop_client, session_token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop_client.key.fingerprint)
        await _seed_hour(migrated_url, installation_id, active=True)

        link = await client.post(
            LINK_PATH, headers={"Authorization": f"Bearer {session_token}"}, json={}
        )
        assert link.status == 200, await link.text()
        link_body = await link.json()
        assert set(link_body) == {"request_id", "server_time", "schema_version", "status", "registration"}
        assert link_body["schema_version"] == "1.0"
        assert link_body["status"] == "ok"
        assert link_body["request_id"]
        registration = link_body["registration"]
        assert set(registration) == {
            "state", "token", "bot_username", "deep_link", "expires_at", "expires_in"
        }
        assert registration["state"] == "pending"
        token = registration["token"]
        assert registration["bot_username"] == BOT_USERNAME
        assert registration["deep_link"].endswith(f"?start={token}")

        confirmed = await _confirm(client, token, 555000111)
        assert confirmed.status == 200, await confirmed.text()
        body = await confirmed.json()
        assert body["registration_state"] == "registered"
        assert body["telegram_id"] == 555000111
        assert body["trial_available"] is True
        assert body["trial_reason"] == "within_hour_no_prior_trial"
        assert body["purchase_available"] is True

        # One-time replay is idempotent; a different Telegram id conflicts.
        replay = await _confirm(client, token, 555000111)
        assert replay.status == 200 and (await replay.json())["telegram_id"] == 555000111
        conflict = await _confirm(client, token, 555000222)
        assert conflict.status == 409

        # Registration creates no trial/entitlement; /me reports it and computed eligibility.
        connection = await _connect(migrated_url)
        try:
            assert await connection.fetchval(
                "SELECT count(*) FROM entitlements WHERE kind = 'trial'"
            ) == 0
        finally:
            await connection.close()
        me = await client.get(f"{MOBILE}/me", headers={"Authorization": f"Bearer {session_token}"})
        assert me.status == 200, await me.text()
        registration = (await me.json())["registration"]
        assert registration["state"] == "registered"
        assert registration["telegram_id"] == 555000111
        assert registration["within_hour"] is True
        assert registration["trial_available"] is True
        assert registration["trial_reason"] == "within_hour_no_prior_trial"
    finally:
        await database.close()
        await client.close()


async def test_registration_after_hour_is_not_trial_eligible(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(migrated_url, settings_factory)
    try:
        pop_client, session_token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop_client.key.fingerprint)
        await _seed_hour(migrated_url, installation_id, active=False)  # hour already ended
        link = await client.post(
            LINK_PATH, headers={"Authorization": f"Bearer {session_token}"}, json={}
        )
        token = (await link.json())["registration"]["token"]
        body = await (await _confirm(client, token, 555000333)).json()
        assert body["trial_available"] is False
        assert body["trial_reason"] == "hour_expired"
        assert body["purchase_available"] is True
        me = await client.get(f"{MOBILE}/me", headers={"Authorization": f"Bearer {session_token}"})
        registration = (await me.json())["registration"]
        assert registration["trial_available"] is False
        assert registration["trial_reason"] == "hour_expired"
    finally:
        await database.close()
        await client.close()


async def test_registration_previous_trial_is_not_eligible(migrated_url, settings_factory):
    connection = await _connect(migrated_url)
    try:
        account_id = await connection.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified', 555000444) RETURNING id"
        )
        await connection.execute(
            """
            INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at, device_limit, revision)
            VALUES ($1, 'trial', 'expired', now() - interval '10 days', now() - interval '3 days', 1, 2)
            """,
            account_id,
        )
    finally:
        await connection.close()
    client, _settings_obj, database = await _app(migrated_url, settings_factory)
    try:
        pop_client, session_token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop_client.key.fingerprint)
        await _seed_hour(migrated_url, installation_id, active=True)
        link = await client.post(
            LINK_PATH, headers={"Authorization": f"Bearer {session_token}"}, json={}
        )
        token = (await link.json())["registration"]["token"]
        body = await (await _confirm(client, token, 555000444)).json()
        assert body["trial_available"] is False
        assert body["trial_reason"] == "trial_already_used"
        assert body["purchase_available"] is True
    finally:
        await database.close()
        await client.close()


async def test_registration_token_is_installation_bound_and_guarded(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(migrated_url, settings_factory)
    try:
        pop_client_a, token_a = await _session_token(client)
        pop_client_b, token_b = await _session_token(client)
        installation_a = await _installation_id(migrated_url, pop_client_a.key.fingerprint)
        installation_b = await _installation_id(migrated_url, pop_client_b.key.fingerprint)
        await _seed_hour(migrated_url, installation_a, active=True)
        await _seed_hour(migrated_url, installation_b, active=True)

        link_a = await client.post(LINK_PATH, headers={"Authorization": f"Bearer {token_a}"}, json={})
        raw_a = (await link_a.json())["registration"]["token"]
        link_b = await client.post(LINK_PATH, headers={"Authorization": f"Bearer {token_b}"}, json={})
        raw_b = (await link_b.json())["registration"]["token"]

        # Bad bot key and unknown token are rejected before any binding.
        assert (await _confirm(client, raw_a, 555000555, key="wrong")).status == 403
        assert (await client.post(CONFIRM_PATH, json={"token": "unknown-token-1234", "telegram_id": 1}, headers={BOT_KEY_HEADER: BOT_KEY})).status == 404

        # Each token binds exactly its own installation.
        assert (await _confirm(client, raw_a, 555000555)).status == 200
        assert (await _confirm(client, raw_b, 555000666)).status == 200
        connection = await _connect(migrated_url)
        try:
            binding_a = await connection.fetchval(
                "SELECT account_id FROM account_bindings WHERE installation_id = $1 AND status='active'",
                installation_a,
            )
            binding_b = await connection.fetchval(
                "SELECT account_id FROM account_bindings WHERE installation_id = $1 AND status='active'",
                installation_b,
            )
            tg_a = await connection.fetchval("SELECT telegram_id FROM accounts WHERE id = $1", binding_a)
            tg_b = await connection.fetchval("SELECT telegram_id FROM accounts WHERE id = $1", binding_b)
            assert (tg_a, tg_b) == (555000555, 555000666)
        finally:
            await connection.close()

        # Revoked installation cannot be confirmed.
        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                "UPDATE installations SET state = 'revoked' WHERE id = $1", installation_b
            )
        finally:
            await connection.close()
        # A different Telegram id on an already-confirmed token is a conflict, and a revoked
        # installation cannot even request a fresh link.
        assert (await _confirm(client, raw_b, 555000777)).status == 409
        revoked_link = await client.post(LINK_PATH, headers={"Authorization": f"Bearer {token_b}"}, json={})
        assert revoked_link.status == 403
    finally:
        await database.close()
        await client.close()


async def test_registration_link_response_envelope_pending_and_registered(migrated_url, settings_factory):
    """Contract: mobile-v1 envelope with nested registration; strict exact key sets.

    Fixture uses a dummy token/deep link only (never a live secret).
    """
    import json
    import re
    from datetime import datetime
    from pathlib import Path

    fixture = json.loads(
        (Path(__file__).parent / "fixtures" / "step036_registration_link_envelope.json").read_text()
    )
    envelope_keys = {"request_id", "server_time", "schema_version", "status", "registration"}
    pending_keys = {"state", "token", "bot_username", "deep_link", "expires_at", "expires_in"}
    registered_keys = {"state"}
    assert set(fixture["pending"]) == envelope_keys
    assert set(fixture["pending"]["registration"]) == pending_keys
    assert set(fixture["registered"]) == envelope_keys
    assert set(fixture["registered"]["registration"]) == registered_keys
    assert fixture["pending"]["registration"]["deep_link"].startswith("https://t.me/")

    client, _settings_obj, database = await _app(migrated_url, settings_factory)
    try:
        pop_client, session_token = await _session_token(client)
        installation_id = await _installation_id(migrated_url, pop_client.key.fingerprint)
        await _seed_hour(migrated_url, installation_id, active=True)

        headers = {"Authorization": f"Bearer {session_token}"}
        pending_body = await (await client.post(LINK_PATH, headers=headers, json={})).json()
        assert set(pending_body) == envelope_keys
        assert pending_body["schema_version"] == "1.0"
        assert pending_body["status"] == "ok"
        assert pending_body["request_id"]
        assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", pending_body["server_time"])
        datetime.strptime(pending_body["server_time"], "%Y-%m-%dT%H:%M:%SZ").replace()
        assert set(pending_body["registration"]) == pending_keys
        assert pending_body["registration"]["state"] == "pending"

        token = pending_body["registration"]["token"]
        assert (await _confirm(client, token, 555000111)).status == 200

        registered_body = await (await client.post(LINK_PATH, headers=headers, json={})).json()
        assert set(registered_body) == envelope_keys
        assert set(registered_body["registration"]) == registered_keys
        assert registered_body["registration"] == {"state": "registered"}
    finally:
        await database.close()
        await client.close()
