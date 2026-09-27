"""Devices GET/DELETE conformance against the canonical closed schemas.

The only additive contract change required by this feature is the two error codes
DEVICE_REMOVED and DEVICE_MANAGEMENT_FORBIDDEN; the explicit errors.json diff is applied to the
in-memory registry here (the canonical repository is never modified or pushed from tests).
"""
from __future__ import annotations

import json
import os
import uuid
from pathlib import Path

import pytest
from jsonschema import Draft202012Validator
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012
from test_auth_flow import KeyMaterial, PoPClient, _challenge, _enroll
from test_auth_flow import _session as _auth_session
from test_s5_device_removal import (
    DEVICES,
    LINK,
    _add_device,
    _app,
    _bound_session,
    _connect,
    _grant,
    _unbound_session,
    confirm_registration,
)

from terlimo_backend.api import create_app  # noqa: F401 - imported for parity with app helpers
from terlimo_backend.auth_api import ApiError
from terlimo_backend.telegram_binding import CONFIRM_PATH

CONTRACT_DIR = Path(os.environ.get("TERLIMO_CONTRACT_DIR", "/home/pavel/projects/terlimo-s1-contracts"))
NEW_ERROR_CODES = {"DEVICE_REMOVED", "DEVICE_MANAGEMENT_FORBIDDEN"}

if not (CONTRACT_DIR / "schemas" / "devices.json").exists():
    pytest.skip("accepted contract schemas not available (set TERLIMO_CONTRACT_DIR)", allow_module_level=True)


def _load(path: Path) -> dict:
    return json.loads(path.read_text(encoding="utf-8"))


def _patched(schema: dict) -> dict:
    if schema.get("$id", "").endswith("/errors.json"):
        enum = schema["$defs"]["ErrorCode"]["enum"]
        for code in NEW_ERROR_CODES:
            if code not in enum:
                enum.append(code)
    return schema


def _registry() -> Registry:
    resources = []
    for path in sorted((CONTRACT_DIR / "schemas").glob("*.json")):
        schema = _patched(_load(path))
        resources.append(
            (schema["$id"], Resource.from_contents(schema, default_specification=DRAFT202012))
        )
    return Registry().with_resources(resources)


REGISTRY = _registry()


def _validator(schema_file: str, def_name: str) -> Draft202012Validator:
    document = _patched(_load(CONTRACT_DIR / "schemas" / schema_file))
    wrapper = {
        "$schema": document.get("$schema"),
        "$id": document["$id"],
        "$ref": f"#/$defs/{def_name}",
        "$defs": document["$defs"],
    }
    return Draft202012Validator(wrapper, registry=REGISTRY)


def _assert_valid(body: dict, schema_file: str, def_name: str) -> None:
    errors = sorted(_validator(schema_file, def_name).iter_errors(body), key=lambda e: e.path)
    assert not errors, [f"{list(error.path)}: {error.message}" for error in errors]


