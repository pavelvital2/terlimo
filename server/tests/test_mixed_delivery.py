"""Mixed SOURCE: actual temporary PG, accepted direct and mobile fake wires."""
import asyncio
import hashlib
from copy import deepcopy
from dataclasses import replace
from datetime import datetime, UTC, timedelta
from pathlib import Path
from uuid import UUID,uuid4
import pytest
from test_external_delivery import environment,claims,close,next_paid
from test_common_capacity import link,confirm,bound
from test_gateway_control import FINGERPRINT
from fake_gateway_admin import FakeGatewayAdmin
from test_referral_trusted import connect
from test_paid_account_core import order
from test_trusted_quotes import Q
from terlimo_backend.common_capacity import activate,admission_lock
from terlimo_backend.external_delivery import freeze_and_enqueue,fulfillment_proof,ExternalDeliveryHandlers,APPLY
from terlimo_backend.mixed_delivery import MixedDeliveryHandlers,RECONCILE,queue,readiness,mobile_receipt
from terlimo_backend.gateway_control import GatewayControlHandlers,ensure_grant,revoke_binding_grants,APPLY_OPERATION
from terlimo_backend.gateway_adapter import GatewayAdminClient
from terlimo_backend.worker import OutboxWorker
from terlimo_backend.db import Database
from terlimo_backend.delivery_plan import obj
from terlimo_backend.payments import apply_paid_entitlement
from terlimo_backend.telegram_trial import trusted_trial
from terlimo_backend.trial_activation import insert_trial

pytestmark=pytest.mark.asyncio
SPKI='MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEf2GWLvpp78Vox1cAEyfhgUKaywhAfUgWYaf5JldPvQZWP_mwVeNQNWK6hBTYmLpeci1h7TWFg-mkxysKZevnHA'


async def setup(url,factory,tmp_path,kind='paid',mobile=True,activate_scope=True):
 e=await environment(url,factory,tmp_path,kind=kind)
 c=e[0];s=replace(e[1],telegram_bot_username='terlimo_test',gateway_local_admin_enabled=True,gateway_admin_main_password='fixture-main',worker_lock_timeout_seconds=1)
 gateway=FakeGatewayAdmin(node_id='fake-mixed-node');await gateway.start()
 gid=await c.fetchval("INSERT INTO gateways(gateway_key,environment,endpoints,registry_state,confirmed_max_workers) VALUES($1,'test',$2::jsonb,'registered',36) RETURNING id",gateway.node_id,{'node_id':gateway.node_id,'admin_socket':gateway.socket_path,'target_workers':36,'peer_ip':'192.0.2.1','dtls_port':443,'wg_port':51820,'dtls_spki_sha256':'a'*64})
 def factory_client(key,endpoints):return GatewayAdminClient(socket_path=endpoints['admin_socket'],main_password='fixture-main')
 gh=GatewayControlHandlers(s,client_factory=factory_client)
 handlers={**e[13].as_handlers(),**gh.as_handlers(),**MixedDeliveryHandlers(s).as_handlers()}
 worker=OutboxWorker(e[12],s,handlers=handlers)
 await claims(e)
 if activate_scope:await activate(c,e[6],e[7],dry_run=False)
 b=None
 if mobile:
  iid,t=await link(c,s,'mixed-real-mobile');await c.execute('UPDATE installations SET public_key_spki_b64=$2 WHERE id=$1',iid,SPKI)
  await confirm(c,s,t);b=await bound(c,e[2],iid)
 return {'e':e,'c':c,'s':s,'gw':gateway,'gid':gid,'b':b,'worker':worker,'handlers':handlers,'gh':gh}


async def done(h):await h['gw'].stop();await close(h['e'])


async def settle(h):
 # Respect actual worker backoff for the deliberate plan-before-RPC race.
 # No operation status or deadline is rewritten to make a test pass.
 for _ in range(3):
  await h['worker'].drain()
  due=await h['c'].fetchval("SELECT min(available_at) FROM outbox_operations WHERE operation_type=$1 AND status='pending' AND last_error='RuntimeError'",APPLY_OPERATION)
  if due is None:return
  await asyncio.sleep(max(0,(due-datetime.now(UTC)).total_seconds())+.02)
 await h['worker'].drain()


