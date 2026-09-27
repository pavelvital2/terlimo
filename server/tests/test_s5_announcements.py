"""S5 announcements core: durable one-way messages, unread view, idempotent read marker."""
from __future__ import annotations

import json
import os
import uuid

import pytest
from aiohttp.test_utils import TestClient, TestServer

from test_auth_flow import MOBILE, KeyMaterial, PoPClient, _challenge, _enroll, _session as _auth_session
from test_step036_onboarding_hour_storage import _connect

from terlimo_backend.api import create_app
from terlimo_backend.db import Database

ANN = f"{MOBILE}/announcements"


def _hdr(token, key=None):
    return {"Authorization": f"Bearer {token}", "Idempotency-Key": key or f"idem-{uuid.uuid4().hex}"}


def _approved_announcement_schemas():
    repo = os.environ.get("TERLIMO_CONTRACTS_DIR", "/tmp/opencode/contracts-perpetual")
    ann_path = os.path.join(repo, "schemas", "announcement.json")
    common_path = os.path.join(repo, "schemas", "common.json")
    if not os.path.exists(ann_path) or not os.path.exists(common_path):
        pytest.skip(f"approved contracts unavailable at {repo}")
    return json.load(open(ann_path)), json.load(open(common_path))


def _approved_validator(def_name):
    jsonschema = pytest.importorskip("jsonschema")
    pytest.importorskip("referencing")
    from referencing import Registry, Resource

    ann, common = _approved_announcement_schemas()
    registry = Registry().with_resources([
        ("common.json", Resource.from_contents(common)),
        ("https://terlimo.local/schemas/common.json", Resource.from_contents(common)),
        ("announcement.json", Resource.from_contents(ann)),
        ("https://terlimo.local/schemas/announcement.json", Resource.from_contents(ann)),
    ])
    root = {"$ref": ann["$id"] + "#/$defs/" + def_name}
    return jsonschema.Draft202012Validator(root, registry=registry)


def _settings(settings_factory, url):
    return settings_factory(url)


async def _app(settings_factory, migrated_url):
    settings = _settings(settings_factory, migrated_url)
    database = Database(settings)
    client = TestClient(TestServer(create_app(settings, database)))
    await client.start_server()
    return client, settings, database


async def _session(client, migrated_url, *, bind_tg=None, scopes=("session:read",)):
    key = KeyMaterial(); pop = PoPClient(key)
    assert (await _enroll(client, pop, await _challenge(client, key, "enrollment"))).status == 200
    if bind_tg is not None:
        c = await _connect(migrated_url)
        try:
            iid = await c.fetchval("SELECT id FROM installations WHERE public_key_fingerprint=$1", pop.key.fingerprint)
            acc = await c.fetchval("INSERT INTO accounts (status, telegram_id) VALUES ('verified',$1) RETURNING id", bind_tg)
            await c.execute("INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active')", acc, iid)
        finally:
            await c.close()
    s = await _session_impl(client, pop, scopes)
    assert s.status == 200, await s.text()
    return (await s.json())["session"]["session_id"]


async def _session_impl(client, pop, scopes):
    return await _auth_session(client, pop, await _challenge(client, pop.key, "session"), list(scopes), f"idem-{uuid.uuid4().hex}")


async def _insert_announcement(migrated_url, settings, *, text, action=None, show_until=None):
    c = await _connect(migrated_url)
    try:
        return await c.fetchval(
            "INSERT INTO announcements (environment, text, action, show_until) VALUES ($1,$2,$3::jsonb,$4) RETURNING id",
            settings.environment, text, json.dumps(action) if action is not None else None, show_until,
        )
    finally:
        await c.close()


