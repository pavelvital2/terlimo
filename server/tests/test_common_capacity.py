"""Shared capacity SOURCE: actual PG handlers and accepted fake direct transport only."""
import asyncio
import hashlib
from copy import deepcopy
from datetime import datetime, UTC, timedelta
from pathlib import Path
from uuid import UUID, uuid4
import pytest

from test_external_delivery import environment,claims,close
from test_referral_trusted import connect
from terlimo_backend.auth_api import ApiError
from terlimo_backend.common_capacity import (activate,admission_lock,occupied_count,live_deadlines,binding_capacity,physical_capacity,plan_membership)
from terlimo_backend.telegram_binding import create_registration_link,confirm_registration
from terlimo_backend.gateway_control import revoke_binding_grants,ensure_grant,GatewayControlHandlers
from terlimo_backend.delivery_plan import freeze_plan,obj
from terlimo_backend.external_delivery import freeze_and_enqueue,fulfillment_proof
from terlimo_backend.payment_products import binding_paid_capacity,paid_limit,cap_existing_extra_grants
from terlimo_backend.session_auth import authenticate_session
from terlimo_backend.mobile_devices import _slots_used
from terlimo_backend.referral import referral_info

pytestmark=pytest.mark.asyncio


def mobile_settings(factory,url):
 return factory(url,telegram_bot_username='terlimo_test',telegram_bot_key='fake-test-key')


async def link(c,s,label,iid=None):
 if iid is None:
  iid=await c.fetchval("INSERT INTO installations(environment,public_key_fingerprint) VALUES('test',$1) RETURNING id",label)
 result=await create_registration_link(c,s,installation_id=iid)
 return iid,result['token']


async def confirm(c,s,token,tg=801):
 return await confirm_registration(c,s,token=token,telegram_id=tg,telegram_username=None)


async def bound(c,account,iid):
 return await c.fetchrow('SELECT * FROM account_bindings WHERE account_id=$1 AND installation_id=$2',account,iid)


async def test_stage_claim_dryrun_no_reservation_two_direct_blocks_mobile(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path,n=2);c=e[0];s=mobile_settings(settings_factory,migrated_url)
 try:
  assert await occupied_count(c,e[2])==0
  with pytest.raises(ValueError,match='proof'):await activate(c,e[6],e[7],dry_run=False)
  await claims(e);assert await occupied_count(c,e[2])==0
  assert (await activate(c,e[6],e[7]))['dry_run']
  assert await occupied_count(c,e[2])==0
  await activate(c,e[6],e[7],dry_run=False)
  original=await c.fetch('SELECT * FROM capacity_admissions ORDER BY admission_order')
  assert (await activate(c,e[6],e[7],dry_run=False))['replay']
  assert await c.fetch('SELECT * FROM capacity_admissions ORDER BY admission_order')==original
  iid,token=await link(c,s,'two-direct-full')
  with pytest.raises(ApiError) as exc:await confirm(c,s,token)
  assert exc.value.code=='DEVICE_LIMIT_REACHED' and exc.value.details['slots_used']==2
  assert await bound(c,e[2],iid) is None
  assert await c.fetchval('SELECT count(*) FROM installations')==1
  assert await c.fetchval('SELECT count(*) FROM account_bindings')==0
 finally:await close(e)


async def test_one_direct_real_mobile_shared_projection_and_no_theft(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path);c=e[0];s=mobile_settings(settings_factory,migrated_url)
 try:
  await claims(e);await activate(c,e[6],e[7],dry_run=False)
  iid,token=await link(c,s,'first-real-mobile');r=await confirm(c,s,token)
  b=await bound(c,e[2],iid)
  assert r['slots_used']==2 and b['status']=='active' and await _slots_used(c,e[2])==2
  admissions=await c.fetch('SELECT kind,rank FROM capacity_live_admissions ORDER BY rank')
  assert [(r['kind'],r['rank']) for r in admissions]==[('direct',1),('mobile',2)]
  iid2,t2=await link(c,s,'next-real-mobile')
  with pytest.raises(ApiError) as exc:await confirm(c,s,t2)
  assert exc.value.code=='DEVICE_LIMIT_REACHED' and await bound(c,e[2],iid2) is None
  raw='synthetic-session';await c.execute("INSERT INTO sessions(account_id,installation_id,scopes,expires_at,token_sha256,binding_id,binding_generation) VALUES($1,$2,'{session:read,session:write}',now()+interval '10 minutes',$3,$4,1)",e[2],iid,hashlib.sha256(raw.encode()).hexdigest(),b['id'])
  ctx=await authenticate_session(c,s,raw,required_scope='session:read')
  assert ctx.slots_used==2 and not ctx.metadata['device_capacity_exceeded']
  await c.execute("UPDATE entitlements SET ends_at=now()-interval '1 second' WHERE id=$1",e[4])
  assert await occupied_count(c,e[2])==2
  # Own referral read remains authorized despite no current commercial access.
  assert await referral_info(c,ctx)
 finally:await close(e)


