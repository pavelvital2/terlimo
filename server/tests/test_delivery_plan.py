"""Actual temporary PG; no worker, network provider, adapter or live mapping proof."""
import asyncio
from copy import deepcopy
from datetime import timedelta,datetime,timezone
from pathlib import Path
from uuid import uuid4,UUID,uuid5

import asyncpg
import pytest
from test_referral_trusted import connect,owner,install
from test_trusted_quotes import _settings,Q,eligible
from test_paid_account_core import order,no_delivery
from terlimo_backend.payments import apply_paid_entitlement
from terlimo_backend.telegram_trial import trusted_trial
from terlimo_backend.delivery_plan import (stage_manifest,freeze_plan,capacity_deadlines,
    capture_paid,obj,packed,digest,stamp,desired_for_rank)
from terlimo_backend.auth_api import rfc3339

pytestmark=pytest.mark.asyncio


def mapping(a,n=2):
    targets=[dict(kind='direct',deployment='offline-test',external_key=f'test:device:{i}',
        grant_uuid=str(uuid5(UUID('97ab0503-5926-51b2-9a62-741f65f11845'),f'test:device:{i}')),fence=str(uuid4()),device_index=i,
        expected_base={'state':'absent','expires_at':None,'revision':0}) for i in range(1,n+1)]
    return dict(id=str(uuid4()),account_id=str(a),evidence_sha256='a'*64,targets=targets),{
        'offline-test':{t['external_key']:{'account_id':str(a),'grant_uuid':t['grant_uuid']} for t in targets}}


async def proof_fixture(c,body):
    """Synthetic proof INSERT belongs to test only, never operator API."""
    await c.execute('INSERT INTO delivery_mapping_proofs(manifest_id,receipt_id,manifest_digest,target_digest,evidence) VALUES($1,$2,$3,$4,$5::jsonb)',UUID(body['id']),uuid4(),digest(body),digest(body['targets']),{'synthetic_test_only':True})


async def paid(c,s):
    a=await eligible(c);oid,_=await order(c,s)
    eid=await apply_paid_entitlement(c,s,order_id=oid)
    f=await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE source_order_id=$1',oid)
    return a,oid,eid,f


async def test_paid_atomic_once_and_snapshot_survives_addon_renewal(migrated_url,settings_factory):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        a,oid,eid,f=await paid(c,s)
        assert f['state']=='awaiting_mapping' and f['manifest_id'] is None
        snap=obj(f['snapshot']);end=await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',eid)
        assert snap['commercial']['credited_product']['pricing']['discount_minor']==10000
        assert snap['commercial']['amount']=='100.00'
        assert len(capacity_deadlines(snap))==2
        addon,_=await order(c,s,body={**Q,'plan_id':'terlimo-extra-device','duration_code':'until:'+rfc3339(end)})
        await apply_paid_entitlement(c,s,order_id=addon)
        af=await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE source_order_id=$1',addon)
        assert len(capacity_deadlines(obj(af['snapshot'])))==3
        slot=await c.fetchval('SELECT id FROM paid_extra_slots WHERE entitlement_id=$1',eid)
        renew,_=await order(c,s,body={**Q,'renew_extra_slot_ids':[str(slot)]})
        await apply_paid_entitlement(c,s,order_id=renew)
        rf=await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE source_order_id=$1',renew)
        assert obj(rf['snapshot'])['commercial']['quote']['product']['renew_extra_slot_ids']==[str(slot)]
        assert capacity_deadlines(obj(rf['snapshot']))==[end+timedelta(days=30)]*3
        for original in (f,af):
            assert await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',original['id'])==original
        assert await apply_paid_entitlement(c,s,order_id=oid)==eid
        assert await c.fetchval('SELECT count(*) FROM delivery_fulfillments')==3
        assert await c.fetchval('SELECT bool_and(needs_grant) FROM payment_orders')
        await no_delivery(c)
    finally:await c.close()