async def test_get_devices_matches_canonical_schema(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        token, account_id, _iid, _bid = await _bound_session(client, migrated_url, telegram_id=991450)
        other = await _add_device(c, account_id)
        await _grant(c, other)
        response = await client.get(DEVICES, headers={"Authorization": f"Bearer {token}"})
        assert response.status == 200
        _assert_valid(await response.json(), "devices.json", "DevicesResponse")
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_get_devices_link_proof_matches_canonical_schema(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(
        settings_factory, migrated_url, telegram_bot_username="bot", telegram_bot_key="key"
    )
    c = await _connect(migrated_url)
    try:
        settings = settings_factory(
            migrated_url, telegram_bot_username="bot", telegram_bot_key="key",
        )
        account_id = await c.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified',991451) RETURNING id"
        )
        await c.execute(
            "INSERT INTO entitlements (account_id, kind, status, ends_at, device_limit) VALUES ($1,'paid','active', now()+interval '30 days', 1)",
            account_id,
        )
        await _add_device(c, account_id)
        tok_c, _iid_c, _key_c, _pop_c = await _unbound_session(client, migrated_url)
        link = await client.post(LINK, headers={"Authorization": f"Bearer {tok_c}"})
        raw = (await link.json())["registration"]["token"]
        with pytest.raises(ApiError) as limit_error:
            await confirm_registration(c, settings, token=raw, telegram_id=991451, telegram_username="u")
        assert limit_error.value.code == "DEVICE_LIMIT_REACHED"
        response = await client.get(DEVICES, headers={"Authorization": f"Bearer {tok_c}"})
        assert response.status == 200
        _assert_valid(await response.json(), "devices.json", "DevicesResponse")
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_delete_responses_match_canonical_schema(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(settings_factory, migrated_url)
    c = await _connect(migrated_url)
    try:
        token, account_id, _iid, _bid = await _bound_session(client, migrated_url, telegram_id=991452)
        with_grant = await _add_device(c, account_id)
        await _grant(c, with_grant)
        no_grant = await _add_device(c, account_id)
        headers = {"Authorization": f"Bearer {token}"}

        applied = await client.delete(f"{DEVICES}/{with_grant}", headers=headers)
        assert applied.status == 200
        _assert_valid(await applied.json(), "devices.json", "DeviceDeleteResponse")
        replay = await client.delete(f"{DEVICES}/{with_grant}", headers=headers)
        assert replay.status == 200
        _assert_valid(await replay.json(), "devices.json", "DeviceDeleteResponse")
        clean = await client.delete(f"{DEVICES}/{no_grant}", headers=headers)
        assert clean.status == 200
        _assert_valid(await clean.json(), "devices.json", "DeviceDeleteResponse")
    finally:
        await database.close()
        await client.close()
        await c.close()


async def test_error_envelopes_match_canonical_schema(migrated_url, settings_factory):
    client, _settings_obj, database = await _app(
        settings_factory, migrated_url, telegram_bot_username="bot", telegram_bot_key="key"
    )
    c = await _connect(migrated_url)
    try:
        # no history at all -> DEVICE_MANAGEMENT_FORBIDDEN
        tok_clean, _iid_clean, _key_clean, _pop_clean = await _unbound_session(client, migrated_url)
        forbidden = await client.get(DEVICES, headers={"Authorization": f"Bearer {tok_clean}"})
        assert forbidden.status == 403
        forbidden_body = await forbidden.json()
        assert forbidden_body["code"] == "DEVICE_MANAGEMENT_FORBIDDEN"
        _assert_valid(forbidden_body, "errors.json", "ErrorResponse")

        # removed installation -> DEVICE_REMOVED (same installation, fresh technical session)
        key = KeyMaterial()
        pop = PoPClient(key)
        assert (await _enroll(client, pop, await _challenge(client, key, "enrollment"))).status == 200
        iid = await c.fetchval(
            "SELECT id FROM installations WHERE public_key_fingerprint=$1", pop.key.fingerprint
        )
        account_id = await c.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified',991453) RETURNING id"
        )
        bid = await c.fetchval(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active') RETURNING id",
            account_id, iid,
        )
        token = (
            await (
                await _auth_session(
                    client, pop, await _challenge(client, key, "session"),
                    ["session:read", "session:write"], f"conf-{uuid.uuid4().hex}",
                )
            ).json()
        )["session"]["session_id"]
        assert (await client.delete(f"{DEVICES}/{bid}", headers={"Authorization": f"Bearer {token}"})).status == 200
        fresh = await _auth_session(
            client, pop, await _challenge(client, key, "session"),
            ["session:read", "session:write"], f"conf2-{uuid.uuid4().hex}",
        )
        fresh_token = (await fresh.json())["session"]["session_id"]
        removed = await client.get(DEVICES, headers={"Authorization": f"Bearer {fresh_token}"})
        assert removed.status == 403
        removed_body = await removed.json()
        assert removed_body["code"] == "DEVICE_REMOVED"
        _assert_valid(removed_body, "errors.json", "ErrorResponse")

        # full limit -> DEVICE_LIMIT_REACHED on the internal Telegram confirm wire
        account = await c.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified',991454) RETURNING id"
        )
        await c.execute(
            "INSERT INTO entitlements (account_id, kind, status, ends_at, device_limit) VALUES ($1,'paid','active', now()+interval '30 days', 1)",
            account,
        )
        await _add_device(c, account)
        tok_l, _iid_l, _key_l, _pop_l = await _unbound_session(client, migrated_url)
        link = await client.post(LINK, headers={"Authorization": f"Bearer {tok_l}"})
        raw = (await link.json())["registration"]["token"]
        confirm = await client.post(
            CONFIRM_PATH,
            json={"token": raw, "telegram_id": 991454},
            headers={"X-Telegram-Bot-Key": "key"},
        )
        assert confirm.status == 409
        confirm_body = await confirm.json()
        assert confirm_body["code"] == "DEVICE_LIMIT_REACHED"
        _assert_valid(confirm_body, "errors.json", "ErrorResponse")
    finally:
        await database.close()
        await client.close()
        await c.close()