@pytest.mark.parametrize('first',['activation','mobile'])
async def test_activation_confirm_same_admission_lock_revalidates(migrated_url,settings_factory,tmp_path,first):
 e=await environment(migrated_url,settings_factory,tmp_path,n=2);c=e[0];c2=await connect(migrated_url);s=mobile_settings(settings_factory,migrated_url)
 try:
  await claims(e);iid,token=await link(c,s,'activation-race')
  # Force both actual APIs to contend at the existing account fence; no seeded scope.
  async with c.transaction():
   await admission_lock(c,e[2])
   if first=='activation':
    other=asyncio.create_task(confirm(c2,s,token));await asyncio.sleep(.02)
    await activate(c,e[6],e[7],dry_run=False)
   else:
    other=asyncio.create_task(activate(c2,e[6],e[7],dry_run=False));await asyncio.sleep(.02)
    await confirm(c,s,token)
   assert not other.done()
  with pytest.raises((ApiError,ValueError)) as exc:await asyncio.wait_for(other,5)
  if first=='activation':assert isinstance(exc.value,ApiError) and exc.value.code=='DEVICE_LIMIT_REACHED'
  else:assert 'mixed activation' in str(exc.value)
  assert await occupied_count(c,e[2])<=2
  assert await c.fetchval('SELECT count(*) FROM capacity_scopes')==(first=='activation')
 finally:await c2.close();await close(e)


async def test_two_mobile_confirmations_no_overbooking(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path);c=e[0];c2=await connect(migrated_url);s=mobile_settings(settings_factory,migrated_url)
 try:
  await claims(e);await activate(c,e[6],e[7],dry_run=False)
  i1,t1=await link(c,s,'concurrent-one');i2,t2=await link(c,s,'concurrent-two')
  results=await asyncio.wait_for(asyncio.gather(confirm(c,s,t1),confirm(c2,s,t2),return_exceptions=True),5)
  assert sum(isinstance(r,dict) for r in results)==1
  assert sum(isinstance(r,ApiError) and r.code=='DEVICE_LIMIT_REACHED' for r in results)==1
  assert await occupied_count(c,e[2])==2
  assert await c.fetchval("SELECT count(*) FROM account_bindings WHERE status='active'")==1
  assert await c.fetchval("SELECT count(*) FROM capacity_admissions WHERE kind='mobile' AND released_at IS NULL")==1
 finally:await c2.close();await close(e)


async def test_revoke_reactivate_exact_mobile_new_order_generation(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path);c=e[0];s=mobile_settings(settings_factory,migrated_url)
 try:
  await claims(e);await activate(c,e[6],e[7],dry_run=False)
  iid,t=await link(c,s,'reactivation-mobile');await confirm(c,s,t);b=await bound(c,e[2],iid)
  direct=await c.fetchrow("SELECT * FROM capacity_admissions WHERE kind='direct'")
  async with c.transaction():
   await admission_lock(c,e[2]);await revoke_binding_grants(c,binding_id=b['id'])
  assert await occupied_count(c,e[2])==1
  assert await c.fetchval('SELECT released_at IS NOT NULL FROM capacity_admissions WHERE binding_id=$1',b['id'])
  _,newtoken=await link(c,s,'unused',iid);await confirm(c,s,newtoken)
  rebound=await bound(c,e[2],iid)
  assert rebound['id']==b['id'] and rebound['account_id']==b['account_id'] and rebound['generation']==b['generation']+2
  assert await c.fetchrow("SELECT * FROM capacity_admissions WHERE kind='direct'")==direct
  assert await c.fetchval('SELECT admission_order FROM capacity_admissions WHERE binding_id=$1 AND released_at IS NULL',b['id'])==3
  assert await c.fetchval('SELECT rank FROM capacity_live_admissions WHERE binding_id=$1',b['id'])==2
  async with c.transaction():
   await admission_lock(c,e[2]);await revoke_binding_grants(c,binding_id=b['id'])
  revoked=await bound(c,e[2],iid)
  holder_i,holder_t=await link(c,s,'retained-current-mobile');await confirm(c,s,holder_t)
  _,blocked_t=await link(c,s,'unused',iid)
  with pytest.raises(ApiError) as exc:await confirm(c,s,blocked_t)
  assert exc.value.code=='DEVICE_LIMIT_REACHED'
  assert await bound(c,e[2],iid)==revoked
  assert (await bound(c,e[2],holder_i))['status']=='active'
  assert await c.fetchrow("SELECT * FROM capacity_admissions WHERE kind='direct'")==direct
 finally:await close(e)