async def issue_mobile(h,source=None):
 source=source or h['e'][5]
 async with h['c'].transaction():
  return await ensure_grant(h['c'],binding_id=h['b']['id'],gateway_id=h['gid'],entitlement_id=source['entitlement_id'],max_lease_seconds=h['s'].gateway_max_lease_seconds)


async def run_kind(h,kind):
 # Filter CLAIM_SQL only to control proof ordering in tests, not reset queue state.
 worker=OutboxWorker(h['e'][12],h['s'],handlers=h['handlers'])
 import terlimo_backend.worker as mod
 # Use actual claiming SQL predicate through a private temporary worker query.
 original=mod.CLAIM_SQL
 filtered=original.replace("WHERE (status = 'pending'", "WHERE operation_type='"+kind+"' AND ((status = 'pending'").replace('        ORDER BY available_at, id','        ) ORDER BY available_at, id')
 # Inspect fixture worker SQL shape before replacing below.
 c=h['c'];op=await c.fetchrow(filtered,worker.worker_id,float(h['s'].worker_lock_timeout_seconds))
 if op:await worker._process(c,op)
 return op


@pytest.mark.parametrize('kind',['paid','trial'])
async def test_direct_only_actual_proof_once_business_reward(migrated_url,settings_factory,tmp_path,kind):
 h=await setup(migrated_url,settings_factory,tmp_path,kind,mobile=False)
 try:
  c=h['c'];source=h['e'][5]
  await settle(h)
  proof=await fulfillment_proof(c,source['id']);assert proof['transport_complete'] and proof['business_completed']
  assert await c.fetchval('SELECT count(*) FROM delivery_completions')==1
  assert await c.fetchval('SELECT count(*) FROM referral_rewards')==1
  if kind=='paid':assert not await c.fetchval('SELECT needs_grant FROM payment_orders WHERE id=$1',h['e'][3])
  before=await c.fetchrow('SELECT * FROM delivery_completions')
  await queue(c,source['id'],'replay');await settle(h)
  assert await c.fetchrow('SELECT * FROM delivery_completions')==before
  assert await c.fetchval('SELECT count(*) FROM referral_rewards')==1
 finally:await done(h)


@pytest.mark.parametrize('kind',['paid','trial'])
async def test_real_mobile_join_requires_destination_then_full_exact_proof(migrated_url,settings_factory,tmp_path,kind):
 h=await setup(migrated_url,settings_factory,tmp_path,kind)
 try:
  c=h['c'];source=h['e'][5]
  await settle(h)
  assert not (await readiness(c,source['id']))['business_completed']
  assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
  assert await c.fetchval('SELECT count(*) FROM delivery_mobile_items')>0
  assert obj(await c.fetchval('SELECT required_grants FROM delivery_mobile_items ORDER BY id LIMIT 1'))==[]
  originals=await c.fetch('SELECT id,operation_id,desired,wire,application_receipt FROM delivery_items ORDER BY id')
  assert await issue_mobile(h)=='enqueued'
  await settle(h)
  proof=await readiness(c,source['id']);assert proof['transport_complete'] and proof['business_completed'],proof
  assert await c.fetchval('SELECT count(*) FROM delivery_completions')==1 and await c.fetchval('SELECT count(*) FROM referral_rewards')==1
  for original in originals:assert await c.fetchrow('SELECT id,operation_id,desired,wire,application_receipt FROM delivery_items WHERE id=$1',original['id'])==original
  assert await c.fetchval('SELECT count(*) FROM delivery_plans')==2
  assert next(iter(h['e'][10].values())).writes==['create']
 finally:await done(h)


