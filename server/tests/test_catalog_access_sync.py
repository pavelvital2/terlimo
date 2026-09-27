"""GET /gateways and POST /access/sync over real HTTP + isolated PostgreSQL.

Synthetic closed fixtures only; the gateway side uses the wire-faithful fake for the durable
apply/readback path (real gateway transport was accepted separately in 03.3).
"""

from __future__ import annotations

import asyncio
import base64
import hashlib
import json
import os
import secrets
import uuid
from dataclasses import replace
from datetime import UTC, datetime, timedelta
from pathlib import Path

import asyncpg
import pytest
from aiohttp.test_utils import TestClient, TestServer
from fake_gateway_admin import FakeGatewayAdmin
from jsonschema import Draft202012Validator
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012
from test_auth_flow import KeyMaterial, PoPClient, _challenge, _enroll, _session

from terlimo_backend.api import create_app
from terlimo_backend.db import Database
from terlimo_backend.gateway_adapter import GatewayAdminClient
from terlimo_backend.gateway_control import GatewayControlHandlers
from terlimo_backend.maintenance import sweep_once
from terlimo_backend.worker import OutboxWorker

CONTRACT_DIR = Path(os.environ.get("TERLIMO_CONTRACT_DIR", "/home/pavel/projects/terlimo-s1-contracts"))
MOBILE = "/api/mobile/v1"
PIN = base64.urlsafe_b64encode(hashlib.sha256(b"catalog-test-pin").digest()).rstrip(b"=").decode()

if not (CONTRACT_DIR / "schemas" / "catalog.json").exists():
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


async def _counts(database_url: str) -> dict[str, int]:
    connection = await _connect(database_url)
    try:
        return {
            table: await connection.fetchval(f"SELECT count(*) FROM {table}")
            for table in ("grants", "outbox_operations", "operation_receipts", "entitlements")
        }
    finally:
        await connection.close()


async def _seed_subject(
    database_url: str,
    *,
    scopes: tuple[str, ...] = ("access:sync", "session:read"),
    entitlement_ends_in: int | None = 30 * 86400,
    entitlement_status: str = "active",
    management_only: bool = False,
    binding_account_mismatch: bool = False,
) -> dict:
    connection = await _connect(database_url)
    try:
        account_id = await connection.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified', $1) RETURNING id",
            secrets.randbelow(1 << 40),
        )
        other_account = await connection.fetchval(
            "INSERT INTO accounts (status) VALUES ('verified') RETURNING id"
        )
        fingerprint = hashlib.sha256(secrets.token_bytes(32)).hexdigest()
        installation_id = await connection.fetchval(
            """
            INSERT INTO installations
                (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test', 'android', $1, 'spki', 'technical') RETURNING id
            """,
            fingerprint,
        )
        if management_only:
            scopes = ("management-only",)
        binding_owner = other_account if binding_account_mismatch else account_id
        binding_id = await connection.fetchval(
            """
            INSERT INTO account_bindings (account_id, installation_id, status)
            VALUES ($1, $2, 'active') RETURNING id
            """,
            binding_owner,
            installation_id,
        )
        entitlement_id = None
        if entitlement_ends_in is not None or entitlement_status != "active":
            ends_at = (
                datetime.now(UTC) + timedelta(seconds=entitlement_ends_in)
                if entitlement_ends_in is not None
                else None
            )
            entitlement_id = await connection.fetchval(
                """
                INSERT INTO entitlements
                    (account_id, kind, status, starts_at, ends_at, device_limit, revision)
                VALUES ($1, 'paid', $2, now() - interval '1 hour', $3, 2, 1)
                RETURNING id
                """,
                account_id,
                entitlement_status,
                ends_at,
            )
        token = secrets.token_urlsafe(32)
        await connection.execute(
            """
            INSERT INTO sessions
                (account_id, installation_id, scopes, generation, expires_at, token_sha256,
                 binding_id, binding_generation)
            VALUES ($1, $2, $3::text[], 1, now() + interval '1 hour', $4, $5, $6)
            """,
            account_id,
            installation_id,
            list(scopes),
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
            "fingerprint": fingerprint,
        }
    finally:
        await connection.close()


async def _add_gateway(
    database_url: str,
    key: str,
    *,
    complete: bool = True,
    admin_socket: str | None = None,
    overrides: dict | None = None,
    capabilities: list | None = None,
) -> uuid.UUID:
    endpoints: dict = {"node_id": key}
    if admin_socket:
        endpoints["admin_socket"] = admin_socket
    if complete:
        endpoints.update(
            {
                "peer_ip": "127.0.0.1",
                "dtls_port": 56300,
                "wg_port": 56302,
                "dtls_spki_sha256": PIN,
                "region": "test",
                "country_code": "XX",
                "target_workers": 36,
            }
        )
    if overrides:
        endpoints.update(overrides)
    connection = await _connect(database_url)
    try:
        return await connection.fetchval(
            """
            INSERT INTO gateways (gateway_key, environment, display_name, endpoints,
                                  capabilities, registry_state)
            VALUES ($1, 'test', $2, $3::jsonb, $4::jsonb, 'registered')
            RETURNING id
            """,
            key,
            key.upper(),
            json.dumps(endpoints),
            json.dumps(capabilities if capabilities is not None else ["managed"]),
        )
    finally:
        await connection.close()


async def _add_applied_grant(
    database_url: str,
    subject: dict,
    gateway_id: uuid.UUID,
    *,
    credential: str | None = None,
    not_after_seconds: int = 600,
) -> None:
    connection = await _connect(database_url)
    try:
        await connection.execute(
            """
            INSERT INTO grants
                (binding_id, gateway_id, desired_generation, applied_generation, not_after,
                 state, gateway_credential, lease_seq, gateway_generation)
            VALUES ($1, $2, 1, 1, now() + make_interval(secs => $3), 'applied', $4, 1, 1)
            """,
            subject["binding_id"],
            gateway_id,
            float(not_after_seconds),
            credential or f"cred-{gateway_id}",
        )
        # Fixture-капитан readback: applied grant implies the node cap was confirmed.
        await connection.execute(
            """
            UPDATE gateways SET confirmed_max_workers = (endpoints->>'target_workers')::int
            WHERE id = $1 AND (endpoints ? 'target_workers')
            """,
            gateway_id,
        )
    finally:
        await connection.close()


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


def _auth(token: str) -> dict[str, str]:
    return {"Authorization": f"Bearer {token}"}


async def connection_fetchval(database_url: str, query: str):
    connection = await _connect(database_url)
    try:
        return await connection.fetchval(query)
    finally:
        await connection.close()


async def _current_revision(database_url: str, installation_id) -> str:
    connection = await _connect(database_url)
    try:
        revision = await connection.fetchval(
            "SELECT revision FROM catalog_revisions WHERE installation_id = $1",
            installation_id,
        )
        return str(revision) if revision is not None else "1"
    finally:
        await connection.close()


async def test_gateways_without_data_subject_serves_browse_only(catalog_env):
    """Owner doc 19 (control404): no credentialed data admission -> display browse branch."""
    client, database_url, _, _ = catalog_env
    none = await _seed_subject(database_url, entitlement_ends_in=None)
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "DELETE FROM entitlements WHERE account_id = $1", none["account_id"]
        )
    finally:
        await connection.close()

    for subject in (
        none,
        await _seed_subject(database_url, entitlement_ends_in=-60),
        await _seed_subject(database_url, management_only=True),
    ):
        response = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert response.status == 200, await response.text()
        body = await response.json()
        assert body["catalog_mode"] == "browse"
        assert "revision" not in body
        for gateway in body["gateways"]:
            assert "access" not in gateway and "transport" not in gateway

    # access sync stays closed for these subjects
    expired = await _seed_subject(database_url, entitlement_ends_in=-60)
    response = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(expired["token"]), "Idempotency-Key": "browse-sync-denied-0001"},
        json={"catalog_revision": "1", "binding_revision": "1"},
    )
    assert response.status == 403


