"""New typed inviter slice only; actual temp PG/accepted fake transports."""
import asyncio
from datetime import timedelta
from uuid import uuid4
import pytest
from test_mixed_delivery import setup,done,issue_mobile,run_kind,settle
from test_referral_trusted import connect,owner
from test_common_capacity import link,confirm,bound
from terlimo_backend.referral_rewards import enqueue_reward,sweep_rewards
from terlimo_backend.trial_activation import insert_trial
from terlimo_backend.mixed_delivery import readiness,RECONCILE
from terlimo_backend.external_delivery import APPLY
from terlimo_backend.gateway_control import APPLY_OPERATION,revoke_binding_grants
from terlimo_backend.delivery_plan import obj

pytestmark=pytest.mark.asyncio

async def earned(h,tg=901):
 c=h['c'];child=await owner(c,tg,'Invited'+str(tg))
 await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',child,h['e'][2])
 async with c.transaction():
  ent=await insert_trial(c,child,await c.fetchval('SELECT clock_timestamp()'))
  rid=await enqueue_reward(c,account_id=child,event_kind='trial',source_entitlement_id=ent['id'])
 return await c.fetchrow('SELECT * FROM referral_rewards WHERE id=$1',rid)

async def reward(c,r):return await c.fetchrow('SELECT * FROM referral_rewards WHERE id=$1',r['id'])

async def usable(h):
 if h['b']:await issue_mobile(h)
 await settle(h)
 assert (await readiness(h['c'],h['e'][5]['id']))['transport_complete']

async def freeze_reward(h,r):
 for _ in range(8):
  await run_kind(h,RECONCILE)
  row=await h['c'].fetchrow('SELECT * FROM delivery_plans WHERE source_id=$1 ORDER BY dispatch_sequence DESC LIMIT 1',r['typed_source_id'])
  if row:return row
 raise AssertionError('reward plan missing')

async def test_external_only_concurrent_once_restart_fullproof_no_recursion(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path,mobile=False);c=h['c'];c2=await connect(migrated_url)
 try:
  await usable(h);original=await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',h['e'][5]['id'])
  olditems=await c.fetch('SELECT id,wire,desired,application_receipt FROM delivery_items')
  r=await earned(h);before=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',h['e'][4]);count=await c.fetchval('SELECT count(*) FROM referral_rewards')
  await asyncio.gather(sweep_rewards(c,h['s']),sweep_rewards(c2,h['s']))
  r=await reward(c,r);assert r['state']=='APPLYING' and r['fulfillment_kind']=='typed'
  after=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',before['id'])
  assert after['ends_at']==before['ends_at']+timedelta(days=r['days']) and after['revision']==before['revision']+1
  source=await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',r['typed_source_id'])
  assert source['source_kind']=='reward' and source['source_reward_id']==r['id'] and source['source_order_id'] is None
  assert await c.fetchval('SELECT count(*) FROM delivery_fulfillments WHERE source_reward_id=$1',r['id'])==1
  assert not (await readiness(c,source['id']))['business_completed']
  # Simulated SOURCE worker restart resumes existing queued immutable source.
  from terlimo_backend.worker import OutboxWorker
  h['worker']=OutboxWorker(h['e'][12],h['s'],handlers=h['handlers'])
  await sweep_rewards(c,h['s']);await settle(h)
  assert (await reward(c,r))['state']=='APPLIED'
  assert (await readiness(c,source['id']))['business_completed']
  await asyncio.gather(sweep_rewards(c,h['s']),sweep_rewards(c2,h['s']))
  assert await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',before['id'])==after
  assert await c.fetchval('SELECT count(*) FROM referral_rewards')==count
  assert await c.fetchval('SELECT count(*) FROM installations')==0
  assert await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',original['id'])==original
  for i in olditems:assert await c.fetchrow('SELECT id,wire,desired,application_receipt FROM delivery_items WHERE id=$1',i['id'])==i
  assert not await c.fetchval('SELECT needs_grant FROM payment_orders WHERE id=$1',h['e'][3])
 finally:await c2.close();await done(h)