@pytest.mark.parametrize('mapped',[True,False])
async def test_mobile_paid_original_capture_not_enqueue_shortcut(migrated_url,settings_factory,tmp_path,mapped):
 h=await setup(migrated_url,settings_factory,tmp_path,activate_scope=mapped)
 try:
  c=h['c'];await settle(h)
  from terlimo_backend.telegram_billing import billing_operation
  from terlimo_backend.payment_products import product_of
  from terlimo_backend.payments import _duration_spec
  from decimal import Decimal
  import json
  q=await billing_operation(c,h['s'],Q,uuid4().hex)
  r=await c.fetchrow('SELECT * FROM s5_payment_quotes WHERE id=$1',UUID(q['quote_id']))
  snapshot={'product':product_of(r),'method':r['method'],'duration':_duration_spec(r['months']),
   'plan':{'plan_id':r['plan_id'],'duration_code':r['duration_code'],'title':'Frozen title','tariff_key':r['tariff_key'],'origin':'paid'}}
  if r['pricing']:snapshot['pricing']=obj(r['pricing'])
  oid=await c.fetchval("""INSERT INTO payment_orders(installation_id,owner_kind,checkout_owner_account_id,checkout_owner_binding_id,
   idempotency_key,source_quote_id,quote,amount,currency,months,tariff_key,status,provider_payment_id,provider_create_state)
   VALUES($1,'installation',$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10,'succeeded',$11,'created') RETURNING id""",
   h['b']['installation_id'],h['e'][2],h['b']['id'],uuid4().hex,r['id'],json.dumps(snapshot),Decimal(r['amount_minor'])/100,
   r['currency'],r['months'],r['tariff_key'],str(uuid4()))
  eid=await apply_paid_entitlement(c,h['s'],order_id=oid)
  source=await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE source_order_id=$1',oid)
  if not mapped:
   assert source is None
   assert not await c.fetchval('SELECT needs_grant FROM payment_orders WHERE id=$1',oid)
   await settle(h)
   assert await c.fetchval('SELECT count(*) FROM delivery_plans')==0
   return
  assert source and source['entitlement_id']==eid
  assert await c.fetchval('SELECT needs_grant FROM payment_orders WHERE id=$1',oid)
  assert await c.fetchval('SELECT count(*) FROM delivery_completions')==0
  await settle(h)
  assert (await readiness(c,source['id']))['business_completed']
  assert not await c.fetchval('SELECT needs_grant FROM payment_orders WHERE id=$1',oid)
 finally:await done(h)


@pytest.mark.parametrize('first',[APPLY,APPLY_OPERATION])
async def test_partial_reverse_order_and_concurrent_completion(migrated_url,settings_factory,tmp_path,first):
 h=await setup(migrated_url,settings_factory,tmp_path)
 c2=await connect(migrated_url)
 try:
  c=h['c'];source=h['e'][5];await issue_mobile(h)
  assert await run_kind(h,RECONCILE)
  assert await run_kind(h,first)
  await run_kind(h,RECONCILE)
  assert not (await readiness(c,source['id']))['business_completed']
  assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
  assert await c.fetchval('SELECT needs_grant FROM payment_orders WHERE id=$1',h['e'][3])
  assert await run_kind(h,APPLY_OPERATION if first==APPLY else APPLY)
  await queue(c,source['id'],'concurrent-completion-one');await queue(c,source['id'],'concurrent-completion-two')
  # Two actual workers race the same source completion through independent pool connections.
  w2=OutboxWorker(h['e'][12],h['s'],handlers=h['handlers'],worker_id='mixed-second')
  await asyncio.wait_for(asyncio.gather(h['worker'].drain(),w2.drain()),8)
  assert (await readiness(c,source['id']))['business_completed']
  assert await c.fetchval('SELECT count(*) FROM delivery_completions')==1
  assert await c.fetchval('SELECT count(*) FROM referral_rewards')==1
 finally:await c2.close();await done(h)


async def test_leave_rejoin_keeps_historical_completion_and_no_old_incarnation_proof(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path)
 try:
  c=h['c'];source=h['e'][5];await issue_mobile(h);await settle(h)
  assert (await readiness(c,source['id']))['business_completed']
  completion=await c.fetchrow('SELECT * FROM delivery_completions')
  mobile=await c.fetchrow('SELECT * FROM delivery_mobile_items')
  originals=await c.fetch('SELECT id,operation_id,desired,wire,application_receipt FROM delivery_items ORDER BY id')
  async with c.transaction():await revoke_binding_grants(c,binding_id=h['b']['id'])
  assert await mobile_receipt(c,source,mobile) is None
  await settle(h)
  iid=h['b']['installation_id'];_,t=await link(c,h['s'],'unused',iid);await confirm(c,h['s'],t)
  h['b']=await bound(c,h['e'][2],iid)
  await settle(h)
  assert await mobile_receipt(c,source,mobile) is None
  r=await readiness(c,source['id']);assert r['business_completed'] and not r['transport_complete']
  # Existing transport's revoke is a permanent tombstone. Same old destination
  # cannot be silently revived; remain pending until an authorized destination exists.
  assert await issue_mobile(h)=='revoked'
  assert await c.fetchval("SELECT rank FROM capacity_live_admissions WHERE kind='direct'")==1
  assert await c.fetchrow('SELECT * FROM delivery_completions')==completion
  assert await c.fetchval('SELECT count(*) FROM referral_rewards')==1
  for original in originals:assert await c.fetchrow('SELECT id,operation_id,desired,wire,application_receipt FROM delivery_items WHERE id=$1',original['id'])==original
 finally:await done(h)


