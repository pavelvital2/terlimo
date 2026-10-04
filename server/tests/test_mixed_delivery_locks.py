"""M1/M2 only: actual PG locks/handlers, deterministic barriers, no runtime."""
import asyncio
from datetime import timedelta
from uuid import UUID
import pytest
from test_mixed_delivery import setup,done,issue_mobile,run_kind,settle
from test_referral_trusted import connect
from terlimo_backend.gateway_control import ensure_grant,revoke_binding_grants,grant_owner_lock
from terlimo_backend.mixed_delivery import RECONCILE,grant_event
from terlimo_backend.payment_products import cap_existing_extra_grants
from terlimo_backend.worker import CLAIM_SQL
from terlimo_backend.delivery_plan import digest,obj

pytestmark=pytest.mark.asyncio

class BarrierConnection:
 def __init__(self,c,predicate,entered,release):self.c=c;self.predicate=predicate;self.entered=entered;self.release=release;self.fired=False
 def __getattr__(self,k):return getattr(self.c,k)
 async def fetchrow(self,q,*args):
  r=await self.c.fetchrow(q,*args)
  if not self.fired and self.predicate(q):
   self.fired=True;self.entered.set();await self.release.wait()
  return r

async def blocked_on(inspect,waiter,holder):
 # Poll concrete pg_blocking_pids, not elapsed sleep as evidence of lock order.
 async with asyncio.timeout(5):
  while True:
   blockers=await inspect.fetchval('SELECT pg_blocking_pids($1)',waiter)
   if holder in blockers:return blockers
   await asyncio.sleep(.005)

async def test_m1_immutable_duplicate_event_while_token_waits_binding(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path);c=h['c'];e=await connect(migrated_url);r=await connect(migrated_url)
 entered=asyncio.Event();release=asyncio.Event();et=rt=None
 try:
  await issue_mobile(h)
  g=await c.fetchrow('SELECT * FROM grants WHERE binding_id=$1',h['b']['id'])
  key='mixed:'+str(g['delivery_source_id'])+':'+digest(['grant',str(g['id']),g['desired_generation'],'desired'])
  original=await c.fetchrow('SELECT * FROM outbox_operations WHERE idempotency_key=$1',key)
  # Claim its ORIGINAL event with actual worker SQL; no status reset/new event.
  claimed=await r.fetchrow(CLAIM_SQL.replace("WHERE (status = 'pending'","WHERE id='"+str(original['id'])+"' AND ((status = 'pending'").replace('        ORDER BY available_at, id','        ) ORDER BY available_at, id'),h['worker'].worker_id,30.)
  assert claimed and claimed['id']==original['id']
  async def ordinary():
   async with e.transaction():
    return await ensure_grant(BarrierConnection(e,lambda q:'SELECT * FROM grants WHERE binding_id' in q,entered,release),binding_id=h['b']['id'],gateway_id=h['gid'],entitlement_id=h['e'][4],max_lease_seconds=h['s'].gateway_max_lease_seconds)
  et=asyncio.create_task(ordinary());await asyncio.wait_for(entered.wait(),5)
  rt=asyncio.create_task(h['worker']._process(r,claimed))
  await blocked_on(c,r.get_server_pid(),e.get_server_pid())
  # Verify R has O token row, while E still owns binding/grant. NOW release E.
  async with c.transaction():
   with pytest.raises(Exception) as err:
    async with c.transaction():await c.fetchrow('SELECT id FROM outbox_operations WHERE id=$1 FOR UPDATE NOWAIT',original['id'])
   assert getattr(err.value,'sqlstate',None)=='55P03'
  release.set();assert await asyncio.wait_for(et,5)=='pending'
  assert await asyncio.wait_for(rt,5) is None
  exact=await c.fetchrow('SELECT operation_type,idempotency_key,payload FROM outbox_operations WHERE id=$1',original['id'])
  assert dict(exact)=={k:original[k] for k in exact.keys()}
  assert await c.fetchval('SELECT count(*) FROM outbox_operations WHERE idempotency_key=$1',key)==1
  assert await c.fetchval('SELECT count(*) FROM delivery_plans')==1
  assert await c.fetchval('SELECT status FROM outbox_operations WHERE id=$1',original['id'])=='done'
 finally:
  release.set()
  for t in (et,rt):
   if t:await asyncio.gather(t,return_exceptions=True)
  await e.close();await r.close();await done(h)

