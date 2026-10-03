"""Only history import/readiness; isolated PG, no live provider or network adapter."""
import copy
import json
import uuid
from datetime import timedelta

import asyncpg
import pytest

from terlimo_backend.referral import initialize_account
from terlimo_backend.referral_history import import_manifest, manifest_sha256
from terlimo_backend.referral_rewards import sweep_rewards
# Minimal DB fixtures adapted from test_referral_rewards; no auth/API suite imports.
async def entitlement(c,a,kind='trial',days=7):
    return await c.fetchrow("INSERT INTO entitlements(account_id,kind,status,starts_at,ends_at,device_limit,source_plan) VALUES($1,$2,'active',now()-interval '1 minute',now()+make_interval(days=>$3),2,'{}') RETURNING *",a,kind,days)


async def installation(c,a,tg):
    i=await c.fetchval("INSERT INTO installations(public_key_fingerprint,public_key_spki_b64,environment,state) VALUES($1,'AA==','test','bound') RETURNING id",uuid.uuid4().hex)
    b=await c.fetchval("INSERT INTO account_bindings(account_id,installation_id) VALUES($1,$2) RETURNING id",a,i)
    return i,b


async def grant(c,b):
    gateway=await c.fetchval("INSERT INTO gateways(gateway_key,environment) VALUES($1,'test') RETURNING id",uuid.uuid4().hex)
    return await c.fetchval("INSERT INTO grants(binding_id,gateway_id,not_after,gateway_credential,state) VALUES($1,$2,now()+interval '10 minutes','private-fixture','pending') RETURNING id",b,gateway)


async def prove(c,g):
    await c.execute("UPDATE grants SET state='applied',applied_generation=desired_generation,applied_not_after=not_after,applied_at=now(),last_readback=$2::jsonb WHERE id=$1",g,json.dumps({'runtime_applied':True,'revoked':False}))


async def account(c,tg):
    return await c.fetchval("INSERT INTO accounts(status,telegram_id) VALUES('verified',$1) RETURNING id",tg)


def member(tg,code=None,inviter=None,trial=False,paid=False,trial_reward='not_earned',paid_reward='not_earned'):
    return dict(telegram_id=tg,code=code,code_absent_verified=code is None,
                referred_by_telegram_id=inviter,trial_used=trial,first_main_paid=paid,
                dispositions=dict(trial=trial_reward,first_main_paid=paid_reward))


def manifest(members=(),rewards=(),complete=True):
    return dict(schema=1,epoch_id=str(uuid.uuid4()),scope='test',complete=complete,
                snapshot_watermark='test-snapshot-1',final_watermark='test-delta-1',writer_fence='test-exclusive-writer-receipt',
                members=list(members),rewards=list(rewards))


def reward(tg,inviter,state='WAITING'):
    target = None if state == 'WAITING' else dict(reference='legacy-target-revision-and-grant',sha256='b'*64,
        base_ends_at='2026-10-01T00:00:00+00:00',target_ends_at='2026-10-08T00:00:00+00:00')
    return dict(source_id=f'legacy-trial-{tg}',invitee_telegram_id=tg,inviter_telegram_id=inviter,
        event_kind='trial',days=7,state=state,
        evidence=dict(source_reference=f'legacy-trial-entitlement-{tg}',source_sha256='a'*64,target=target))


async def load(c,m,**kwargs):
    return await import_manifest(c,m,expected_sha256=manifest_sha256(m),scope='test',dry_run=False,**kwargs)


@pytest.mark.asyncio
async def test_complete_coverage_new_user_legacy_code_and_canonical_history(migrated_url):
    c=await asyncpg.connect(migrated_url)
    try:
        a=await account(c,101);b=await account(c,102)
        m=manifest([member(101,'OldCODE'),member(102,trial=True,paid=True)])
        await load(c,m,activate=True)
        assert await c.fetchval('SELECT imported_trial_used AND imported_first_main_paid FROM referral_benefits WHERE account_id=$1',b)
        for owner in (a,b): await initialize_account(c,owner)
        assert await c.fetchval('SELECT referral_code FROM accounts WHERE id=$1',a)=='OldCODE'
        used=await c.fetchrow('SELECT * FROM referral_benefits WHERE account_id=$1',b)
        assert used['history_state']=='ready' and used['imported_trial_used'] and used['imported_first_main_paid']
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0  # not_earned is not a fake reward
        fresh=await account(c,103)
        await initialize_account(c,fresh,new_account=True)
        code=await c.fetchval('SELECT referral_code FROM accounts WHERE id=$1',fresh)
        assert code and await c.fetchval('SELECT history_proof FROM referral_benefits WHERE account_id=$1',fresh)=='absent'
        await initialize_account(c,fresh)
        assert code==await c.fetchval('SELECT referral_code FROM accounts WHERE id=$1',fresh)
        await entitlement(c,fresh)  # genuinely new post-cutover activity is NOT legacy
        await initialize_account(c,fresh)
        assert not await c.fetchval('SELECT imported_trial_used FROM referral_benefits WHERE account_id=$1',fresh)
        old_local=await account(c,104)
        await entitlement(c,old_local,days=-1)
        await c.execute("INSERT INTO referral_benefits(account_id,imported_first_main_paid) VALUES($1,true)",old_local)
        await initialize_account(c,old_local)
        flags=await c.fetchrow('SELECT * FROM referral_benefits WHERE account_id=$1',old_local)
        assert flags['imported_trial_used'] and flags['imported_first_main_paid'] and flags['history_state']=='ready'
        historical_paid=await account(c,105)
        pi,pb=await installation(c,historical_paid,105)
        await c.execute("""INSERT INTO payment_orders(installation_id,binding_id,account_id,idempotency_key,quote,
            amount,currency,months,tariff_key,status) VALUES($1,$2,$3,'history-paid','{}',200,'RUB',1,'test','succeeded')""",pi,pb,historical_paid)
        await initialize_account(c,historical_paid)
        assert await c.fetchval('SELECT imported_first_main_paid FROM referral_benefits WHERE account_id=$1',historical_paid)
        assert await c.fetchval('SELECT count(*) FROM referral_history_staging')==2  # no manual fresh-user staging
    finally: await c.close()