async def test_gateways_incomplete_registry_is_not_empty_success(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    await _add_gateway(database_url, "gw-incomplete", complete=False)
    response = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert response.status == 503
    body = await response.json()
    assert body["code"] == "SERVICE_UNAVAILABLE"
    assert body["details"]["reason"] == "registry_descriptors_incomplete"


async def test_gateways_advertises_confirmed_applied_access_only(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    applied_gateway = await _add_gateway(database_url, "gw-applied")
    revoked_gateway = await _add_gateway(database_url, "gw-revoked")
    await _add_applied_grant(database_url, subject, applied_gateway, credential="applied-cred")
    connection = await _connect(database_url)
    try:
        await connection.execute(
            """
            INSERT INTO grants
                (binding_id, gateway_id, desired_generation, applied_generation, not_after,
                 state, gateway_credential, last_readback)
            VALUES ($1, $2, 1, 1, now() + interval '10 minutes', 'revoked', 'revoked-cred',
                    '{"revoked": true}'::jsonb)
            """,
            subject["binding_id"],
            revoked_gateway,
        )
    finally:
        await connection.close()

    response = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert response.status == 200, await response.text()
    body = await response.json()
    errors = sorted(_validator("catalog.json", "CatalogResponse").iter_errors(body), key=lambda e: e.path)
    assert not errors, [error.message for error in errors]
    assert body["revision"] == await _current_revision(database_url, subject["installation_id"])
    assert body["valid_until"] > body["server_time"]
    assert body["gateways"][0]["gateway_id"] == "gw-applied"
    access = body["gateways"][0]["access"]
    assert access["password"] == "applied-cred"
    assert access["generation"] == "1"
    assert access["device_ref"] == subject["fingerprint"]
    assert all(gateway["gateway_id"] != "gw-revoked" for gateway in body["gateways"])


async def test_access_sync_creates_desired_grants_and_operation(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    gateway = await _add_gateway(database_url, "gw-sync-1")
    catalog_revision = await _current_revision(database_url, subject["installation_id"])
    response = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-0000001"},
        json={"catalog_revision": catalog_revision, "binding_revision": "1"},
    )
    assert response.status == 200, await response.text()
    body = await response.json()
    errors = sorted(_validator("operation.json", "AccessSyncResponse").iter_errors(body), key=lambda e: e.path)
    assert not errors, [error.message for error in errors]
    assert body["access_application_state"] == "pending"
    operation_id = uuid.UUID(body["operation_id"])
    connection = await _connect(database_url)
    try:
        grant = await connection.fetchrow(
            "SELECT * FROM grants WHERE gateway_id = $1", gateway
        )
        operation = await connection.fetchrow(
            "SELECT * FROM outbox_operations WHERE correlation_id = $1", operation_id
        )
    finally:
        await connection.close()
    assert grant["desired_generation"] == 1 and grant["state"] == "pending"
    assert grant["applied_generation"] is None
    assert operation["operation_type"] == "gateway.apply_grant"
    assert operation["account_id"] == subject["account_id"]
    assert operation["binding_id"] == subject["binding_id"]


async def test_access_sync_retry_digest_conflict_and_concurrency(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    await _add_gateway(database_url, "gw-sync-2")
    catalog_revision = await _current_revision(database_url, subject["installation_id"])
    headers = {**_auth(subject["token"]), "Idempotency-Key": "sync-key-0000002"}
    first = await client.post(
        f"{MOBILE}/access/sync",
        headers=headers,
        json={"catalog_revision": catalog_revision, "binding_revision": "1"},
    )
    assert first.status == 200
    first_body = await first.json()
    before = await _counts(database_url)

    retry = await client.post(
        f"{MOBILE}/access/sync",
        headers=headers,
        json={"catalog_revision": catalog_revision, "binding_revision": "1"},
    )
    assert retry.status == 200
    assert (await retry.json())["operation_id"] == first_body["operation_id"]
    assert await _counts(database_url) == before

    # A different body under the same key is a material digest change, not a revision conflict.
    conflict = await client.post(
        f"{MOBILE}/access/sync",
        headers=headers,
        json={"catalog_revision": catalog_revision, "binding_revision": "2"},
    )
    assert conflict.status == 409
    assert (await conflict.json())["code"] == "IDEMPOTENCY_CONFLICT"

    # The first sync made the visible grant set part of the revision; admit the concurrent
    # pair against the current revision, exactly as a fresh GET would provide it.
    live_revision = await _current_revision(database_url, subject["installation_id"])
    concurrent_headers = {**_auth(subject["token"]), "Idempotency-Key": "sync-key-0000003"}
    responses = await asyncio.gather(
        *[
            client.post(
                f"{MOBILE}/access/sync",
                headers=concurrent_headers,
                json={"catalog_revision": live_revision, "binding_revision": "1"},
            )
            for _ in range(2)
        ]
    )
    bodies = [await response.json() for response in responses]
    assert all(response.status == 200 for response in responses), bodies
    assert len({body["operation_id"] for body in bodies}) == 1
    connection = await _connect(database_url)
    try:
        assert await connection.fetchval(
            "SELECT count(*) FROM operation_receipts WHERE idempotency_key = 'sync-key-0000003'"
        ) == 1
    finally:
        await connection.close()


async def test_access_sync_auth_revision_and_ownership_denied_without_effects(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    await _add_gateway(database_url, "gw-sync-3")
    catalog_revision = await _current_revision(database_url, subject["installation_id"])
    wrong_scope = await _seed_subject(database_url, scopes=("session:read",))
    foreign = await _seed_subject(database_url, binding_account_mismatch=True)
    before = await _counts(database_url)

    response = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(wrong_scope["token"]), "Idempotency-Key": "sync-key-0000004"},
        json={"catalog_revision": catalog_revision, "binding_revision": "1"},
    )
    assert response.status == 403
    assert (await response.json())["code"] == "ACCESS_DENIED"

    stale_catalog = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-0000005"},
        json={"catalog_revision": "99", "binding_revision": "1"},
    )
    assert stale_catalog.status == 409
    assert (await stale_catalog.json())["code"] == "REVISION_CONFLICT"

    response = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(foreign["token"]), "Idempotency-Key": "sync-key-0000006"},
        json={"catalog_revision": catalog_revision, "binding_revision": "1"},
    )
    # A session pointing at a binding owned by another account fails the identity fence.
    assert response.status == 401
    assert (await response.json())["code"] == "SESSION_INVALID"
    assert await _counts(database_url) == before


async def test_access_sync_durable_apply_then_catalog_and_status(catalog_env):
    client, database_url, settings, database = catalog_env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-sync-4")
    await fake.start()
    try:
        subject = await _seed_subject(database_url)
        await _add_gateway(database_url, "gw-sync-4", admin_socket=fake.socket_path)
        catalog_revision = await _current_revision(database_url, subject["installation_id"])
        response = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-0000007"},
            json={"catalog_revision": catalog_revision, "binding_revision": "1"},
        )
        assert response.status == 200, await response.text()
        operation_id = (await response.json())["operation_id"]

        handlers = GatewayControlHandlers(
            settings,
            client_factory=lambda _key, endpoints: GatewayAdminClient(
                socket_path=str(endpoints["admin_socket"]),
                main_password="fixture-main",
                timeout_seconds=5,
            ),
        ).as_handlers()
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="catalog-test")
        assert await worker.drain() == 2
        assert len(fake.grants) == 1

        status = await client.get(
            f"{MOBILE}/operations/{operation_id}", headers=_auth(subject["token"])
        )
        assert status.status == 200
        status_body = await status.json()
        errors = sorted(
            _validator("operation.json", "OperationResponse").iter_errors(status_body),
            key=lambda e: e.path,
        )
        assert not errors, [error.message for error in errors]
        assert status_body["state"] == "applied"
        assert status_body["per_node"][0]["state"] == "applied"

        catalog = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        catalog_body = await catalog.json()
        assert [gateway["gateway_id"] for gateway in catalog_body["gateways"]] == ["gw-sync-4"]
        assert catalog_body["gateways"][0]["access"]["generation"] == "1"

        retry = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-0000007"},
            json={"catalog_revision": catalog_revision, "binding_revision": "1"},
        )
        retry_body = await retry.json()
        assert retry_body["operation_id"] == operation_id
        assert retry_body["access_application_state"] == "applied"
        connection = await _connect(database_url)
        try:
            assert await connection.fetchval("SELECT count(*) FROM grants") == 1
        finally:
            await connection.close()
    finally:
        await fake.stop()


