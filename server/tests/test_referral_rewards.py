"""Referral event receipts and retryable core/grant extension on isolated PostgreSQL."""
import hashlib
import json
import uuid
from datetime import timedelta

import asyncpg
import pytest

from terlimo_backend.auth_api import ApiError, now_utc
from terlimo_backend.referral_rewards import enqueue_reward, sweep_rewards, sweep_trial_rewards
from terlimo_backend.trial_activation import activate_trial
from test_step036_trial_activation import FakeChecker


async def owner(c, tg):
    a = await c.fetchval("INSERT INTO accounts(status,telegram_id) VALUES('verified',$1) RETURNING id",tg)
    await c.execute("INSERT INTO referral_benefits(account_id,history_state) VALUES($1,'ready')",a)
    return a


async def installation(c, a, tg):
    i = await c.fetchval("INSERT INTO installations(public_key_fingerprint,public_key_spki_b64,environment,state) VALUES($1,'AA==','test','bound') RETURNING id", uuid.uuid4().hex)
    b = await c.fetchval("INSERT INTO account_bindings(account_id,installation_id) VALUES($1,$2) RETURNING id",a,i)
    await c.execute("INSERT INTO registration_links(token_sha256,installation_id,environment,status,expires_at,confirmed_at,telegram_id,within_hour,trial_available,trial_reason) VALUES($1,$2,'test','confirmed',now()+interval '1 hour',now(),$3,true,true,'within_hour_no_prior_trial')",hashlib.sha256(uuid.uuid4().bytes).hexdigest(),i,tg)
    return i,b


async def entitlement(c,a,kind='trial',days=7,bonus=False):
    return await c.fetchrow("INSERT INTO entitlements(account_id,kind,status,starts_at,ends_at,device_limit,source_plan) VALUES($1,$2,'active',now()-interval '1 minute',now()+make_interval(days=>$3),2,$4::jsonb) RETURNING *",a,kind,days,json.dumps({'referral_bonus_days':3} if bonus else {}))


async def grant(c,b):
    gtw = await c.fetchval("INSERT INTO gateways(gateway_key,environment) VALUES($1,'test') RETURNING id",uuid.uuid4().hex)
    return await c.fetchval("INSERT INTO grants(binding_id,gateway_id,not_after,gateway_credential,state) VALUES($1,$2,now()+interval '10 minutes','private-fixture','pending') RETURNING id",b,gtw)


async def prove(c,g):
    await c.execute("UPDATE grants SET state='applied',applied_generation=desired_generation,applied_not_after=not_after,applied_at=now(),last_readback=$2::jsonb WHERE id=$1",g,json.dumps({'runtime_applied':True,'revoked':False}))


@pytest.mark.asyncio
async def test_referred_trial_once_two_installations_and_existing_history(migrated_url,settings_factory):
    c=await asyncpg.connect(migrated_url)
    await c.set_type_codec('jsonb',schema='pg_catalog',encoder=json.dumps,decoder=json.loads)
    try:
        inviter=await owner(c,801);a=await owner(c,802)
        await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',a,inviter)
        i,_=await installation(c,a,802);i2,_=await installation(c,a,802)
        checker=FakeChecker();settings=settings_factory(migrated_url)
        first=await activate_trial(c,settings,checker,installation_id=i)
        from datetime import datetime
        assert datetime.fromisoformat(first['trial']['ends_at'])-datetime.fromisoformat(first['trial']['starts_at'])==timedelta(days=10)
        replay=await activate_trial(c,settings,checker,installation_id=i2)
        assert replay['trial']['replay'] and checker.calls==1
        assert await c.fetchval("SELECT count(*) FROM entitlements WHERE account_id=$1",a)==1
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
        await c.execute("UPDATE entitlements SET status='expired' WHERE account_id=$1",a)
        await activate_trial(c,settings,checker,installation_id=i)
        assert checker.calls==1
        legacy=await owner(c,803);await c.execute("UPDATE referral_benefits SET history_state='ineligible' WHERE account_id=$1",legacy)
        li,_=await installation(c,legacy,803)
        await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',legacy,inviter)
        await c.execute("UPDATE referral_benefits SET imported_trial_used=true WHERE account_id=$1",legacy)
        with pytest.raises(ApiError,match='TRIAL_ALREADY_USED'):
            await activate_trial(c,settings,checker,installation_id=li)
        existing=await entitlement(c,legacy,days=-1)
        replay=await activate_trial(c,settings,checker,installation_id=li)
        assert replay['trial']['replay'] and await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',existing['id'])==existing['ends_at']
    finally:
        await c.close()