async def test_unread_list_expiry_action_and_strict_shape(migrated_url, settings_factory):
    client, settings, database = await _app(settings_factory, migrated_url)
    try:
        token = await _session(client, migrated_url, bind_tg=991001)
        live = await _insert_announcement(migrated_url, settings, text="hello", action={"kind": "refresh_catalog"})
        expired = await _insert_announcement(migrated_url, settings, text="old", show_until=None)
        c = await _connect(migrated_url)
        try:
            await c.execute("UPDATE announcements SET show_until = now() - interval '1 minute' WHERE id=$1", expired)
        finally:
            await c.close()
        evil = await _insert_announcement(migrated_url, settings, text="evil", action={"kind": "evil", "url": "https://x"})
        r = await client.get(ANN, headers={"Authorization": f"Bearer {token}"})
        assert r.status == 200, await r.text()
        body = await r.json()
        assert set(body) == {"request_id", "server_time", "schema_version", "status", "announcements", "unread_count"}
        by_id = {a["announcement_id"]: a for a in body["announcements"]}
        assert str(live) in by_id and str(expired) not in by_id and str(evil) in by_id
        assert set(by_id[str(live)]) == {
            "announcement_id", "title", "text", "action", "valid_until", "unread", "revision"
        }
        assert by_id[str(live)]["action"] == {"type": "refresh_catalog"} and by_id[str(live)]["unread"] is True
        assert by_id[str(live)]["title"] == "" and by_id[str(live)]["valid_until"] is None
        assert by_id[str(evil)]["action"] == {"type": "none"}  # non-allowlisted kind is neutral, no URL
        assert body["unread_count"] == 2
    finally:
        await database.close()
        await client.close()


async def test_read_marker_idempotent_and_unread_count(migrated_url, settings_factory):
    client, settings, database = await _app(settings_factory, migrated_url)
    try:
        token = await _session(client, migrated_url, bind_tg=991002)
        ann = await _insert_announcement(migrated_url, settings, text="read me")
        first = await client.post(f"{ANN}/{ann}/read", headers=_hdr(token))
        assert first.status == 200, await first.text()
        b1 = await first.json()
        assert b1["announcement_id"] == str(ann) and b1["read"] is True
        second = await client.post(f"{ANN}/{ann}/read", headers=_hdr(token))
        assert second.status == 200 and (await second.json())["read_at"] == b1["read_at"]
        r = await client.get(ANN, headers={"Authorization": f"Bearer {token}"})
        body = await r.json()
        # read messages are no longer returned as unread; list and count agree
        assert body["unread_count"] == 0 and body["announcements"] == []
    finally:
        await database.close()
        await client.close()


async def test_owner_isolation_404_and_401(migrated_url, settings_factory):
    client, settings, database = await _app(settings_factory, migrated_url)
    try:
        token_a = await _session(client, migrated_url, bind_tg=991003)
        token_b = await _session(client, migrated_url, bind_tg=991004)
        ann = await _insert_announcement(migrated_url, settings, text="for A")
        await client.post(f"{ANN}/{ann}/read", headers=_hdr(token_a))
        # B still sees it unread; A's marker is account-scoped
        body_b = await (await client.get(ANN, headers={"Authorization": f"Bearer {token_b}"})).json()
        assert body_b["unread_count"] == 1
        body_a = await (await client.get(ANN, headers={"Authorization": f"Bearer {token_a}"})).json()
        assert body_a["unread_count"] == 0
        # missing/foreign id neutral 404, expired 404
        missing = await client.post(f"{ANN}/{uuid.uuid4()}/read", headers=_hdr(token_a))
        assert missing.status == 404
        expired = await _insert_announcement(migrated_url, settings, text="exp", show_until=None)
        c = await _connect(migrated_url)
        try:
            await c.execute("UPDATE announcements SET show_until = now() - interval '1 minute' WHERE id=$1", expired)
        finally:
            await c.close()
        assert (await client.post(f"{ANN}/{expired}/read", headers=_hdr(token_a))).status == 404
        assert (await client.get(ANN)).status == 401
    finally:
        await database.close()
        await client.close()


async def test_unbound_session_has_empty_surface(migrated_url, settings_factory):
    client, settings, database = await _app(settings_factory, migrated_url)
    try:
        token = await _session(client, migrated_url, bind_tg=None)
        await _insert_announcement(migrated_url, settings, text="global")
        body = await (await client.get(ANN, headers={"Authorization": f"Bearer {token}"})).json()
        assert body["announcements"] == [] and body["unread_count"] == 0
    finally:
        await database.close()
        await client.close()


