"""Session auth + GET /me + own operation status over real HTTP and PostgreSQL.

Reads never create trial/hour/grant; foreign operations are neutral 404; accepted is never
reported as applied; MeResponse/OperationResponse are validated against the accepted C01 DTOs.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import secrets
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path

import asyncpg
import pytest
from aiohttp.test_utils import TestClient, TestServer
from jsonschema import Draft202012Validator
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

from terlimo_backend import mobile_account
from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.session_auth import AuthError, authenticate_session

CONTRACT_DIR = Path(os.environ.get("TERLIMO_CONTRACT_DIR", "/home/pavel/projects/terlimo-s1-contracts"))
MOBILE = "/api/mobile/v1"

if not (CONTRACT_DIR / "schemas" / "subscription.json").exists():
    pytest.skip("accepted contract schemas not available (set TERLIMO_CONTRACT_DIR)", allow_module_level=True)


def _load(path: Path) -> dict:
    return json.loads(path.read_text(encoding="utf-8"))


def _registry() -> Registry:
    resources = []
    for path in sorted((CONTRACT_DIR / "schemas").glob("*.json")):
        schema = _load(path)
        resources.append(
            (schema["$id"], Resource.from_contents(schema, default_specification=DRAFT202012))
        )
    return Registry().with_resources(resources)


REGISTRY = _registry()


def _validator(schema_file: str, def_name: str) -> Draft202012Validator:
    document = _load(CONTRACT_DIR / "schemas" / schema_file)
    wrapper = {
        "$schema": document.get("$schema"),
        "$id": document["$id"],
        "$ref": f"#/$defs/{def_name}",
        "$defs": document["$defs"],
    }
    return Draft202012Validator(wrapper, registry=REGISTRY)


async def _connect(database_url: str) -> asyncpg.Connection:
    return await asyncpg.connect(database_url, timeout=10)


async def _count(database_url: str, table: str) -> int:
    connection = await _connect(database_url)
    try:
        return await connection.fetchval(f"SELECT count(*) FROM {table}")
    finally:
        await connection.close()


async def _counts(database_url: str) -> dict[str, int]:
    tables = ("entitlements", "grants", "outbox_operations", "operation_receipts", "auth_challenges", "sessions")
    return {table: await _count(database_url, table) for table in tables}


async def _seed(
    database_url: str,
    *,
    environment: str = "test",
    scopes: tuple[str, ...] = ("session:read",),
    session_expires_in: int = 3600,
    session_revoked: bool = False,
    installation_state: str = "technical",
    with_account: bool = True,
    telegram: bool = True,
    binding_status: str | None = "active",
    entitlement: dict | None = None,
    hour: dict | None = None,
) -> dict:
    connection = await _connect(database_url)
    try:
        account_id = None
        if with_account:
            account_id = await connection.fetchval(
                """
                INSERT INTO accounts (status, telegram_id)
                VALUES ('verified', $1) RETURNING id
                """,
                secrets.randbelow(1 << 40) if telegram else None,
            )
        fingerprint = hashlib.sha256(secrets.token_bytes(32)).hexdigest()
        installation_id = await connection.fetchval(
            """
            INSERT INTO installations
                (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ($1, 'android', $2, 'spki', $3) RETURNING id
            """,
            environment,
            fingerprint,
            installation_state,
        )
        binding_id = None
        if binding_status is not None:
            binding_id = await connection.fetchval(
                """
                INSERT INTO account_bindings (account_id, installation_id, status)
                VALUES ($1, $2, $3) RETURNING id
                """,
                account_id,
                installation_id,
                binding_status,
            )
        entitlement_id = None
        if entitlement is not None:
            ends_at = (
                datetime.now(UTC) + timedelta(seconds=entitlement["ends_in"])
                if entitlement.get("ends_in") is not None
                else None
            )
            starts_at = (
                datetime.now(UTC) + timedelta(seconds=entitlement["starts_in"])
                if entitlement.get("starts_in") is not None
                else datetime.now(UTC)
            )
            entitlement_id = await connection.fetchval(
                """
                INSERT INTO entitlements
                    (account_id, kind, status, starts_at, ends_at, device_limit, revision)
                VALUES ($1, $2, $3, $4, $5, 2, $6) RETURNING id
                """,
                account_id,
                entitlement["kind"],
                entitlement["status"],
                starts_at,
                ends_at,
                entitlement.get("revision", 1),
            )
        hour_id = None
        if hour is not None:
            hour_installation = None if hour.get("ambiguous") else installation_id
            hour_id = await connection.fetchval(
                """
                INSERT INTO entitlements
                    (account_id, installation_id, kind, status, starts_at, ends_at,
                     device_limit, revision)
                VALUES ($1, $2, 'onboarding_hour', $3, now() - interval '30 minutes',
                        now() + make_interval(secs => $4), 2, 1)
                RETURNING id
                """,
                account_id,
                hour_installation,
                hour["status"],
                float(hour["ends_in"]),
            )
        token = secrets.token_urlsafe(32)
        session_id = await connection.fetchval(
            """
            INSERT INTO sessions
                (account_id, installation_id, scopes, generation, expires_at, revoked_at,
                 token_sha256, binding_id, binding_generation)
            VALUES ($1, $2, $3::text[], 1, $4, $5, $6, $7, $8)
            RETURNING id
            """,
            account_id,
            installation_id,
            list(scopes),
            datetime.now(UTC) + timedelta(seconds=session_expires_in),
            datetime.now(UTC) if session_revoked else None,
            hashlib.sha256(token.encode()).hexdigest(),
            binding_id,
            1 if binding_id is not None else None,
        )
        return {
            "token": token,
            "account_id": account_id,
            "installation_id": installation_id,
            "binding_id": binding_id,
            "entitlement_id": entitlement_id,
            "hour_id": hour_id,
            "session_id": session_id,
            "fingerprint": fingerprint,
        }
    finally:
        await connection.close()


@pytest.fixture
async def me_env(migrated_url, settings_factory):
    settings = settings_factory(migrated_url)
    client = TestClient(TestServer(create_app(settings, Database(settings))))
    await client.start_server()
    try:
        yield client, migrated_url, settings
    finally:
        await client.close()


def _auth(token: str) -> dict[str, str]:
    return {"Authorization": f"Bearer {token}"}


async def _get_me(client: TestClient, token: str):
    return await client.get(f"{MOBILE}/me", headers=_auth(token))


async def test_me_active_trial_matches_dto_and_read_creates_nothing(me_env):
    client, database_url, _ = me_env
    seeded = await _seed(
        database_url,
        entitlement={"kind": "trial", "status": "active", "ends_in": 7 * 86400},
    )
    before = await _counts(database_url)
    response = await _get_me(client, seeded["token"])
    assert response.status == 200, await response.text()
    body = await response.json()
    errors = sorted(_validator("subscription.json", "MeResponse").iter_errors(body), key=lambda e: e.path)
    assert not errors, [error.message for error in errors]
    assert body["account_state"] == "ACTIVE_TRIAL"
    assert body["entitlement"]["type"] == "trial"
    assert body["entitlement"]["status"] == "active"
    assert body["entitlement"]["slots_used"] == 1
    assert body["binding_status"] == "active"
    assert body["binding_revision"] == "1"
    assert body["management_only"] is False
    assert body["onboarding"]["state"] == "not_started"
    assert body["grant_resolution"]["data_access"] == "subscription_data"
    assert body["grant_resolution"]["effective_deadline"] == body["entitlement"]["valid_until"]
    assert await _counts(database_url) == before


async def test_me_no_entitlement_and_expired(me_env):
    client, database_url, _ = me_env
    none_seeded = await _seed(database_url)
    body = await (await _get_me(client, none_seeded["token"])).json()
    assert body["account_state"] == "VERIFIED_NO_ENTITLEMENT"
    assert body["entitlement"]["type"] == "none"
    assert body["entitlement"]["status"] == "none"
    assert body["entitlement"]["revision"] == "0"
    assert body["grant_resolution"]["data_access"] == "none"

    expired = await _seed(
        database_url,
        entitlement={"kind": "paid", "status": "active", "ends_in": -3600},
    )
    body = await (await _get_me(client, expired["token"])).json()
    assert body["account_state"] == "EXPIRED"
    assert body["entitlement"]["status"] == "expired"
    assert body["grant_resolution"]["data_access"] == "none"
    assert body["grant_resolution"]["effective_deadline"] is None


async def test_me_management_only_and_hour_visibility(me_env):
    client, database_url, _ = me_env
    management = await _seed(
        database_url,
        scopes=("management-only",),
        with_account=False,
        binding_status=None,
    )
    body = await (await _get_me(client, management["token"])).json()
    assert body["account_state"] == "UNLINKED"
    assert body["binding_revision"] is None
    assert body["management_only"] is True
    assert body["grant_resolution"]["data_access"] == "none"
    assert body["grant_resolution"]["effective_deadline"] is None

    hour_seeded = await _seed(database_url, hour={"status": "active", "ends_in": 1800})
    body = await (await _get_me(client, hour_seeded["token"])).json()
    assert body["onboarding"]["state"] == "active"
    assert body["onboarding"]["duration_seconds"] == 3600
    assert body["onboarding"]["extends_on_refresh"] is False
    assert body["grant_resolution"]["data_access"] == "onboarding_hour"
    assert body["grant_resolution"]["effective_deadline"] == body["onboarding"]["not_after"]


async def test_me_binding_revision_is_real_fence_and_nullable(me_env):
    client, database_url, _ = me_env
    seeded = await _seed(
        database_url,
        entitlement={"kind": "paid", "status": "active", "ends_in": 86400},
    )
    first = await (await _get_me(client, seeded["token"])).json()
    assert first["binding_revision"] == "1"

    # A generation bump is a fence change: the old bearer fails closed, a session issued
    # against the new generation reports the real binding revision.
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await connection.execute(
            "UPDATE account_bindings SET generation = 4 WHERE id = $1", seeded["binding_id"]
        )
    finally:
        await connection.close()
    stale = await _get_me(client, seeded["token"])
    assert stale.status == 401
    assert (await stale.json())["code"] == "SESSION_INVALID"

    token = secrets.token_urlsafe(32)
    connection = await asyncpg.connect(database_url, timeout=10)
    try:
        await connection.execute(
            """
            INSERT INTO sessions
                (account_id, installation_id, scopes, generation, expires_at, token_sha256,
                 binding_id, binding_generation)
            VALUES ($1, $2, ARRAY['session:read'], 1, now() + interval '1 hour', $3, $4, 4)
            """,
            seeded["account_id"],
            seeded["installation_id"],
            hashlib.sha256(token.encode()).hexdigest(),
            seeded["binding_id"],
        )
    finally:
        await connection.close()
    second = await (await _get_me(client, token)).json()
    assert second["binding_revision"] == "4"
    assert int(second["revision"]) > int(first["revision"])
    assert second["binding_revision"] != first["revision"]

    unbound = await _seed(
        database_url,
        scopes=("management-only",),
        with_account=False,
        binding_status=None,
    )
    body = await (await _get_me(client, unbound["token"])).json()
    assert body["binding_revision"] is None
    errors = sorted(_validator("subscription.json", "MeResponse").iter_errors(body), key=lambda e: e.path)
    assert not errors, [error.message for error in errors]


async def test_me_auth_failures(me_env):
    client, database_url, _ = me_env
    missing = await client.get(f"{MOBILE}/me")
    assert missing.status == 401

    expired = await _seed(database_url, session_expires_in=-60)
    assert (await _get_me(client, expired["token"])).status == 401
    assert (await (await _get_me(client, expired["token"])).json())["code"] == "SESSION_EXPIRED"

    revoked = await _seed(database_url, session_revoked=True)
    response = await _get_me(client, revoked["token"])
    assert response.status == 401
    assert (await response.json())["code"] == "SESSION_INVALID"

    device = await _seed(database_url, installation_state="revoked")
    response = await _get_me(client, device["token"])
    assert response.status == 403
    assert (await response.json())["code"] == "DEVICE_REVOKED"

    wrong_env = await _seed(database_url, environment="production")
    assert (await _get_me(client, wrong_env["token"])).status == 401


async def test_authorizer_requires_scope_and_account_ownership(me_env):
    _client, database_url, settings = me_env
    seeded = await _seed(database_url, scopes=("session:read",))
    connection = await _connect(database_url)
    try:
        context = await authenticate_session(connection, settings, seeded["token"])
        assert context.installation_ref == seeded["fingerprint"]
        with pytest.raises(AuthError) as error:
            await authenticate_session(
                connection, settings, seeded["token"], required_scope="access:sync"
            )
        assert error.value.code == "ACCESS_DENIED" and error.value.http == 403
    finally:
        await connection.close()

    scoped = await _seed(database_url, scopes=("access:sync",))
    connection = await _connect(database_url)
    try:
        context = await authenticate_session(
            connection, settings, scoped["token"], required_scope="access:sync"
        )
        assert context.binding_status == "active"
    finally:
        await connection.close()


async def test_operation_status_owner_only_and_accepted_not_applied(me_env):
    client, database_url, _ = me_env
    seeded = await _seed(database_url)
    foreign = await _seed(database_url)

    connection = await _connect(database_url)
    try:
        gateway_id = await connection.fetchval(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, registry_state)
            VALUES ('me-gw-1', 'test', '{}'::jsonb, 'registered') RETURNING id
            """
        )
        grant_id = await connection.fetchval(
            """
            INSERT INTO grants
                (binding_id, gateway_id, desired_generation, applied_generation, not_after, state)
            VALUES ($1, $2, 1, 0, now() + interval '10 minutes', 'pending')
            RETURNING id
            """,
            seeded["binding_id"],
            gateway_id,
        )
        op_payload = json.dumps(
            {"grant_id": str(grant_id), "generation": "1", "action": "apply", "not_after": "x"}
        )
        pending_op = await connection.fetchval(
            """
            INSERT INTO outbox_operations
                (operation_type, payload, idempotency_key, gateway_id, account_id, binding_id,
                 target_revision, status)
            VALUES ('gateway.apply_grant', $4::jsonb, 'me-op-pending', $1, $2, $3, 1, 'pending')
            RETURNING id
            """,
            gateway_id,
            seeded["account_id"],
            seeded["binding_id"],
            op_payload,
        )
        applied_op = await connection.fetchval(
            """
            INSERT INTO outbox_operations
                (operation_type, payload, idempotency_key, gateway_id, account_id, binding_id,
                 target_revision, status)
            VALUES ('gateway.apply_grant', $4::jsonb, 'me-op-accepted', $1, $2, $3, 1, 'done')
            RETURNING id
            """,
            gateway_id,
            seeded["account_id"],
            seeded["binding_id"],
            op_payload,
        )
        foreign_op = await connection.fetchval(
            """
            INSERT INTO outbox_operations
                (operation_type, payload, idempotency_key, account_id, status)
            VALUES ('gateway.apply_grant', '{}'::jsonb, 'me-op-foreign', $1, 'done')
            RETURNING id
            """,
            foreign["account_id"],
        )
    finally:
        await connection.close()

    response = await client.get(f"{MOBILE}/operations/{pending_op}", headers=_auth(seeded["token"]))
    assert response.status == 200, await response.text()
    body = await response.json()
    errors = sorted(_validator("operation.json", "OperationResponse").iter_errors(body), key=lambda e: e.path)
    assert not errors, [error.message for error in errors]
    assert body["state"] == "queued"
    assert body["access_application_state"] == "pending"

    # accepted (outbox done) but the grant is not applied -> applying, never applied.
    response = await client.get(f"{MOBILE}/operations/{applied_op}", headers=_auth(seeded["token"]))
    body = await response.json()
    assert body["state"] == "applying"
    assert body["access_application_state"] == "pending"

    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE grants SET applied_generation = 1, state = 'applied' WHERE id = $1", grant_id
        )
    finally:
        await connection.close()
    response = await client.get(f"{MOBILE}/operations/{applied_op}", headers=_auth(seeded["token"]))
    body = await response.json()
    assert body["state"] == "applied"
    assert body["access_application_state"] == "applied"
    assert body["per_node"][0]["state"] == "applied"

    foreign_response = await client.get(
        f"{MOBILE}/operations/{foreign_op}", headers=_auth(seeded["token"])
    )
    assert foreign_response.status == 404
    assert (await foreign_response.json())["code"] == "NOT_FOUND"

    unknown = await client.get(f"{MOBILE}/operations/{uuid.uuid4()}", headers=_auth(seeded["token"]))
    assert unknown.status == 404
    unauthorized = await client.get(f"{MOBILE}/operations/{pending_op}")
    assert unauthorized.status == 401


