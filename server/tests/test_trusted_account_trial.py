"""Bounded actual local HTTP/PG trial integration; synthetic membership only."""
import asyncio,json,hashlib
from uuid import uuid4
from pathlib import Path
from datetime import timedelta
import asyncpg,pytest,pytest_asyncio
from aiohttp import web
from aiohttp.test_utils import TestClient,TestServer
from terlimo_backend.db import Database
from terlimo_backend.auth_api import ApiError
from terlimo_backend.telegram_account import ensure_account
from terlimo_backend.referral_history import import_manifest,manifest_sha256
from terlimo_backend.referral_trusted import trusted_operation
from terlimo_backend.telegram_trial import register_trusted_trial_routes,PATH
from terlimo_backend.trial_activation import (TRIAL_CHANNEL_KEY,ChannelMembershipUnavailable,activate_trial)
from terlimo_backend.payments import apply_paid_entitlement

E=Path(__file__).resolve().parents[3]/'evidence';E.mkdir(exist_ok=True)
class Checker:
 def __init__(self):self.mode=True;self.calls=[];self.entered=asyncio.Event();self.release=asyncio.Event()
 async def is_member(self,tg):
  self.calls.append(tg)
  if self.mode=='delay':self.entered.set();await self.release.wait();return True
  if self.mode=='unavailable':raise ChannelMembershipUnavailable('synthetic boundary unavailable')
  return self.mode
@pytest_asyncio.fixture
async def h(migrated_url,settings_factory,request):
 settings=settings_factory(migrated_url,telegram_bot_key='synthetic-trial-key',telegram_bot_token='synthetic-token',telegram_trial_channel_id='@fixture')
 db=Database(settings);await db.connect();c=await asyncpg.connect(migrated_url);await Database._init_connection(c)
 checker=Checker();app=web.Application();app[TRIAL_CHANNEL_KEY]=checker;register_trusted_trial_routes(app,settings,db)
 client=TestClient(TestServer(app));await client.start_server();calls=[]
 async def post(tg=101,operation='activate',body=None,key='synthetic-trial-key'):
  response=await client.post(PATH,json=body if body is not None else {'operation':operation,'telegram_id':tg},headers={'X-Telegram-Bot-Key':key})
  value=await response.json();calls.append({'body':body if body is not None else {'operation':operation,'telegram_id':tg},'http':response.status,'response':value});return response.status,value
 async def account(tg=101):return (await ensure_account(c,{'operation':'ensure','telegram_id':tg}))['account']['account_ref']
 async def coverage(members):
  doc={'schema':1,'epoch_id':str(uuid4()),'scope':'test','complete':True,'snapshot_watermark':'fixture-snapshot','final_watermark':'fixture-final','writer_fence':'isolated-exclusive-fixture','members':members,'rewards':[]}
  await import_manifest(c,doc,expected_sha256=manifest_sha256(doc),scope='test',activate=True,dry_run=False)
 async def ready(tg=101):
  a=await account(tg);await coverage([member(tg)]);await ensure_account(c,{'operation':'ensure','telegram_id':tg});return await c.fetchval('SELECT id FROM accounts WHERE telegram_id=$1',tg)
 try:yield {'c':c,'url':migrated_url,'settings':settings,'db':db,'checker':checker,'post':post,'account':account,'coverage':coverage,'ready':ready,'calls':calls,'client':client,'app':app}
 finally:
  (E/(request.node.name+'.database.safe.json')).write_text(json.dumps(await snapshot({'c':c}),indent=2)+'\n')
  (E/(request.node.name+'.safe.json')).write_text(json.dumps({'http':calls,'membership_calls':checker.calls},indent=2,default=str)+'\n')
  await client.close();await c.close();await db.close()

def member(tg,trial=False):return dict(telegram_id=tg,code=None,code_absent_verified=True,referred_by_telegram_id=None,trial_used=trial,first_main_paid=False,dispositions={'trial':'not_earned','first_main_paid':'not_earned'})
async def count(h,table='entitlements'):return await h['c'].fetchval('SELECT count(*) FROM '+table)
async def mobile_binding(h,a,within=True):
 c=h['c'];i=await c.fetchval("INSERT INTO installations(public_key_fingerprint,public_key_spki_b64,environment,state) VALUES($1,'AA==','test','bound') RETURNING id",uuid4().hex)
 await c.execute('INSERT INTO account_bindings(account_id,installation_id) VALUES($1,$2)',a,i)
 await c.execute("INSERT INTO registration_links(token_sha256,installation_id,environment,status,expires_at,confirmed_at,telegram_id,within_hour,trial_available,trial_reason) VALUES($1,$2,'test','confirmed',now()+interval '10 minutes',now(),101,$3,$3,'fixture')",uuid4().hex,i,within)
 return i
