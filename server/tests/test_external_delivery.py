"""Only0044 affected PG + actual directadapter/strictclient fakeUnix scenarios."""
import asyncio
from copy import deepcopy
from dataclasses import replace
from datetime import timedelta,datetime,UTC
from pathlib import Path
from uuid import UUID,uuid4

import pytest
from test_delivery_plan import paid,mapping
from test_referral_trusted import connect
from test_trusted_quotes import _settings,Q,eligible
from test_paid_account_core import order
from terlimo_backend.payments import apply_paid_entitlement
from terlimo_backend.telegram_trial import trusted_trial
from terlimo_backend.delivery_plan import stage_manifest,freeze_plan,digest,obj,capture_paid
from terlimo_backend.external_delivery import (schedule_claims,freeze_and_enqueue,ExternalDeliveryHandlers,fulfillment_proof,CLAIM,APPLY)
from terlimo_backend.direct_delivery_client import DirectDeliveryClient
from terlimo_backend.db import Database
from terlimo_backend.worker import OutboxWorker
from tools.wdtt_direct_adapter import DirectAdapter
from tests.adapter_offline.test_wdtt_direct_v3 import Fake,H,Crash
from tests.direct_delivery_offline.test_direct_delivery_client import Wire

pytestmark=pytest.mark.asyncio


async def environment(url,settings_factory,tmp_path,n=1,base='absent',kind='paid'):
 c=await connect(url);settings=_settings(settings_factory,url)
 if kind=='paid':a,oid,eid,source=await paid(c,settings)
 else:
  a=await eligible(c)
  class Checker:
   async def is_member(self,tg):return True
  result=await trusted_trial(c,Checker(),{'operation':'activate','telegram_id':801})
  source=await c.fetchrow("SELECT * FROM delivery_fulfillments WHERE source_kind='trial'")
  oid=None;eid=source['entitlement_id']
 body,allow=mapping(a,n)
 if base!='absent':
  for t in body['targets']:t['expected_base']={'state':'active','expires_at':'2033-05-18T03:33:20Z','revision':2147483648}
 record={'version':1,'account_id':str(a),'targets':deepcopy(body['targets']),'legacy_writer_record_sha256':'a'*64}
 body['evidence_sha256']=digest(record)
 await stage_manifest(c,body,allow,dry_run=False)
 # Separate temp adapter journal per synthetic external key. Same accepted protocol.
 adapters={};backends={}
 for t in body['targets']:
  f=Fake(base!='absent')
  if f.row:f.row=replace(f.row,label='tlm:'+t['grant_uuid'])
  adapters[t['external_key']]=DirectAdapter(f,H,state_dir=tmp_path/t['external_key'].replace(':','_'),v3_enabled=True);backends[t['external_key']]=f
 class RoutingWire(Wire):
  def write(self,body):
   import json
   self.adapter=adapters[json.loads(body)['external_key']]
   super().write(body)
 wire=RoutingWire(next(iter(adapters.values())))
 client=DirectDeliveryClient(Path('/isolated.sock'),opener=wire.open)
 db=Database(settings);await db.connect();handlers=ExternalDeliveryHandlers({'offline-test':client})
 worker=OutboxWorker(db,settings,handlers=handlers.as_handlers())
 return c,settings,a,oid,eid,source,body,allow,record,adapters,backends,wire,db,handlers,worker


async def close(env):
 await env[12].close();await env[0].close()
 for adapter in env[9].values():adapter.close()


async def claims(env):
 c=env[0];body=env[6]
 await schedule_claims(c,UUID(body['id']),env[7],env[8],dry_run=False)
 await env[14].drain()