@pytest.mark.asyncio
async def test_trial_requires_current_readback_not_intent_or_old_lease(migrated_url):
    c=await asyncpg.connect(migrated_url)
    try:
        inviter=await owner(c,811);a=await owner(c,812)
        await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',a,inviter)
        _,b=await installation(c,a,812);e=await entitlement(c,a,bonus=True);g=await grant(c,b)
        from terlimo_backend.referral_rewards import record_trial_target
        await record_trial_target(c,entitlement_id=e['id'],grant_id=g,generation=1)
        assert await sweep_trial_rewards(c)==0
        await prove(c,g);await c.execute("UPDATE grants SET desired_generation=2 WHERE id=$1",g)
        assert await sweep_trial_rewards(c)==0
        await record_trial_target(c,entitlement_id=e['id'],grant_id=g,generation=2)
        assert await c.fetchval('SELECT generation FROM referral_trial_targets')==2
        # The old generation-1 readback remains insufficient after correlation
        # advances; only publication of generation 2 can prove this same trial.
        assert await sweep_trial_rewards(c)==0
        await prove(c,g)
        assert await sweep_trial_rewards(c)==1
        assert await sweep_trial_rewards(c)==0
        r=await c.fetchrow('SELECT * FROM referral_rewards')
        assert r['state']=='WAITING' and r['days']==7
    finally:
        await c.close()


@pytest.mark.asyncio
async def test_waiting_no_resurrection_extension_once_then_readback(migrated_url,settings_factory,monkeypatch):
    c=await asyncpg.connect(migrated_url)
    try:
        inviter=await owner(c,821);a=await owner(c,822)
        await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',a,inviter)
        source=await entitlement(c,a,bonus=True)
        rid=await enqueue_reward(c,account_id=a,event_kind='trial',source_entitlement_id=source['id'])
        assert rid and await enqueue_reward(c,account_id=a,event_kind='trial',source_entitlement_id=source['id']) is None
        old=await entitlement(c,inviter,kind='paid',days=-1)
        settings=settings_factory(migrated_url)
        assert await sweep_rewards(c,settings)==0
        assert await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',old['id'])==old['ends_at']
        _,b=await installation(c,inviter,821);target=await entitlement(c,inviter,kind='paid');g=await grant(c,b)
        await prove(c,g)
        calls=[]
        async def ordinary_enqueue(connection,**kw):
            calls.append(kw)
            if len(calls)==2:
                await connection.execute("UPDATE grants SET desired_generation=2,state='pending' WHERE id=$1",g)
            return 'pending'
        monkeypatch.setattr('terlimo_backend.referral_rewards.ensure_grant',ordinary_enqueue)
        assert await sweep_rewards(c,settings)==0
        row=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',target['id'])
        assert row['ends_at']==target['ends_at']+timedelta(days=7)
        assert row['device_limit']==target['device_limit'] and row['revision']==target['revision']+1
        assert await sweep_rewards(c,settings)==0
        assert await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',target['id'])==row['ends_at']
        await prove(c,g)
        assert await sweep_rewards(c,settings)==1 and await sweep_rewards(c,settings)==0
        assert await c.fetchval('SELECT state FROM referral_rewards WHERE id=$1',rid)=='APPLIED'
        assert calls and all(k['entitlement_id']==target['id'] for k in calls)
    finally:
        await c.close()