async def snapshot(h):
 result={}
 for r in await h['c'].fetch("SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename"):
  t=r['tablename'];rows=await h['c'].fetch('SELECT row_to_json(x)::text AS row FROM "'+t+'" x');result[t]=hashlib.sha256('\n'.join(sorted(x['row'] for x in rows)).encode()).hexdigest()
 return result

@pytest.mark.parametrize('body',[{}, {'operation':'activate','telegram_id':True},{'operation':'activate','telegram_id':'101'},{'operation':'activate','telegram_id':0},{'operation':'activate','telegram_id':2**63},{'operation':'other','telegram_id':101},{'operation':'activate','telegram_id':101,'account_ref':'foreign'},{'operation':'activate','telegram_id':101,'days':10}])
async def test_strict_body(h,body):
 before=await snapshot(h);status,data=await h['post'](body=body);assert status==400 and data['code']=='BAD_MESSAGE';assert await snapshot(h)==before;assert not h['checker'].calls
async def test_backend_auth_and_no_foreign_or_nonverified_owner(h):
 a=await h['ready']();before=await snapshot(h)
 assert (await h['post'](key='wrong'))[0]==403
 assert (await h['post'](tg=999))[1]['code']=='REGISTRATION_REQUIRED'
 await h['c'].execute("UPDATE accounts SET status='unlinked' WHERE id=$1",a)
 assert (await h['post']())[1]['code']=='REGISTRATION_REQUIRED'
 assert await count(h)==0 and not h['checker'].calls

@pytest.mark.parametrize('referred',[False,True])
async def test_account_no_installation_7_or_10_frozen_repeat_status(h,referred):
 a=await h['ready']()
 if referred:
  parent=await h['account'](301);await ensure_account(h['c'],{'operation':'ensure','telegram_id':301})
  code=await h['c'].fetchval('SELECT referral_code FROM accounts WHERE telegram_id=301')
  await trusted_operation(h['c'],{'operation':'attach','telegram_id':101,'code':code},'fixture-referral-trial')
 before=await snapshot(h);status,result=await h['post']();assert status==200,result
 trial=result['trial'];assert trial['state']=='active' and trial['replay'] is False and trial['revision']==1 and trial['provisioning_state']=='external_pending'
 row=await h['c'].fetchrow('SELECT * FROM entitlements');assert row['ends_at']-row['starts_at']==timedelta(days=10 if referred else 7);assert str(row['id'])==trial['entitlement_id']
 after=await snapshot(h);assert [t for t in before if before[t]!=after[t]]==['entitlements']
 for operation in ('status','activate'):
  status,result=await h['post'](operation=operation);assert status==200 and result['trial']=={**trial,'replay':True}
 assert await snapshot(h)==after and h['checker'].calls==[101]
 assert await count(h,'installations')==await count(h,'grants')==await count(h,'referral_rewards')==await count(h,'outbox_operations')==0

@pytest.mark.parametrize('condition',['pending','used','expired','paid','imported'])
async def test_history_used_expired_and_commercial_protected(h,condition):
 c=h['c']
 if condition=='pending':await h['account']();expected='SERVICE_UNAVAILABLE'
 elif condition=='used':
  await h['account']();await h['coverage']([member(101,True)]);await ensure_account(c,{'operation':'ensure','telegram_id':101});expected='TRIAL_ALREADY_USED'
 else:
  a=await h['ready']();kind='trial' if condition=='expired' else condition
  await c.execute("INSERT INTO entitlements(account_id,kind,status,starts_at,ends_at,device_limit) VALUES($1,$2,$3,now()-interval '10 days',now()+make_interval(days=>$4),2)",a,kind,'expired' if condition=='expired' else 'active',-3 if condition=='expired' else 30);expected='SUBSCRIPTION_ACTIVE'
 before=await snapshot(h)
 for operation in ('status','activate'):
  status,data=await h['post'](operation=operation)
  if condition=='expired':assert status==200 and data['trial']['state']=='used'
  else:assert status==(503 if condition=='pending' else 409) and data['code']==expected
 assert await snapshot(h)==before and not h['checker'].calls

@pytest.mark.parametrize('failure',[False,'unavailable'])
async def test_membership_failure_then_explicit_success(h,failure):
 await h['ready']();before=await snapshot(h);h['checker'].mode=failure
 status,data=await h['post']();assert status==(403 if failure is False else 503)
 assert await snapshot(h)==before
 h['checker'].mode=True;assert (await h['post']())[0]==200 and await count(h)==1

async def test_status_no_membership_or_write(h):
 await h['ready']();before=await snapshot(h);status,data=await h['post'](operation='status')
 assert status==200 and data['trial']=={'state':'none','starts_at':None,'ends_at':None,'replay':False,'entitlement_id':None,'revision':None,'provisioning_state':None}
 assert not h['checker'].calls and await snapshot(h)==before