async def test_hour_sync_operation_uses_installation_owner_and_hour_grant_readback(me_env):
    client, database_url, _ = me_env
    owner = await _seed(
        database_url, with_account=False, binding_status=None,
        hour={"status": "active", "ends_in": 1800},
    )
    foreign = await _seed(
        database_url, with_account=False, binding_status=None,
        hour={"status": "active", "ends_in": 1800},
    )
    parent_id = uuid.uuid4()
    connection = await _connect(database_url)
    try:
        gateway_id = await connection.fetchval(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, registry_state)
            VALUES ($1, 'test', '{}'::jsonb, 'registered') RETURNING id
            """,
            f"hour-op-{uuid.uuid4().hex}",
        )
        grant_id = await connection.fetchval(
            """
            INSERT INTO grants
                (installation_id, hour_entitlement_id, gateway_id, desired_generation,
                 applied_generation, not_after, state)
            VALUES ($1, $2, $3, 1, NULL, now() + interval '10 minutes', 'pending')
            RETURNING id
            """,
            owner["installation_id"], owner["hour_id"], gateway_id,
        )
        opaque_id = await connection.fetchval("SELECT opaque_id FROM grants WHERE id = $1", grant_id)
        gateway_key = await connection.fetchval("SELECT gateway_key FROM gateways WHERE id = $1", gateway_id)
        await connection.execute(
            """
            INSERT INTO outbox_operations
                (id, operation_type, payload, idempotency_key, correlation_id, status)
            VALUES ($1, 'access.sync', $2::jsonb, $3, $1, 'done')
            """,
            parent_id,
            json.dumps({"targets": [gateway_key], "hour_entitlement_id": str(owner["hour_id"])}),
            f"hour-parent-{parent_id}",
        )
        child_id = await connection.fetchval(
            """
            INSERT INTO outbox_operations
                (operation_type, payload, idempotency_key, correlation_id, gateway_id,
                 target_revision, status)
            VALUES ('gateway.apply_grant', $1::jsonb, $2, $3, $4, 1, 'pending')
            RETURNING id
            """,
            json.dumps({"grant_id": str(opaque_id), "generation": "1", "action": "apply",
                        "hour_entitlement_id": str(owner["hour_id"])}),
            f"hour-child-{parent_id}", parent_id, gateway_id,
        )
    finally:
        await connection.close()

    for operation_id in (parent_id, child_id):
        pending = await client.get(f"{MOBILE}/operations/{operation_id}", headers=_auth(owner["token"]))
        assert pending.status == 200, await pending.text()
        assert (await pending.json())["access_application_state"] == "pending"

    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE grants SET state = 'applied', applied_generation = 1 WHERE id = $1", grant_id
        )
        await connection.execute("UPDATE outbox_operations SET status = 'done' WHERE id = $1", child_id)
    finally:
        await connection.close()
    for operation_id in (parent_id, child_id):
        applied = await client.get(f"{MOBILE}/operations/{operation_id}", headers=_auth(owner["token"]))
        assert applied.status == 200, await applied.text()
        body = await applied.json()
        assert body["state"] == "applied"
        assert body["access_application_state"] == "applied"
        assert body["per_node"][0]["state"] == "applied"

    connection = await _connect(database_url)
    try:
        await connection.execute("UPDATE grants SET desired_generation = 2 WHERE id = $1", grant_id)
    finally:
        await connection.close()
    for operation_id in (parent_id, child_id):
        rejected = await client.get(f"{MOBILE}/operations/{operation_id}", headers=_auth(owner["token"]))
        assert rejected.status == 200, await rejected.text()
        assert (await rejected.json())["state"] == "rejected"
        other = await client.get(f"{MOBILE}/operations/{operation_id}", headers=_auth(foreign["token"]))
        assert other.status == 404


async def test_commercial_window_and_hour_do_not_hide_paid(me_env):
    client, database_url, _ = me_env

    # Q1a: a future commercial right is not active.
    future = await _seed(
        database_url,
        entitlement={"kind": "paid", "status": "active", "starts_in": 86400, "ends_in": 2 * 86400},
    )
    body = await (await _get_me(client, future["token"])).json()
    assert body["account_state"] == "VERIFIED_NO_ENTITLEMENT"
    assert body["entitlement"]["type"] == "paid"
    assert body["entitlement"]["status"] == "none"
    assert body["grant_resolution"]["data_access"] == "none"

    # Q1b: an active paid right must not be hidden by an active onboarding hour.
    paid_and_hour = await _seed(
        database_url,
        entitlement={"kind": "paid", "status": "active", "starts_in": 0, "ends_in": 600},
        hour={"status": "active", "ends_in": 1800},
    )
    body = await (await _get_me(client, paid_and_hour["token"])).json()
    assert body["account_state"] == "ACTIVE_PAID"
    assert body["grant_resolution"]["data_access"] == "subscription_data"
    assert body["grant_resolution"]["effective_deadline"] == body["entitlement"]["valid_until"]
    assert body["entitlement"]["valid_until"] is not None
    assert body["onboarding"]["state"] == "active"

    # Boundary: ends_at in the past is expired; indefinite end stays effective.
    expired = await _seed(
        database_url,
        entitlement={"kind": "paid", "status": "active", "starts_in": -3600, "ends_in": -1},
    )
    body = await (await _get_me(client, expired["token"])).json()
    assert body["account_state"] == "EXPIRED"
    assert body["entitlement"]["status"] == "expired"

    indefinite = await _seed(
        database_url, entitlement={"kind": "imported", "status": "active", "ends_in": None}
    )
    body = await (await _get_me(client, indefinite["token"])).json()
    assert body["account_state"] == "ACTIVE_PAID"
    assert body["entitlement"]["perpetual_commercial"] is True
    assert body["grant_resolution"]["data_access"] == "subscription_data"


async def test_operation_confirmation_requires_target_and_action(me_env):
    client, database_url, _ = me_env
    seeded = await _seed(database_url)
    connection = await _connect(database_url)
    try:
        gateway_id = await connection.fetchval(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, registry_state)
            VALUES ('me-gw-2', 'test', '{}'::jsonb, 'registered') RETURNING id
            """
        )
        grant_id = await connection.fetchval(
            """
            INSERT INTO grants
                (binding_id, gateway_id, desired_generation, applied_generation, not_after, state)
            VALUES ($1, $2, 2, 2, now() + interval '10 minutes', 'applied')
            RETURNING id
            """,
            seeded["binding_id"],
            gateway_id,
        )
        # Q2: a stale operation (target 1) must not borrow the success of generation 2.
        stale_op = await connection.fetchval(
            """
            INSERT INTO outbox_operations
                (operation_type, payload, idempotency_key, gateway_id, account_id, binding_id,
                 target_revision, status)
            VALUES ('gateway.apply_grant', $4::jsonb, 'me-op-stale', $1, $2, $3, 1, 'done')
            RETURNING id
            """,
            gateway_id,
            seeded["account_id"],
            seeded["binding_id"],
            json.dumps({"grant_id": str(grant_id), "generation": "1", "action": "apply", "not_after": "x"}),
        )
        # Q2b: a done gateway operation without binding/gateway/readback is not success.
        orphan_op = await connection.fetchval(
            """
            INSERT INTO outbox_operations
                (operation_type, payload, idempotency_key, account_id, target_revision, status)
            VALUES ('gateway.apply_grant', $2::jsonb, 'me-op-orphan', $1, 1, 'done')
            RETURNING id
            """,
            seeded["account_id"],
            json.dumps({"generation": "1", "action": "apply"}),
        )
    finally:
        await connection.close()

    response = await client.get(f"{MOBILE}/operations/{stale_op}", headers=_auth(seeded["token"]))
    body = await response.json()
    assert body["state"] == "rejected"
    assert body["access_application_state"] == "rejected"

    response = await client.get(f"{MOBILE}/operations/{orphan_op}", headers=_auth(seeded["token"]))
    body = await response.json()
    assert body["state"] == "rejected"
    assert body["access_application_state"] == "rejected"