async def test_claim_stage_no_ownership_alltargets_and_exact_apply(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path,n=2)
 c,s,a,oid,eid,source,body,allow,record,adapters,backends,w,db,h,worker=e
 try:
  with pytest.raises(ValueError,match='proof'):await freeze_plan(c,source['id'],UUID(body['id']))
  assert (await schedule_claims(c,UUID(body['id']),allow,record))['dry_run']
  assert await c.fetchval('SELECT count(*) FROM outbox_operations')==0
  await schedule_claims(c,UUID(body['id']),allow,record,dry_run=False)
  await worker.run_once();assert await c.fetchval('SELECT count(*) FROM delivery_mapping_proofs')==0
  assert not any(f.writes for f in backends.values())
  await worker.run_once();assert await c.fetchval('SELECT count(*) FROM delivery_mapping_proofs')==1
  original_claims=await c.fetch('SELECT claim_wire,claim_digest,claim_operation_id FROM delivery_physical_targets ORDER BY external_key')
  await schedule_claims(c,UUID(body['id']),allow,record,dry_run=False);await worker.drain()
  assert await c.fetch('SELECT claim_wire,claim_digest,claim_operation_id FROM delivery_physical_targets ORDER BY external_key')==original_claims
  await freeze_and_enqueue(c,source['id'],UUID(body['id']))
  await worker.run_once();assert not (await fulfillment_proof(c,source['id']))['transport_complete']
  await worker.run_once();proof=await fulfillment_proof(c,source['id'])
  assert proof['transport_complete'] and proof['all_observed_current_usable'] and not proof['business_completed']
  assert await c.fetchval('SELECT needs_grant FROM payment_orders WHERE id=$1',oid)
  assert await c.fetchval('SELECT count(*) FROM installations')==0
  assert all(f.writes==['create'] for f in backends.values())
  rows=await c.fetch('SELECT wire,request_digest,dispatch_started,application_receipt,observation FROM delivery_items')
  assert all(r['dispatch_started'] and r['application_receipt'] for r in rows)
  assert all('artifact' not in str(obj(r['observation'])) and 'password' not in str(obj(r['application_receipt'])) for r in rows)
 finally:await close(e)


async def reset_claim(c,oid):
 await c.execute("UPDATE outbox_operations SET status='pending',claim_token=NULL,lease_expires_at=NULL,available_at=now() WHERE id=$1",oid)


@pytest.mark.parametrize('crash_at',['before_io','after_ack'])
async def test_exact_wire_crash_restart_no_second_extension(migrated_url,settings_factory,tmp_path,monkeypatch,crash_at):
 e=await environment(migrated_url,settings_factory,tmp_path)
 c,s,a,oid,eid,source,body,allow,record,adapters,backends,w,db,h,worker=e
 try:
  await claims(e);await freeze_and_enqueue(c,source['id'],UUID(body['id']))
  client=h.clients['offline-test'];original=client.apply
  async def crash(req):
   if crash_at=='after_ack':await original(req)
   raise Crash()
  monkeypatch.setattr(client,'apply',crash)
  with pytest.raises(Crash):await worker.run_once()
  item=await c.fetchrow('SELECT * FROM delivery_items');wire=item['wire']
  assert item['dispatch_started'] and item['application_receipt'] is None
  assert bool(next(iter(backends.values())).writes)==(crash_at=='after_ack')
  await reset_claim(c,item['outbox_id']);monkeypatch.setattr(client,'apply',original)
  await worker.run_once();after=await c.fetchrow('SELECT * FROM delivery_items')
  assert after['wire']==wire and after['application_receipt'] and after['delivery_revision']==1
  assert next(iter(backends.values())).writes==['create']
  assert w.sent==wire.encode()+b'\n'
 finally:await close(e)