@pytest.mark.asyncio
async def test_paid_rewards_periods_history_and_duplicate(migrated_url):
    c=await asyncpg.connect(migrated_url)
    try:
        inviter=await owner(c,831)
        for n,(months,days) in enumerate([(1,7),(3,14),(6,30)]):
            a=await owner(c,840+n);await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',a,inviter)
            e=await entitlement(c,a,kind='paid')
            i,_=await installation(c,a,840+n)
            order=await c.fetchval("INSERT INTO payment_orders(installation_id,account_id,idempotency_key,quote,amount,currency,months,tariff_key,status,applied_entitlement_id) VALUES($1,$2,$3,'{}',200,'RUB',$4,'fixture','succeeded',$5) RETURNING id",i,a,uuid.uuid4().hex,months,e['id'])
            await c.execute('UPDATE referral_benefits SET first_paid_order_id=$2 WHERE account_id=$1',a,order)
            await enqueue_reward(c,account_id=a,event_kind='first_main_paid',source_entitlement_id=e['id'],source_order_id=order,months=months)
            await enqueue_reward(c,account_id=a,event_kind='first_main_paid',source_entitlement_id=e['id'],source_order_id=order,months=months)
            assert await c.fetchval('SELECT days FROM referral_rewards WHERE invitee_account_id=$1',a)==days
        a=await owner(c,850);await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',a,inviter)
        await c.execute("UPDATE referral_benefits SET history_state='history_pending' WHERE account_id=$1",a)
        e=await entitlement(c,a,kind='paid')
        assert await enqueue_reward(c,account_id=a,event_kind='first_main_paid',source_entitlement_id=e['id'],months=1) is None
        await c.execute("UPDATE referral_benefits SET history_state='ready',imported_first_main_paid=true WHERE account_id=$1",a)
        assert await enqueue_reward(c,account_id=a,event_kind='first_main_paid',source_entitlement_id=e['id'],months=1) is None
        await c.execute("UPDATE referral_benefits SET imported_first_main_paid=false WHERE account_id=$1",a)
        i,_=await installation(c,a,850)
        await c.execute("INSERT INTO payment_orders(installation_id,account_id,idempotency_key,quote,amount,currency,months,tariff_key,status,applied_entitlement_id) VALUES($1,$2,$3,'{\"product\":{\"kind\":\"subscription\"}}',200,'RUB',1,'fixture','succeeded',$4)",i,a,uuid.uuid4().hex,e['id'])
        assert await enqueue_reward(c,account_id=a,event_kind='first_main_paid',source_entitlement_id=e['id'],months=1) is None

    finally:
        await c.close()


@pytest.mark.asyncio
async def test_later_paid_apply_does_not_prove_pending_trial(migrated_url):
    from terlimo_backend.referral_rewards import record_trial_target
    c=await asyncpg.connect(migrated_url)
    try:
        inviter=await owner(c,861);a=await owner(c,862)
        await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',a,inviter)
        _,b=await installation(c,a,862);e=await entitlement(c,a,bonus=True);g=await grant(c,b)
        await record_trial_target(c,entitlement_id=e['id'],grant_id=g,generation=1)
        await entitlement(c,a,kind='paid')
        await prove(c,g)
        assert await sweep_trial_rewards(c)==0
        await c.execute('UPDATE grants SET desired_generation=2 WHERE id=$1',g)
        await record_trial_target(c,entitlement_id=e['id'],grant_id=g,generation=2)
        assert await c.fetchval('SELECT generation FROM referral_trial_targets')==1
        await prove(c,g)
        assert await sweep_trial_rewards(c)==0
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
    finally:
        await c.close()

# Existing wire-faithful Unix admin fake: real adapter/outbox/readback publication.
from test_gateway_control import gateway_env, _run_once
from terlimo_backend.gateway_control import ensure_grant