async def test_original_transaction_rollback_and_capture_guard(migrated_url,settings_factory):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        await eligible(c);oid,_=await order(c,s)
        with pytest.raises(RuntimeError):
            async with c.transaction():
                await apply_paid_entitlement(c,s,order_id=oid)
                assert await c.fetchval('SELECT count(*) FROM delivery_fulfillments')==1
                raise RuntimeError('simulate caller rollback')
        assert await c.fetchval('SELECT count(*) FROM delivery_fulfillments')==0
        assert await c.fetchval('SELECT count(*) FROM entitlements')==0
        with pytest.raises(ValueError):await capture_paid(c,oid)
        await apply_paid_entitlement(c,s,order_id=oid)
        assert await c.fetchval('SELECT count(*) FROM delivery_fulfillments')==1
    finally:await c.close()


async def test_trusted_trial_once_original_snapshot(migrated_url):
    c=await connect(migrated_url)
    class Checker:
        calls=0
        async def is_member(self,tg):self.calls+=1;return True
    checker=Checker()
    try:
        await eligible(c)
        first=await trusted_trial(c,checker,{'operation':'activate','telegram_id':801})
        f=await c.fetchrow('SELECT * FROM delivery_fulfillments')
        assert f['source_kind']=='trial' and f['source_order_id'] is None and f['state']=='awaiting_mapping'
        snapshot=obj(f['snapshot']);assert len(capacity_deadlines(snapshot))==2
        assert datetime.fromisoformat(snapshot['ends_at'])-datetime.fromisoformat(snapshot['starts_at'])==timedelta(days=10)
        await c.execute("UPDATE entitlements SET ends_at=ends_at+interval '1 day',revision=revision+1 WHERE id=$1",f['entitlement_id'])
        replay=await trusted_trial(c,checker,{'operation':'activate','telegram_id':801})
        assert replay['trial']['entitlement_id']==first['trial']['entitlement_id'] and checker.calls==1
        assert await c.fetchrow('SELECT * FROM delivery_fulfillments')==f
        await no_delivery(c)
    finally:await c.close()


async def test_stage_dryrun_replay_immutable_no_access(migrated_url,settings_factory):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        a,oid,eid,f=await paid(c,s);body,allow=mapping(a)
        before=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',eid)
        assert (await stage_manifest(c,body,allow))['state']=='staged'
        assert await c.fetchval('SELECT count(*) FROM delivery_manifests')==0
        result=await stage_manifest(c,body,allow,dry_run=False);assert not result['replay']
        assert (await stage_manifest(c,body,allow,dry_run=False))['replay']
        changed=deepcopy(body);changed['evidence_sha256']='b'*64
        with pytest.raises(ValueError,match='manifest conflict'):await stage_manifest(c,changed,allow,dry_run=False)
        with pytest.raises(asyncpg.CheckViolationError):await c.execute("UPDATE delivery_targets SET account_id=$1",uuid4())
        with pytest.raises(asyncpg.RaiseError):await c.execute(Path('server/terlimo_backend/migrations/versions/0043_delivery_plan.down.sql').read_text())
        assert await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',eid)==before
        assert await c.fetchval('SELECT count(*) FROM installations')==0
        assert await c.fetchval('SELECT count(*) FROM delivery_items')==0
        await no_delivery(c)
    finally:await c.close()


