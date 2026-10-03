"""Real isolated PostgreSQL: permanent owner codes and durable keyed referral receipts."""

import json
from types import SimpleNamespace
from uuid import UUID

import asyncpg
import pytest

from terlimo_backend.auth_api import ApiError
from terlimo_backend.referral import (
    change_candidate,
    initialize_account,
    referral_info,
    stage_history,
)
from terlimo_backend.telegram_binding import confirm_registration, create_registration_link


async def connection(url):
    c = await asyncpg.connect(url)
    await c.set_type_codec("jsonb", schema="pg_catalog", encoder=json.dumps, decoder=json.loads)
    return c


async def account(c, tg, code=None, new=False):
    aid = await c.fetchval(
        "INSERT INTO accounts(status,telegram_id) VALUES('verified',$1) RETURNING id", tg
    )
    if code or new:
        await stage_history(
            c,
            [
                {
                    "telegram_id": tg,
                    "code": code,
                    "referred_by_telegram_id": None,
                    "proven_new": new,
                    "trial_used": False,
                    "first_main_paid": False,
                }
            ],
            source_sha256="a" * 64,
        )
    await initialize_account(c, aid, new_account=True)
    return aid


async def install(c, n):
    return await c.fetchval(
        "INSERT INTO installations(environment,public_key_fingerprint) VALUES('test',$1) RETURNING id",
        n,
    )


def settings(factory, url):
    return factory(url, telegram_bot_username="terlimo_test", telegram_bot_key="safe-test-key")


async def test_unknown_legacy_stays_pending_and_proven_new_is_permanent(migrated_url):
    c = await connection(migrated_url)
    try:
        aid = await account(c, 11)
        assert await c.fetchval("SELECT referral_code FROM accounts WHERE id=$1", aid) is None
        assert (
            await c.fetchval("SELECT history_state FROM referral_benefits WHERE account_id=$1", aid)
            == "history_pending"
        )
        new = await account(c, 12, new=True)
        code = await c.fetchval("SELECT referral_code FROM accounts WHERE id=$1", new)
        await initialize_account(c, new, new_account=True)
        assert await c.fetchval("SELECT referral_code FROM accounts WHERE id=$1", new) == code
    finally:
        await c.close()


async def test_candidate_replay_clear_conflict_and_durable_invalid(migrated_url):
    c = await connection(migrated_url)
    try:
        await account(c, 21, "Ab12")
        iid = await install(c, "cand")
        kwargs = {"installation_id": iid, "code": "Ab12", "idempotency_key": "candidate-key-1"}
        first = await change_candidate(c, **kwargs)
        assert await change_candidate(c, **kwargs) == first
        with pytest.raises(ApiError) as error:
            await change_candidate(c, **{**kwargs, "code": "Another"})
        assert error.value.code == "IDEMPOTENCY_CONFLICT"
        assert (
            await change_candidate(
                c, installation_id=iid, code=None, idempotency_key="candidate-clear-1", clear=True
            )
        )["candidate"]["state"] == "cleared"
        for _ in range(2):
            with pytest.raises(ApiError) as error:
                await change_candidate(
                    c, installation_id=iid, code="Missing", idempotency_key="candidate-invalid-1"
                )
            assert error.value.code == "REFERRAL_CODE_INVALID"
        assert (
            await c.fetchval(
                "SELECT count(*) FROM referral_operations WHERE idempotency_key='candidate-invalid-1'"
            )
            == 1
        )
    finally:
        await c.close()