async def test_trial_reward_is_atomic_with_real_outbox_readback(gateway_env):
    _, settings, database, ids, url = gateway_env
    async with database.acquire() as c:
        inviter=await owner(c,871)
        await c.execute("INSERT INTO referral_benefits(account_id,history_state) VALUES($1,'ready')",ids['account'])
        await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',ids['account'],inviter)
        await c.execute("UPDATE entitlements SET kind='trial',source_plan=$2::jsonb WHERE id=$1",ids['entitlement'],{'referral_bonus_days':3})
        assert await ensure_grant(c,binding_id=ids['binding'],gateway_id=ids['gateway'],entitlement_id=ids['entitlement'],max_lease_seconds=settings.gateway_max_lease_seconds)=='enqueued'
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
        assert await c.fetchval('SELECT count(*) FROM referral_trial_targets')==1
    assert await _run_once(database,settings,url)>=1
    async with database.acquire() as c:
        assert await c.fetchval('SELECT days FROM referral_rewards')==7
        assert await c.fetchval('SELECT state FROM referral_rewards')=='WAITING'
        assert await sweep_trial_rewards(c)==0


async def test_reward_uses_real_grant_path_without_changing_slots(gateway_env):
    _, settings, database, ids, url = gateway_env
    async with database.acquire() as c:
        a=await owner(c,881)
        await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',a,ids['account'])
        await c.execute("INSERT INTO referral_benefits(account_id,history_state) VALUES($1,'ready')",ids['account'])
        source=await entitlement(c,a,bonus=True)
        await enqueue_reward(c,account_id=a,event_kind='trial',source_entitlement_id=source['id'])
        assert await ensure_grant(c,binding_id=ids['binding'],gateway_id=ids['gateway'],entitlement_id=ids['entitlement'],max_lease_seconds=settings.gateway_max_lease_seconds)=='enqueued'
    await _run_once(database,settings,url)
    async with database.acquire() as c:
        before=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',ids['entitlement'])
        assert await sweep_rewards(c,settings)==1
        after=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',ids['entitlement'])
        assert after['ends_at']==before['ends_at']+timedelta(days=7)
        assert after['device_limit']==before['device_limit'] and after['revision']==before['revision']+1
        assert await sweep_rewards(c,settings)==0
        assert await c.fetchval('SELECT state FROM referral_rewards')=='APPLIED'


@pytest.mark.asyncio
async def test_protected_import_preserves_waiting_applied_and_conflicts(migrated_url,settings_factory):
    from terlimo_backend.referral_rewards import import_reward_receipts
    c=await asyncpg.connect(migrated_url)
    try:
        inviter=await owner(c,891);await owner(c,892);await owner(c,893)
        entries=[{'source_id':'legacy-trial-892','invitee_telegram_id':892,'inviter_telegram_id':891,
                  'event_kind':'trial','days':7,'state':'WAITING'},
                 {'source_id':'legacy-paid-893','invitee_telegram_id':893,'inviter_telegram_id':891,
                  'event_kind':'first_main_paid','days':14,'state':'APPLIED'}]
        digest='a'*64
        assert await import_reward_receipts(c,entries,digest)==2
        assert await import_reward_receipts(c,entries,digest)==0
        rows=await c.fetch('SELECT * FROM referral_rewards ORDER BY legacy_source_id')
        assert all(r['source_entitlement_id'] is None and r['imported'] for r in rows)
        assert await sweep_rewards(c,settings_factory(migrated_url))==0
        changed=[dict(entries[0],days=8)]
        with pytest.raises(ValueError,match='receipt conflict'):
            await import_reward_receipts(c,changed,digest)
        assert await c.fetchval("SELECT days FROM referral_rewards WHERE legacy_source_id='legacy-trial-892'")==7
        with pytest.raises(ValueError,match='ownership unresolved'):
            await import_reward_receipts(c,[dict(entries[0],source_id='bad',invitee_telegram_id=999999)],digest)
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==2
    finally:
        await c.close()