@pytest.mark.parametrize('case',['nonverified','foreign_mobile','duplicate','unlisted','foreign_allowlist','bad_grant','fractional_base','unknown_field','overcapacity','mixed','order'])
async def test_stage_validation_and_conflicts(migrated_url,settings_factory,case):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        a,oid,eid,f=await paid(c,s);body,allow=mapping(a,3 if case=='overcapacity' else 2)
        if case=='nonverified':await c.execute("UPDATE accounts SET status='unlinked' WHERE id=$1",a)
        if case in ('foreign_mobile','mixed'):
            other=await owner(c,888,'Other') if case=='foreign_mobile' else a
            await install(c,other)
            b=await c.fetchrow('SELECT * FROM account_bindings WHERE account_id=$1',other)
            body['targets']=[dict(kind='mobile',binding_id=str(b['id']),bound_at=stamp(b['bound_at'])),body['targets'][0]]
        if case=='duplicate':body['targets'][1]=body['targets'][0]
        if case=='unlisted':allow={}
        if case=='foreign_allowlist':allow['offline-test'][body['targets'][0]['external_key']]['account_id']=str(uuid4())
        if case=='bad_grant':body['targets'][0]['grant_uuid']=str(uuid4())
        if case=='fractional_base':body['targets'][0]['expected_base']={'state':'active','expires_at':'2026-10-04T00:00:00.123Z','revision':0}
        if case=='unknown_field':body['owned']=True
        if case=='order':body['targets'].reverse()
        if case in ('mixed','overcapacity','order'):
            result=await stage_manifest(c,body,allow,dry_run=False)
            assert result['state']=='conflict'
            assert {'mixed':'mixed_order_requires_cutover','overcapacity':'overcapacity','order':'direct_order_conflict'}[case] in result['conflicts']
        else:
            with pytest.raises(ValueError):await stage_manifest(c,body,allow,dry_run=False)
            assert await c.fetchval('SELECT count(*) FROM delivery_manifests')==0
        assert await c.fetchval('SELECT count(*) FROM delivery_items')==0
        await no_delivery(c)
    finally:await c.close()


async def test_plan_requires_proof_freezes_exact_set_and_concurrent_replay(migrated_url,settings_factory):
    c=await connect(migrated_url);c2=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        a,oid,eid,f=await paid(c,s);body,allow=mapping(a);mid=UUID(body['id'])
        await stage_manifest(c,body,allow,dry_run=False)
        with pytest.raises(ValueError,match='proof'):await freeze_plan(c,f['id'],mid)
        assert await c.fetchval('SELECT state FROM delivery_fulfillments')=='awaiting_mapping'
        await proof_fixture(c,body)
        results=await asyncio.gather(freeze_plan(c,f['id'],mid),freeze_plan(c2,f['id'],mid))
        assert results[0]==results[1] and results[0]['state']=='planned'
        items=await c.fetch('SELECT * FROM delivery_items ORDER BY operation_id');assert len(items)==2
        assert all(i['preparation']=='unprepared' and i['request'] is None for i in items)
        await c.execute("UPDATE entitlements SET ends_at=ends_at+interval '100 days',revision=revision+1 WHERE id=$1",eid)
        assert await freeze_plan(c,f['id'],mid)==results[0]
        assert await c.fetch('SELECT * FROM delivery_items ORDER BY operation_id')==items
        assert all(obj(i['desired'])['expires_at']==stamp(datetime.fromisoformat(obj(f['snapshot'])['ends_at']).replace(microsecond=0)) for i in items)
        with pytest.raises(ValueError):await freeze_plan(c,f['id'],uuid4())
        with pytest.raises(asyncpg.CheckViolationError):await c.execute("UPDATE delivery_items SET preparation='prepared',request='{}',request_digest=$1",'a'*64)
        with pytest.raises(asyncpg.CheckViolationError):await c.execute("UPDATE delivery_fulfillments SET snapshot='{}'")
        assert await c.fetchval('SELECT needs_grant FROM payment_orders WHERE id=$1',oid)
        await no_delivery(c)
    finally:await c.close();await c2.close()


async def test_capacity_sorted_extra_absolute_expired_nonfinite():
    now=datetime(2026,10,4,tzinfo=timezone.utc);end=now+timedelta(days=30)
    snap=dict(kind='paid',base_limit=2,device_limit=4,ends_at=stamp(end),captured_at=stamp(now),extras=[
        {'slot_id':'unused','expires_at':stamp(now+timedelta(days=2))},
        {'slot_id':'unused2','expires_at':stamp(now+timedelta(days=10))},
        {'slot_id':'expired','expires_at':stamp(now-timedelta(days=1))}])
    assert capacity_deadlines(snap)==[end,end,now+timedelta(days=10),now+timedelta(days=2)]
    # Planning months later cannot replace the frozen absolute deadlines.
    assert capacity_deadlines(snap)[2]<now+timedelta(days=100)
    assert capacity_deadlines({**snap,'ends_at':None,'device_limit':3})==[None]*3
    assert capacity_deadlines({**snap,'kind':'trial','device_limit':2})==[end]*2
    assert capacity_deadlines({**snap,'kind':'imported','device_limit':4})==[end]*4


