"""Actual internal route + shared mutation on isolated PG; no external services."""
import asyncio
import json
import uuid
from types import SimpleNamespace

import asyncpg
import pytest
from aiohttp import web
from aiohttp.test_utils import TestClient, TestServer

from terlimo_backend.auth_api import ApiError
from terlimo_backend.db import Database
from terlimo_backend.referral import attach_candidate, referral_info, stage_history
from terlimo_backend.referral_trusted import PATH, register_trusted_referral_routes, trusted_operation
from terlimo_backend.telegram_binding import _account_advisory

KEY='isolated-backend-key'


async def connect(url):
    c=await asyncpg.connect(url)
    await Database._init_connection(c)
    return c


async def owner(c,tg,code=None):
    a=await c.fetchval("INSERT INTO accounts(status,telegram_id,referral_code) VALUES('verified',$1,$2) RETURNING id",tg,code)
    if code:
        await c.execute("INSERT INTO referral_benefits(account_id,history_state) VALUES($1,'ready')",a)
    return a


async def install(c,a):
    i=await c.fetchval("INSERT INTO installations(environment,public_key_fingerprint) VALUES('test',$1) RETURNING id",uuid.uuid4().hex)
    await c.execute("INSERT INTO account_bindings(account_id,installation_id) VALUES($1,$2)",a,i)
    return i


async def client_for(settings):
    db=Database(settings);await db.connect()
    app=web.Application(client_max_size=64*1024)
    register_trusted_referral_routes(app,settings,db)
    client=TestClient(TestServer(app));await client.start_server()
    return client,db


@pytest.mark.asyncio
async def test_actual_route_key_verified_identity_readiness_and_no_subscription(migrated_url,settings_factory):
    c=await connect(migrated_url)
    client,db=await client_for(settings_factory(migrated_url,telegram_bot_key=KEY))
    try:
        a=await owner(c,301)
        await stage_history(c,[dict(telegram_id=301,code='Kept301',referred_by_telegram_id=None,proven_new=False,trial_used=False,first_main_paid=False)],source_sha256='a'*64)
        for headers in ({},{'X-Telegram-Bot-Key':'wrong'}):
            r=await client.post(PATH,json={'operation':'read','telegram_id':301},headers=headers)
            assert r.status==403
        assert await c.fetchval('SELECT count(*) FROM referral_benefits')==0
        headers={'X-Telegram-Bot-Key':KEY}
        r=await client.post(PATH,json={'operation':'read','telegram_id':301},headers=headers)
        assert r.status==200
        info=(await r.json())['referral']
        assert info['code']=='Kept301' and info['terms_version']=='referral-20261003-v1'
        assert info['account_ref']==str(a)
        assert await c.fetchval('SELECT count(*) FROM entitlements')==0
        assert await c.fetchval('SELECT count(*) FROM installations')==0
        r=await client.post(PATH,json={'operation':'read','telegram_id':399},headers=headers)
        assert r.status==409 and (await r.json())['code']=='REGISTRATION_REQUIRED'
        pending=await owner(c,302)
        r=await client.post(PATH,json={'operation':'read','telegram_id':302},headers=headers)
        assert r.status==503 and (await r.json())['code']=='REFERRAL_HISTORY_PENDING'
        for body in ({'operation':'read','telegram_id':True},
                     {'operation':'read','telegram_id':301,'account_ref':str(a)},
                     {'operation':'attach','telegram_id':301,'code':'X','caller':'site'}):
            assert (await client.post(PATH,json=body,headers=headers)).status==400
        assert await c.fetchval('SELECT referral_code FROM accounts WHERE id=$1',pending) is None
    finally: await client.close();await db.close();await c.close()


@pytest.mark.asyncio
async def test_attach_exact_key_replay_precedes_eligibility_and_body_conflict(migrated_url,settings_factory):
    c=await connect(migrated_url)
    client,db=await client_for(settings_factory(migrated_url,telegram_bot_key=KEY))
    try:
        parent=await owner(c,311,'Parent');child=await owner(c,312,'Child')
        body={'operation':'attach','telegram_id':312,'code':'Parent'}
        headers={'X-Telegram-Bot-Key':KEY,'Idempotency-Key':'persistent-key-001'}
        assert (await client.post(PATH,json=body,headers={'X-Telegram-Bot-Key':KEY})).status==400
        first=await client.post(PATH,json=body,headers=headers)
        assert first.status==200
        proof=(await first.json())['attribution']
        assert set(proof)=={'receipt_id','account_ref','idempotency_key','state','reason'}
        assert proof['state']=='attached' and proof['reason'] is None
        # Dynamic state changes cannot rewrite/reject the committed receipt.
        await c.execute("UPDATE referral_benefits SET history_state='history_pending' WHERE account_id=$1",parent)
        again=await client.post(PATH,json=body,headers=headers)
        assert again.status==200 and (await again.json())['attribution']==proof
        changed=await client.post(PATH,json={**body,'code':'Child'},headers=headers)
        assert changed.status==409 and (await changed.json())['code']=='IDEMPOTENCY_CONFLICT'
        assert await c.fetchval('SELECT referred_by_account_id FROM accounts WHERE id=$1',child)==parent
        assert await c.fetchval('SELECT count(*) FROM referral_trusted_operations')==1
        assert await c.fetchval('SELECT jsonb_typeof(result) FROM referral_trusted_operations')=='object'
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
        assert await c.fetchval('SELECT count(*) FROM entitlements')==0
    finally: await client.close();await db.close();await c.close()