@pytest.mark.asyncio
async def test_incomplete_staged_unactivated_and_missing_coverage_stay_pending(migrated_url):
    c=await asyncpg.connect(migrated_url)
    try:
        a=await account(c,111);fresh=await account(c,112)
        await initialize_account(c,fresh,new_account=True)
        assert await c.fetchval('SELECT history_state FROM referral_benefits WHERE account_id=$1',fresh)=='history_pending'
        m=manifest([member(111,'Known')],complete=False)
        await load(c,m)
        for owner in (a,fresh): await initialize_account(c,owner)
        assert await c.fetchval('SELECT count(*) FROM accounts WHERE referral_code IS NOT NULL')==0
        with pytest.raises(ValueError,match='incomplete'): await load(c,m,activate=True)
        assert not await c.fetchval('SELECT active FROM referral_history_epochs')
    finally: await c.close()


@pytest.mark.asyncio
async def test_full_batch_dryrun_conflicts_exact_replay_and_explicit_activation(migrated_url):
    c=await asyncpg.connect(migrated_url)
    try:
        a=await account(c,121);b=await account(c,122)
        m=manifest([member(121,'A'),member(122,'B')])
        await import_manifest(c,m,expected_sha256=manifest_sha256(m),scope='test',activate=True)
        assert await c.fetchval('SELECT count(*) FROM referral_history_epochs')==0
        await load(c,m)
        await initialize_account(c,a)
        assert await c.fetchval('SELECT referral_code FROM accounts WHERE id=$1',a) is None
        await load(c,m,activate=True);await load(c,m,activate=True)
        assert await c.fetchval('SELECT count(*) FROM referral_history_staging')==2
        changed=copy.deepcopy(m);changed['members'][0]['code']='Changed'
        with pytest.raises(ValueError,match='epoch conflict'): await load(c,changed)
        other=manifest([member(121,'A'),member(999,'Missing')])
        with pytest.raises(ValueError): await load(c,other)
        assert await c.fetchval('SELECT count(*) FROM referral_history_epochs')==1
        with pytest.raises(ValueError,match='scope'): await import_manifest(c,m,expected_sha256=manifest_sha256(m),scope='production')
        await initialize_account(c,a)
        assert await c.fetchval('SELECT referral_code FROM accounts WHERE id=$1',a)=='A'
    finally: await c.close()


@pytest.mark.asyncio
@pytest.mark.parametrize('state',['PENDING_REMOTE','REMOTE_APPLIED'])
async def test_ambiguous_remote_is_preserved_but_never_runnable(migrated_url,state):
    c=await asyncpg.connect(migrated_url)
    try:
        inviter=await account(c,131);a=await account(c,132)
        m=manifest([member(131,'Parent'),member(132,'Child',131,trial=True,trial_reward='earned')],[reward(132,131,state)])
        await load(c,m,activate=True)
        await initialize_account(c,inviter);await initialize_account(c,a)
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
        assert await c.fetchval('SELECT history_state FROM referral_benefits WHERE account_id=$1',a)=='history_pending'
        assert await c.fetchval('SELECT imported_trial_used FROM referral_benefits WHERE account_id=$1',a)
        saved=json.loads(await c.fetchval('SELECT manifest FROM referral_history_epochs'))
        assert saved['rewards'][0]['state']==state
    finally: await c.close()