async def test_keyed_registration_replays_exact_token_and_terminal_receipt(
    migrated_url, settings_factory
):
    c = await connection(migrated_url)
    s = settings(settings_factory, migrated_url)
    try:
        inviter = await account(c, 31, "Ab12")
        iid = await install(c, "keyed")
        await stage_history(
            c,
            [
                {
                    "telegram_id": 32,
                    "code": None,
                    "referred_by_telegram_id": None,
                    "proven_new": True,
                    "trial_used": False,
                    "first_main_paid": False,
                }
            ],
            source_sha256="b" * 64,
        )
        cand = (
            await change_candidate(
                c, installation_id=iid, code="Ab12", idempotency_key="candidate-key-1"
            )
        )["candidate"]["id"]
        kwargs = {
            "installation_id": iid,
            "referral_candidate_id": cand,
            "idempotency_key": "registration-key-1",
        }
        pending = await create_registration_link(c, s, **kwargs)
        assert await create_registration_link(c, s, **kwargs) == pending
        with pytest.raises(ApiError) as error:
            await create_registration_link(c, s, installation_id=iid)
        assert error.value.code == "REFERRAL_CANDIDATE_LOCKED"
        await confirm_registration(
            c, s, token=pending["token"], telegram_id=32, telegram_username=None
        )
        terminal = await create_registration_link(c, s, **kwargs)
        receipt = terminal["referral_attribution"]
        assert receipt["state"] == "attached"
        assert receipt["candidate_id"] == cand
        assert receipt["registration_id"] == pending["referral_registration"]["registration_id"]
        assert receipt["idempotency_key"] == "registration-key-1"
        assert (
            await c.fetchval(
                "SELECT referred_by_account_id FROM accounts WHERE id=$1",
                UUID(receipt["account_ref"]),
            )
            == inviter
        )
        assert (await create_registration_link(c, s, **kwargs)) == terminal
    finally:
        await c.close()


async def test_foreign_candidate_and_keyed_expiry_are_durable(migrated_url, settings_factory):
    c = await connection(migrated_url)
    s = settings(settings_factory, migrated_url)
    try:
        await account(c, 41, "Ab12")
        iid = await install(c, "original")
        other = await install(c, "foreign")
        cand = (
            await change_candidate(
                c, installation_id=iid, code="Ab12", idempotency_key="candidate-key-1"
            )
        )["candidate"]["id"]
        with pytest.raises(ApiError):
            await create_registration_link(
                c,
                s,
                installation_id=other,
                referral_candidate_id=cand,
                idempotency_key="registration-key-1",
            )
        kwargs = {
            "installation_id": iid,
            "referral_candidate_id": cand,
            "idempotency_key": "registration-key-1",
        }
        pending = await create_registration_link(c, s, **kwargs)
        await c.execute(
            "UPDATE registration_links SET expires_at=now()-interval '1 second' WHERE id=$1",
            UUID(pending["referral_registration"]["registration_id"]),
        )
        for _ in range(2):
            with pytest.raises(ApiError) as error:
                await create_registration_link(c, s, **kwargs)
            assert error.value.code == "REGISTRATION_EXPIRED"
            assert error.value.details["referral_registration"] == pending["referral_registration"]
        assert (
            await c.fetchval(
                "SELECT status FROM registration_links WHERE id=$1",
                UUID(pending["referral_registration"]["registration_id"]),
            )
            == "expired"
        )
    finally:
        await c.close()


async def test_semantic_self_reject_finishes_registration_and_replays(
    migrated_url, settings_factory
):
    c = await connection(migrated_url)
    s = settings(settings_factory, migrated_url)
    try:
        await account(c, 51, "Ab12")
        iid = await install(c, "self")
        cand = (
            await change_candidate(
                c, installation_id=iid, code="Ab12", idempotency_key="candidate-key-1"
            )
        )["candidate"]["id"]
        kwargs = {
            "installation_id": iid,
            "referral_candidate_id": cand,
            "idempotency_key": "registration-key-1",
        }
        pending = await create_registration_link(c, s, **kwargs)
        confirmed = await confirm_registration(
            c, s, token=pending["token"], telegram_id=51, telegram_username=None
        )
        assert confirmed["referral_attribution"]["reason"] == "self"
        terminal = await create_registration_link(c, s, **kwargs)
        assert (
            terminal["state"] == "registered"
            and terminal["referral_attribution"]["state"] == "rejected"
        )
        assert (
            await confirm_registration(
                c, s, token=pending["token"], telegram_id=51, telegram_username=None
            )
        )["referral_attribution"] == confirmed["referral_attribution"]
    finally:
        await c.close()


async def test_expired_access_own_code_requires_verified_binding_not_entitlement(migrated_url):
    c = await connection(migrated_url)
    try:
        aid = await account(c, 61, "Ab12")
        iid = await install(c, "expired")
        await c.execute(
            "INSERT INTO account_bindings(account_id,installation_id,status) VALUES($1,$2,'active')",
            aid,
            iid,
        )
        context = SimpleNamespace(account_id=aid, installation_id=iid, binding={"status": "active"})
        info = await referral_info(c, context)
        assert info["code"] == "Ab12" and info["benefits"]["discount"]["state"] == "ineligible"
        assert info["links"]["web"] == "https://terlimo.xyz/?ref=uAb12"
        with pytest.raises(ApiError):
            await referral_info(c, SimpleNamespace(account_id=None, binding=None))
    finally:
        await c.close()


