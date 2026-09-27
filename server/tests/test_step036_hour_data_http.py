"""Real-auth HTTP chain for the installation-scoped onboarding_hour data subject.

Uses the accepted enrollment/challenge/session and explicit-connect/start handlers (synthetic
PoP keys, ephemeral certs, isolated TEST PostgreSQL). No manual entitlement/session inserts:
the hour entitlement is created only by the real Start (evidence) handler, and access:sync is
issued only after that Start.
"""
from __future__ import annotations

import secrets

from aiohttp import ClientSession, TCPConnector
from fake_gateway_admin import FakeGatewayAdmin
from test_auth_flow import _challenge, _session
from test_catalog_access_sync import (
    MOBILE,
    GatewayControlHandlers,
    OutboxWorker,
    _auth,
)
from test_step036_explicit_connect import (
    BACKGROUND_BODY,  # noqa: F401  (kept for parity with the accepted harness)
    GATEWAY_KEY,
    _enrolled_session,
    _post_start,
    _ready_intent,
    _start_envelope,
    _start_stack,
)
from test_step036_onboarding_hour_storage import _connect, _seed_gateway

from terlimo_backend.db import Database
from terlimo_backend.evidence_transport import EVIDENCE_LISTENER_KEY
from terlimo_backend.gateway_adapter import GatewayAdminClient
from terlimo_backend.onboarding_hour import expire_hour_units, revoke_hour_unit


async def _request_session(client, key, pop_client, scopes):
    return await _session(
        client,
        pop_client,
        await _challenge(client, key, "session"),
        list(scopes),
        f"idem-{secrets.token_hex(8)}",
    )


async def _current_subject_revision(url: str, installation_ref: str) -> str:
    connection = await _connect(url)
    try:
        value = await connection.fetchval(
            "SELECT revision FROM catalog_revisions WHERE installation_id = "
            "(SELECT id FROM installations WHERE public_key_fingerprint = $1)",
            installation_ref,
        )
        return str(value) if value is not None else "1"
    finally:
        await connection.close()