async def test_subject_revision_is_monotonic_and_stable(me_env):
    client, database_url, _ = me_env
    seeded = await _seed(
        database_url,
        entitlement={"kind": "paid", "status": "active", "ends_in": 86400, "revision": 5},
    )
    first = await (await _get_me(client, seeded["token"])).json()
    revision_one = int(first["revision"])

    # Ordinary session refresh for the same subject and snapshot must not change the revision.
    connection = await _connect(database_url)
    try:
        refreshed_token = secrets.token_urlsafe(32)
        await connection.execute(
            """
            INSERT INTO sessions
                (account_id, installation_id, scopes, generation, expires_at, token_sha256,
                 binding_id, binding_generation)
            VALUES ($1, $2, ARRAY['session:read'], 1, now() + interval '1 hour', $3, $4, 1)
            """,
            seeded["account_id"],
            seeded["installation_id"],
            hashlib.sha256(refreshed_token.encode()).hexdigest(),
            seeded["binding_id"],
        )
    finally:
        await connection.close()
    refreshed = await (await _get_me(client, refreshed_token)).json()
    assert int(refreshed["revision"]) == revision_one

    # Q3: replacing an entitlement with a lower internal revision must not lower the subject
    # revision (no max() of unrelated revisions).
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "DELETE FROM entitlements WHERE account_id = $1", seeded["account_id"]
        )
        await connection.execute(
            """
            INSERT INTO entitlements
                (account_id, kind, status, starts_at, ends_at, device_limit, revision)
            VALUES ($1, 'paid', 'active', now(), now() + interval '30 days', 2, 1)
            """,
            seeded["account_id"],
        )
    finally:
        await connection.close()
    after = await (await _get_me(client, seeded["token"])).json()
    assert int(after["revision"]) > revision_one

    # Concurrent reads keep one consistent revision (no lost increments, no torn snapshot).
    concurrent = await asyncio.gather(
        *[_get_me(client, seeded["token"]) for _ in range(4)]
    )
    revisions = {int((await response.json())["revision"]) for response in concurrent}
    assert revisions == {int(after["revision"])}