async def test_bad_body_key_conflict_and_original_rejection_replay(migrated_url):
    c = await connection(migrated_url)
    try:
        await account(c, 71, "Ab12")
        iid = await install(c, "badbody")
        with pytest.raises(ApiError) as error:
            await change_candidate(
                c, installation_id=iid, code="bad!", idempotency_key="candidate-badbody"
            )
        assert error.value.code == "BAD_MESSAGE"
        with pytest.raises(ApiError) as error:
            await change_candidate(
                c, installation_id=iid, code="Ab12", idempotency_key="candidate-badbody"
            )
        assert error.value.code == "IDEMPOTENCY_CONFLICT"
        with pytest.raises(ApiError) as error:
            await change_candidate(
                c, installation_id=iid, code="bad!", idempotency_key="candidate-badbody"
            )
        assert error.value.code == "BAD_MESSAGE"
        assert await c.fetchval("SELECT count(*) FROM referral_candidates") == 0
    finally:
        await c.close()


async def test_unknown_history_confirmation_retry_not_terminal_reject(
    migrated_url, settings_factory
):
    c = await connection(migrated_url)
    s = settings(settings_factory, migrated_url)
    try:
        await account(c, 81, "Ab12")
        iid = await install(c, "unknownhistory")
        cand = (
            await change_candidate(
                c, installation_id=iid, code="Ab12", idempotency_key="candidate-key-1"
            )
        )["candidate"]["id"]
        pending = await create_registration_link(
            c,
            s,
            installation_id=iid,
            referral_candidate_id=cand,
            idempotency_key="registration-key-1",
        )
        with pytest.raises(ApiError) as error:
            await confirm_registration(
                c, s, token=pending["token"], telegram_id=82, telegram_username=None
            )
        assert error.value.code == "REFERRAL_HISTORY_PENDING" and error.value.retryable
        row = await c.fetchrow(
            "SELECT status,referral_attribution FROM registration_links WHERE id=$1",
            UUID(pending["referral_registration"]["registration_id"]),
        )
        assert row["status"] == "pending" and row["referral_attribution"] is None
        await stage_history(
            c,
            [
                {
                    "telegram_id": 82,
                    "code": None,
                    "referred_by_telegram_id": None,
                    "proven_new": True,
                    "trial_used": False,
                    "first_main_paid": False,
                }
            ],
            source_sha256="c" * 64,
        )
        assert (
            await confirm_registration(
                c, s, token=pending["token"], telegram_id=82, telegram_username=None
            )
        )["referral_attribution"]["state"] == "attached"
    finally:
        await c.close()


async def test_imported_spelling_first_history_and_receipt_preserved(migrated_url):
    c = await connection(migrated_url)
    try:
        parent = await account(c, 91, "ParentCode")
        aid = await account(c, 92)
        entry = {
            "telegram_id": 92,
            "code": "LegacyAb12",
            "referred_by_telegram_id": 91,
            "proven_new": False,
            "trial_used": True,
            "first_main_paid": True,
        }
        await stage_history(c, [entry], source_sha256="d" * 64)
        await initialize_account(c, aid)
        assert (
            await c.fetchval("SELECT history_state FROM referral_benefits WHERE account_id=$1", aid)
            == "history_pending"
        )
        from terlimo_backend.referral_rewards import import_reward_receipts

        await import_reward_receipts(
            c,
            [
                {
                    "source_id": "legacy-trial-safe",
                    "invitee_telegram_id": 92,
                    "inviter_telegram_id": 91,
                    "event_kind": "trial",
                    "days": 7,
                    "state": "APPLIED",
                },
                {
                    "source_id": "legacy-paid-safe",
                    "invitee_telegram_id": 92,
                    "inviter_telegram_id": 91,
                    "event_kind": "first_main_paid",
                    "days": 14,
                    "state": "WAITING",
                },
            ],
            source_sha256="d" * 64,
        )
        await initialize_account(c, aid)
        iid = await install(c, "imported")
        await c.execute(
            "INSERT INTO account_bindings(account_id,installation_id,status) VALUES($1,$2,'active')",
            aid,
            iid,
        )
        info = await referral_info(
            c, SimpleNamespace(account_id=aid, installation_id=iid, binding={"status": "active"})
        )
        assert info["code"] == "LegacyAb12" and info["attribution"]["state"] == "attached"
        UUID(info["attribution"]["receipt_id"])
        assert info["benefits"]["discount"]["state"] == "consumed"
        assert info["benefits"]["trial_bonus_days"] == 0
        await initialize_account(c, aid)
        assert (
            await referral_info(
                c,
                SimpleNamespace(account_id=aid, installation_id=iid, binding={"status": "active"}),
            )
        ) == info
        with pytest.raises(ValueError):
            await stage_history(c, [{**entry, "trial_used": False}], source_sha256="d" * 64)
        assert (
            await c.fetchval("SELECT referred_by_account_id FROM accounts WHERE id=$1", aid)
            == parent
        )
    finally:
        await c.close()