async def test_same_trial_superseded_lease_refresh_keeps_exact_reward_target(gateway_env):
    from terlimo_backend.referral_rewards import record_trial_target
    _, settings, database, ids, url = gateway_env
    async with database.acquire() as c:
        inviter = await owner(c, 901)
        await c.execute("INSERT INTO referral_benefits(account_id,history_state) VALUES($1,'ready')", ids['account'])
        await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1', ids['account'], inviter)
        await c.execute("UPDATE entitlements SET kind='trial',source_plan=$2::jsonb WHERE id=$1", ids['entitlement'], {'referral_bonus_days': 3})
        kwargs = dict(binding_id=ids['binding'], gateway_id=ids['gateway'], entitlement_id=ids['entitlement'], max_lease_seconds=settings.gateway_max_lease_seconds)
        assert await ensure_grant(c, **kwargs) == 'enqueued'
        g = await c.fetchrow('SELECT * FROM grants')
        assert await c.fetchval('SELECT generation FROM referral_trial_targets') == 1
        # The original intent has not applied. Its lease is now in the renewal
        # margin: the real writer supersedes it rather than lowering generation.
        await c.execute("UPDATE grants SET not_after=now()+interval '1 second' WHERE id=$1", g['id'])
        assert await ensure_grant(c, **kwargs) == 'enqueued'
        assert await c.fetchval('SELECT generation FROM referral_trial_targets') == 2
        await record_trial_target(c, entitlement_id=ids['entitlement'], grant_id=g['id'], generation=1)
        assert await c.fetchval('SELECT generation FROM referral_trial_targets') == 2
        assert await sweep_trial_rewards(c) == 0
    await _run_once(database, settings, url)
    async with database.acquire() as c:
        first = await c.fetchrow('SELECT * FROM grants')
        assert first['desired_generation'] == first['applied_generation'] == 2
        assert first['lease_seq'] == 1
        assert await c.fetchval("SELECT count(*) FROM outbox_operations WHERE status='failed' AND target_revision=1") >= 1
        assert await c.fetchval('SELECT count(*) FROM referral_rewards') == 1
        assert await c.fetchval('SELECT days FROM referral_rewards') == 7
        await c.execute("UPDATE grants SET not_after=now()+interval '1 second' WHERE id=$1", g['id'])
        assert await ensure_grant(c, **kwargs) == 'enqueued'
        assert await c.fetchval('SELECT generation FROM referral_trial_targets') == 3
    await _run_once(database, settings, url)
    async with database.acquire() as c:
        final = await c.fetchrow('SELECT * FROM grants')
        assert final['desired_generation'] == final['applied_generation'] == 3
        assert final['lease_seq'] == 2
        assert await sweep_trial_rewards(c) == 0
        assert await c.fetchval('SELECT count(*) FROM referral_rewards') == 1


@pytest.mark.asyncio
async def test_trial_targets_refuse_foreign_and_revoked_proof(migrated_url):
    from terlimo_backend.referral_rewards import confirm_trial_target, record_trial_target
    c = await asyncpg.connect(migrated_url)
    try:
        inviter = await owner(c, 911)
        invitee = await owner(c, 912)
        foreign = await owner(c, 913)
        await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1', invitee, inviter)
        _, b = await installation(c, invitee, 912)
        _, fb = await installation(c, foreign, 913)
        e = await entitlement(c, invitee, bonus=True)
        g = await grant(c, b)
        fg = await grant(c, fb)
        await record_trial_target(c, entitlement_id=e['id'], grant_id=fg, generation=1)
        assert await c.fetchval('SELECT count(*) FROM referral_trial_targets') == 0
        await record_trial_target(c, entitlement_id=e['id'], grant_id=g, generation=1)
        await prove(c, g)
        await c.execute('UPDATE account_bindings SET account_id=$2 WHERE id=$1', b, foreign)
        assert await confirm_trial_target(c, grant_id=g) == 0
        await c.execute('UPDATE account_bindings SET account_id=$2 WHERE id=$1', b, invitee)
        await c.execute("UPDATE account_bindings SET status='revoked' WHERE id=$1", b)
        assert await confirm_trial_target(c, grant_id=g) == 0
        await c.execute("UPDATE account_bindings SET status='active' WHERE id=$1", b)
        await c.execute("UPDATE installations SET state='revoked' WHERE id=(SELECT installation_id FROM account_bindings WHERE id=$1)", b)
        assert await confirm_trial_target(c, grant_id=g) == 0
        await c.execute("UPDATE installations SET state='bound' WHERE id=(SELECT installation_id FROM account_bindings WHERE id=$1)", b)
        await c.execute("UPDATE grants SET state='revoked',desired_generation=2 WHERE id=$1", g)
        await record_trial_target(c, entitlement_id=e['id'], grant_id=g, generation=2)
        assert await c.fetchval('SELECT generation FROM referral_trial_targets') == 1
        assert await confirm_trial_target(c, grant_id=g) == 0
        assert await c.fetchval('SELECT count(*) FROM referral_rewards') == 0
    finally:
        await c.close()