async def test_hour_is_installation_bound(me_env):
    client, database_url, _ = me_env
    first = await _seed(database_url, hour={"status": "active", "ends_in": 1800})
    second = await _seed(database_url, hour={"status": "active", "ends_in": 2400})
    # Make both installations belong to one account.
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE installations SET state = state WHERE id = $1", second["installation_id"]
        )
        await connection.execute(
            """
            UPDATE account_bindings SET account_id = $1 WHERE installation_id = $2
            """,
            first["account_id"],
            second["installation_id"],
        )
        await connection.execute(
            "UPDATE sessions SET account_id = $1 WHERE installation_id = $2",
            first["account_id"],
            second["installation_id"],
        )
    finally:
        await connection.close()

    body_one = await (await _get_me(client, first["token"])).json()
    body_two = await (await _get_me(client, second["token"])).json()
    assert body_one["onboarding"]["state"] == "active"
    assert body_two["onboarding"]["state"] == "active"
    assert body_one["onboarding"]["not_after"] != body_two["onboarding"]["not_after"]
    assert body_one["grant_resolution"]["data_access"] == "onboarding_hour"
    assert body_two["grant_resolution"]["data_access"] == "onboarding_hour"

    # An undetermined (installation_id NULL) hour grants no data and must not be attributed.
    ambiguous = await _seed(database_url, hour={"status": "active", "ends_in": 1800, "ambiguous": True})
    body = await (await _get_me(client, ambiguous["token"])).json()
    assert body["onboarding"]["state"] == "not_started"
    assert body["grant_resolution"]["data_access"] == "none"

    # An hour of an unlinked installation stays visible despite account_id NULL.
    unlinked = await _seed(
        database_url,
        with_account=False,
        binding_status=None,
        hour={"status": "active", "ends_in": 1800},
    )
    body = await (await _get_me(client, unlinked["token"])).json()
    assert body["account_state"] == "UNLINKED"
    assert body["onboarding"]["state"] == "active"
    assert body["grant_resolution"]["data_access"] == "onboarding_hour"


