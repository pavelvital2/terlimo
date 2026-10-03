"""Trusted ensure only: actual HTTP and isolated PostgreSQL/mobile confirmation."""
import asyncio
from uuid import UUID

import pytest
from aiohttp import web
from aiohttp.test_utils import TestClient, TestServer

from terlimo_backend.auth_api import ApiError
from terlimo_backend.db import Database
from terlimo_backend.referral import stage_history
from terlimo_backend.telegram_account import PATH, ensure_account, register_trusted_account_routes
from terlimo_backend.telegram_binding import confirm_registration, create_registration_link
from test_referral_trusted import connect, owner, KEY
from test_referral_history_coverage import manifest, load
from test_referral_identity import install

BODY={'operation':'ensure','telegram_id':701}

async def empty_effects(c):
    for table in ('installations','account_bindings','entitlements','referral_rewards','payment_orders','grants','registration_links'):
        assert await c.fetchval(f'SELECT count(*) FROM {table}')==0,table

@pytest.mark.asyncio
async def test_actual_route_auth_strict_input_pending_and_repeat(migrated_url,settings_factory):
    c=await connect(migrated_url)
    db=Database(settings_factory(migrated_url,telegram_bot_key=KEY));await db.connect()
    app=web.Application();register_trusted_account_routes(app,db.settings if hasattr(db,'settings') else settings_factory(migrated_url,telegram_bot_key=KEY),db)
    client=TestClient(TestServer(app));await client.start_server()
    try:
        for headers in ({},{'X-Telegram-Bot-Key':'bad'}):
            assert (await client.post(PATH,json=BODY,headers=headers)).status==403
        h={'X-Telegram-Bot-Key':KEY}
        for tg in (True,False,0,-1,2**63,1.5,'701',None):
            assert (await client.post(PATH,json={**BODY,'telegram_id':tg},headers=h)).status==400
        for extra in ('account_ref','caller','status'):
            assert (await client.post(PATH,json={**BODY,extra:'x'},headers=h)).status==400
        for body in ({'operation':'read','telegram_id':701},{'operation':'ensure'},[],None):
            assert (await client.post(PATH,json=body,headers=h)).status==400
        assert await c.fetchval('SELECT count(*) FROM accounts')==0
        r=await client.post(PATH,json=BODY,headers=h);assert r.status==200
        result=await r.json();assert set(result)=={'request_id','account'}
        a=UUID(result['account']['account_ref'])
        assert result['account']=={'account_ref':str(a),'status':'verified','history_state':'history_pending'}
        before=await c.fetchrow('SELECT * FROM accounts WHERE id=$1',a)
        again=await client.post(PATH,json=BODY,headers=h)
        assert (await again.json())['account']==result['account']
        assert before==await c.fetchrow('SELECT * FROM accounts WHERE id=$1',a)
        assert before['referral_code'] is None
        await empty_effects(c)
    finally:await client.close();await db.close();await c.close()

@pytest.mark.asyncio
@pytest.mark.parametrize('complete',[False,True])
async def test_coverage_new_identity_and_no_access(migrated_url,complete):
    c=await connect(migrated_url)
    try:
        await load(c,manifest(complete=complete),activate=complete)
        result=await ensure_account(c,BODY)
        assert result['account']['history_state']==('ready' if complete else 'history_pending')
        a=UUID(result['account']['account_ref'])
        row=await c.fetchrow('SELECT * FROM referral_benefits WHERE account_id=$1',a)
        assert not row['imported_trial_used'] and not row['imported_first_main_paid']
        assert bool(await c.fetchval('SELECT referral_code FROM accounts WHERE id=$1',a))==complete
        assert await ensure_account(c,BODY)==result
        await empty_effects(c)
    finally:await c.close()