async def test_hour_data_subject_real_auth_chain(migrated_url, settings_factory, tmp_path):
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id=GATEWAY_KEY)
    await fake.start()
    client, settings, material = await _start_stack(migrated_url, settings_factory, tmp_path)
    try:
        _gid, endpoints = await _seed_gateway(migrated_url, key=GATEWAY_KEY)
        endpoints["admin_socket"] = fake.socket_path
        connection = await _connect(migrated_url)
        try:
            await connection.execute(
                "UPDATE gateways SET endpoints = $2::jsonb WHERE gateway_key = $1",
                GATEWAY_KEY,
                endpoints,
            )
        finally:
            await connection.close()
        key, pop_client, token = await _enrolled_session(client)

        # Before Start: the preferred access:sync scope is refused for an unlinked installation.
        denied = await _request_session(client, key, pop_client, ("session:read", "access:sync"))
        assert denied.status == 403, await denied.text()
        assert (await denied.json())["code"] == "ACCESS_DENIED"
        management = await _request_session(
            client, key, pop_client, ("session:read", "management-only")
        )
        assert management.status == 200, await management.text()
        management_token = (await management.json())["session"]["session_id"]

        # Real Start creates the installation-scoped hour.
        intent_id, ready = await _ready_intent(
            client, pop_client, token, migrated_url, settings.onboarding_secret_key,
            request_key="hour-http-1",
        )
        node_session = ClientSession(connector=TCPConnector(ssl=material.node_context()))
        try:
            port = client.server.app[EVIDENCE_LISTENER_KEY].port
            envelope = _start_envelope(
                ready["credential_id"], pop_client,
                intent_id=intent_id, request_key="hour-http-1",
                challenge=ready["start_challenge"],
            )
            status, started = await _post_start(node_session, port, envelope)
            assert status == 200 and started["state"] == "started"
        finally:
            await node_session.close()

        # After Start: a fresh preferred session (no management-only) is issued.
        preferred = await _request_session(
            client, key, pop_client, ("session:read", "session:write", "access:sync")
        )
        assert preferred.status == 200, await preferred.text()
        data_token = (await preferred.json())["session"]["session_id"]

        # The old management-only token still receives no data and cannot sync.
        browse = await client.get(f"{MOBILE}/gateways", headers=_auth(management_token))
        assert browse.status == 200 and (await browse.json())["catalog_mode"] == "browse"
        denied_sync = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(management_token), "Idempotency-Key": "hour-http-mgmt-1"},
            json={"catalog_revision": "1", "binding_revision": "1"},
        )
        assert denied_sync.status == 403, await denied_sync.text()

        me_active = await client.get(f"{MOBILE}/me", headers=_auth(data_token))
        assert me_active.status == 200, await me_active.text()
        me_active_body = await me_active.json()
        assert me_active_body["grant_resolution"]["data_access"] == "onboarding_hour"

        # Pending -> sync/worker -> credential catalog through the real API.
        pending = await client.get(f"{MOBILE}/gateways", headers=_auth(data_token))
        assert pending.status == 409, await pending.text()
        pending_body = await pending.json()
        assert pending_body["code"] == "ACCESS_SYNC_PENDING"
        tokens = pending_body["details"]
        sync = await client.post(
            f"{MOBILE}/access/sync",
            headers={**_auth(data_token), "Idempotency-Key": "hour-http-sync-1"},
            json={
                "catalog_revision": tokens["catalog_revision"],
                "binding_revision": tokens["binding_revision"],
            },
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
        database = Database(settings)
        await database.ensure_ready()
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="hour-http")
        await worker.drain()
        catalog = await client.get(f"{MOBILE}/gateways", headers=_auth(data_token))
        catalog_body = await catalog.json()
        assert catalog.status == 200 and catalog_body["gateways"][0]["access"]["grant_id"]
        assert catalog_body["gateways"][0]["transport"]["protocol"] == "wdtt-v17"

        # Real entitlement revoke (not bootstrap cleanup) closes the data path.
        connection = await _connect(migrated_url)
        try:
            entitlement_id = await connection.fetchval(
                "SELECT id FROM entitlements WHERE kind = 'onboarding_hour'"
            )
            installation_id = await connection.fetchval(
                "SELECT installation_id FROM entitlements WHERE id = $1", entitlement_id
            )
        finally:
            await connection.close()
        async with database.acquire() as writer:
            async with writer.transaction():
                assert await revoke_hour_unit(
                    writer, installation_id=installation_id, entitlement_id=entitlement_id
                )
            await worker.drain()
        await database.close()
        after = await client.get(f"{MOBILE}/gateways", headers=_auth(data_token))
        after_body = await after.json()
        assert after.status == 200 and after_body.get("catalog_mode") == "browse"

        # After revoke a fresh access:sync session is refused again.
        refused = await _request_session(client, key, pop_client, ("session:read", "access:sync"))
        assert refused.status == 403, await refused.text()

        # Expiry writer remains idempotent when the hour is already revoked.
        connection = await _connect(migrated_url)
        try:
            async with connection.transaction():
                assert await expire_hour_units(connection, limit=5) == 0
        finally:
            await connection.close()

        # Commit the actual handler responses as fixtures (synthetic data only; the marker
        # lives in a separate manifest, never inside the wire JSON).
        import json as _json
        import pathlib

        fixtures = pathlib.Path(__file__).parent / "fixtures"
        fixtures.mkdir(exist_ok=True)
        for name, payload in (
            ("step036_hour_me_active.json", me_active_body),
            ("step036_hour_gateways_pending.json", pending_body),
            ("step036_hour_gateways_data.json", catalog_body),
            ("step036_hour_gateways_expired.json", after_body),
            ("step036_hour_me_expired.json", await (await client.get(f"{MOBILE}/me", headers=_auth(data_token))).json()),
        ):
            (fixtures / name).write_text(_json.dumps(payload, indent=2, sort_keys=True) + "\n")
        (fixtures / "step036_hour_manifest.json").write_text(
            _json.dumps(
                {
                    "_synthetic": True,
                    "_generated_by": "test_hour_data_subject_real_auth_chain",
                    "_note": "Wire files are unmodified handler output from the real auth/start chain; values are synthetic TEST markers.",
                    "_contract": "binding_revision numeric slot: binding generation (paid) or stable hour entitlement revision (hour); changes on incarnation/revoke.",
                },
                indent=2,
                sort_keys=True,
            )
            + "\n"
        )
    finally:
        await fake.stop()