def _parse_ts(value: str) -> datetime:
    return datetime.fromisoformat(value)


async def test_gateways_revision_is_monotonic_and_material(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    gateway = await _add_gateway(database_url, "gw-rev")
    await _add_applied_grant(database_url, subject, gateway, credential="rev-cred")
    first = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert first.status == 200
    first_revision = (await first.json())["revision"]

    # Repeated GETs with identical material never churn the revision.
    again = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert (await again.json())["revision"] == first_revision

    # A direct registry/descriptor change in TEST is reflected.
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE gateways SET endpoints = endpoints || '{\"region\":\"z2\"}'::jsonb WHERE id = $1",
            gateway,
        )
    finally:
        await connection.close()
    changed = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    changed_revision = (await changed.json())["revision"]
    assert int(changed_revision) > int(first_revision)

    before = await _counts(database_url)
    stale = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-rev-0001"},
        json={"catalog_revision": first_revision, "binding_revision": "1"},
    )
    assert stale.status == 409
    assert (await stale.json())["code"] == "REVISION_CONFLICT"
    assert await _counts(database_url) == before

    fresh = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-rev-0002"},
        json={"catalog_revision": changed_revision, "binding_revision": "1"},
    )
    assert fresh.status == 200, await fresh.text()
    fresh_revision = (await fresh.json())["revision"]
    # The already-applied grant is unchanged, so the admitted revision stays the live one.
    assert int(fresh_revision) >= int(changed_revision)
    observed = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert (await observed.json())["revision"] == fresh_revision

    # Revocation is material and the absence is authoritative.
    connection = await _connect(database_url)
    try:
        await connection.execute(
            """
            UPDATE grants SET state = 'revoked', applied_generation = desired_generation,
                              last_readback = '{"revoked": true}'::jsonb
            WHERE gateway_id = $1
            """,
            gateway,
        )
    finally:
        await connection.close()
    revoked = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    revoked_body = await revoked.json()
    assert revoked.status == 200
    assert revoked_body["gateways"] == []
    assert int(revoked_body["revision"]) > int(fresh_revision)


async def test_access_sync_identical_replay_survives_registry_revision_change(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    gateway = await _add_gateway(database_url, "gw-replay")
    revision = await _current_revision(database_url, subject["installation_id"])
    headers = {**_auth(subject["token"]), "Idempotency-Key": "sync-key-replay-01"}
    body = {"catalog_revision": revision, "binding_revision": "1"}
    first = await client.post(f"{MOBILE}/access/sync", headers=headers, json=body)
    assert first.status == 200, await first.text()
    operation_id = (await first.json())["operation_id"]
    before = await _counts(database_url)

    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE gateways SET endpoints = endpoints || '{\"region\":\"z3\"}'::jsonb WHERE id = $1",
            gateway,
        )
    finally:
        await connection.close()

    retry = await client.post(f"{MOBILE}/access/sync", headers=headers, json=body)
    assert retry.status == 200, await retry.text()
    retry_body = await retry.json()
    assert retry_body["operation_id"] == operation_id
    assert await _counts(database_url) == before

    different = await client.post(
        f"{MOBILE}/access/sync",
        headers=headers,
        json={"catalog_revision": revision, "binding_revision": "2"},
    )
    assert different.status == 409
    assert (await different.json())["code"] == "IDEMPOTENCY_CONFLICT"


async def test_access_receipt_is_durable_across_retention_sweep(catalog_env):
    client, database_url, settings, _ = catalog_env
    subject = await _seed_subject(database_url)
    await _add_gateway(database_url, "gw-sweep")
    revision = await _current_revision(database_url, subject["installation_id"])
    headers = {**_auth(subject["token"]), "Idempotency-Key": "sync-key-sweep-01"}
    body = {"catalog_revision": revision, "binding_revision": "1"}
    first = await client.post(f"{MOBILE}/access/sync", headers=headers, json=body)
    assert first.status == 200, await first.text()
    operation_id = (await first.json())["operation_id"]

    sweep_settings = replace(
        settings, receipt_result_ttl_seconds=0, idempotency_window_seconds=0
    )
    connection = await _connect(database_url)
    try:
        result = await sweep_once(connection, sweep_settings)
        receipt = await connection.fetchrow(
            """
            SELECT result, result_expires_at, retain_until FROM operation_receipts
            WHERE op = 'access.sync'
            """
        )
    finally:
        await connection.close()
    assert result["purged_receipt_results"] == 0
    assert result["deleted_receipts"] == 0
    assert receipt["result"] is not None
    assert receipt["result_expires_at"] is None and receipt["retain_until"] is None

    retry = await client.post(f"{MOBILE}/access/sync", headers=headers, json=body)
    assert retry.status == 200
    assert (await retry.json())["operation_id"] == operation_id


async def test_catalog_registry_error_is_whole_response(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    valid_gateway = await _add_gateway(database_url, "gw-valid-b5")
    await _add_applied_grant(database_url, subject, valid_gateway, credential="valid-b5-cred")
    revision = (await (await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))).json())[
        "revision"
    ]
    headers = {**_auth(subject["token"]), "Idempotency-Key": "sync-key-b5-ok-01"}
    body = {"catalog_revision": revision, "binding_revision": "1"}
    first = await client.post(f"{MOBILE}/access/sync", headers=headers, json=body)
    assert first.status == 200, await first.text()
    operation_id = (await first.json())["operation_id"]

    # A single malformed registered node makes the whole registry an error: no partial catalog.
    await _add_gateway(database_url, "gw-bad-b5", overrides={"target_workers": 0})
    response = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert response.status == 503, await response.text()
    error = await response.json()
    assert error["code"] == "SERVICE_UNAVAILABLE" and error["retryable"] is True
    assert error["details"]["reason"] == "registry_descriptors_incomplete"
    assert "gateways" not in error

    # Replay and poll of the client's own operation survive the live catalog error.
    before = await _counts(database_url)
    replay = await client.post(f"{MOBILE}/access/sync", headers=headers, json=body)
    assert replay.status == 200, await replay.text()
    assert (await replay.json())["operation_id"] == operation_id
    assert await _counts(database_url) == before
    status = await client.get(
        f"{MOBILE}/operations/{operation_id}", headers=_auth(subject["token"])
    )
    assert status.status == 200, await status.text()

    # A NEW sync against the malformed registry is refused and rolls back all its effects.
    blocked = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-b5-new-01"},
        json={
            "catalog_revision": await _current_revision(
                database_url, subject["installation_id"]
            ),
            "binding_revision": "1",
        },
    )
    assert blocked.status == 503, await blocked.text()
    assert (await blocked.json())["details"]["reason"] == "registry_descriptors_incomplete"
    assert await _counts(database_url) == before

    # registry_state removal is a legitimate removal, not malformation: not blocked.
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE gateways SET registry_state = 'disabled' WHERE gateway_key = 'gw-bad-b5'"
        )
    finally:
        await connection.close()
    response = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert response.status == 200, await response.text()
    body_after = await response.json()
    assert [gateway["gateway_id"] for gateway in body_after["gateways"]] == ["gw-valid-b5"]

    # After the registered descriptor is repaired, the full strictly validated catalog returns.
    connection = await _connect(database_url)
    try:
        await connection.execute(
            """
            UPDATE gateways
            SET endpoints = jsonb_set(endpoints, '{target_workers}', '36'::jsonb),
                registry_state = 'registered'
            WHERE gateway_key = 'gw-bad-b5'
            """
        )
    finally:
        await connection.close()
    repaired_gateway = await connection_fetchval(
        database_url, "SELECT id FROM gateways WHERE gateway_key = 'gw-bad-b5'"
    )
    await _add_applied_grant(
        database_url, subject, repaired_gateway, credential="repaired-b5-cred"
    )
    response = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert response.status == 200, await response.text()
    body_final = await response.json()
    assert [gateway["gateway_id"] for gateway in body_final["gateways"]] == [
        "gw-bad-b5",
        "gw-valid-b5",
    ]
    errors = sorted(
        _validator("catalog.json", "CatalogResponse").iter_errors(body_final), key=lambda e: e.path
    )
    assert not errors, [error.message for error in errors]

    recovered = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-b5-rec-01"},
        json={
            "catalog_revision": await _current_revision(
                database_url, subject["installation_id"]
            ),
            "binding_revision": "1",
        },
    )
    assert recovered.status == 200, await recovered.text()