@pytest.mark.parametrize('field',['registration_id','generation'])
async def test_wrong_mobile_wire_readback_never_business_proof(migrated_url,settings_factory,tmp_path,monkeypatch,field):
 h=await setup(migrated_url,settings_factory,tmp_path)
 try:
  c=h['c'];source=h['e'][5];await issue_mobile(h);await run_kind(h,RECONCILE);await run_kind(h,APPLY)
  original=h['gw']._readback_body
  def wrong(password,entry):
   result=original(password,entry);result[field]='foreign-installation' if field=='registration_id' else '999';return result
  monkeypatch.setattr(h['gw'],'_readback_body',wrong)
  await run_kind(h,APPLY_OPERATION);await run_kind(h,RECONCILE)
  assert not (await readiness(c,source['id']))['business_completed']
  assert await c.fetchval('SELECT count(*) FROM delivery_mobile_proofs')==0
  assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
 finally:await done(h)


async def test_new_source_during_issued_direct_preserves_order_and_w1_wakeup(migrated_url,settings_factory,tmp_path,monkeypatch):
 h=await setup(migrated_url,settings_factory,tmp_path,mobile=False)
 ready=asyncio.Event();release=asyncio.Event();task=None
 try:
  c=h['c'];await run_kind(h,RECONCILE)
  client=h['e'][13].clients['offline-test'];original=client.apply
  async def held(req):
   response=await original(req)
   if req.revision==1:ready.set();await release.wait()
   return response
  monkeypatch.setattr(client,'apply',held)
  task=asyncio.create_task(run_kind(h,APPLY));await asyncio.wait_for(ready.wait(),3)
  first=await c.fetchrow('SELECT * FROM delivery_items');oldsource=h['e'][5]
  newer=await next_paid(c,h['s']);await run_kind(h,RECONCILE)
  nextitem=await c.fetchrow('SELECT * FROM delivery_items WHERE fulfillment_id=$1',newer['id'])
  assert nextitem['delivery_order']>first['delivery_order']
  await run_kind(h,APPLY)
  assert await c.fetchval('SELECT last_error FROM outbox_operations WHERE id=$1',nextitem['outbox_id'])=='physical_predecessor_pending'
  release.set();await task;await settle(h)
  applied=await c.fetchrow('SELECT * FROM delivery_items WHERE id=$1',nextitem['id'])
  assert applied['application_receipt'] is not None and applied['expected_revision']==1
  assert await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',oldsource['id'])==oldsource
  assert await c.fetchval('SELECT first_paid_order_id FROM referral_benefits WHERE account_id=$1',h['e'][2])==h['e'][3]
  assert (await readiness(c,newer['id']))['business_completed']
  desired=obj(applied['desired']);backend=next(iter(h['e'][10].values()))
  assert backend.row.expires_at==int(datetime.fromisoformat(desired['expires_at']).timestamp())
 finally:
  release.set()
  if task:await asyncio.gather(task,return_exceptions=True)
  await done(h)


async def test_unacknowledged_direct_blocks_membership_successor(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path,mobile=False)
 try:
  c=h['c'];await run_kind(h,RECONCILE)
  # Fake existing admin mutates then loses ACK. An existing active row is needed
  # because Fake.create has no uncertainty switch; hold uncertainty on later expiry.
  await settle(h)
  newer=await next_paid(c,h['s']);await run_kind(h,RECONCILE)
  backend=next(iter(h['e'][10].values()));backend.uncertain=True
  await run_kind(h,APPLY)
  _,t=await link(c,h['s'],'joined-after-unacked');iid=await c.fetchval('SELECT installation_id FROM registration_links WHERE token_sha256=$1',hashlib.sha256(t.encode()).hexdigest())
  await confirm(c,h['s'],t);h['b']=await bound(c,h['e'][2],iid)
  await issue_mobile(h,newer);await settle(h)
  assert not (await readiness(c,newer['id']))['business_completed']
  assert await c.fetchval("SELECT count(*) FROM outbox_operations WHERE last_error='physical_predecessor_pending'")>=1
  assert await c.fetchval('SELECT count(*) FROM delivery_items WHERE dispatch_started AND application_receipt IS NULL')==1
 finally:await done(h)