def _assert_snapshot_consistent(body: dict) -> None:
    resolution = body["grant_resolution"]
    if resolution["data_access"] == "subscription_data":
        assert body["entitlement"]["status"] == "active"
        assert resolution["effective_deadline"] == body["entitlement"]["valid_until"]
        assert body["account_state"] in ("ACTIVE_TRIAL", "ACTIVE_PAID")
    elif resolution["data_access"] == "onboarding_hour":
        assert body["onboarding"]["state"] == "active"
        assert resolution["effective_deadline"] == body["onboarding"]["not_after"]
    else:
        assert resolution["effective_deadline"] is None


async def test_me_serialization_conflicts_are_bounded(me_env, monkeypatch):
    client, database_url, _ = me_env
    seeded = await _seed(
        database_url, entitlement={"kind": "paid", "status": "active", "ends_in": 3600}
    )
    calls = {"count": 0}

    async def forced_conflict(*_args, **_kwargs):
        calls["count"] += 1
        raise asyncpg.exceptions.SerializationError("forced conflict")

    monkeypatch.setattr(mobile_account, "_subject_revision", forced_conflict)
    before = await _counts(database_url)
    response = await _get_me(client, seeded["token"])
    assert response.status == 503
    body = await response.json()
    assert body["code"] == "SERVICE_UNAVAILABLE"
    assert body["retryable"] is True
    assert calls["count"] == 3
    assert await _counts(database_url) == before