@pytest.mark.asyncio
async def test_applied_never_extends_waiting_extends_once_and_replay_preserves_progress(migrated_url,settings_factory,monkeypatch):
    c=await asyncpg.connect(migrated_url)
    try:
        inviter=await account(c,141);a=await account(c,142);b=await account(c,143)
        _,binding=await installation(c,inviter,141);e=await entitlement(c,inviter);g=await grant(c,binding);await prove(c,g)
        async def fake_grant(*args,**kwargs): return 'unchanged'
        monkeypatch.setattr('terlimo_backend.referral_rewards.ensure_grant',fake_grant)
        m=manifest([member(141,'Parent'),member(142,'Applied',141,trial=True,trial_reward='earned'),
            member(143,'Waiting',141,trial=True,trial_reward='earned')],[reward(142,141,'APPLIED'),reward(143,141)])
        await load(c,m)
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0  # staged-only is not runnable
        await load(c,m,activate=True)
        for owner in (inviter,a,b): await initialize_account(c,owner)
        assert await c.fetchval("SELECT count(*) FROM referral_benefits WHERE history_state='ready'")==3
        assert await sweep_rewards(c,settings_factory(migrated_url))==1
        assert await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',e['id'])==e['ends_at']+timedelta(days=7)
        await load(c,m,activate=True)
        assert await sweep_rewards(c,settings_factory(migrated_url))==0
        assert await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',e['id'])==e['ends_at']+timedelta(days=7)
        proof=json.loads(await c.fetchval("SELECT import_evidence FROM referral_rewards WHERE invitee_account_id=$1",a))
        assert proof['target']['reference']=='legacy-target-revision-and-grant'
    finally: await c.close()


@pytest.mark.asyncio
async def test_conflicting_batch_and_unresolved_disposition_do_not_make_benefits(migrated_url):
    c=await asyncpg.connect(migrated_url)
    try:
        parent=await account(c,151);a=await account(c,152);b=await account(c,153)
        await c.execute("UPDATE accounts SET referral_code='Taken' WHERE id=$1",b)
        bad=manifest([member(151,'Good'),member(152,'Taken')])
        with pytest.raises(ValueError,match='code owner'): await load(c,bad,activate=True)
        assert await c.fetchval('SELECT count(*) FROM referral_history_staging')==0
        assert await c.fetchval('SELECT count(*) FROM referral_history_epochs')==0
        m=manifest([member(151,'Parent'),member(152,'Unclear',151,trial=True,trial_reward='unresolved'),member(153,'Taken')])
        await load(c,m,activate=True)
        await initialize_account(c,a)
        assert await c.fetchval('SELECT history_state FROM referral_benefits WHERE account_id=$1',a)=='history_pending'
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
        # A complete absence proof cannot overwrite a later discovered canonical owner/code.
        other=await account(c,154)
        await c.execute("UPDATE accounts SET referral_code='Canonical' WHERE id=$1",other)
        await initialize_account(c,other)
        assert await c.fetchval('SELECT history_state FROM referral_benefits WHERE account_id=$1',other)=='history_pending'
    finally: await c.close()


@pytest.mark.parametrize('mutate',[
    lambda m:m.update(complete='true'),
    lambda m:m['members'][0].update(telegram_id=True),
    lambda m:m['members'].append(copy.deepcopy(m['members'][0])),
    lambda m:m['members'][0].update(trial_used=True,dispositions={'trial':'earned','first_main_paid':'not_earned'}),
    lambda m:m['members'][0].update(code='Wrong',code_absent_verified=True),
])
def test_manifest_validation_rejects_coercion_duplicate_and_inconsistent_proof(mutate):
    from terlimo_backend.referral_history import validate_manifest
    m=manifest([member(161)])
    mutate(m)
    with pytest.raises(ValueError): validate_manifest(m)


@pytest.mark.asyncio
async def test_reward_db_conflict_rolls_back_entire_batch_and_used_trial_cannot_reenqueue(migrated_url):
    from terlimo_backend.referral_rewards import enqueue_reward
    c=await asyncpg.connect(migrated_url)
    try:
        parent=await account(c,171);child=await account(c,172)
        e=await entitlement(c,child)
        await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',child,parent)
        await c.execute("INSERT INTO referral_benefits(account_id,history_state,imported_trial_used) VALUES($1,'ready',true)",child)
        assert await enqueue_reward(c,account_id=child,event_kind='trial',source_entitlement_id=e['id']) is None
        await c.execute("INSERT INTO referral_rewards(invitee_account_id,inviter_account_id,event_kind,source_entitlement_id,days) VALUES($1,$2,'trial',$3,7)",child,parent,e['id'])
        m=manifest([member(171,'Parent'),member(172,'Child',171,trial=True,trial_reward='earned')],[reward(172,171)])
        with pytest.raises(ValueError,match='receipt conflict'): await load(c,m,activate=True)
        assert await c.fetchval('SELECT count(*) FROM referral_history_epochs')==0
        assert await c.fetchval('SELECT count(*) FROM referral_history_staging')==0
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==1
    finally: await c.close()