@pytest.mark.parametrize('first',[APPLY,APPLY_OPERATION])
async def test_mixed_reward_partial_reverse_exact_completion(migrated_url,settings_factory,tmp_path,first):
 h=await setup(migrated_url,settings_factory,tmp_path);c=h['c']
 try:
  await usable(h);r=await earned(h);await sweep_rewards(c,h['s']);r=await reward(c,r);await freeze_reward(h,r)
  await run_kind(h,first)
  for _ in range(4):await run_kind(h,RECONCILE)
  assert (await reward(c,r))['state']=='APPLYING' and not (await readiness(c,r['typed_source_id']))['business_completed']
  await settle(h)
  assert (await reward(c,r))['state']=='APPLIED'
  assert (await readiness(c,r['typed_source_id']))['transport_complete']
  assert await c.fetchval('SELECT count(*) FROM delivery_completions WHERE source_id=$1',r['typed_source_id'])==1
  assert await c.fetchval('SELECT delivery_source_id FROM grants WHERE binding_id=$1',h['b']['id'])==r['typed_source_id']
 finally:await done(h)

@pytest.mark.parametrize('condition',['inactive','expired','missing_map','claim_only','partial','unverified'])
async def test_unusable_inviter_waits_without_extension_or_destinations(migrated_url,settings_factory,tmp_path,condition):
 h=await setup(migrated_url,settings_factory,tmp_path,mobile=condition=='partial',activate_scope=condition!='missing_map');c=h['c']
 try:
  r=await earned(h)
  if condition in ('inactive','expired'):await c.execute("UPDATE entitlements SET status=$2,ends_at=clock_timestamp()-interval '1 second' WHERE id=$1",h['e'][4],'revoked' if condition=='inactive' else 'expired')
  if condition=='unverified':await c.execute("UPDATE accounts SET status='unlinked' WHERE id=$1",h['e'][2])
  if condition=='partial':await settle(h)
  old=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',h['e'][4])
  counts={t:await c.fetchval('SELECT count(*) FROM '+t) for t in ('installations','account_bindings','grants','delivery_physical_targets','delivery_fulfillments')}
  await sweep_rewards(c,h['s']);current=await reward(c,r)
  assert current['state']=='WAITING' and current['typed_source_id'] is None
  assert await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',old['id'])==old
  for t,count in counts.items():assert await c.fetchval('SELECT count(*) FROM '+t)==count
 finally:await done(h)