async def test_owner_freeze_barrier_blocks_real_admission(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path,mobile=False);c2=await connect(migrated_url)
 try:
  c=h['c'];iid,t=await link(c,h['s'],'owner-freeze-barrier')
  async with c.transaction():
   # Exact first reconcile fence, before benefit/paid-account/source rows.
   await c.fetchval('SELECT id FROM accounts WHERE id=$1 FOR SHARE',h['e'][2])
   task=asyncio.create_task(confirm(c2,h['s'],t));await asyncio.sleep(.04)
   assert not task.done()
   await run_kind(h,RECONCILE)
   assert obj(await c.fetchval('SELECT members FROM delivery_plans'))[0]['kind']=='direct'
  await asyncio.wait_for(task,5)
  await settle(h)
  assert await c.fetchval('SELECT count(*) FROM delivery_plans')==2
  assert len(obj(await c.fetchval('SELECT members FROM delivery_plans ORDER BY dispatch_sequence DESC LIMIT 1')))==2
 finally:await c2.close();await done(h)


async def test_gateway_lost_token_cannot_publish_mapped_proof(migrated_url,settings_factory,tmp_path,monkeypatch):
 h=await setup(migrated_url,settings_factory,tmp_path);c2=await connect(migrated_url)
 ready=asyncio.Event();release=asyncio.Event();task=None
 try:
  c=h['c'];await issue_mobile(h);await run_kind(h,RECONCILE)
  from terlimo_backend.worker import CLAIM_SQL
  filtered=CLAIM_SQL.replace("WHERE (status = 'pending'", "WHERE operation_type='"+APPLY_OPERATION+"' AND ((status = 'pending'").replace('        ORDER BY available_at, id','        ) ORDER BY available_at, id')
  old=await c.fetchrow(filtered,'old-proof-owner',.1)
  original=h['gh']._client_factory
  class Held:
   def __init__(self,client):self.client=client
   def __getattr__(self,name):return getattr(self.client,name)
   async def grant_provision(self,*args,**kwargs):
    result=await self.client.grant_provision(*args,**kwargs);ready.set();await release.wait();return result
  monkeypatch.setattr(h['gh'],'_client_factory',lambda *args:Held(original(*args)))
  task=asyncio.create_task(h['gh'].apply_grant(c,old));await asyncio.wait_for(ready.wait(),3)
  await asyncio.sleep(.15)
  replacement=await c2.fetchrow(filtered,'new-proof-owner',1.0)
  assert replacement['id']==old['id'] and replacement['claim_token']!=old['claim_token']
  release.set();assert await task==('failed','claim_token_lost')
  assert await c2.fetchval('SELECT applied_generation FROM grants WHERE binding_id=$1',h['b']['id']) is None
  assert await c2.fetchval('SELECT count(*) FROM delivery_completions')==0
 finally:
  release.set()
  if task:await asyncio.gather(task,return_exceptions=True)
  await c2.close();await done(h)