@pytest.mark.asyncio
async def test_legacy_flags_attribution_and_nonverified_not_promoted(migrated_url):
    c=await connect(migrated_url)
    try:
        a=await owner(c,701)
        await stage_history(c,[dict(telegram_id=701,code='Kept701',referred_by_telegram_id=None,proven_new=False,trial_used=True,first_main_paid=True)],source_sha256='a'*64)
        assert (await ensure_account(c,BODY))['account']['history_state']=='ready'
        parent=await owner(c,702,'Parent702')
        await c.execute('UPDATE accounts SET referred_by_account_id=$2,referral_attribution_receipt_id=gen_random_uuid() WHERE id=$1',a,parent)
        before=await c.fetchrow('SELECT * FROM accounts WHERE id=$1',a)
        await ensure_account(c,BODY)
        assert before==await c.fetchrow('SELECT * FROM accounts WHERE id=$1',a)
        flags=await c.fetchrow('SELECT * FROM referral_benefits WHERE account_id=$1',a)
        assert flags['imported_trial_used'] and flags['imported_first_main_paid']
        await c.execute("INSERT INTO accounts(status,telegram_id) VALUES('unlinked',703)")
        before=await c.fetchrow('SELECT * FROM accounts WHERE telegram_id=703')
        with pytest.raises(ApiError) as e:await ensure_account(c,{**BODY,'telegram_id':703})
        assert e.value.code=='REGISTRATION_REQUIRED'
        assert before==await c.fetchrow('SELECT * FROM accounts WHERE telegram_id=703')
        assert not await c.fetchval('SELECT 1 FROM referral_benefits WHERE account_id=$1',before['id'])
        await empty_effects(c)
    finally:await c.close()

class BeforeInsert:
    """Pause actual operation after absent mapping read, before its INSERT."""
    def __init__(self,c):self.c=c;self.arrived=asyncio.Event();self.release=asyncio.Event()
    def __getattr__(self,k):return getattr(self.c,k)
    async def pause(self,q):
        if 'INSERT INTO accounts' in q:
            self.arrived.set();await asyncio.wait_for(self.release.wait(),5)
    async def fetchval(self,q,*a):await self.pause(q);return await self.c.fetchval(q,*a)
    async def fetchrow(self,q,*a):await self.pause(q);return await self.c.fetchrow(q,*a)

@pytest.mark.asyncio
@pytest.mark.parametrize('loser',['ensure','mobile','second_ensure'])
async def test_real_mapping_insert_race_and_same_token_retry(migrated_url,settings_factory,loser):
    c1=await connect(migrated_url);c2=await connect(migrated_url)
    settings=settings_factory(migrated_url,telegram_bot_username='test_bot',telegram_bot_key=KEY)
    try:
        iid=await install(c1,'ensure-race')
        pending=await create_registration_link(c1,settings,installation_id=iid)
        async def mobile(c):
            return await confirm_registration(c,settings,token=pending['token'],telegram_id=701,telegram_username=None)
        paused=BeforeInsert(c1)
        task=asyncio.create_task(mobile(paused) if loser=='mobile' else ensure_account(paused,BODY))
        await asyncio.wait_for(paused.arrived.wait(),5)
        if loser=='ensure':await mobile(c2)
        else:await ensure_account(c2,BODY)
        paused.release.set()
        with pytest.raises(ApiError) as e:await asyncio.wait_for(task,5)
        assert e.value.code=='REVISION_CONFLICT' and e.value.retryable
        result=await ensure_account(c1,BODY)
        await mobile(c2)  # Explicit same-token retry; no replacement link or identity.
        a=UUID(result['account']['account_ref'])
        assert await c1.fetchval('SELECT count(*) FROM accounts')==1
        assert await c1.fetchval('SELECT account_id FROM account_bindings WHERE installation_id=$1',iid)==a
        assert await c1.fetchval('SELECT count(*) FROM installations')==1
        assert await c1.fetchval('SELECT count(*) FROM registration_links')==1
        assert await c1.fetchval('SELECT count(*) FROM entitlements')==0
        assert await c1.fetchval('SELECT count(*) FROM referral_rewards')==0
    finally:await c1.close();await c2.close()