@pytest.mark.parametrize('failure',['generation','registration_id','rejoin'])
async def test_reward_stale_wrong_incarnation_proof_never_applied(migrated_url,settings_factory,tmp_path,monkeypatch,failure):
 h=await setup(migrated_url,settings_factory,tmp_path);c=h['c']
 try:
  await usable(h);r=await earned(h);await sweep_rewards(c,h['s']);r=await reward(c,r);batch=await freeze_reward(h,r)
  item=await c.fetchrow('SELECT * FROM delivery_mobile_items WHERE plan_id=$1',batch['id'])
  from terlimo_backend.mixed_delivery import mobile_receipt
  assert await mobile_receipt(c,await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',r['typed_source_id']),item) is None
  if failure=='rejoin':
   await run_kind(h,APPLY_OPERATION)
   async with c.transaction():await revoke_binding_grants(c,binding_id=h['b']['id'])
   # Rejoin before draining direct delivery: no intermediate direct-only completion.
   iid=h['b']['installation_id'];_,t=await link(c,h['s'],'reward-rejoin',iid);await confirm(c,h['s'],t)
   h['b']=await bound(c,h['e'][2],iid)
   await settle(h)
   assert h['b']['generation']!=item['binding_generation']
   assert await issue_mobile(h,await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',r['typed_source_id']))=='revoked'
   assert await c.fetchval("SELECT rank FROM capacity_live_admissions WHERE kind='direct'")==1
  else:
   original=h['gw']._readback_body
   def wrong(password,entry):
    result=original(password,entry);result[failure]='999' if failure=='generation' else 'foreign-registration';return result
   monkeypatch.setattr(h['gw'],'_readback_body',wrong)
   await run_kind(h,APPLY_OPERATION);await run_kind(h,APPLY)
   for _ in range(4):await run_kind(h,RECONCILE)
  assert (await reward(c,r))['state']=='APPLYING'
  assert not (await readiness(c,r['typed_source_id']))['business_completed']
  assert await c.fetchval('SELECT count(*) FROM delivery_completions WHERE source_id=$1',r['typed_source_id'])==0
  assert await c.fetchval('SELECT count(*) FROM delivery_fulfillments WHERE source_reward_id=$1',r['id'])==1
 finally:await done(h)

async def test_unknown_issued_reward_preserves_wire_and_blocks_new_paid(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path,mobile=False);c=h['c']
 try:
  await usable(h);r=await earned(h);await sweep_rewards(c,h['s']);r=await reward(c,r);await freeze_reward(h,r)
  backend=next(iter(h['e'][10].values()));backend.uncertain=True
  await run_kind(h,APPLY)
  original=await c.fetchrow('SELECT * FROM delivery_items WHERE fulfillment_id=$1',r['typed_source_id'])
  assert original['dispatch_started'] and original['application_receipt'] is None
  from test_external_delivery import next_paid
  newer=await next_paid(c,h['s'])
  for _ in range(8):
   await run_kind(h,RECONCILE)
   if await c.fetchval('SELECT count(*) FROM delivery_items WHERE fulfillment_id=$1',newer['id']):break
  await run_kind(h,APPLY)
  nextitem=await c.fetchrow('SELECT * FROM delivery_items WHERE fulfillment_id=$1',newer['id'])
  assert await c.fetchval('SELECT last_error FROM outbox_operations WHERE id=$1',nextitem['outbox_id'])=='physical_predecessor_pending'
  await sweep_rewards(c,h['s']);await run_kind(h,RECONCILE)
  assert (await reward(c,r))['state']=='APPLYING' and not (await readiness(c,r['typed_source_id']))['business_completed']
  assert await c.fetchrow('SELECT * FROM delivery_items WHERE id=$1',original['id'])==original
  assert await c.fetchval('SELECT count(*) FROM delivery_fulfillments WHERE source_reward_id=$1',r['id'])==1
 finally:await done(h)

async def test_superseded_unsent_reward_cannot_complete_from_new_paid(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path,mobile=False);c=h['c']
 try:
  await usable(h);r=await earned(h);await sweep_rewards(c,h['s']);r=await reward(c,r);await freeze_reward(h,r)
  source=await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',r['typed_source_id'])
  from test_external_delivery import next_paid
  newer=await next_paid(c,h['s']);await settle(h)
  assert (await readiness(c,newer['id']))['business_completed']
  assert (await reward(c,r))['state']=='APPLYING' and not (await readiness(c,source['id']))['business_completed']
  assert await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',source['id'])==source
  assert await c.fetchval("SELECT count(*) FROM delivery_items WHERE fulfillment_id=$1 AND outcome='superseded'",source['id'])==1
 finally:await done(h)

async def test_reward_extends_base_not_existing_extra_expiry(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path);c=h['c']
 try:
  await usable(h);extra=await c.fetchval("SELECT clock_timestamp()+interval '10 minutes'")
  await c.execute('INSERT INTO paid_extra_slots(entitlement_id,expires_at) VALUES($1,$2)',h['e'][4],extra)
  from test_mixed_delivery import SPKI
  iid,t=await link(c,h['s'],'extra-third');await c.execute('UPDATE installations SET public_key_spki_b64=$2 WHERE id=$1',iid,SPKI)
  await confirm(c,h['s'],t);third=await bound(c,h['e'][2],iid)
  from test_external_delivery import next_paid
  prior=await next_paid(c,h['s']);await issue_mobile(h,prior);h['b']=third;await issue_mobile(h,prior);await settle(h)
  assert (await readiness(c,prior['id']))['transport_complete']
  slots=await c.fetch('SELECT * FROM paid_extra_slots ORDER BY id');old=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',h['e'][4])
  r=await earned(h);await sweep_rewards(c,h['s']);r=await reward(c,r);await settle(h)
  assert (await reward(c,r))['state']=='APPLIED'
  assert await c.fetch('SELECT * FROM paid_extra_slots ORDER BY id')==slots
  assert await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',old['id'])==old['ends_at']+timedelta(days=r['days'])
  snap=obj(await c.fetchval('SELECT snapshot FROM delivery_fulfillments WHERE id=$1',r['typed_source_id']))
  assert snap['rank_deadlines'][2]==extra.isoformat()
 finally:await done(h)

async def test_historical_applied_reward_persists_after_membership_join(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path,mobile=False);c=h['c']
 try:
  await usable(h);r=await earned(h);await sweep_rewards(c,h['s']);r=await reward(c,r);await settle(h)
  applied=await reward(c,r);completion=await c.fetchrow('SELECT * FROM delivery_completions WHERE source_id=$1',r['typed_source_id'])
  iid,t=await link(c,h['s'],'later-inviter-mobile');await confirm(c,h['s'],t);await settle(h)
  proof=await readiness(c,r['typed_source_id'])
  assert proof['business_completed'] and not proof['transport_complete']
  assert await reward(c,r)==applied
  assert await c.fetchrow('SELECT * FROM delivery_completions WHERE source_id=$1',r['typed_source_id'])==completion
 finally:await done(h)

async def test_ordinary_unmapped_mobile_applying_resume_and_imported_applied_preserved(migrated_url,settings_factory,tmp_path):
 from dataclasses import replace
 h=await setup(migrated_url,settings_factory,tmp_path,activate_scope=False);c=h['c']
 try:
  s=h['s'];h['s']=replace(s,gateway_max_lease_seconds=30);await issue_mobile(h);await settle(h);h['s']=s
  r=await earned(h);old=await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',h['e'][4])
  await sweep_rewards(c,s);r=await reward(c,r)
  assert r['state']=='APPLYING' and r['fulfillment_kind']=='mobile' and r['typed_source_id'] is None
  assert await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',h['e'][4])==old+timedelta(days=r['days'])
  await settle(h);await sweep_rewards(c,s);assert (await reward(c,r))['state']=='APPLIED'
  assert await c.fetchval("SELECT count(*) FROM delivery_fulfillments WHERE source_kind='reward'")==0
  from terlimo_backend.referral_rewards import import_reward_receipts
  await owner(c,991,'Imported991')
  entry={'source_id':'isolated-applied-proof','invitee_telegram_id':991,'inviter_telegram_id':801,'event_kind':'trial','days':3,'state':'APPLIED',
   'evidence':{'source_reference':'fixture-old-source','source_sha256':'a'*64,'target':{'reference':'fixture-existing-target','sha256':'b'*64,'base_ends_at':old.isoformat(),'target_ends_at':(old+timedelta(days=3)).isoformat()}}}
  assert await import_reward_receipts(c,[entry],'c'*64,require_evidence=True)==1
  receipt=await c.fetchrow('SELECT * FROM referral_rewards WHERE legacy_source_id=$1',entry['source_id'])
  await sweep_rewards(c,s);assert await c.fetchrow('SELECT * FROM referral_rewards WHERE id=$1',receipt['id'])==receipt
  assert receipt['fulfillment_kind']=='mobile' and receipt['typed_source_id'] is None
 finally:await done(h)

async def test_actual_admission_waits_reward_owner_then_normal_catchup(migrated_url,settings_factory,tmp_path):
 from test_mixed_delivery_locks import BarrierConnection,blocked_on
 from test_mixed_delivery import SPKI
 h=await setup(migrated_url,settings_factory,tmp_path,mobile=False);c=h['c'];d=await connect(migrated_url);inspect=await connect(migrated_url)
 entered=asyncio.Event();release=asyncio.Event();sweep=admit=None
 try:
  await usable(h);r=await earned(h);old=await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',h['e'][4])
  iid,t=await link(c,h['s'],'reward-barrier-admission');await c.execute('UPDATE installations SET public_key_spki_b64=$2 WHERE id=$1',iid,SPKI)
  class ExactRewardBarrier:
   def __getattr__(self,k):return getattr(c,k)
   async def fetchrow(self,q,*args):
    row=await c.fetchrow(q,*args)
    if 'SELECT * FROM referral_rewards WHERE id=' in q and args[0]==r['id']:
     entered.set();await release.wait()
    return row
  proxy=ExactRewardBarrier()
  sweep=asyncio.create_task(sweep_rewards(proxy,h['s']));await asyncio.wait_for(entered.wait(),5)
  admit=asyncio.create_task(confirm(d,h['s'],t));await blocked_on(inspect,d.get_server_pid(),c.get_server_pid())
  activity=await inspect.fetchval('SELECT query FROM pg_stat_activity WHERE pid=$1',d.get_server_pid())
  assert 'accounts' in activity and 'FOR UPDATE' in activity
  release.set();await asyncio.wait_for(sweep,5);await asyncio.wait_for(admit,5)
  r=await reward(c,r);h['b']=await bound(c,h['e'][2],iid);await settle(h)
  assert r['state']=='APPLYING' and not (await readiness(c,r['typed_source_id']))['business_completed']
  assert await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',h['e'][4])==old+timedelta(days=r['days'])
  assert await c.fetchval('SELECT count(*) FROM grants')==0
  await issue_mobile(h,await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',r['typed_source_id']));await settle(h)
  assert (await reward(c,r))['state']=='APPLIED'
 finally:
  release.set()
  for task in (sweep,admit):
   if task:await asyncio.gather(task,return_exceptions=True)
  await d.close();await inspect.close();await done(h)

async def test_migration_roundtrip_and_durable_typed_identity(migrated_url,settings_factory,tmp_path):
 from pathlib import Path
 c=await connect(migrated_url)
 versions=Path(__file__).parents[1]/'terlimo_backend/migrations/versions'
 try:
  async with c.transaction():await c.execute((versions/'0047_typed_inviter.down.sql').read_text())
  async with c.transaction():await c.execute((versions/'0047_typed_inviter.sql').read_text())
 finally:await c.close()
 h=await setup(migrated_url,settings_factory,tmp_path,mobile=False);c=h['c']
 try:
  await usable(h);r=await earned(h);await sweep_rewards(c,h['s']);r=await reward(c,r)
  for q,arg in [
   ('UPDATE referral_rewards SET days=days+1 WHERE id=$1',r['id']),
   ('UPDATE referral_rewards SET typed_source_id=NULL WHERE id=$1',r['id']),
   ('UPDATE referral_rewards SET target_ends_at=NULL WHERE id=$1',r['id']),
   ('UPDATE delivery_fulfillments SET source_reward_id=NULL WHERE id=$1',r['typed_source_id']),
   ('DELETE FROM referral_rewards WHERE id=$1',r['id'])]:
   with pytest.raises(Exception) as err:
    async with c.transaction():await c.execute(q,arg)
   assert getattr(err.value,'sqlstate',None)=='23514'
  with pytest.raises(Exception,match='forward recovery'):
   async with c.transaction():await c.execute((versions/'0047_typed_inviter.down.sql').read_text())
  await settle(h);assert (await reward(c,r))['state']=='APPLIED'
  with pytest.raises(Exception):
   async with c.transaction():await c.execute("UPDATE referral_rewards SET state='APPLYING' WHERE id=$1",r['id'])
 finally:await done(h)

async def test_mapped_historical_mobile_applying_not_converted_or_extended(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path);c=h['c']
 try:
  await usable(h);r=await earned(h);ent=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',h['e'][4])
  grant=await c.fetchrow('SELECT * FROM grants WHERE binding_id=$1',h['b']['id'])
  import json
  # Isolated historical APPLYING fixture: saved old extension and exact grant evidence.
  await c.execute("""UPDATE referral_rewards SET state='APPLYING',target_entitlement_id=$2,
   target_revision=$3,target_base_ends_at=$4,target_ends_at=$5,grant_targets=$6::jsonb WHERE id=$1""",
   r['id'],ent['id'],ent['revision'],ent['ends_at']-timedelta(days=r['days']),ent['ends_at'],json.dumps({str(grant['id']):grant['desired_generation']}))
  old=await reward(c,r);await sweep_rewards(c,h['s']);now=await reward(c,r)
  assert now['state']=='APPLIED' and now['fulfillment_kind']=='mobile' and now['typed_source_id'] is None
  assert {k:now[k] for k in ('target_entitlement_id','target_revision','target_base_ends_at','target_ends_at','grant_targets')}=={k:old[k] for k in ('target_entitlement_id','target_revision','target_base_ends_at','target_ends_at','grant_targets')}
  assert await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',ent['id'])==ent
  assert await c.fetchval("SELECT count(*) FROM delivery_fulfillments WHERE source_kind='reward'")==0
  with pytest.raises(Exception,match='cannot convert historical'):
   async with c.transaction():await c.execute("UPDATE referral_rewards SET fulfillment_kind='typed' WHERE id=$1",r['id'])
 finally:await done(h)

async def test_mapped_waiting_with_historical_targets_stays_unresolved(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path,mobile=False);c=h['c']
 try:
  await usable(h);r=await earned(h)
  await c.execute('UPDATE referral_rewards SET target_entitlement_id=$2 WHERE id=$1',r['id'],h['e'][4])
  old=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',h['e'][4]);await sweep_rewards(c,h['s'])
  assert (await reward(c,r))['state']=='WAITING' and (await reward(c,r))['typed_source_id'] is None
  assert await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',old['id'])==old
 finally:await done(h)