@pytest.mark.parametrize('mapped',[True,False])
async def test_ordinary_mobile_trial_capture_and_unmapped_preserved(migrated_url,settings_factory,tmp_path,mapped):
 h=await setup(migrated_url,settings_factory,tmp_path,mobile=False,activate_scope=mapped)
 try:
  c=h['c'];iid,t=await link(c,h['s'],'ordinary-trial-hour')
  await c.execute('UPDATE installations SET public_key_spki_b64=$2 WHERE id=$1',iid,SPKI)
  await c.execute("INSERT INTO entitlements(installation_id,kind,status,ends_at,device_limit) VALUES($1,'onboarding_hour','active',now()+interval '1 hour',1)",iid)
  await confirm(c,h['s'],t);h['b']=await bound(c,h['e'][2],iid)
  # Genuine fixture bootstrap right has elapsed; its captured source bytes stay intact.
  await c.execute("UPDATE entitlements SET status='expired',ends_at=now()-interval '1 second' WHERE id=$1",h['e'][4])
  from terlimo_backend.trial_activation import activate_trial
  class Checker:
   calls=0
   async def is_member(self,tg):self.calls+=1;return True
  checker=Checker();r=await activate_trial(c,h['s'],checker,installation_id=iid)
  assert r['trial']['state']=='active' and not r['trial']['replay']
  row=await c.fetchrow("SELECT * FROM entitlements WHERE account_id=$1 AND kind='trial'",h['e'][2])
  assert obj(row['source_plan'])['referral_bonus_days']==3
  source=await c.fetchrow("SELECT * FROM delivery_fulfillments WHERE entitlement_id=$1",row['id'])
  assert bool(source)==mapped
  assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
  await activate_trial(c,h['s'],checker,installation_id=iid);assert checker.calls==1
  if mapped:
   await settle(h);assert not (await readiness(c,source['id']))['business_completed']
   await issue_mobile(h,source);await settle(h)
   assert (await readiness(c,source['id']))['business_completed']
   assert await c.fetchval("SELECT count(*) FROM referral_rewards WHERE event_kind='trial'")==1
  else:
   async with c.transaction():
    await ensure_grant(c,binding_id=h['b']['id'],gateway_id=h['gid'],entitlement_id=row['id'],max_lease_seconds=h['s'].gateway_max_lease_seconds)
   await settle(h)
   assert await c.fetchval('SELECT count(*) FROM delivery_plans')==0
   assert await c.fetchval('SELECT delivery_source_id FROM grants WHERE binding_id=$1',h['b']['id']) is None
 finally:await done(h)


async def test_hour_only_destination_never_completes_commercial(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path)
 try:
  c=h['c'];iid=h['b']['installation_id']
  hour=await c.fetchval("INSERT INTO entitlements(installation_id,kind,status,ends_at,device_limit) VALUES($1,'onboarding_hour','active',now()+interval '1 hour',1) RETURNING id",iid)
  from terlimo_backend.gateway_control import ensure_hour_grant
  async with c.transaction():
   assert await ensure_hour_grant(c,installation_id=iid,hour_entitlement_id=hour,gateway_id=h['gid'],max_lease_seconds=300)=='enqueued'
  await settle(h)
  g=await c.fetchrow('SELECT * FROM grants WHERE hour_entitlement_id=$1',hour)
  assert g['state']=='applied' and g['delivery_source_id'] is None
  item=await c.fetchrow('SELECT * FROM delivery_mobile_items ORDER BY rank LIMIT 1')
  assert obj(item['required_grants'])==[] and await mobile_receipt(c,h['e'][5],item) is None
  assert not (await readiness(c,h['e'][5]['id']))['business_completed']
  assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
 finally:await done(h)


async def test_expired_extra_rank_cannot_publish_or_complete(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path)
 try:
  c=h['c'];first=h['b'];end=await c.fetchval("SELECT clock_timestamp()+interval '3 seconds'")
  await c.execute('INSERT INTO paid_extra_slots(entitlement_id,expires_at) VALUES($1,$2)',h['e'][4],end)
  iid,t=await link(c,h['s'],'extra-ranked-third');await c.execute('UPDATE installations SET public_key_spki_b64=$2 WHERE id=$1',iid,SPKI)
  await confirm(c,h['s'],t);third=await bound(c,h['e'][2],iid)
  source=await next_paid(c,h['s']);await issue_mobile(h,source);h['b']=third;await issue_mobile(h,source)
  frozen=None
  for _ in range(8):
   await run_kind(h,RECONCILE)
   frozen=await c.fetchrow('SELECT * FROM delivery_mobile_items WHERE binding_id=$1 ORDER BY rank DESC LIMIT 1',third['id'])
   if frozen:break
  assert frozen['rank']==3 and frozen['deadline']==end
  await asyncio.sleep(max(0,(end-datetime.now(UTC)).total_seconds())+.03)
  await settle(h)
  assert await mobile_receipt(c,source,frozen) is None
  assert await c.fetchval('SELECT applied_generation FROM grants WHERE binding_id=$1',third['id']) is None
  assert await c.fetchval("SELECT count(*) FROM outbox_operations WHERE last_error='DEVICE_LIMIT_REACHED'")>=1
  assert not (await readiness(c,source['id']))['business_completed']
  assert await c.fetchval('SELECT needs_grant FROM payment_orders WHERE id=$1',source['source_order_id'])
 finally:await done(h)