async def test_me_snapshot_is_consistent_under_interleaved_updates(me_env):
    client, database_url, _ = me_env
    seeded = await _seed(
        database_url, entitlement={"kind": "paid", "status": "active", "ends_in": 3600}
    )

    async def updater() -> None:
        connection = await _connect(database_url)
        try:
            for step in range(20):
                await connection.execute(
                    """
                    UPDATE entitlements
                    SET ends_at = now() + make_interval(secs => $2), revision = revision + 1
                    WHERE id = $1
                    """,
                    seeded["entitlement_id"],
                    3600 + step * 60,
                )
                await asyncio.sleep(0.01)
        finally:
            await connection.close()

    task = asyncio.create_task(updater())
    responses = await asyncio.gather(*[_get_me(client, seeded["token"]) for _ in range(10)])
    await task
    validator = _validator("subscription.json", "MeResponse")
    successes = 0
    for response in responses:
        body = await response.json()
        if response.status == 503:
            # Bounded retry exhausted: a retryable error, never a partial/mixed body.
            assert body["code"] == "SERVICE_UNAVAILABLE"
            assert "grant_resolution" not in body
            continue
        assert response.status == 200, body
        successes += 1
        errors = sorted(validator.iter_errors(body), key=lambda error: error.path)
        assert not errors, [error.message for error in errors]
        _assert_snapshot_consistent(body)
    assert successes >= 1


async def test_me_concurrent_first_get_converges_on_one_revision(me_env):
    client, database_url, _ = me_env
    seeded = await _seed(
        database_url, entitlement={"kind": "paid", "status": "active", "ends_in": 3600}
    )
    responses = await asyncio.gather(*[_get_me(client, seeded["token"]) for _ in range(4)])
    bodies = [await response.json() for response in responses]
    assert all(response.status == 200 for response in responses), bodies
    assert {body["revision"] for body in bodies} == {"1"}
    assert await _count(database_url, "subject_revisions") == 1