async def test_access_sync_all_applied_is_durable_noop(catalog_env):
    client, database_url, settings, database = catalog_env
    subject = await _seed_subject(database_url)
    gateway = await _add_gateway(database_url, "gw-noop")
    await _add_applied_grant(database_url, subject, gateway, credential="noop-cred")
    revision = (await (await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))).json())[
        "revision"
    ]
    headers = {**_auth(subject["token"]), "Idempotency-Key": "sync-key-noop-01"}
    body = {"catalog_revision": revision, "binding_revision": "1"}
    response = await client.post(f"{MOBILE}/access/sync", headers=headers, json=body)
    assert response.status == 200, await response.text()
    payload = await response.json()
    errors = sorted(
        _validator("operation.json", "AccessSyncResponse").iter_errors(payload), key=lambda e: e.path
    )
    assert not errors, [error.message for error in errors]
    assert payload["access_application_state"] == "applied"
    assert payload["grants"][0]["state"] == "active"
    operation_id = uuid.UUID(payload["operation_id"])

    connection = await _connect(database_url)
    try:
        parent = await connection.fetchrow(
            "SELECT * FROM outbox_operations WHERE id = $1", operation_id
        )
        children = await connection.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE correlation_id = $1 AND id <> $1",
            operation_id,
        )
    finally:
        await connection.close()
    assert parent is not None and parent["operation_type"] == "access.sync"
    assert parent["status"] == "done" and parent["account_id"] == subject["account_id"]
    assert children == 0
    # No extra gateway apply is invented to justify the operation.
    assert await OutboxWorker(database, settings).drain() == 0

    status = await client.get(
        f"{MOBILE}/operations/{operation_id}", headers=_auth(subject["token"])
    )
    assert status.status == 200, await status.text()
    status_body = await status.json()
    assert status_body["state"] == "applied"
    assert status_body["per_node"] == [
        {
            "gateway_id": "gw-noop",
            "state": "applied",
            "applied_generation": "1",
            "not_after": status_body["per_node"][0]["not_after"],
            "last_error": None,
            "attempts": 0,
        }
    ]

    retry = await client.post(f"{MOBILE}/access/sync", headers=headers, json=body)
    assert retry.status == 200
    assert (await retry.json())["operation_id"] == str(operation_id)
    assert (await retry.json())["access_application_state"] == "applied"

    foreign = await _seed_subject(database_url)
    denied = await client.get(
        f"{MOBILE}/operations/{operation_id}", headers=_auth(foreign["token"])
    )
    assert denied.status == 404


async def test_catalog_valid_until_bounded_by_deadlines(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url, entitlement_ends_in=180)
    gateway = await _add_gateway(database_url, "gw-deadline")
    await _add_applied_grant(
        database_url, subject, gateway, credential="deadline-cred", not_after_seconds=60
    )
    connection = await _connect(database_url)
    try:
        grant_not_after = await connection.fetchval(
            "SELECT not_after FROM grants WHERE gateway_id = $1", gateway
        )
        entitlement_ends_at = await connection.fetchval(
            "SELECT ends_at FROM entitlements WHERE account_id = $1", subject["account_id"]
        )
    finally:
        await connection.close()
    response = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert response.status == 200, await response.text()
    body = await response.json()
    valid_until = _parse_ts(body["valid_until"])
    assert valid_until <= grant_not_after
    assert valid_until <= entitlement_ends_at
    assert _parse_ts(body["issued_at"]) <= valid_until
    assert valid_until <= _parse_ts(body["issued_at"]) + timedelta(
        seconds=600
    )


async def test_access_sync_legacy_correlation_receipt_still_resolves(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    await _add_gateway(database_url, "gw-legacy")
    revision = await _current_revision(database_url, subject["installation_id"])
    headers = {**_auth(subject["token"]), "Idempotency-Key": "sync-key-legacy-01"}
    body = {"catalog_revision": revision, "binding_revision": "1"}
    first = await client.post(f"{MOBILE}/access/sync", headers=headers, json=body)
    assert first.status == 200, await first.text()
    operation_id = uuid.UUID((await first.json())["operation_id"])

    # Simulate a receipt stored by the accepted slice (correlation id without a parent row).
    connection = await _connect(database_url)
    try:
        await connection.execute("DELETE FROM outbox_operations WHERE id = $1", operation_id)
    finally:
        await connection.close()

    retry = await client.post(f"{MOBILE}/access/sync", headers=headers, json=body)
    assert retry.status == 200, await retry.text()
    retry_body = await retry.json()
    assert retry_body["operation_id"] == str(operation_id)
    assert retry_body["access_application_state"] == "pending"
    assert retry_body["grants"][0]["gateway_id"] == "gw-legacy"
    status = await client.get(
        f"{MOBILE}/operations/{operation_id}", headers=_auth(subject["token"])
    )
    assert status.status == 200, await status.text()


async def test_grant_expiry_is_pending_not_removal(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url, entitlement_ends_in=3600)
    gateway = await _add_gateway(database_url, "gw-expiry")
    await _add_applied_grant(
        database_url, subject, gateway, credential="expiry-cred", not_after_seconds=2
    )
    connection = await _connect(database_url)
    try:
        grant_not_after = await connection.fetchval(
            "SELECT not_after FROM grants WHERE gateway_id = $1", gateway
        )
        entitlement_ends_at = await connection.fetchval(
            "SELECT ends_at FROM entitlements WHERE account_id = $1", subject["account_id"]
        )
    finally:
        await connection.close()

    before = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert before.status == 200, await before.text()
    before_body = await before.json()
    assert [gateway["gateway_id"] for gateway in before_body["gateways"]] == ["gw-expiry"]
    assert _parse_ts(before_body["valid_until"]) <= grant_not_after
    assert _parse_ts(before_body["valid_until"]) <= entitlement_ends_at
    revision_before = before_body["revision"]
    business_before = await _counts(database_url)

    # Repeats before the transition are stable: no revision churn per GET.
    again = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert (await again.json())["revision"] == revision_before

    # Time-only expiry, without any SQL write: the absence is a pending renewal, never a removal,
    # and never a credential leak.
    await asyncio.sleep(3)
    expired = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert expired.status == 409, await expired.text()
    expired_body = await expired.json()
    errors = sorted(
        _validator("errors.json", "ErrorResponse").iter_errors(expired_body), key=lambda e: e.path
    )
    assert not errors, [error.message for error in errors]
    assert expired_body["code"] == "ACCESS_SYNC_PENDING"
    assert expired_body["retryable"] is True
    assert "retry_after_ms" in expired_body
    tokens = expired_body["details"]
    assert tokens["catalog_revision"] == await _current_revision(
        database_url, subject["installation_id"]
    )
    assert tokens["binding_revision"] == "1"
    assert "expiry-cred" not in json.dumps(expired_body)
    revision_after = tokens["catalog_revision"]
    assert int(revision_after) > int(revision_before)
    assert await _counts(database_url) == business_before

    # Stable again after the transition (no per-GET bump).
    stable = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert (await stable.json())["details"]["catalog_revision"] == revision_after

    # A stale NEW operation is refused; the admission tokens renew through the worker path.
    stale = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-expiry-01"},
        json={"catalog_revision": revision_before, "binding_revision": "1"},
    )
    assert stale.status == 409
    assert (await stale.json())["code"] == "REVISION_CONFLICT"
    fresh = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-expiry-02"},
        json={"catalog_revision": revision_after, "binding_revision": tokens["binding_revision"]},
    )
    assert fresh.status == 200, await fresh.text()
    fresh_body = await fresh.json()
    assert fresh_body["access_application_state"] == "pending"
    assert fresh_body["grants"][0]["desired_generation"] == "2"
    assert fresh_body["grants"][0]["state"] == "pending"


