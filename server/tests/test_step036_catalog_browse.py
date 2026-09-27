"""Browse catalog before access (owner doc 19, control404) — targeted checks.

Reuses the accepted catalog test helpers; synthetic fixtures only.
"""
from __future__ import annotations

import json
import os
from pathlib import Path

import pytest
from aiohttp.test_utils import TestClient, TestServer
from test_catalog_access_sync import (
    MOBILE,
    _add_applied_grant,
    _add_gateway,
    _auth,
    _connect,
    _counts,
    _seed_subject,
)

from terlimo_backend.api import create_app
from terlimo_backend.db import Database

FIXTURES = Path(os.path.dirname(__file__)) / "fixtures"


@pytest.fixture
async def catalog_env(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, gateway_local_admin_enabled=True)
    database = Database(settings)
    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    try:
        yield client, migrated_url, settings, database
    finally:
        await client.close()
BROWSE_FIXTURE = json.loads((FIXTURES / "step036_catalog_browse.json").read_text())
CREDENTIAL_FIXTURE = json.loads((FIXTURES / "step036_catalog_credential.json").read_text())
INTENT_FIXTURE = json.loads((FIXTURES / "step036_intent_payload_gateway_key.json").read_text())


async def _browse_body(client, token):
    response = await client.get(f"{MOBILE}/gateways", headers=_auth(token))
    assert response.status == 200, await response.text()
    return await response.json()


async def test_browse_for_unlinked_and_linked_without_subscription(catalog_env):
    client, database_url, _settings, _database = catalog_env
    await _add_gateway(database_url, "gw-alpha")
    await _add_gateway(database_url, "gw-beta", capabilities=[])
    await _add_gateway(database_url, "gw-gamma", overrides={"region": "eu", "country_code": "DE"})

    management = await _seed_subject(database_url, management_only=True)
    no_subscription = await _seed_subject(database_url, entitlement_ends_in=None)
    before = await _counts(database_url)

    for subject in (management, no_subscription):
        body = await _browse_body(client, subject["token"])
        assert body["schema_version"] == "1.0" == BROWSE_FIXTURE["schema_version"]
        assert body["status"] == "ok"
        assert body["catalog_mode"] == "browse"
        assert "revision" not in body
        assert set(body) == set(BROWSE_FIXTURE) - {"synthetic"}
        for gateway in body["gateways"]:
            assert set(gateway) <= {"gateway_id", "name", "region", "country_code"}
            assert "access" not in gateway and "transport" not in gateway
        assert [g["gateway_id"] for g in body["gateways"]] == ["gw-alpha", "gw-beta", "gw-gamma"]
        assert body["gateways"][2]["region"] == "eu" and body["gateways"][2]["country_code"] == "DE"

    after = await _counts(database_url)
    assert before == after


async def test_browse_empty_registry_is_real_success(catalog_env):
    client, database_url, _settings, _database = catalog_env
    subject = await _seed_subject(database_url, management_only=True)
    body = await _browse_body(client, subject["token"])
    assert body["catalog_mode"] == "browse" and body["gateways"] == []


async def test_browse_keeps_auth_guards(catalog_env):
    from test_catalog_access_sync import _connect

    client, database_url, _settings, _database = catalog_env
    await _add_gateway(database_url, "gw-alpha")

    revoked_session = await _seed_subject(database_url, management_only=True)
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE sessions SET revoked_at = now() WHERE installation_id = $1",
            revoked_session["installation_id"],
        )
    finally:
        await connection.close()
    response = await client.get(f"{MOBILE}/gateways", headers=_auth(revoked_session["token"]))
    assert response.status == 401

    revoked_installation = await _seed_subject(database_url, management_only=True)
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE installations SET state = 'revoked' WHERE id = $1",
            revoked_installation["installation_id"],
        )
    finally:
        await connection.close()
    response = await client.get(f"{MOBILE}/gateways", headers=_auth(revoked_installation["token"]))
    assert response.status == 403


async def test_credential_catalog_for_all_active_access_kinds(catalog_env):
    client, database_url, _settings, _database = catalog_env
    gateway_id = await _add_gateway(database_url, "gw-alpha")
    paid = await _seed_subject(database_url)
    await _add_applied_grant(database_url, paid, gateway_id)
    trial = await _seed_subject(database_url)
    await _add_applied_grant(database_url, trial, gateway_id)
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE entitlements SET kind = 'trial' WHERE id = $1", trial["entitlement_id"]
        )
    finally:
        await connection.close()

    for subject in (paid, trial):
        response = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert response.status == 200, await response.text()
        body = await response.json()
        assert "catalog_mode" not in body and "revision" in body
        assert body["schema_version"] == CREDENTIAL_FIXTURE["wire_schema_version"]
        assert set(body) == set(CREDENTIAL_FIXTURE["top_level_keys"])
        gateway = body["gateways"][0]
        assert set(gateway) <= set(CREDENTIAL_FIXTURE["gateway_keys"])
        assert set(CREDENTIAL_FIXTURE["transport_keys"]) <= set(gateway["transport"])
        assert set(CREDENTIAL_FIXTURE["access_keys"]) == set(gateway["access"])


def test_intent_payload_fixture_shapes():
    from terlimo_backend import pop as backend_pop

    with_key = INTENT_FIXTURE["with_gateway_key"]
    legacy = INTENT_FIXTURE["legacy_without_gateway_key"]
    assert with_key["gateway_key"] == "gw-beta"
    assert "gateway_key" not in legacy
    assert set(legacy) == set(with_key) - {"gateway_key"}
    for payload in (with_key, legacy):
        assert set(payload) <= set(backend_pop.KNOWN_TOP_LEVEL)
        assert payload["scope"] in backend_pop.PER_SCOPE_FIELDS
        for required in backend_pop.REQUIRED_ACTION_FIELDS[payload["scope"]]:
            assert required in payload
        backend_pop.canonical_json(payload)