async def test_trusted_mobile_race_single_engine_and_wire(h):
 a=await h['ready']();i=await mobile_binding(h,a);h['checker'].mode='delay'
 task=asyncio.create_task(h['post']());await asyncio.wait_for(h['checker'].entered.wait(),5)
 checker=Checker();c=await asyncpg.connect(h['url']);await Database._init_connection(c)
 try:mobile=await activate_trial(c,h['settings'],checker,installation_id=i)
 finally:await c.close()
 assert set(mobile)=={'trial','account_state'} and set(mobile['trial'])=={'state','starts_at','ends_at','replay'}
 assert mobile['trial']['replay'] is False
 h['checker'].release.set();status,data=await asyncio.wait_for(task,5);assert status==200 and data['trial']['replay'] is True
 assert data['trial']['starts_at']==mobile['trial']['starts_at'] and data['trial']['ends_at']==mobile['trial']['ends_at'] and await count(h)==1

async def test_delayed_membership_real_paid_writer_wins(h):
 a=await h['ready']();i=await mobile_binding(h,a);h['checker'].mode='delay'
 task=asyncio.create_task(h['post']());await asyncio.wait_for(h['checker'].entered.wait(),5)
 c=await asyncpg.connect(h['url']);await Database._init_connection(c)
 try:
  b=await c.fetchval('SELECT id FROM account_bindings WHERE installation_id=$1',i)
  order=await c.fetchval("INSERT INTO payment_orders(installation_id,binding_id,account_id,idempotency_key,quote,amount,currency,months,tariff_key,status) VALUES($1,$2,$3,'fixture-paid-delay','{}',200,'RUB',1,'test','succeeded') RETURNING id",i,b,a)
  paid=await apply_paid_entitlement(c,h['settings'],order_id=order);assert paid is not None
  row=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',paid)
 finally:await c.close()
 h['checker'].release.set();status,data=await asyncio.wait_for(task,5);assert status==409 and data['code']=='SUBSCRIPTION_ACTIVE'
 assert dict(await h['c'].fetchrow('SELECT * FROM entitlements WHERE id=$1',paid))==dict(row)
 assert await h['c'].fetchval("SELECT count(*) FROM entitlements WHERE kind='trial'")==0

async def test_mobile_binding_and_within_hour_denial(h):
 a=await h['ready']();i=await mobile_binding(h,a,False)
 with pytest.raises(ApiError) as e:await activate_trial(h['c'],h['settings'],h['checker'],installation_id=i)
 assert e.value.code=='TRIAL_NOT_ELIGIBLE' and not h['checker'].calls
 await h['c'].execute("UPDATE account_bindings SET status='revoked' WHERE installation_id=$1",i)
 with pytest.raises(ApiError) as e:await activate_trial(h['c'],h['settings'],h['checker'],installation_id=i)
 assert e.value.code=='REGISTRATION_REQUIRED' and await count(h)==0

async def test_actual_mobile_http_wire_and_boundary(h):
 from terlimo_backend.trial_activation import ACTIVATE_PATH,_activate_handler
 a=await h['ready']();i=await mobile_binding(h,a,False)
 token='synthetic-mobile-session'
 b=await h['c'].fetchval('SELECT id FROM account_bindings WHERE installation_id=$1',i)
 await h['c'].execute("INSERT INTO sessions(account_id,installation_id,scopes,expires_at,token_sha256,binding_id,binding_generation) VALUES($1,$2,'{session:read,session:write}',now()+interval '10 minutes',$3,$4,1)",a,i,hashlib.sha256(token.encode()).hexdigest(),b)
 # Existing HTTP wrapper and real session authorizer; no application/worker startup.
 app=web.Application();app[TRIAL_CHANNEL_KEY]=h['checker'];app.router.add_post(ACTIVATE_PATH,_activate_handler(h['settings'],h['db']))
 client=TestClient(TestServer(app));await client.start_server()
 async def activate():
  r=await client.post(ACTIVATE_PATH,json={},headers={'Authorization':'Bearer '+token});return r.status,await r.json()
 try:
  status,data=await activate();assert status==403 and data['code']=='TRIAL_NOT_ELIGIBLE' and not h['checker'].calls
  await h['c'].execute('UPDATE registration_links SET within_hour=true WHERE installation_id=$1',i)
  status,data=await activate();assert status==200 and set(data)=={'request_id','status','trial','account_state'}
  assert set(data['trial'])=={'state','starts_at','ends_at','replay'} and data['trial']['replay'] is False
  status,replay=await activate();assert status==200 and replay['trial']=={**data['trial'],'replay':True}
  assert h['checker'].calls==[101] and await count(h)==1
 finally:await client.close()