async def test_catalog_pending_admission_tokens_bootstrap_and_apply(catalog_env):
    client, database_url, settings, database = catalog_env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-boot")
    await fake.start()
    try:
        subject = await _seed_subject(database_url)
        await _add_gateway(database_url, "gw-boot", admin_socket=fake.socket_path)
        business_before = await _counts(database_url)

        # Initial subject with an active right and no applied grants: pending with admission
        # tokens, not an authoritative empty catalog, and no business side effect from GET.
        initial = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert initial.status == 409, await initial.text()
        initial_body = await initial.json()
        errors = sorted(
            _validator("errors.json", "ErrorResponse").iter_errors(initial_body),
            key=lambda e: e.path,
        )
        assert not errors, [error.message for error in errors]
        assert initial_body["code"] == "ACCESS_SYNC_PENDING"
        assert initial_body["retryable"] is True and "retry_after_ms" in initial_body
        tokens = initial_body["details"]
        assert tokens["binding_revision"] == "1"
        assert tokens["catalog_revision"] == await _current_revision(
            database_url, subject["installation_id"]
        )
        assert await _counts(database_url) == business_before

        # The tokens admit a NEW sync without any extra catalog round trip.
        sync = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-boot-01"},
            json={
                "catalog_revision": tokens["catalog_revision"],
                "binding_revision": tokens["binding_revision"],
            },
        )
        assert sync.status == 200, await sync.text()
        sync_body = await sync.json()
        assert sync_body["access_application_state"] == "pending"
        operation_id = sync_body["operation_id"]

        # Server-side pending without the local operation id: the catalog keeps signalling
        # nonterminal instead of an authoritative empty list.
        pending = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert pending.status == 409
        assert (await pending.json())["code"] == "ACCESS_SYNC_PENDING"

        handlers = GatewayControlHandlers(
            settings,
            client_factory=lambda _key, endpoints: GatewayAdminClient(
                socket_path=str(endpoints["admin_socket"]),
                main_password="fixture-main",
                timeout_seconds=5,
            ),
        ).as_handlers()
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="catalog-boot")
        assert await worker.drain() == 2

        applied = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert applied.status == 200, await applied.text()
        applied_body = await applied.json()
        assert [gateway["gateway_id"] for gateway in applied_body["gateways"]] == ["gw-boot"]
        status = await client.get(
            f"{MOBILE}/operations/{operation_id}", headers=_auth(subject["token"])
        )
        assert status.status == 200
    finally:
        await fake.stop()


async def test_catalog_application_failure_is_blocked_503(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    await _add_gateway(database_url, "gw-dead")
    revision = await _current_revision(database_url, subject["installation_id"])
    sync = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-dead-01"},
        json={"catalog_revision": revision, "binding_revision": "1"},
    )
    assert sync.status == 200, await sync.text()
    operation_id = uuid.UUID((await sync.json())["operation_id"])
    connection = await _connect(database_url)
    try:
        await connection.execute(
            """
            UPDATE outbox_operations SET status = 'dead', last_error = 'NODE_UNAVAILABLE'
            WHERE correlation_id = $1 AND id <> $1
            """,
            operation_id,
        )
        attempts_after_dead = await connection.fetchval(
            "SELECT attempts FROM outbox_operations WHERE correlation_id = $1 AND id <> $1",
            operation_id,
        )
    finally:
        await connection.close()
    business_before = await _counts(database_url)

    # An actual application failure is never an authoritative removal and never a catalog.
    failed = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert failed.status == 503, await failed.text()
    failed_body = await failed.json()
    errors = sorted(
        _validator("errors.json", "ErrorResponse").iter_errors(failed_body), key=lambda e: e.path
    )
    assert not errors, [error.message for error in errors]
    assert failed_body["code"] == "SERVICE_UNAVAILABLE"
    assert failed_body["retryable"] is True and "retry_after_ms" in failed_body
    assert failed_body["details"]["reason"] == "access_application_failed"
    tokens = failed_body["details"]
    assert isinstance(tokens["catalog_revision"], str) and isinstance(tokens["binding_revision"], str)
    assert "gateways" not in failed_body
    assert await _counts(database_url) == business_before

    # A NEW key must not silently restart or duplicate the failed application.
    blocked = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-dead-02"},
        json={
            "catalog_revision": tokens["catalog_revision"],
            "binding_revision": tokens["binding_revision"],
        },
    )
    assert blocked.status == 503, await blocked.text()
    assert (await blocked.json())["details"]["reason"] == "access_application_failed"
    assert await _counts(database_url) == business_before
    connection = await _connect(database_url)
    try:
        assert await connection.fetchval(
            "SELECT attempts FROM outbox_operations WHERE correlation_id = $1 AND id <> $1",
            operation_id,
        ) == attempts_after_dead
        assert await connection.fetchval(
            "SELECT count(*) FROM outbox_operations WHERE correlation_id = $1",
            operation_id,
        ) == 2
    finally:
        await connection.close()