async def test_full_slots_terminal_receipt_after_reconnect_and_foreign_binding_fence(
    migrated_url, settings_factory
):
    c = await connection(migrated_url)
    s = settings(settings_factory, migrated_url)
    try:
        await account(c, 101, "Ab12")
        owner = await account(c, 102, new=True)
        for n in ("occupied1", "occupied2"):
            occupied = await install(c, n)
            await c.execute(
                "INSERT INTO account_bindings(account_id,installation_id,status) VALUES($1,$2,'active')",
                owner,
                occupied,
            )
        iid = await install(c, "fullslots")
        cand = (
            await change_candidate(
                c, installation_id=iid, code="Ab12", idempotency_key="candidate-key-1"
            )
        )["candidate"]["id"]
        kwargs = {
            "installation_id": iid,
            "referral_candidate_id": cand,
            "idempotency_key": "registration-key-1",
        }
        pending = await create_registration_link(c, s, **kwargs)
        with pytest.raises(ApiError) as error:
            await confirm_registration(
                c, s, token=pending["token"], telegram_id=102, telegram_username=None
            )
        assert error.value.code == "DEVICE_LIMIT_REACHED"
        terminal = await create_registration_link(c, s, **kwargs)
        assert terminal["state"] == "registered" and terminal["referral_attribution"][
            "account_ref"
        ] == str(owner)
        await c.close()
        c = await connection(migrated_url)
        assert await create_registration_link(c, s, **kwargs) == terminal
        foreign = await account(c, 103, new=True)
        await c.execute(
            "INSERT INTO account_bindings(account_id,installation_id,status) VALUES($1,$2,'active')",
            foreign,
            iid,
        )
        with pytest.raises(ApiError) as error:
            await create_registration_link(c, s, **kwargs)
        assert error.value.code == "REGISTRATION_CONFLICT"
    finally:
        await c.close()


async def test_existing_active_right_rejects_new_attribution_but_expired_allows(migrated_url,settings_factory):
    c=await connection(migrated_url);s=settings(settings_factory,migrated_url)
    try:
        inviter=await account(c,901,'Inviter901');aid=await account(c,902,'Own902')
        e=await c.fetchval("INSERT INTO entitlements(account_id,kind,status,starts_at,ends_at,device_limit) VALUES($1,'paid','active',now()-interval '1 day',now()+interval '1 day',2) RETURNING id",aid)
        async def registration(name,key):
            iid=await install(c,name)
            candidate=(await change_candidate(c,installation_id=iid,code='Inviter901',idempotency_key=key+'-candidate'))['candidate']['id']
            pending=await create_registration_link(c,s,installation_id=iid,referral_candidate_id=candidate,idempotency_key=key+'-registration')
            return await confirm_registration(c,s,token=pending['token'],telegram_id=902,telegram_username=None)
        active=await registration('active-attribution','active-key')
        assert active['referral_attribution']['state']=='rejected' and active['referral_attribution']['reason']=='ineligible'
        assert await c.fetchval('SELECT referred_by_account_id FROM accounts WHERE id=$1',aid) is None
        await c.execute("UPDATE entitlements SET ends_at=now()-interval '1 second' WHERE id=$1",e)
        expired=await registration('expired-attribution','expired-key')
        assert expired['referral_attribution']['state']=='attached'
        assert await c.fetchval('SELECT referred_by_account_id FROM accounts WHERE id=$1',aid)==inviter
    finally:await c.close()