async def test_bounded_reward_sweep_rotates_inactive_waiting_without_losing_days(gateway_env):
    _, settings, database, ids, url = gateway_env
    async with database.acquire() as c:
        async with c.transaction():
            await c.execute('SET LOCAL enable_seqscan=off')
            plan = await c.fetch("EXPLAIN SELECT id,inviter_account_id FROM referral_rewards WHERE state IN ('WAITING','APPLYING') ORDER BY updated_at,id LIMIT 2")
            assert 'referral_rewards_sweep_queue' in '\n'.join(row[0] for row in plan)
        inactive_ids = []
        for number in (921, 923):
            inviter = await owner(c, number)
            invitee = await owner(c, number + 1)
            await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1', invitee, inviter)
            source = await entitlement(c, invitee, bonus=True)
            rid = await enqueue_reward(c, account_id=invitee, event_kind='trial', source_entitlement_id=source['id'])
            inactive_ids.append(rid)
            await c.execute("UPDATE referral_rewards SET created_at=now()-interval '2 days',updated_at=now()-interval '2 days' WHERE id=$1", rid)
        invitee = await owner(c, 925)
        await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1', invitee, ids['account'])
        source = await entitlement(c, invitee, bonus=True)
        active_rid = await enqueue_reward(c, account_id=invitee, event_kind='trial', source_entitlement_id=source['id'])
        await c.execute("UPDATE referral_rewards SET updated_at=now()-interval '1 day' WHERE id=$1", active_rid)
        before = await c.fetchrow('SELECT * FROM entitlements WHERE id=$1', ids['entitlement'])
        assert await ensure_grant(c, binding_id=ids['binding'], gateway_id=ids['gateway'], entitlement_id=ids['entitlement'], max_lease_seconds=settings.gateway_max_lease_seconds) == 'enqueued'
    await _run_once(database, settings, url)
    async with database.acquire() as c:
        assert await sweep_rewards(c, settings, limit=2) == 0
        assert await c.fetchval('SELECT state FROM referral_rewards WHERE id=$1', active_rid) == 'WAITING'
        assert await sweep_rewards(c, settings, limit=2) == 1
        assert await c.fetchval('SELECT state FROM referral_rewards WHERE id=$1', active_rid) == 'APPLIED'
        pending = await c.fetch('SELECT state,days FROM referral_rewards WHERE id=ANY($1::uuid[])', inactive_ids)
        assert len(pending) == 2 and all(row['state'] == 'WAITING' and row['days'] == 7 for row in pending)
        assert await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1', ids['entitlement']) == before['ends_at'] + timedelta(days=7)
        assert await c.fetchval('SELECT count(*) FROM entitlements WHERE account_id=ANY(SELECT inviter_account_id FROM referral_rewards WHERE id=ANY($1::uuid[]))', inactive_ids) == 0
        assert await sweep_rewards(c, settings, limit=2) == 0