async def test_lost_claim_fenced_and_sql_locks_released_during_unix(migrated_url,settings_factory,tmp_path,monkeypatch):
 e=await environment(migrated_url,settings_factory,tmp_path)
 c,s,a,oid,eid,source,body,allow,record,adapters,backends,w,db,h,worker=e
 c2=await connect(migrated_url);entered=asyncio.Event();release=asyncio.Event()
 try:
  await claims(e);await freeze_and_enqueue(c,source['id'],UUID(body['id']))
  client=h.clients['offline-test'];original=client.apply
  async def barrier(req):
   result=await original(req);entered.set();await release.wait();return result
  monkeypatch.setattr(client,'apply',barrier);task=asyncio.create_task(worker.run_once());await asyncio.wait_for(entered.wait(),3)
  item=await c2.fetchrow('SELECT * FROM delivery_items');p=await c2.fetchrow('SELECT * FROM delivery_physical_targets')
  # If preparation retained any SQL lock during Unix, NOWAIT rejects here.
  async with c2.transaction():
   await c2.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1 FOR UPDATE NOWAIT',source['id'])
   await c2.fetchrow('SELECT * FROM delivery_physical_targets WHERE id=$1 FOR UPDATE NOWAIT',p['id'])
   await c2.fetchrow('SELECT * FROM delivery_items WHERE id=$1 FOR UPDATE NOWAIT',item['id'])
   await c2.fetchrow('SELECT * FROM outbox_operations WHERE id=$1 FOR UPDATE NOWAIT',item['outbox_id'])
   newtoken=uuid4()
   await c2.execute("UPDATE outbox_operations SET claim_token=$2,lease_expires_at=now()+interval '1 hour' WHERE id=$1",item['outbox_id'],newtoken)
  release.set();await task
  assert await c.fetchval('SELECT application_receipt IS NULL FROM delivery_items')
  assert await c.fetchval('SELECT proven_revision FROM delivery_physical_targets')==0
  assert await c.fetchval('SELECT claim_token FROM outbox_operations WHERE id=$1',item['outbox_id'])==newtoken
  monkeypatch.setattr(client,'apply',original)
  newop=await c.fetchrow('SELECT * FROM outbox_operations WHERE id=$1',item['outbox_id'])
  await worker._process(c,newop)
  assert await c.fetchval('SELECT application_receipt IS NOT NULL FROM delivery_items')
  assert next(iter(backends.values())).writes==['create']
 finally:release.set();await c2.close();await close(e)


async def next_paid(c,s):
 oid,_=await order(c,s);await apply_paid_entitlement(c,s,order_id=oid)
 return await c.fetchrow('SELECT * FROM delivery_fulfillments WHERE source_order_id=$1',oid)


async def later_manifest(c,body,allow):
 new=deepcopy(body);new['id']=str(uuid4());rec={'version':1,'account_id':new['account_id'],'targets':deepcopy(new['targets']),'legacy_writer_record_sha256':'a'*64}
 new['evidence_sha256']=digest(rec);await stage_manifest(c,new,allow,dry_run=False)
 return new,rec


async def test_newer_before_older_samephysical_latermanifest_order_and_capture(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path)
 c,s,a,oid,eid,old,body,allow,record,adapters,backends,w,db,h,worker=e
 try:
  await claims(e);old_snapshot=await c.fetchval('SELECT snapshot FROM delivery_fulfillments WHERE id=$1',old['id'])
  new=await next_paid(c,s);assert new['source_sequence']==old['source_sequence']+1
  m2,r2=await later_manifest(c,body,allow);await schedule_claims(c,UUID(m2['id']),allow,r2,dry_run=False);await worker.drain()
  assert await c.fetchval('SELECT count(*) FROM delivery_physical_targets')==1
  assert await c.fetchval('SELECT count(*) FROM delivery_mapping_proofs')==2
  await freeze_and_enqueue(c,new['id'],UUID(m2['id']));await worker.drain()
  assert (await fulfillment_proof(c,new['id']))['transport_complete']
  physical=await c.fetchrow('SELECT * FROM delivery_physical_targets');assert physical['proven_revision']==1
  writes=list(next(iter(backends.values())).writes)
  # Old original source planned/enqueued later must never become a fresh revision.
  await freeze_and_enqueue(c,old['id'],UUID(body['id']));await worker.drain()
  item=await c.fetchrow('SELECT * FROM delivery_items WHERE fulfillment_id=$1',old['id'])
  assert item['outcome']=='superseded' and not item['dispatch_started'] and item['wire'] is None
  assert next(iter(backends.values())).writes==writes
  assert await c.fetchval('SELECT snapshot FROM delivery_fulfillments WHERE id=$1',old['id'])==old_snapshot
  assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
  assert await c.fetchval('SELECT bool_and(needs_grant) FROM payment_orders')
  changed=deepcopy(m2);changed['id']=str(uuid4());changed['targets'][0]['fence']=str(uuid4())
  badrec={'version':1,'account_id':changed['account_id'],'targets':deepcopy(changed['targets']),'legacy_writer_record_sha256':'a'*64};changed['evidence_sha256']=digest(badrec)
  await stage_manifest(c,changed,allow,dry_run=False)
  with pytest.raises(ValueError,match='transfer'):await schedule_claims(c,UUID(changed['id']),allow,badrec,dry_run=False)
  assert await c.fetchrow('SELECT * FROM delivery_physical_targets')==physical
 finally:await close(e)


