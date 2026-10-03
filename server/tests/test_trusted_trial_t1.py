"""Deterministic actual PG owner/advisory wait graph, not coroutine overlap."""
import asyncio,json,hashlib,types
from pathlib import Path
from dataclasses import replace
from uuid import UUID,uuid4
from datetime import timedelta
import asyncpg,pytest
from test_trusted_account_trial import h,E,Checker
from terlimo_backend.db import Database
from terlimo_backend.telegram_billing import billing_operation
from terlimo_backend.payments import ProviderPayment,apply_paid_entitlement
from terlimo_backend.telegram_trial import trusted_trial

class Provider:
 async def create_payment(self,**kw):
  return ProviderPayment(provider_payment_id='offline-t1-'+kw['order_ref'],pay_url='https://checkout.invalid/'+kw['order_ref'],qr=None,variant=kw['method'])

class BarrierConnection:
 """Delegate real SQL unchanged; pause only AFTER the selected acquired lock."""
 def __init__(self,c,role,trial_locked,paid_share,release):
  self.c=c;self.role=role;self.trial_locked=trial_locked;self.paid_share=paid_share;self.release=release;self.paused=False
 def __getattr__(self,k):return getattr(self.c,k)
 async def execute(self,q,*a,**kw):
  value=await self.c.execute(q,*a,**kw)
  if self.role=='trial' and 'pg_advisory_xact_lock' in q and a and str(a[0]).startswith('trial:'):
   self.trial_locked.set();await self.release.wait()
  return value
 async def fetchval(self,q,*a,**kw):
  value=await self.c.fetchval(q,*a,**kw)
  if self.role=='paid' and 'FROM accounts WHERE id=$1 FOR SHARE' in q and not self.paused:
   self.paused=True;self.paid_share.set()
  return value

async def wait_blocked(observer,blocked,blocking):
 for _ in range(200):
  if blocking in await observer.fetchval('SELECT pg_blocking_pids($1)',blocked):return
  await asyncio.sleep(.01)
 raise AssertionError('Expected paid→trial SQL lock wait absent')

@pytest.mark.parametrize('version',['old','fixed'])
async def test_t1_account_owned_paid_wait_graph(h,version):
 a=await h['ready']();s=replace(h['settings'],payment_price_rub_1=200,payment_price_rub_3=480,payment_price_rub_6=840,payment_tariff_key='terlimo-200-30d-v1',platega_methods='sbp,international,crypto')
 quote=await billing_operation(h['c'],s,{'operation':'quote','telegram_id':101,'plan_id':'terlimo-30d','duration_code':'days:30','method':'sbp'},uuid4().hex)
 result=await billing_operation(h['c'],s,{'operation':'create','telegram_id':101,'quote_id':quote['quote_id']},uuid4().hex,provider=Provider())
 order_id=UUID(result['payment_id'])
 await h['c'].execute("UPDATE payment_orders SET status='succeeded' WHERE id=$1",order_id)
 order=await h['c'].fetchrow('SELECT * FROM payment_orders WHERE id=$1',order_id)
 assert order['owner_kind']=='telegram_account' and order['trusted_owner_account_id']==a and order['installation_id'] is None
 ct=await asyncpg.connect(h['url']);cp=await asyncpg.connect(h['url'])
 for c in (ct,cp):await Database._init_connection(c);await c.execute("SET deadlock_timeout='300ms'")
 trial_locked=asyncio.Event();paid_share=asyncio.Event();release=asyncio.Event()
 t=BarrierConnection(ct,'trial',trial_locked,paid_share,release);p=BarrierConnection(cp,'paid',trial_locked,paid_share,release)
 trial_fn=trusted_trial
 if version=='old':
  original=(Path(__file__).parent/'fixtures/trusted_trial_t1_original.py.txt').read_bytes()
  assert hashlib.sha256(original).hexdigest()=='4eee2cc82a17b719cbf4d9f554079953e084536ffbe0981aaa09c05ab4d1db40'
  source=original.decode()
  module=types.ModuleType('terlimo_backend._t1_original_trial');module.__package__='terlimo_backend';exec(compile(source,'original-e268165-telegram_trial.py','exec'),module.__dict__);trial_fn=module.trusted_trial
 tasks=[]
 try:
  tasks.append(asyncio.create_task(trial_fn(t,Checker(),{'operation':'activate','telegram_id':101})))
  await asyncio.wait_for(trial_locked.wait(),5)
  tasks.append(asyncio.create_task(apply_paid_entitlement(p,s,order_id=order_id)))
  await asyncio.wait_for(paid_share.wait(),5)
  await wait_blocked(h['c'],cp.get_server_pid(),ct.get_server_pid())
  locks=await h['c'].fetch("SELECT pid,locktype,mode,granted FROM pg_locks WHERE pid=ANY($1::int[]) AND locktype IN ('advisory','transactionid','tuple') ORDER BY pid,locktype,mode",[ct.get_server_pid(),cp.get_server_pid()])
  release.set();results=await asyncio.wait_for(asyncio.gather(*tasks,return_exceptions=True),8)
  deadlocks=[x for x in results if isinstance(x,asyncpg.DeadlockDetectedError)]
  if version=='old':assert len(deadlocks)==1,results
  else:
   assert not any(isinstance(x,BaseException) for x in results),results
   assert results[0]['trial']['state']=='active' and results[1] is not None
   paid=await h['c'].fetchrow('SELECT * FROM entitlements WHERE id=$1',results[1])
   assert paid['kind']=='paid' and paid['status']=='active' and paid['ends_at']-paid['starts_at']==timedelta(days=30)
   applied=await h['c'].fetchrow('SELECT * FROM payment_orders WHERE id=$1',order_id)
   assert applied['applied_entitlement_id']==paid['id'] and applied['credited_entitlement_revision']==paid['revision'] and applied['credit_review_reason'] is None
   assert await h['c'].fetchval("SELECT count(*) FROM entitlements WHERE kind='paid'")==1
   assert await h['c'].fetchval("SELECT count(*) FROM entitlements WHERE kind='trial'")==1
   assert await h['c'].fetchval('SELECT count(*) FROM installations')==0
   assert await h['c'].fetchval('SELECT count(*) FROM grants')==0
  assert not ct.is_in_transaction() and not cp.is_in_transaction()
  migrations=await h['c'].fetch('SELECT * FROM schema_migrations ORDER BY id')
  assert migrations[-1]['id']=='0042_account_orders' and len(migrations)==42
  (E/('t1-'+version+'.lock-graph.safe.json')).write_text(json.dumps({'version':version,'status':'PASS','schema_migrations':[dict(x) for x in migrations],'paid_blocks_on_trial_before_release':True,'acquired_lock_snapshot':[dict(x) for x in locks],'outcomes':[{'error':type(x).__name__,'sqlstate':getattr(x,'sqlstate',None),'trace':str(x)} if isinstance(x,BaseException) else str(x) for x in results],'account_owned_order':{'owner_kind':order['owner_kind'],'installation_id':None,'quote_id':str(order['quote_id']) if 'quote_id' in order else quote['quote_id']}},indent=2,default=str)+'\n')
 finally:
  release.set()
  for task in tasks:
   if not task.done():task.cancel()
  await asyncio.gather(*tasks,return_exceptions=True);await ct.close();await cp.close()