async def test_multiple_existing_authorized_gateways_one_seat_fullproof(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path);second=FakeGatewayAdmin(node_id='fake-second-node');await second.start()
 try:
  c=h['c'];await issue_mobile(h)
  gid=await c.fetchval("INSERT INTO gateways(gateway_key,environment,endpoints,registry_state,confirmed_max_workers) VALUES($1,'test',$2::jsonb,'registered',36) RETURNING id",second.node_id,{'node_id':second.node_id,'admin_socket':second.socket_path,'target_workers':36,'peer_ip':'192.0.2.2','dtls_port':443,'wg_port':51820,'dtls_spki_sha256':'b'*64})
  async with c.transaction():
   await ensure_grant(c,binding_id=h['b']['id'],gateway_id=gid,entitlement_id=h['e'][4],max_lease_seconds=h['s'].gateway_max_lease_seconds)
  await run_kind(h,RECONCILE);await run_kind(h,APPLY);await run_kind(h,APPLY_OPERATION);await run_kind(h,RECONCILE)
  from terlimo_backend.common_capacity import occupied_count
  assert await occupied_count(c,h['e'][2])==2
  assert len(obj(await c.fetchval('SELECT required_grants FROM delivery_mobile_items ORDER BY rank LIMIT 1')))==2
  assert not (await readiness(c,h['e'][5]['id']))['business_completed']
  await settle(h);assert (await readiness(c,h['e'][5]['id']))['business_completed']
  assert await c.fetchval('SELECT count(*) FROM delivery_completions')==1
 finally:await second.stop();await done(h)


async def test_mobile_rpc_requires_frozen_set_then_normal_retry(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path)
 try:
  c=h['c'];await issue_mobile(h)
  op=await run_kind(h,APPLY_OPERATION)
  assert await c.fetchval('SELECT last_error FROM outbox_operations WHERE id=$1',op['id'])=='RuntimeError'
  assert await c.fetchval('SELECT applied_generation FROM grants WHERE binding_id=$1',h['b']['id']) is None
  for _ in range(6):
   await run_kind(h,RECONCILE)
   if await c.fetchval('SELECT count(*) FROM delivery_mobile_items WHERE jsonb_array_length(required_grants)>0'):break
  available=await c.fetchval('SELECT available_at FROM outbox_operations WHERE id=$1',op['id'])
  await asyncio.sleep(max(0,(available-datetime.now(UTC)).total_seconds())+.02)
  await settle(h)
  assert (await readiness(c,h['e'][5]['id']))['business_completed']
 finally:await done(h)


async def test_empty_migration_rollback_and_durable_plan_refusal(migrated_url,settings_factory,tmp_path):
 c=await connect(migrated_url)
 root=Path(__file__).parents[1]/'terlimo_backend/migrations/versions'
 down=(root/'0046_mixed_delivery.down.sql').read_text();up=(root/'0046_mixed_delivery.sql').read_text()
 try:
  async with c.transaction():await c.execute(down)
  assert await c.fetchval("SELECT to_regclass('delivery_plans')") is None
  async with c.transaction():await c.execute(up)
  assert await c.fetchval("SELECT to_regclass('delivery_plans')")=='delivery_plans'
 finally:await c.close()
 h=await setup(migrated_url,settings_factory,tmp_path,mobile=False)
 try:
  c=h['c'];await run_kind(h,RECONCILE)
  import asyncpg
  with pytest.raises(asyncpg.RaiseError,match='forward recovery'):
   async with c.transaction():await c.execute(down)
  batch=await c.fetchrow('SELECT * FROM delivery_plans LIMIT 1')
  with pytest.raises(asyncpg.IntegrityConstraintViolationError):
   async with c.transaction():await c.execute('UPDATE delivery_plans SET dispatch_sequence=dispatch_sequence+1 WHERE id=$1',batch['id'])
  assert await c.fetchrow('SELECT * FROM delivery_plans WHERE id=$1',batch['id'])==batch
 finally:await done(h)