async def test_shared_live_extra_deadlines_and_gateway_fence(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path,n=2);c=e[0];s=mobile_settings(settings_factory,migrated_url)
 try:
  await claims(e)
  now=await c.fetchval('SELECT clock_timestamp()');end=now+timedelta(days=30);long=now+timedelta(days=3);short=now+timedelta(days=1)
  # Explicit synthetic commercial expiry fixture, no provider/order mutations.
  await c.execute('UPDATE entitlements SET paid_base_device_limit=1,device_limit=3,ends_at=$2 WHERE id=$1',e[4],end)
  await c.execute('INSERT INTO paid_extra_slots(entitlement_id,expires_at) VALUES($1,$2),($1,$3)',e[4],long,short)
  await activate(c,e[6],e[7],dry_run=False)
  iid,t=await link(c,s,'extra-ranked-mobile');await confirm(c,s,t);b=await bound(c,e[2],iid)
  ent=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',e[4])
  assert await live_deadlines(c,ent,now)==[end,long,short] and await paid_limit(c,ent,now)==3
  physical=await c.fetch("SELECT physical_id FROM capacity_live_admissions WHERE kind='direct' ORDER BY rank")
  assert await physical_capacity(c,ent,physical[0]['physical_id'],now)==(True,end)
  assert await physical_capacity(c,ent,physical[1]['physical_id'],now)==(True,long)
  assert await binding_paid_capacity(c,ent,b['id'],now)==(True,short)
  gid=await c.fetchval("INSERT INTO gateways(gateway_key,environment,endpoints,registry_state) VALUES('capacity-gw','test','{}','registered') RETURNING id")
  async with c.transaction():assert await ensure_grant(c,binding_id=b['id'],gateway_id=gid,entitlement_id=ent['id'],max_lease_seconds=864000)=='enqueued'
  assert await c.fetchval('SELECT not_after FROM grants WHERE binding_id=$1',b['id'])==short
  # Existing maintenance uses common ranks: a shorter live extra updates only mobile lease.
  earlier=now+timedelta(hours=6)
  await c.execute('UPDATE paid_extra_slots SET expires_at=$2 WHERE entitlement_id=$1 AND expires_at=$3',ent['id'],earlier,short)
  assert await cap_existing_extra_grants(c,max_lease_seconds=864000)==1
  assert await c.fetchval('SELECT not_after FROM grants WHERE binding_id=$1',b['id'])==earlier
  op=await c.fetchrow("SELECT * FROM outbox_operations WHERE operation_type='gateway.apply_grant' ORDER BY created_at DESC,id DESC LIMIT 1")
  # Select latest actual generation, not arbitrary created_at tie.
  op=await c.fetchrow("SELECT o.* FROM outbox_operations o JOIN grants g ON (o.payload->>'grant_id')=g.opaque_id::text WHERE o.operation_type='gateway.apply_grant' AND (o.payload->>'generation')=g.desired_generation::text")
  await c.execute('UPDATE paid_extra_slots SET expires_at=$2 WHERE entitlement_id=$1 AND expires_at=$3',ent['id'],now-timedelta(seconds=1),earlier)
  calls=[]
  def forbidden(*args):calls.append(args);raise AssertionError('expired rank must not dispatch')
  outcome=await GatewayControlHandlers(s,client_factory=forbidden).apply_grant(c,op)
  assert outcome==('failed','DEVICE_LIMIT_REACHED') and calls==[]
  assert await occupied_count(c,e[2])==3 and not (await binding_capacity(c,ent,b['id']))[0]
  assert (await physical_capacity(c,ent,physical[1]['physical_id']))[0]
 finally:await close(e)


async def test_frozen_prior_plan_immutable_and_mixed_incomplete(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path);c=e[0];s=mobile_settings(settings_factory,migrated_url)
 try:
  await claims(e);await activate(c,e[6],e[7],dry_run=False)
  await freeze_and_enqueue(c,e[5]['id'],UUID(e[6]['id']))
  original=await c.fetchrow('SELECT * FROM delivery_items');source=await c.fetchrow('SELECT * FROM delivery_fulfillments')
  iid,t=await link(c,s,'mixed-after-freeze');await confirm(c,s,t)
  membership=await plan_membership(c,e[2],UUID(e[6]['id']))
  assert membership['activated'] and not membership['complete'] and membership['reason']=='incomplete_account_plan'
  assert [m['kind'] for m in membership['members']]==['direct','mobile']
  with pytest.raises(ValueError,match='incomplete_account_plan'):await freeze_plan(c,e[5]['id'],UUID(e[6]['id']))
  await e[14].run_once()
  assert await c.fetchrow('SELECT * FROM delivery_items')==original
  assert await c.fetchrow('SELECT * FROM delivery_fulfillments')==source
  assert not any(f.writes for f in e[10].values())
  proof=await fulfillment_proof(c,e[5]['id'])
  assert not proof['transport_complete'] and proof['plan_status']=='incomplete_account_plan'
  assert await c.fetchval("SELECT last_error FROM outbox_operations WHERE operation_type='external_direct_apply'")=='incomplete_account_plan'
 finally:await close(e)