async def test_paid_replay_and_stage_real_lock_barrier(migrated_url,settings_factory):
    c=await connect(migrated_url);c2=await connect(migrated_url);c3=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        a=await eligible(c);oid,_=await order(c,s);body,allow=mapping(a)
        async with c.transaction():
            eid=await apply_paid_entitlement(c,s,order_id=oid)
            replay=asyncio.create_task(apply_paid_entitlement(c2,s,order_id=oid))
            for _ in range(100):
                waiting=await c.fetchval("SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1",c2.get_server_pid())
                if waiting:break
                await asyncio.sleep(.01)
            assert waiting and not replay.done()
            # Owner SHARE held by paid writer is compatible; stage does not wait for
            # paid-account lock or lock an entitlement. Uncommitted source is not proof.
            staged=await asyncio.wait_for(stage_manifest(c3,body,allow,dry_run=False),3)
            assert staged['state']=='conflict' and 'no_current_capacity' in staged['conflicts']
        assert await asyncio.wait_for(replay,3)==eid
        assert await c.fetchval('SELECT count(*) FROM delivery_fulfillments')==1
        await no_delivery(c)
    finally:await c.close();await c2.close();await c3.close()


async def test_mobile_paid_does_not_create_external_source(migrated_url,settings_factory):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        await eligible(c);oid,_=await order(c,s,mobile=True)
        assert await apply_paid_entitlement(c,s,order_id=oid)
        assert await c.fetchval('SELECT count(*) FROM delivery_fulfillments')==0
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==1
        row=await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',oid)
        assert row['binding_id']==row['checkout_owner_binding_id'] and row['credited_entitlement_revision']==1
    finally:await c.close()


async def test_empty_migration_down_and_closed_shapes(migrated_url):
    c=await connect(migrated_url);p=Path('server/terlimo_backend/migrations/versions')
    try:
        await c.execute((p/'0043_delivery_plan.down.sql').read_text())
        await c.execute((p/'0043_delivery_plan.sql').read_text())
        assert await c.fetchval('SELECT count(*) FROM delivery_fulfillments')==0
    finally:await c.close()


async def test_expired_source_desired_is_inactive_without_extension():
    past=datetime(2026,1,1,tzinfo=timezone.utc);now=past+timedelta(days=60)
    snap={'status':'active','starts_at':stamp(past-timedelta(days=7))}
    desired=desired_for_rank(snap,1,past,now,'b'*64)
    assert desired['state']=='inactive' and desired['expires_at']==stamp(past)


async def test_trial_concurrent_capture_single_source(migrated_url):
    c=await connect(migrated_url);c2=await connect(migrated_url)
    class Checker:
        async def is_member(self,tg):await asyncio.sleep(0);return True
    try:
        await eligible(c)
        results=await asyncio.gather(trusted_trial(c,Checker(),{'operation':'activate','telegram_id':801}),
                                    trusted_trial(c2,Checker(),{'operation':'activate','telegram_id':801}))
        assert results[0]['trial']['entitlement_id']==results[1]['trial']['entitlement_id']
        assert await c.fetchval('SELECT count(*) FROM delivery_fulfillments')==1
        await no_delivery(c)
    finally:await c.close();await c2.close()


async def test_physical_target_cannot_move_account_in_new_manifest(migrated_url,settings_factory):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        a,oid,eid,f=await paid(c,s);body,allow=mapping(a)
        await stage_manifest(c,body,allow,dry_run=False)
        b=await owner(c,990,'Second990');other=deepcopy(body);other['id']=str(uuid4());other['account_id']=str(b)
        for item in allow['offline-test'].values():item['account_id']=str(b)
        with pytest.raises(ValueError,match='mapping owner conflict'):
            await stage_manifest(c,other,allow,dry_run=False)
        assert await c.fetchval('SELECT count(*) FROM delivery_manifests')==1
        await no_delivery(c)
    finally:await c.close()