async def test_issued_pending_blocks_successor_and_bigint(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path,base='active')
 c,s,a,oid,eid,source,body,allow,record,adapters,backends,w,db,h,worker=e
 try:
  await claims(e);f=next(iter(backends.values()));f.uncertain=True
  await freeze_and_enqueue(c,source['id'],UUID(body['id']));await worker.drain()
  first=await c.fetchrow('SELECT * FROM delivery_items');assert first['delivery_revision']==2147483649
  assert first['application_receipt'] is None and first['outcome']=='pending'
  assert await c.fetchval('SELECT status FROM outbox_operations WHERE id=$1',first['outbox_id'])=='failed'
  assert await c.fetchval('SELECT target_revision IS NULL FROM outbox_operations WHERE id=$1',first['outbox_id'])
  newer=await next_paid(c,s);await freeze_and_enqueue(c,newer['id'],UUID(body['id']));await worker.drain()
  second=await c.fetchrow('SELECT * FROM delivery_items WHERE fulfillment_id=$1',newer['id'])
  assert not second['dispatch_started'] and second['wire'] is None
  f.uncertain=False;await reset_claim(c,first['outbox_id']);await worker.run_once()
  assert obj(await c.fetchval('SELECT observation FROM delivery_items WHERE id=$1',first['id']))['code']=='admin_completion_unknown'
  assert f.writes==['expiry'] and not (await fulfillment_proof(c,source['id']))['transport_complete']
 finally:await close(e)


async def test_claim_mismatch_no_autoownership(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path)
 try:
  f=next(iter(e[10].values()));t=e[6]['targets'][0]
  seed=Fake();f.row=replace(seed.row,label='tlm:'+t['grant_uuid'])
  await claims(e)
  assert await e[0].fetchval('SELECT count(*) FROM delivery_mapping_proofs')==0
  assert await e[0].fetchval('SELECT claim_receipt IS NULL FROM delivery_physical_targets')
  assert not f.writes
 finally:await close(e)


@pytest.mark.parametrize('case',['revoked','expired','unverified'])
async def test_current_lifecycle_barrier_no_old_activation(migrated_url,settings_factory,tmp_path,case):
 e=await environment(migrated_url,settings_factory,tmp_path)
 try:
  c=e[0];await claims(e);await freeze_and_enqueue(c,e[5]['id'],UUID(e[6]['id']))
  if case=='revoked':await c.execute("UPDATE entitlements SET status='revoked' WHERE id=$1",e[4])
  elif case=='expired':await c.execute("UPDATE entitlements SET ends_at=now()-interval '1 second' WHERE id=$1",e[4])
  else:await c.execute("UPDATE accounts SET status='unlinked' WHERE id=$1",e[2])
  await e[14].run_once()
  item=await c.fetchrow('SELECT * FROM delivery_items')
  assert not item['dispatch_started'] and item['application_receipt'] is None
  assert not next(iter(e[10].values())).writes
  assert not (await fulfillment_proof(c,e[5]['id']))['transport_complete']
 finally:await close(e)