@pytest.mark.parametrize('boundary',['stale','owner','overcapacity','current-mobile'])
async def test_activation_rejects_stale_owner_overcapacity_and_imported_mixed(migrated_url,settings_factory,tmp_path,boundary):
 e=await environment(migrated_url,settings_factory,tmp_path,n=2);c=e[0];s=mobile_settings(settings_factory,migrated_url)
 try:
  await claims(e);body=deepcopy(e[6]);allow=deepcopy(e[7])
  if boundary=='stale':body['evidence_sha256']='b'*64
  elif boundary=='owner':
   await c.execute("UPDATE accounts SET status='unlinked' WHERE id=$1",e[2])
  elif boundary=='overcapacity':await c.execute('UPDATE entitlements SET paid_base_device_limit=1 WHERE id=$1',e[4])
  else:
   iid,t=await link(c,s,'mobile-before-activation');await confirm(c,s,t)
  with pytest.raises(ValueError):await activate(c,body,allow,dry_run=False)
  assert await c.fetchval('SELECT count(*) FROM capacity_scopes')==0
  assert await c.fetchval('SELECT count(*) FROM capacity_admissions')==0
 finally:await close(e)


@pytest.mark.parametrize('kind',['trial','imported'])
async def test_mapped_nonpaid_live_limit_no_capacity_bypass(migrated_url,settings_factory,tmp_path,kind):
 e=await environment(migrated_url,settings_factory,tmp_path,kind='trial');c=e[0];s=mobile_settings(settings_factory,migrated_url)
 try:
  await claims(e)
  if kind=='imported':await c.execute("UPDATE entitlements SET kind='imported' WHERE id=$1",e[4])
  await activate(c,e[6],e[7],dry_run=False)
  iid,t=await link(c,s,'nonpaid-mobile');await confirm(c,s,t);b=await bound(c,e[2],iid)
  await c.execute('UPDATE entitlements SET device_limit=1 WHERE id=$1',e[4])
  ent=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',e[4])
  assert not (await binding_paid_capacity(c,ent,b['id']))[0] and await occupied_count(c,e[2])==2
  _,t2=await link(c,s,'nonpaid-third')
  with pytest.raises(ApiError) as exc:await confirm(c,s,t2)
  assert exc.value.code=='DEVICE_LIMIT_REACHED'
 finally:await close(e)


async def test_unmapped_actual_registration_capacity_unchanged(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path);c=e[0];s=mobile_settings(settings_factory,migrated_url)
 try:
  # Staged direct registry does not activate or steal mobile-only places.
  for label in ('old-first','old-second'):
   iid,t=await link(c,s,label);assert (await confirm(c,s,t))['binding_status']=='active'
  _,t=await link(c,s,'old-third')
  with pytest.raises(ApiError) as exc:await confirm(c,s,t)
  assert exc.value.code=='DEVICE_LIMIT_REACHED' and exc.value.details['slots_used']==2
  assert await c.fetchval('SELECT count(*) FROM capacity_admissions')==0
  assert await occupied_count(c,e[2])==2
 finally:await close(e)


async def test_empty_migration_reversible_durable_scope_refuses_down(migrated_url,settings_factory,tmp_path):
 e=await environment(migrated_url,settings_factory,tmp_path);c=e[0]
 try:
  versions=Path(__file__).parents[1]/'terlimo_backend/migrations/versions'
  async with c.transaction():
   await c.execute((versions/'0045_common_capacity.down.sql').read_text())
   await c.execute((versions/'0045_common_capacity.sql').read_text())
  await claims(e);await activate(c,e[6],e[7],dry_run=False)
  with pytest.raises(Exception,match='durable activated capacity'):
   async with c.transaction():await c.execute((versions/'0045_common_capacity.down.sql').read_text())
  with pytest.raises(Exception,match='immutable'):
   async with c.transaction():await c.execute('UPDATE capacity_scopes SET activated_at=now()')
 finally:await close(e)