async def test_gateways_empty_registry_is_not_removal(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    response = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert response.status == 503
    body = await response.json()
    assert body["code"] == "SERVICE_UNAVAILABLE"
    assert body["details"]["reason"] == "registry_descriptors_incomplete"
    assert "gateways" not in body


async def test_disabled_last_node_is_authoritative_empty(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    gateway = await _add_gateway(database_url, "gw-last")
    await _add_applied_grant(database_url, subject, gateway, credential="last-cred")
    served = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert served.status == 200
    served_body = await served.json()
    assert [gateway["gateway_id"] for gateway in served_body["gateways"]] == ["gw-last"]
    business_before = await _counts(database_url)

    # Supported removal lifecycle: a disabled registry tombstone (never inferred from revision).
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE gateways SET registry_state = 'disabled' WHERE id = $1", gateway
        )
    finally:
        await connection.close()
    removed = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert removed.status == 200, await removed.text()
    removed_body = await removed.json()
    assert removed_body["gateways"] == []
    assert int(removed_body["revision"]) > int(served_body["revision"])
    assert await _counts(database_url) == business_before

    # Repeated authoritative empty is stable and consistent.
    stable = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert stable.status == 200
    assert (await stable.json())["revision"] == removed_body["revision"]


async def test_registry_lifecycle_marker_covers_authoritative_removal(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    gateway = await _add_gateway(database_url, "gw-physical")
    pending = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert pending.status == 409  # no desired application yet

    connection = await _connect(database_url)
    try:
        # The authoritative registry lifecycle records the published environment; there is no
        # runtime registry-admin route in this Backend (documented), so this is the durable
        # marker a future/real lifecycle sets at publication time.
        await connection.execute(
            "INSERT INTO registry_environments (environment) VALUES ('test')"
        )
        await connection.execute("DELETE FROM gateways WHERE id = $1", gateway)
        await connection.execute(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, registry_state)
            VALUES ('gw-other-env', 'production', '{}'::jsonb, 'registered')
            """
        )
    finally:
        await connection.close()

    removed = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert removed.status == 200, await removed.text()
    removed_body = await removed.json()
    assert removed_body["gateways"] == []
    assert removed_body["revision"] == await _current_revision(
        database_url, subject["installation_id"]
    )
    stable = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert (await stable.json())["revision"] == removed_body["revision"]


async def test_malformed_registered_with_tombstone_is_whole_503(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    await _add_gateway(database_url, "gw-tombstone")
    await _add_gateway(database_url, "gw-malformed", overrides={"target_workers": 0})
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE gateways SET registry_state = 'disabled' WHERE gateway_key = 'gw-tombstone'"
        )
    finally:
        await connection.close()
    response = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert response.status == 503
    body = await response.json()
    assert body["details"]["reason"] == "registry_descriptors_incomplete"
    assert "gateways" not in body


async def test_linked_pop_session_admits_real_access_sync(catalog_env):
    client, database_url, _, _ = catalog_env
    key = KeyMaterial()
    pop_client = PoPClient(key, environment="test")
    challenge = await _challenge(client, key, "enrollment")
    enrollment = await _enroll(client, pop_client, challenge)
    assert enrollment.status == 200, await enrollment.text()

    # Isolated TEST fixture only: account/binding/entitlement rows plus the synthetic gateway.
    connection = await _connect(database_url)
    try:
        installation_id = await connection.fetchval(
            "SELECT id FROM installations WHERE public_key_fingerprint = $1", key.fingerprint
        )
        account_id = await connection.fetchval(
            "INSERT INTO accounts (status, telegram_id) VALUES ('verified', 700000041) RETURNING id"
        )
        binding_id = await connection.fetchval(
            """
            INSERT INTO account_bindings (account_id, installation_id, status)
            VALUES ($1, $2, 'active') RETURNING id
            """,
            account_id,
            installation_id,
        )
        await connection.execute(
            """
            INSERT INTO entitlements
                (account_id, kind, status, starts_at, ends_at, device_limit, revision)
            VALUES ($1, 'paid', 'active', now() - interval '1 hour',
                    now() + interval '30 days', 2, 1)
            """,
            account_id,
        )
    finally:
        await connection.close()
    gateway = await _add_gateway(database_url, "gw-linked")
    await _add_applied_grant(
        database_url,
        {"binding_id": binding_id, "fingerprint": key.fingerprint},
        gateway,
        credential="linked-cred",
    )

    session_challenge = await _challenge(client, key, "session")
    session = await _session(
        client,
        pop_client,
        session_challenge,
        ["session:read", "access:sync"],
        "linked-cat-00000001",
    )
    assert session.status == 200, await session.text()
    session_body = await session.json()
    assert session_body["session"]["account_ref"] == str(account_id)
    token = session_body["session"]["session_id"]
    headers = {"Authorization": f"Bearer {token}"}

    me = await client.get(f"{MOBILE}/me", headers=headers)
    assert me.status == 200, await me.text()
    me_body = await me.json()
    assert me_body["account_state"] == "ACTIVE_PAID"
    assert me_body["binding_revision"] == "1"

    catalog = await client.get(f"{MOBILE}/gateways", headers=headers)
    assert catalog.status == 200, await catalog.text()
    assert [item["gateway_id"] for item in (await catalog.json())["gateways"]] == ["gw-linked"]

    revision = await _current_revision(database_url, installation_id)
    sync = await client.post(
        f"{MOBILE}/access/sync",
        headers={**headers, "Idempotency-Key": "linked-cat-sync-01"},
        json={"catalog_revision": revision, "binding_revision": "1"},
    )
    assert sync.status == 200, await sync.text()
    assert (await sync.json())["access_application_state"] == "applied"


async def test_registry_node_identity_mismatch_is_rejected(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    await _add_gateway(database_url, "gw-identity", overrides={"node_id": "gw-other-node"})
    before = await _counts(database_url)

    response = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert response.status == 503, await response.text()
    body = await response.json()
    assert body["code"] == "SERVICE_UNAVAILABLE"
    assert body["details"]["reason"] == "registry_node_identity_mismatch"
    assert "gateways" not in body

    blocked = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-identity-01"},
        json={
            "catalog_revision": await _current_revision(
                database_url, subject["installation_id"]
            ),
            "binding_revision": "1",
        },
    )
    assert blocked.status == 503
    assert (await blocked.json())["details"]["reason"] == "registry_node_identity_mismatch"
    assert await _counts(database_url) == before


async def test_catalog_publishes_only_confirmed_generation_lease_seq_pair(catalog_env):
    client, database_url, settings, database = catalog_env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-pair")
    await fake.start()
    try:
        subject = await _seed_subject(database_url)
        gateway = await _add_gateway(database_url, "gw-pair", admin_socket=fake.socket_path)
        revision = await _current_revision(database_url, subject["installation_id"])
        sync = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-pair-01"},
            json={"catalog_revision": revision, "binding_revision": "1"},
        )
        assert sync.status == 200, await sync.text()

        # Pending: no verified admission and no lease_seq pair in the catalog response.
        pending = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert pending.status == 409
        assert "lease_seq" not in json.dumps(await pending.json())

        handlers = GatewayControlHandlers(
            settings,
            client_factory=lambda _key, endpoints: GatewayAdminClient(
                socket_path=str(endpoints["admin_socket"]),
                main_password="fixture-main",
                timeout_seconds=5,
            ),
        ).as_handlers()
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="catalog-pair")
        assert await worker.drain() == 2

        applied = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert applied.status == 200, await applied.text()
        applied_body = await applied.json()
        errors = sorted(
            _validator("catalog.json", "CatalogResponse").iter_errors(applied_body),
            key=lambda e: e.path,
        )
        assert not errors, [error.message for error in errors]
        access = applied_body["gateways"][0]["access"]
        assert access["generation"] == "1" and access["lease_seq"] == "1"
        assert applied_body["gateways"][0]["target_workers"] == 36
        confirmed = await connection_fetchval(
            database_url, f"SELECT confirmed_max_workers FROM gateways WHERE id = '{gateway}'"
        ) if False else None
        connection = await _connect(database_url)
        try:
            confirmed = await connection.fetchval(
                "SELECT confirmed_max_workers FROM gateways WHERE id = $1", gateway
            )
            await connection.execute(
                "UPDATE grants SET not_after = now() - interval '1 second' WHERE gateway_id = $1",
                gateway,
            )
        finally:
            await connection.close()
        assert confirmed == 36

        # A refresh moves the lease sequence; the pending phase publishes nothing new.
        refresh_probe = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert refresh_probe.status == 409
        refresh_revision = (await refresh_probe.json())["details"]["catalog_revision"]
        refresh_sync = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-pair-02"},
            json={"catalog_revision": refresh_revision, "binding_revision": "1"},
        )
        assert refresh_sync.status == 200, await refresh_sync.text()
        pending_again = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert pending_again.status == 409
        assert await worker.drain() == 2
        refreshed = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert refreshed.status == 200, await refreshed.text()
        refreshed_access = (await refreshed.json())["gateways"][0]["access"]
        # access.generation is the node wire (auth) generation, not the management generation:
        # the refresh moved desired/applied to 2 but the node auth generation is still 1.
        assert refreshed_access["generation"] == "1"
        assert refreshed_access["lease_seq"] == "2"
        connection = await _connect(database_url)
        try:
            refreshed_row = await connection.fetchrow(
                "SELECT applied_generation, gateway_generation FROM grants WHERE gateway_id = $1",
                gateway,
            )
        finally:
            await connection.close()
        assert int(refreshed_row["applied_generation"]) == 2
        assert int(refreshed_row["gateway_generation"]) == 1
    finally:
        await fake.stop()


async def test_unconfirmed_worker_cap_self_heals_through_sync(catalog_env):
    client, database_url, settings, database = catalog_env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-cap")
    await fake.start()
    try:
        subject = await _seed_subject(database_url)
        gateway = await _add_gateway(database_url, "gw-cap", admin_socket=fake.socket_path)
        revision = await _current_revision(database_url, subject["installation_id"])
        sync = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-cap-0002"},
            json={"catalog_revision": revision, "binding_revision": "1"},
        )
        assert sync.status == 200, await sync.text()
        handlers = GatewayControlHandlers(
            settings,
            client_factory=lambda _key, endpoints: GatewayAdminClient(
                socket_path=str(endpoints["admin_socket"]),
                main_password="fixture-main",
                timeout_seconds=5,
            ),
        ).as_handlers()
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="catalog-cap")
        assert await worker.drain() == 2

        # Simulate a legacy applied row without a readback-confirmed cap: pending, not removal.
        connection = await _connect(database_url)
        try:
            await connection.execute(
                "UPDATE gateways SET confirmed_max_workers = NULL WHERE id = $1", gateway
            )
        finally:
            await connection.close()
        pending = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert pending.status == 409
        assert (await pending.json())["code"] == "ACCESS_SYNC_PENDING"

        # One sync forces exactly one verification refresh; the confirmed cap returns.
        revision = (await pending.json())["details"]["catalog_revision"]
        resync = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-cap-0003"},
            json={"catalog_revision": revision, "binding_revision": "1"},
        )
        assert resync.status == 200, await resync.text()
        assert await worker.drain() == 2
        connection = await _connect(database_url)
        try:
            confirmed = await connection.fetchval(
                "SELECT confirmed_max_workers FROM gateways WHERE id = $1", gateway
            )
        finally:
            await connection.close()
        assert confirmed == 36
        applied = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert applied.status == 200, await applied.text()
    finally:
        await fake.stop()


async def test_mismatched_worker_cap_gets_no_admission(catalog_env):
    client, database_url, settings, database = catalog_env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-cap-bad", max_workers=18)
    await fake.start()
    try:
        subject = await _seed_subject(database_url)
        gateway = await _add_gateway(database_url, "gw-cap-bad", admin_socket=fake.socket_path)
        revision = await _current_revision(database_url, subject["installation_id"])
        sync = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "sync-key-cap-0004"},
            json={"catalog_revision": revision, "binding_revision": "1"},
        )
        assert sync.status == 200, await sync.text()
        handlers = GatewayControlHandlers(
            settings,
            client_factory=lambda _key, endpoints: GatewayAdminClient(
                socket_path=str(endpoints["admin_socket"]),
                main_password="fixture-main",
                timeout_seconds=5,
            ),
        ).as_handlers()
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="catalog-cap-bad")
        assert await worker.drain() >= 1
        connection = await _connect(database_url)
        try:
            last_error = await connection.fetchval(
                "SELECT last_error FROM outbox_operations WHERE operation_type = 'gateway.apply_grant'"
            )
            confirmed = await connection.fetchval(
                "SELECT confirmed_max_workers FROM gateways WHERE id = $1", gateway
            )
        finally:
            await connection.close()
        assert "worker_cap_mismatch" in (last_error or "")
        assert confirmed is None

        failed = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert failed.status == 503
        failed_body = await failed.json()
        assert failed_body["details"]["reason"] == "access_application_failed"
        assert "lease_seq" not in json.dumps(failed_body)
    finally:
        await fake.stop()


async def test_desired_revoke_without_readback_is_not_removal(catalog_env):
    client, database_url, _, _ = catalog_env
    subject = await _seed_subject(database_url)
    gateway = await _add_gateway(database_url, "gw-revoke-pending")
    await _add_applied_grant(database_url, subject, gateway, credential="rev-pending-cred")
    served = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert served.status == 200

    # Desired DB revoke without a confirmed readback is neither an authoritative removal nor a
    # resurrected active grant.
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE grants SET state = 'revoked', last_readback = NULL WHERE gateway_id = $1",
            gateway,
        )
    finally:
        await connection.close()
    unconfirmed = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert unconfirmed.status == 503, await unconfirmed.text()
    unconfirmed_body = await unconfirmed.json()
    assert unconfirmed_body["details"]["reason"] == "access_application_failed"
    assert "gateways" not in unconfirmed_body

    # A confirmed readback makes the same absence authoritative.
    connection = await _connect(database_url)
    try:
        await connection.execute(
            """
            UPDATE grants SET applied_generation = desired_generation,
                              last_readback = '{"revoked": true}'::jsonb
            WHERE gateway_id = $1
            """,
            gateway,
        )
    finally:
        await connection.close()
    removed = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    assert removed.status == 200, await removed.text()
    assert (await removed.json())["gateways"] == []


async def _apply_once(client, database_url, settings, database, gateway_key, subject, fake, key):
    probe = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
    probe_body = await probe.json()
    if probe.status == 200:
        revision = probe_body["revision"]
    elif probe.status == 409:
        revision = probe_body["details"]["catalog_revision"]
    else:
        revision = await _current_revision(database_url, subject["installation_id"])
    sync = await client.post(
        f"{MOBILE}/access/sync",
        headers={**_auth(subject["token"]), "Idempotency-Key": key},
        json={"catalog_revision": revision, "binding_revision": "1"},
    )
    assert sync.status == 200, await sync.text()
    handlers = GatewayControlHandlers(
        settings,
        client_factory=lambda _key, endpoints: GatewayAdminClient(
            socket_path=str(endpoints["admin_socket"]),
            main_password="fixture-main",
            timeout_seconds=5,
        ),
    ).as_handlers()
    worker = OutboxWorker(database, settings, handlers=handlers, worker_id=f"vk-{gateway_key}")
    assert await worker.drain() >= 1
    return sync


async def test_catalog_emits_confirmed_node_vk_hashes(catalog_env):
    client, database_url, settings, database = catalog_env
    fake = FakeGatewayAdmin(
        main_password="fixture-main",
        node_id="gw-vk",
        vk_hashes=["vk-hash-alpha", "vk-hash-beta"],
    )
    await fake.start()
    try:
        subject = await _seed_subject(database_url)
        gateway = await _add_gateway(database_url, "gw-vk", admin_socket=fake.socket_path)
        await _apply_once(
            client, database_url, settings, database, "gw-vk", subject, fake, "vk-sync-00000001"
        )
        connection = await _connect(database_url)
        try:
            stored = await connection.fetchval(
                "SELECT vk_hashes FROM gateways WHERE id = $1", gateway
            )
        finally:
            await connection.close()
        if isinstance(stored, str):
            stored = json.loads(stored)
        assert stored == ["vk-hash-alpha", "vk-hash-beta"]
        catalog = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        body = await catalog.json()
        errors = sorted(
            _validator("catalog.json", "CatalogResponse").iter_errors(body), key=lambda e: e.path
        )
        assert not errors, [error.message for error in errors]
        assert body["gateways"][0]["access"]["vk_hashes"] == ["vk-hash-alpha", "vk-hash-beta"]
    finally:
        await fake.stop()


async def test_vk_snapshot_absence_malformed_and_clearing(catalog_env):
    client, database_url, settings, database = catalog_env
    subject = await _seed_subject(database_url)

    async def stored_for(gateway):
        connection = await _connect(database_url)
        try:
            value = await connection.fetchval(
                "SELECT vk_hashes FROM gateways WHERE id = $1", gateway
            )
        finally:
            await connection.close()
        return json.loads(value) if isinstance(value, str) else value

    old_node = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-old", vk_hashes=None)
    await old_node.start()
    try:
        gateway = await _add_gateway(database_url, "gw-old", admin_socket=old_node.socket_path)
        connection = await _connect(database_url)
        try:
            await connection.execute(
                "UPDATE gateways SET vk_hashes = '[\"old-hash\"]'::jsonb WHERE id = $1", gateway
            )
        finally:
            await connection.close()
        await _apply_once(
            client, database_url, settings, database, "gw-old", subject, old_node, "vk-sync-00000002"
        )
        assert await stored_for(gateway) == ["old-hash"]  # absent field never overwrites
    finally:
        await old_node.stop()

    malformed = FakeGatewayAdmin(
        main_password="fixture-main", node_id="gw-bad", vk_hashes=[f"h{i}" for i in range(9)]
    )
    await malformed.start()
    try:
        gateway = await _add_gateway(database_url, "gw-bad", admin_socket=malformed.socket_path)
        connection = await _connect(database_url)
        try:
            await connection.execute(
                "UPDATE gateways SET vk_hashes = '[\"keep\"]'::jsonb WHERE id = $1", gateway
            )
        finally:
            await connection.close()
        await _apply_once(
            client, database_url, settings, database, "gw-bad", subject, malformed, "vk-sync-00000003"
        )
        assert await stored_for(gateway) == ["keep"]  # malformed (>8) keeps last-good
    finally:
        await malformed.stop()

    failing = FakeGatewayAdmin(
        main_password="fixture-main", node_id="gw-fail", fail_engine_status=True
    )
    await failing.start()
    try:
        gateway = await _add_gateway(database_url, "gw-fail", admin_socket=failing.socket_path)
        connection = await _connect(database_url)
        try:
            await connection.execute(
                "UPDATE gateways SET vk_hashes = '[\"keep2\"]'::jsonb WHERE id = $1", gateway
            )
        finally:
            await connection.close()
        await _apply_once(
            client, database_url, settings, database, "gw-fail", subject, failing, "vk-sync-00000004"
        )
        assert await stored_for(gateway) == ["keep2"]  # rejected readback keeps last-good
    finally:
        await failing.stop()

    cleared = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-clear", vk_hashes=[])
    await cleared.start()
    try:
        gateway = await _add_gateway(database_url, "gw-clear", admin_socket=cleared.socket_path)
        connection = await _connect(database_url)
        try:
            await connection.execute(
                "UPDATE gateways SET vk_hashes = '[\"to-clear\"]'::jsonb WHERE id = $1", gateway
            )
        finally:
            await connection.close()
        await _apply_once(
            client, database_url, settings, database, "gw-clear", subject, cleared, "vk-sync-00000005"
        )
        assert await stored_for(gateway) == []  # confirmed empty clears the old snapshot
        catalog = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        body = await catalog.json()
        cleared_entry = next(gw for gw in body["gateways"] if gw["gateway_id"] == "gw-clear")
        assert "vk_hashes" not in cleared_entry["access"]
    finally:
        await cleared.stop()


async def _wire_env(catalog_env, key: str):
    _client, database_url, settings, database = catalog_env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id=key)
    await fake.start()
    subject = await _seed_subject(database_url)
    gateway = await _add_gateway(database_url, key, admin_socket=fake.socket_path)
    handlers = GatewayControlHandlers(
        settings,
        client_factory=lambda _key, endpoints: GatewayAdminClient(
            socket_path=str(endpoints["admin_socket"]),
            main_password="fixture-main",
            timeout_seconds=5,
        ),
    ).as_handlers()
    worker = OutboxWorker(database, settings, handlers=handlers, worker_id=f"wire-{key}")
    return fake, subject, gateway, worker


async def _expire_grant(database_url: str, gateway) -> None:
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE grants SET not_after = now() - interval '1 second' WHERE gateway_id = $1",
            gateway,
        )
    finally:
        await connection.close()


async def _grant_generations(database_url: str, gateway) -> dict:
    connection = await _connect(database_url)
    try:
        row = await connection.fetchrow(
            """
            SELECT desired_generation, applied_generation, gateway_generation, lease_seq
            FROM grants WHERE gateway_id = $1
            """,
            gateway,
        )
        return {name: int(value) for name, value in row.items() if value is not None}
    finally:
        await connection.close()


async def test_catalog_generation_is_node_wire_generation_across_refreshes(catalog_env):
    """Provision gen1 + two refreshes: desired/applied=3, node auth gen stays 1, catalog
    advertises exactly the node wire generation 1 (challenge-compatible), lease_seq=3."""
    client, database_url, _, _ = catalog_env
    fake, subject, gateway, worker = await _wire_env(catalog_env, "gw-wire")
    try:
        revision = await _current_revision(database_url, subject["installation_id"])
        sync = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "sync-wire-000001"},
            json={"catalog_revision": revision, "binding_revision": "1"},
        )
        assert sync.status == 200, await sync.text()
        assert await worker.drain() == 2
        first = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert first.status == 200, await first.text()
        first_access = (await first.json())["gateways"][0]["access"]
        assert first_access["generation"] == "1" and first_access["lease_seq"] == "1"

        for refresh, idem in ((2, "sync-wire-000002"), (3, "sync-wire-000003")):
            await _expire_grant(database_url, gateway)
            probe = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
            assert probe.status == 409
            probe_revision = (await probe.json())["details"]["catalog_revision"]
            sync = await client.post(
                f"{MOBILE}/access/sync",
                headers={**_auth(subject["token"]), "Idempotency-Key": idem},
                json={"catalog_revision": probe_revision, "binding_revision": "1"},
            )
            assert sync.status == 200, await sync.text()
            assert await worker.drain() == 2, f"refresh {refresh} did not apply+profile"
            assert (await _grant_generations(database_url, gateway))["applied_generation"] == refresh

        assert (await _grant_generations(database_url, gateway)) == {
            "desired_generation": 3,
            "applied_generation": 3,
            "gateway_generation": 1,
            "lease_seq": 3,
        }
        node_entry = next(iter(fake.grants.values()))
        assert node_entry["generation"] == 1, "fake node must not auto-raise its auth generation"

        served = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert served.status == 200, await served.text()
        body = await served.json()
        errors = sorted(
            _validator("catalog.json", "CatalogResponse").iter_errors(body),
            key=lambda e: e.path,
        )
        assert not errors, [error.message for error in errors]
        access = body["gateways"][0]["access"]
        assert access["generation"] == "1"
        assert access["lease_seq"] == "3"
        assert access["generation"] == str(node_entry["generation"])
        assert access["generation"] != "3", "management generation leaked into the wire field"
    finally:
        await fake.stop()


async def test_catalog_missing_or_invalid_wire_generation_is_pending_fail_closed(catalog_env):
    """Absent/zero/invalid gateway_generation never becomes a false confirmed descriptor and
    never falls back to applied_generation: the catalog stays whole-response pending."""
    client, database_url, _, _ = catalog_env
    fake, subject, gateway, worker = await _wire_env(catalog_env, "gw-wire-bad")
    try:
        revision = await _current_revision(database_url, subject["installation_id"])
        sync = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(subject["token"]), "Idempotency-Key": "sync-wire-bad-0001"},
            json={"catalog_revision": revision, "binding_revision": "1"},
        )
        assert sync.status == 200, await sync.text()
        assert await worker.drain() == 2
        served = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert served.status == 200
        assert (await served.json())["gateways"][0]["access"]["generation"] == "1"

        connection = await _connect(database_url)
        try:
            # The column is NOT NULL DEFAULT 0 (0005): absence is 0, plus invalid negatives.
            for bad in (0, -1):
                await connection.execute(
                    "UPDATE grants SET gateway_generation = $2 WHERE gateway_id = $1",
                    gateway,
                    bad,
                )
                pending = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
                assert pending.status == 409, f"bad wire generation {bad} was advertised"
                payload = await pending.json()
                assert payload["code"] == "ACCESS_SYNC_PENDING"
                assert "lease_seq" not in json.dumps(payload), (
                    f"bad wire generation {bad} produced a descriptor"
                )
        finally:
            await connection.execute(
                "UPDATE grants SET gateway_generation = 1 WHERE gateway_id = $1", gateway
            )
            await connection.close()

        recovered = await client.get(f"{MOBILE}/gateways", headers=_auth(subject["token"]))
        assert recovered.status == 200
        assert (await recovered.json())["gateways"][0]["access"]["generation"] == "1"
    finally:
        await fake.stop()


def test_wire_generation_guard_is_none_for_absent_or_invalid_values():
    from terlimo_backend.mobile_catalog import _wire_generation

    assert _wire_generation({"gateway_generation": 1}) == "1"
    assert _wire_generation({"gateway_generation": 7}) == "7"
    assert _wire_generation({"gateway_generation": 0}) is None
    assert _wire_generation({"gateway_generation": -3}) is None
    assert _wire_generation({"gateway_generation": None}) is None
    assert _wire_generation({"gateway_generation": "x"}) is None