@pytest.mark.parametrize('case',['unsupported_nonfinite','legacy_order_unproven'])
async def test_unsupported_or_pre_slice_source_stays_unresolved(migrated_url,settings_factory,tmp_path,case):
 e=await environment(migrated_url,settings_factory,tmp_path)
 try:
  c=e[0];await claims(e)
  # Explicit synthetic lifecycle/import fixture, not backfill production proof.
  original=e[5];snap=deepcopy(obj(original['snapshot']))
  if case=='unsupported_nonfinite':
   snap['ends_at']=None;snap['rank_deadlines']=[None]*2
   await c.execute('UPDATE entitlements SET ends_at=NULL WHERE id=$1',e[4])
  source=await c.fetchrow('''INSERT INTO delivery_fulfillments(account_id,source_kind,entitlement_id,source_revision,snapshot,snapshot_digest,source_sequence)
   VALUES($1,'trial',$2,$3,$4::jsonb,$5,$6) RETURNING *''',e[2],e[4],original['source_revision'],snap,digest(snap),2 if case=='unsupported_nonfinite' else None)
  await freeze_and_enqueue(c,source['id'],UUID(e[6]['id']));await e[14].drain()
  item=await c.fetchrow('SELECT * FROM delivery_items WHERE fulfillment_id=$1',source['id'])
  error=await c.fetchval('SELECT last_error FROM outbox_operations WHERE id=$1',item['outbox_id'])
  assert error==('unsupported_nonfinite_expiry' if case=='unsupported_nonfinite' else 'source_order_unproven')
  assert not item['dispatch_started'] and item['wire'] is None and item['application_receipt'] is None
  assert not next(iter(e[10].values())).writes
 finally:await close(e)


@pytest.mark.parametrize('after',['disabled','natural_expiry'])
async def test_historical_proof_not_current_after_disable_or_expiry(migrated_url,settings_factory,tmp_path,after):
 e=await environment(migrated_url,settings_factory,tmp_path)
 try:
  c=e[0];await claims(e);await freeze_and_enqueue(c,e[5]['id'],UUID(e[6]['id']));await e[14].drain()
  first=await c.fetchrow('SELECT * FROM delivery_items');oldreceipt=first['application_receipt']
  adapter=next(iter(e[9].values()));f=next(iter(e[10].values()))
  if after=='disabled':
   old=obj(first['request']);adapter.execute(old|{'operation_id':str(uuid4()),'revision':2,'expected_revision':1,'desired_state':'inactive','expires_at':None})
  else:
   f.row=replace(f.row,status='expired')
   # Use accepted adapter clock's natural expiry path without changing stored source.
   import tools.wdtt_direct_adapter as module
   previous=module.time.time;module.time.time=lambda:f.row.expires_at+1
  try:
   await reset_claim(c,first['outbox_id']);await e[14].run_once()
  finally:
   if after=='natural_expiry':module.time.time=previous
  updated=await c.fetchrow('SELECT * FROM delivery_items')
  assert updated['application_receipt']==oldreceipt
  proof=await fulfillment_proof(c,e[5]['id'])
  assert proof['transport_complete'] and not proof['all_observed_current_usable']
  assert obj(updated['observation'])['historical_fulfilled']
  assert not obj(updated['observation'])['observed_current_usable']
  if after=='disabled':assert await c.fetchval('SELECT accepted_revision FROM delivery_physical_targets')==2
 finally:await close(e)


async def test_original_capture_sequence_once_no_award_or_aggregate_completion(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path)
 try:
  c=e[0];source=e[5];snapshot=source['snapshot']
  async with c.transaction():assert await capture_paid(c,e[3])==source
  assert await c.fetchval('SELECT sequence FROM delivery_source_counters')==1
  assert await c.fetchval('SELECT snapshot FROM delivery_fulfillments')==snapshot
  assert await c.fetchval('SELECT count(*) FROM outbox_operations')==0
  assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
  assert await c.fetchval('SELECT needs_grant FROM payment_orders WHERE id=$1',e[3])
  assert not (await fulfillment_proof(c,source['id']))['transport_complete']
 finally:await close(e)


async def test_concurrent_alltarget_claim_commit_aggregates_once(migrated_url,settings_factory,tmp_path,monkeypatch):
 e=await environment(migrated_url,settings_factory,tmp_path,n=2)
 try:
  await schedule_claims(e[0],UUID(e[6]['id']),e[7],e[8],dry_run=False)
  client=e[13].clients['offline-test'];original=client.claim;ready=asyncio.Event();release=asyncio.Event();hits=0
  async def barrier(req):
   nonlocal hits
   result=await original(req);hits+=1
   if hits==2:ready.set()
   await release.wait();return result
  monkeypatch.setattr(client,'claim',barrier)
  second=OutboxWorker(e[12],e[1],handlers=e[13].as_handlers(),worker_id='second-fixture')
  tasks=[asyncio.create_task(w.run_once()) for w in (e[14],second)]
  await asyncio.wait_for(ready.wait(),3);release.set();await asyncio.gather(*tasks)
  assert await e[0].fetchval('SELECT count(*) FROM delivery_physical_targets WHERE claim_receipt IS NOT NULL')==2
  assert await e[0].fetchval('SELECT count(*) FROM delivery_mapping_proofs')==1
  assert not any(f.writes for f in e[10].values())
 finally:await close(e)