async def test_account_scoped_visibility_and_read_errors(migrated_url, settings_factory):
    client, settings, database = await _app(settings_factory, migrated_url)
    try:
        token_a = await _session(client, migrated_url, bind_tg=991005)
        token_b = await _session(client, migrated_url, bind_tg=991006)
        c = await _connect(migrated_url)
        try:
            acc_a = await c.fetchval("SELECT id FROM accounts WHERE telegram_id=991005")
            scoped = await c.fetchval(
                "INSERT INTO announcements (environment, text, scope_subject_ref) VALUES ($1,'for A only',$2) RETURNING id",
                settings.environment, acc_a,
            )
        finally:
            await c.close()
        # A sees and can read it; B must not see it nor be able to read it
        body_a = await (await client.get(ANN, headers={"Authorization": f"Bearer {token_a}"})).json()
        assert any(a["announcement_id"] == str(scoped) for a in body_a["announcements"])
        body_b = await (await client.get(ANN, headers={"Authorization": f"Bearer {token_b}"})).json()
        assert all(a["announcement_id"] != str(scoped) for a in body_b["announcements"])
        assert (await client.post(f"{ANN}/{scoped}/read", headers=_hdr(token_b))).status == 404
        c = await _connect(migrated_url)
        try:
            assert await c.fetchval("SELECT count(*) FROM announcement_reads WHERE announcement_id=$1", scoped) == 0
        finally:
            await c.close()
        assert (await client.post(f"{ANN}/{scoped}/read", headers=_hdr(token_a))).status == 200
        # malformed/missing uuid -> neutral 404
        assert (await client.post(f"{ANN}/not-a-uuid/read", headers=_hdr(token_a))).status == 404
        # changing-operation key is required and bounded
        assert (await client.post(f"{ANN}/{scoped}/read", headers={"Authorization": f"Bearer {token_a}"})).status == 400
        short = await client.post(f"{ANN}/{scoped}/read", headers={"Authorization": f"Bearer {token_a}", "Idempotency-Key": "short"})
        assert short.status == 400 and (await short.json())["details"]["reason"] == "idempotency_key_required"
    finally:
        await database.close()
        await client.close()


async def test_unread_only_list_excludes_read_but_keeps_others(migrated_url, settings_factory):
    client, settings, database = await _app(settings_factory, migrated_url)
    try:
        token = await _session(client, migrated_url, bind_tg=991007)
        a1 = await _insert_announcement(migrated_url, settings, text="m1")
        a2 = await _insert_announcement(migrated_url, settings, text="m2")
        assert (await client.post(f"{ANN}/{a1}/read", headers=_hdr(token))).status == 200
        body = await (await client.get(ANN, headers={"Authorization": f"Bearer {token}"})).json()
        ids = {a["announcement_id"] for a in body["announcements"]}
        assert str(a1) not in ids and str(a2) in ids
        assert body["unread_count"] == 1 == len(body["announcements"])
    finally:
        await database.close()
        await client.close()


async def test_read_then_expire_replay_is_404_and_replay_stable_when_visible(migrated_url, settings_factory):
    client, settings, database = await _app(settings_factory, migrated_url)
    try:
        token = await _session(client, migrated_url, bind_tg=991008)
        ann = await _insert_announcement(migrated_url, settings, text="m")
        first = await client.post(f"{ANN}/{ann}/read", headers=_hdr(token))
        assert first.status == 200
        read_at = (await first.json())["read_at"]
        # stable original read_at while still visible/unexpired
        again = await client.post(f"{ANN}/{ann}/read", headers=_hdr(token))
        assert again.status == 200 and (await again.json())["read_at"] == read_at
        # after expiry a replay is neutral 404 (fallback obeys visibility/expiry)
        c = await _connect(migrated_url)
        try:
            await c.execute("UPDATE announcements SET show_until = now() - interval '1 minute' WHERE id=$1", ann)
        finally:
            await c.close()
        assert (await client.post(f"{ANN}/{ann}/read", headers=_hdr(token))).status == 404
        # marker row itself is untouched by read; it is simply not visible
        c = await _connect(migrated_url)
        try:
            assert await c.fetchval("SELECT count(*) FROM announcement_reads WHERE announcement_id=$1", ann) == 1
        finally:
            await c.close()
    finally:
        await database.close()
        await client.close()


