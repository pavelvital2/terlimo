"""payment:write scope issuance: only a verified linked account session gets it; it is
preactivation (no entitlement needed), grants no data access, and existing onboarding_hour /
access:sync semantics are unchanged. Isolated migrated PostgreSQL."""
from __future__ import annotations

import uuid

from test_auth_flow import KeyMaterial, PoPClient, _challenge, _enroll, _session
from test_step036_onboarding_hour_storage import _connect
from test_s5_checkout_owner import StubProvider
from test_s5_checkout_route import _stack


async def _pop(client) -> PoPClient:
    key = KeyMaterial(); pop = PoPClient(key)
    assert (await _enroll(client, pop, await _challenge(client, key, "enrollment"))).status == 200
    return pop


async def _request_session(client, pop, scopes):
    return await _session(client, pop, await _challenge(client, pop.key, "session"), scopes, f"idem-{uuid.uuid4().hex}")


async def test_verified_linked_preactivation_gets_payment_write(migrated_url, settings_factory):
    client, _s, database = await _stack(settings_factory, migrated_url, StubProvider())
    try:
        pop = await _pop(client)
        c = await _connect(migrated_url)
        try:
            iid = await c.fetchval("SELECT id FROM installations WHERE public_key_fingerprint=$1", pop.key.fingerprint)
            acc = await c.fetchval("INSERT INTO accounts (status, telegram_id) VALUES ('verified',$1) RETURNING id", 892001)
            await c.execute("INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')", acc, iid)
        finally:
            await c.close()
        r = await _request_session(client, pop, ["session:read", "session:write", "payment:write"])
        assert r.status == 200, await r.text()
        scopes = set((await r.json())["session"]["scopes"])
        assert "payment:write" in scopes
        assert "access:sync" not in scopes  # data access is not implied and was not requested
        c = await _connect(migrated_url)
        try:
            assert await c.fetchval("SELECT count(*) FROM entitlements") == 0
        finally:
            await c.close()
    finally:
        await database.close()
        await client.close()


async def test_unlinked_does_not_get_payment_write(migrated_url, settings_factory):
    client, _s, database = await _stack(settings_factory, migrated_url, StubProvider())
    try:
        pop = await _pop(client)
        r = await _request_session(client, pop, ["session:read", "payment:write"])
        assert r.status == 403, await r.text()
        assert (await r.json())["code"] == "ACCESS_DENIED"
    finally:
        await database.close()
        await client.close()


async def test_revoked_and_unverified_links_do_not_get_payment_write(migrated_url, settings_factory):
    client, _s, database = await _stack(settings_factory, migrated_url, StubProvider())
    try:
        pop_revoked = await _pop(client)
        c = await _connect(migrated_url)
        try:
            iid = await c.fetchval("SELECT id FROM installations WHERE public_key_fingerprint=$1", pop_revoked.key.fingerprint)
            acc = await c.fetchval("INSERT INTO accounts (status, telegram_id) VALUES ('verified',$1) RETURNING id", 892002)
            await c.execute("INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'revoked')", acc, iid)
        finally:
            await c.close()
        revoked = await _request_session(client, pop_revoked, ["session:read", "payment:write"])
        assert revoked.status == 403, await revoked.text()
        assert (await revoked.json())["code"] == "ACCESS_DENIED"

        pop_unverified = await _pop(client)
        c = await _connect(migrated_url)
        try:
            iid = await c.fetchval("SELECT id FROM installations WHERE public_key_fingerprint=$1", pop_unverified.key.fingerprint)
            acc = await c.fetchval("INSERT INTO accounts (status, telegram_id) VALUES ('unlinked',$1) RETURNING id", 892003)
            await c.execute("INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')", acc, iid)
        finally:
            await c.close()
        unverified = await _request_session(client, pop_unverified, ["session:read", "payment:write"])
        assert unverified.status == 403, await unverified.text()
        assert (await unverified.json())["code"] == "ACCESS_DENIED"
    finally:
        await database.close()
        await client.close()


async def test_access_sync_semantics_unchanged(migrated_url, settings_factory):
    client, _s, database = await _stack(settings_factory, migrated_url, StubProvider())
    try:
        # unlinked without an authoritative onboarding_hour: access:sync stays denied
        pop = await _pop(client)
        denied = await _request_session(client, pop, ["session:read", "access:sync"])
        assert denied.status == 403, await denied.text()
        assert (await denied.json())["code"] == "ACCESS_DENIED"

        # verified linked session may still request access:sync together with payment:write
        pop2 = await _pop(client)
        c = await _connect(migrated_url)
        try:
            iid = await c.fetchval("SELECT id FROM installations WHERE public_key_fingerprint=$1", pop2.key.fingerprint)
            acc = await c.fetchval("INSERT INTO accounts (status, telegram_id) VALUES ('verified',$1) RETURNING id", 892004)
            await c.execute("INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')", acc, iid)
        finally:
            await c.close()
        both = await _request_session(client, pop2, ["session:read", "access:sync", "payment:write"])
        assert both.status == 200, await both.text()
        scopes = set((await both.json())["session"]["scopes"])
        assert {"access:sync", "payment:write"} <= scopes
    finally:
        await database.close()
        await client.close()