@pytest.mark.asyncio
@pytest.mark.parametrize('case,reason',[('invalid','invalid'),('self','self'),('already','already_attributed'),('active','ineligible'),('ineligible','ineligible')])
async def test_terminal_rejections_are_durable_not_duplicate_attribution(migrated_url,case,reason):
    c=await connect(migrated_url)
    try:
        p=await owner(c,321,'Parent');a=await owner(c,322,'Own')
        if case=='already': await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',a,p)
        if case=='active': await c.execute("INSERT INTO entitlements(account_id,kind,status,starts_at,ends_at,device_limit) VALUES($1,'trial','active',now(),now()+interval '1 day',2)",a)
        if case=='ineligible': await c.execute("UPDATE referral_benefits SET history_state='ineligible' WHERE account_id=$1",a)
        body={'operation':'attach','telegram_id':322,'code':'Own' if case=='self' else 'Unknown' if case=='invalid' else 'Parent'}
        first=await trusted_operation(c,body,'stable-reject-key')
        assert first['attribution']['state']=='rejected' and first['attribution']['reason']==reason
        assert await trusted_operation(c,body,'stable-reject-key')==first
        assert await c.fetchval('SELECT count(*) FROM referral_trusted_operations')==1
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
    finally: await c.close()


@pytest.mark.asyncio
async def test_transient_history_does_not_persist_rejection_and_mobile_still_authorizes_binding(migrated_url):
    c=await connect(migrated_url)
    try:
        p=await owner(c,331,'Parent');a=await owner(c,332)
        body={'operation':'attach','telegram_id':332,'code':'Parent'}
        with pytest.raises(ApiError,match='REFERRAL_HISTORY_PENDING'): await trusted_operation(c,body,'pending-key-001')
        assert await c.fetchval('SELECT count(*) FROM referral_trusted_operations')==0
        await stage_history(c,[dict(telegram_id=332,code='New332',referred_by_telegram_id=None,proven_new=False,trial_used=False,first_main_paid=False)],source_sha256='b'*64)
        result=await trusted_operation(c,body,'pending-key-001')
        iid=await install(c,a)
        own=SimpleNamespace(account_id=a,installation_id=iid,binding={'status':'active'})
        assert (await referral_info(c,own))['attribution']['receipt_id']==result['attribution']['receipt_id']
        for ctx in (SimpleNamespace(account_id=p,installation_id=iid,binding={'status':'active'}),
                    SimpleNamespace(account_id=a,installation_id=None,binding={'status':'active'}),
                    SimpleNamespace(account_id=a,installation_id=iid,binding={'status':'revoked'})):
            with pytest.raises(ApiError,match='ACCESS_DENIED'): await referral_info(c,ctx)
    finally: await c.close()


@pytest.mark.asyncio
@pytest.mark.parametrize('first_channel',['mobile','trusted'])
async def test_competing_mobile_and_trusted_attach_keep_one_owner(migrated_url,first_channel):
    c=await connect(migrated_url);c2=await connect(migrated_url)
    try:
        p1=await owner(c,341,'ParentA');p2=await owner(c,342,'ParentB');a=await owner(c,343,'Child')
        iid=await install(c,a)
        cid=await c.fetchval("INSERT INTO referral_candidates(installation_id,code) VALUES($1,'ParentA') RETURNING id",iid)
        link=await c.fetchrow("INSERT INTO registration_links(token_sha256,installation_id,environment,expires_at,referral_candidate_id,referral_idempotency_key) VALUES($1,$2,'test',now()+interval '1 hour',$3,'mobile-key-001') RETURNING *",uuid.uuid4().hex,iid,cid)
        entered=asyncio.Event();release=asyncio.Event()
        async def mobile(conn,hold=False):
            async with conn.transaction():
                await _account_advisory(conn,a)
                if hold: entered.set();await release.wait()
                return await attach_candidate(conn,link,a)
        async def trusted(conn,hold=False):
            async with conn.transaction():
                if hold:
                    await _account_advisory(conn,a);entered.set();await release.wait()
                return (await trusted_operation(conn,{'operation':'attach','telegram_id':343,'code':'ParentB'},'trusted-key-001'))['attribution']
        first=asyncio.create_task((mobile if first_channel=='mobile' else trusted)(c,True))
        await entered.wait()
        second=asyncio.create_task((trusted if first_channel=='mobile' else mobile)(c2))
        release.set()
        one,two=await asyncio.wait_for(asyncio.gather(first,second),5)
        assert one['state']=='attached' and two['reason']=='already_attributed'
        assert await c.fetchval('SELECT referred_by_account_id FROM accounts WHERE id=$1',a)==(p1 if first_channel=='mobile' else p2)
        assert await c.fetchval('SELECT count(*) FROM referral_trusted_operations')==1
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
        assert await c.fetchval('SELECT count(*) FROM entitlements')==0
        projection=(await trusted_operation(c,{'operation':'read','telegram_id':343}))['referral']
        assert projection['attribution']=={'state':'attached','receipt_id':one['receipt_id'],'reason':None}
        mobile_receipt=await c.fetchval('SELECT referral_attribution FROM registration_links WHERE id=$1',link['id'])
        assert mobile_receipt['candidate_id']==str(cid) and mobile_receipt['registration_id']==str(link['id'])
        assert mobile_receipt['idempotency_key']=='mobile-key-001'
    finally: await c.close();await c2.close()