async def test_wire_and_receipt_immutable_down_refuses_durable_state(migrated_url,settings_factory,tmp_path):
 import asyncpg
 e=await environment(migrated_url,settings_factory,tmp_path)
 try:
  c=e[0];await claims(e);await freeze_and_enqueue(c,e[5]['id'],UUID(e[6]['id']));await e[14].drain()
  saved=await c.fetchrow('SELECT * FROM delivery_items')
  for sql in ["UPDATE delivery_items SET wire='{}'", "UPDATE delivery_items SET dispatch_started=false", "UPDATE delivery_items SET application_receipt='{}'", "UPDATE delivery_physical_targets SET proven_revision=0", "UPDATE delivery_fulfillments SET source_sequence=NULL"]:
   with pytest.raises(asyncpg.CheckViolationError):await c.execute(sql)
  with pytest.raises(asyncpg.RaiseError):await c.execute(Path('server/terlimo_backend/migrations/versions/0044_external_delivery.down.sql').read_text())
  assert await c.fetchrow('SELECT * FROM delivery_items')==saved
 finally:await close(e)


async def test_additive0044_down_empty_and_reapply(migrated_url):
 c=await connect(migrated_url)
 try:
  await c.execute(Path('server/terlimo_backend/migrations/versions/0044_external_delivery.down.sql').read_text())
  assert await c.fetchval("SELECT to_regclass('delivery_physical_targets') IS NULL")
  await c.execute(Path('server/terlimo_backend/migrations/versions/0044_external_delivery.sql').read_text())
  assert await c.fetchval("SELECT to_regclass('delivery_physical_targets') IS NOT NULL")
 finally:await c.close()


async def test_unconfirmed_duplicate_manifest_joins_same_claim_stream(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path)
 try:
  c=e[0];first=await schedule_claims(c,UUID(e[6]['id']),e[7],e[8],dry_run=False)
  m2,r2=await later_manifest(c,e[6],e[7]);second=await schedule_claims(c,UUID(m2['id']),e[7],r2,dry_run=False)
  assert first['outbox_ids']==second['outbox_ids']
  assert await c.fetchval('SELECT count(*) FROM delivery_physical_targets')==1
  assert await c.fetchval('SELECT count(*) FROM outbox_operations')==1
  await e[14].drain();assert await c.fetchval('SELECT count(*) FROM delivery_mapping_proofs')==2
  assert not next(iter(e[10].values())).writes
 finally:await close(e)


async def test_original_trial_capture_paid_supersession_never_activates_old_trial(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path,kind='trial')
 try:
  c=e[0];old=e[5];original=obj(old['snapshot'])
  class Checker:
   async def is_member(self,tg):raise AssertionError('replay must not recapture')
  await trusted_trial(c,Checker(),{'operation':'activate','telegram_id':801})
  assert await c.fetchval('SELECT sequence FROM delivery_source_counters')==1
  await claims(e);await freeze_and_enqueue(c,old['id'],UUID(e[6]['id']))
  newer=await next_paid(c,e[1]);assert newer['source_sequence']==2
  await e[14].drain()
  item=await c.fetchrow('SELECT * FROM delivery_items WHERE fulfillment_id=$1',old['id'])
  assert item['outcome']=='superseded' and not item['dispatch_started']
  assert not next(iter(e[10].values())).writes
  assert obj(await c.fetchval('SELECT snapshot FROM delivery_fulfillments WHERE id=$1',old['id']))==original
  await freeze_and_enqueue(c,newer['id'],UUID(e[6]['id']));await e[14].drain()
  assert (await fulfillment_proof(c,newer['id']))['transport_complete']
  assert not (await fulfillment_proof(c,old['id']))['transport_complete']
  assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
 finally:await close(e)