async def test_dto_matches_approved_mobile_v1_schemas(migrated_url, settings_factory):
    """Real serializer output must validate against the approved mobile-v1 schemas exactly."""
    client, settings, database = await _app(settings_factory, migrated_url)
    try:
        token_a = await _session(client, migrated_url, bind_tg=991009)
        token_b = await _session(client, migrated_url, bind_tg=991010)
        c = await _connect(migrated_url)
        try:
            acc_a = await c.fetchval("SELECT id FROM accounts WHERE telegram_id=991009")
            scoped = await c.fetchval(
                "INSERT INTO announcements (environment, text, action, scope_subject_ref, show_until)"
                " VALUES ($1,'scoped live',$2::jsonb,$3, now()+interval '1 hour') RETURNING id",
                settings.environment, json.dumps({"kind": "refresh_catalog"}), acc_a,
            )
            global_no_expiry = await c.fetchval(
                "INSERT INTO announcements (environment, text) VALUES ($1,'global no expiry') RETURNING id",
                settings.environment,
            )
            expired = await c.fetchval(
                "INSERT INTO announcements (environment, text, show_until)"
                " VALUES ($1,'expired', now()-interval '1 minute') RETURNING id",
                settings.environment,
            )
        finally:
            await c.close()
        body = await (await client.get(ANN, headers={"Authorization": f"Bearer {token_a}"})).json()
        _approved_validator("AnnouncementsResponse").validate(body)
        ids = [a["announcement_id"] for a in body["announcements"]]
        assert str(scoped) in ids and str(global_no_expiry) in ids and str(expired) not in ids
        assert body["unread_count"] == len(ids) == 2
        by_id = {a["announcement_id"]: a for a in body["announcements"]}
        assert by_id[str(scoped)]["action"] == {"type": "refresh_catalog"}
        assert by_id[str(scoped)]["valid_until"] is not None
        assert by_id[str(global_no_expiry)]["valid_until"] is None
        assert all(a["unread"] is True and a["revision"].isdigit() for a in body["announcements"])
        # account isolation: B sees only the global item and cannot read the scoped one
        body_b = await (await client.get(ANN, headers={"Authorization": f"Bearer {token_b}"})).json()
        _approved_validator("AnnouncementsResponse").validate(body_b)
        ids_b = [a["announcement_id"] for a in body_b["announcements"]]
        assert str(scoped) not in ids_b and str(global_no_expiry) in ids_b
        assert (await client.post(f"{ANN}/{scoped}/read", headers=_hdr(token_b))).status == 404
        # read marker: approved ReadMarkerResponse, stable replay, unread_count follows
        first = await client.post(f"{ANN}/{scoped}/read", headers=_hdr(token_a))
        assert first.status == 200
        marker = await first.json()
        _approved_validator("ReadMarkerResponse").validate(marker)
        assert marker["announcement_id"] == str(scoped) and marker["read"] is True
        replay = await (await client.post(f"{ANN}/{scoped}/read", headers=_hdr(token_a))).json()
        _approved_validator("ReadMarkerResponse").validate(replay)
        assert replay["read_at"] == marker["read_at"]
        after = await (await client.get(ANN, headers={"Authorization": f"Bearer {token_a}"})).json()
        _approved_validator("AnnouncementsResponse").validate(after)
        assert str(scoped) not in [a["announcement_id"] for a in after["announcements"]]
        assert after["unread_count"] == 1
    finally:
        await database.close()
        await client.close()