async def test_m2_actual_maintenance_refresh_excludes_mapped_revoke_before_binding(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path);c=h['c'];m=await connect(migrated_url);d=await connect(migrated_url)
 entered=asyncio.Event();release=asyncio.Event();mt=dt=None
 try:
  # Ordinary extra-ranked mobile with actual existing ensure; no financial source rewrite.
  end=await c.fetchval("SELECT clock_timestamp()+interval '10 minutes'")
  await c.execute('UPDATE entitlements SET paid_base_device_limit=1 WHERE id=$1',h['e'][4])
  await c.execute('INSERT INTO paid_extra_slots(entitlement_id,expires_at) VALUES($1,$2)',h['e'][4],end)
  await issue_mobile(h)
  g=await c.fetchrow('SELECT * FROM grants WHERE binding_id=$1',h['b']['id'])
  shorter=end-timedelta(minutes=5)
  await c.execute('UPDATE paid_extra_slots SET expires_at=$2 WHERE entitlement_id=$1',h['e'][4],shorter)
  proxy=BarrierConnection(m,lambda q:'SELECT * FROM grants WHERE binding_id' in q,entered,release)
  mt=asyncio.create_task(cap_existing_extra_grants(proxy,max_lease_seconds=h['s'].gateway_max_lease_seconds))
  await asyncio.wait_for(entered.wait(),5)
  async def revoke():
   async with d.transaction():return await revoke_binding_grants(d,binding_id=h['b']['id'])
  dt=asyncio.create_task(revoke())
  await blocked_on(c,d.get_server_pid(),m.get_server_pid())
  # D waits owner, has NOT acquired owner UPDATE or binding: inspect real row conflicts.
  inspector=await connect(migrated_url)
  try:
   async with inspector.transaction():
    await inspector.fetchval('SELECT id FROM accounts WHERE id=$1 FOR KEY SHARE NOWAIT',h['e'][2])
    with pytest.raises(Exception) as err:
     async with inspector.transaction():await inspector.fetchval('SELECT id FROM account_bindings WHERE id=$1 FOR UPDATE NOWAIT',h['b']['id'])
    assert getattr(err.value,'sqlstate',None)=='55P03'
  finally:await inspector.close()
  release.set();assert await asyncio.wait_for(mt,5)==1
  assert await asyncio.wait_for(dt,5)==1
  after=await c.fetchrow('SELECT * FROM grants WHERE id=$1',g['id'])
  b=await c.fetchrow('SELECT * FROM account_bindings WHERE id=$1',h['b']['id'])
  assert after['state']=='revoked' and after['desired_generation']==g['desired_generation']+2
  assert b['status']=='revoked' and b['generation']==h['b']['generation']+1
  assert not await c.fetchval('SELECT EXISTS(SELECT 1 FROM capacity_admissions WHERE binding_id=$1 AND released_at IS NULL)',b['id'])
 finally:
  release.set()
  for t in (mt,dt):
   if t:await asyncio.gather(t,return_exceptions=True)
  await m.close();await d.close();await done(h)

@pytest.mark.parametrize('mapped',[True,False])
async def test_early_owner_fence_revalidates_changed_binding_owner(migrated_url,settings_factory,tmp_path,mapped):
 h=await setup(migrated_url,settings_factory,tmp_path,activate_scope=mapped);c=h['c'];e=await connect(migrated_url)
 entered=asyncio.Event();release=asyncio.Event();task=None
 try:
  # Pause after an actual owner pre-read, before the owner lock. Scope is unchanged.
  class BeforeOwner:
   def __getattr__(self,k):return getattr(e,k)
   async def fetchval(self,q,*args):
    value=await e.fetchval(q,*args)
    if q=='SELECT account_id FROM account_bindings WHERE id=$1':entered.set();await release.wait()
    return value
  async def ensure():
   async with e.transaction():return await ensure_grant(BeforeOwner(),binding_id=h['b']['id'],gateway_id=h['gid'],entitlement_id=h['e'][4],max_lease_seconds=h['s'].gateway_max_lease_seconds)
  task=asyncio.create_task(ensure());await asyncio.wait_for(entered.wait(),5)
  other=await c.fetchval("INSERT INTO accounts(status,telegram_id) VALUES('verified',987001) RETURNING id")
  await c.execute('UPDATE account_bindings SET account_id=$2 WHERE id=$1',h['b']['id'],other)
  await c.execute('UPDATE entitlements SET account_id=$2 WHERE id=$1',h['e'][4],other)
  release.set();assert await asyncio.wait_for(task,5)=='ownership_mismatch'
  assert await c.fetchval('SELECT count(*) FROM grants')==0
  assert await c.fetchval("SELECT count(*) FROM outbox_operations WHERE operation_type='gateway.apply_grant'")==0
 finally:
  release.set()
  if task:await asyncio.gather(task,return_exceptions=True)
  await e.close();await done(h)


async def test_unmapped_unchanged_ensure_adds_no_owner_row_lock(migrated_url,settings_factory,tmp_path):
 h=await setup(migrated_url,settings_factory,tmp_path,activate_scope=False);c=h['c'];e=await connect(migrated_url)
 try:
  await issue_mobile(h)
  async with c.transaction():
   await c.fetchval('SELECT id FROM accounts WHERE id=$1 FOR UPDATE',h['e'][2])
   async with e.transaction():
    result=await asyncio.wait_for(ensure_grant(e,binding_id=h['b']['id'],gateway_id=h['gid'],entitlement_id=h['e'][4],max_lease_seconds=h['s'].gateway_max_lease_seconds),3)
   assert result=='pending'
  assert await c.fetchval('SELECT count(*) FROM grants')==1
  assert await c.fetchval("SELECT count(*) FROM outbox_operations WHERE operation_type='gateway.apply_grant'")==1
 finally:await e.close();await done(h)
