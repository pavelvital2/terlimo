"""Only shared quote/account-owner paths; isolated PG and local API, no provider."""
import asyncio
import json
from dataclasses import replace
from datetime import datetime
from pathlib import Path
from uuid import UUID,uuid4

import asyncpg
import pytest
from aiohttp import web
from aiohttp.test_utils import TestClient,TestServer

from terlimo_backend.auth_api import ApiError
from terlimo_backend.db import Database
from terlimo_backend.telegram_billing import PATH,billing_operation,register_trusted_billing_routes
from terlimo_backend.s5_payments import QUOTES_PATH
from test_referral_trusted import connect,owner
from types import SimpleNamespace
from terlimo_backend.s5_payments import register_s5_payment_routes
from test_referral_trusted import install
BOT_KEY='test-bot-key'

def _settings(factory,url):
    return factory(url,telegram_bot_key=BOT_KEY,payment_currency='RUB',
        payment_tariff_key='terlimo-200-30d-v1',payment_price_rub_1=200,
        payment_price_rub_3=480,payment_price_rub_6=840,platega_methods='sbp,international,crypto')

Q={'operation':'quote','telegram_id':801,'plan_id':'terlimo-30d','duration_code':'days:30','method':'sbp'}
K='account-quote-key-001'

async def eligible(c):
    p=await owner(c,802,'Parent802');a=await owner(c,801,'Own801')
    await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',a,p)
    return a

async def unchanged_effects(c):
    for table in ('installations','account_bindings','payment_orders','entitlements','referral_rewards'):
        assert await c.fetchval(f'SELECT count(*) FROM {table}')==0
    assert await c.fetchval('SELECT count(*) FROM referral_benefits WHERE reserved_order_id IS NOT NULL OR consumed_order_id IS NOT NULL')==0

@pytest.mark.asyncio
async def test_actual_api_closed_auth_identity_and_discount_snapshot(migrated_url,settings_factory):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    db=Database(s);await db.connect();app=web.Application();register_trusted_billing_routes(app,s,db)
    client=TestClient(TestServer(app));await client.start_server()
    try:
        a=await eligible(c);h={'X-Telegram-Bot-Key':BOT_KEY,'Idempotency-Key':K}
        for headers in ({},{'X-Telegram-Bot-Key':'wrong'}):assert (await client.post(PATH,json=Q,headers=headers)).status==403
        for extra in ('amount','discount','account_ref','caller','binding'):
            assert (await client.post(PATH,json={**Q,extra:'x'},headers=h)).status==400
        for tg in (True,0,-1,2**63,'801'):
            assert (await client.post(PATH,json={**Q,'telegram_id':tg},headers=h)).status==400
        assert (await client.post(PATH,json=Q,headers={'X-Telegram-Bot-Key':BOT_KEY})).status==400
        assert (await client.post(PATH,json={**Q,'telegram_id':999},headers=h)).status==409
        await c.execute("INSERT INTO accounts(status,telegram_id) VALUES('unlinked',803)")
        assert (await client.post(PATH,json={**Q,'telegram_id':803},headers=h)).status==409
        plans=await client.post(PATH,json={'operation':'plans','telegram_id':801},headers=h)
        assert plans.status==200;plans=await plans.json()
        assert plans['plans'][0]['amount']['amount_minor']==20000
        response=await client.post(PATH,json=Q,headers=h);assert response.status==200,await response.text()
        result=await response.json()
        assert result['amount']=={'amount_minor':10000,'currency':'RUB'}
        assert result['pricing']['base_amount_minor']==20000 and result['pricing']['discount_minor']==10000
        assert result['product']['base_amount_minor']==20000 and result['product']['device_limit']==2
        assert 'owner_account_id' not in result['product']
        product=result['product'];assert (datetime.fromisoformat(product['valid_until'].replace('Z','+00:00'))-datetime.fromisoformat(product['valid_from'].replace('Z','+00:00'))).days==30
        row=await c.fetchrow('SELECT * FROM s5_payment_quotes WHERE id=$1',UUID(result['quote_id']))
        assert row['installation_id'] is None and row['trusted_owner_account_id']==a and row['owner_kind']=='telegram_account'
        await unchanged_effects(c)
    finally:await client.close();await db.close();await c.close()

@pytest.mark.asyncio
async def test_replay_before_mutable_state_and_foreign_owner_and_concurrency(migrated_url,settings_factory):
    c=await connect(migrated_url);c2=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        a=await eligible(c)
        results=await asyncio.gather(billing_operation(c,s,Q,K),billing_operation(c2,s,Q,K))
        assert results[0]==results[1] and await c.fetchval('SELECT count(*) FROM s5_payment_quotes')==1
        before=await c.fetchrow('SELECT * FROM s5_payment_quotes')
        await c.execute("UPDATE referral_benefits SET history_state='history_pending',imported_first_main_paid=true WHERE account_id=$1",a)
        s2=replace(s,payment_price_rub_1=900,platega_methods='',payment_tariff_key='changed')
        assert await billing_operation(c,s2,Q,K)==results[0]
        assert before==await c.fetchrow('SELECT * FROM s5_payment_quotes')
        with pytest.raises(ApiError) as e:await billing_operation(c,s2,{**Q,'method':'card'},K)
        assert e.value.code=='IDEMPOTENCY_CONFLICT'
        for op in ({'operation':'plans','telegram_id':801},Q):
            with pytest.raises(ApiError) as e:await billing_operation(c,s,op,K+'new')
            assert e.value.code=='REFERRAL_HISTORY_PENDING' and e.value.retryable
        foreign=await billing_operation(c,s,{**Q,'telegram_id':802},K)
        assert foreign['quote_id']!=results[0]['quote_id'] and foreign['amount']['amount_minor']==20000
        await unchanged_effects(c)
    finally:await c.close();await c2.close()

@pytest.mark.asyncio
@pytest.mark.parametrize('mode',['no_inviter','imported_paid','paid_history','pending','low_price'])
async def test_eligibility_no_fallback(migrated_url,settings_factory,mode):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        a=await eligible(c)
        if mode=='no_inviter':await c.execute('UPDATE accounts SET referred_by_account_id=NULL WHERE id=$1',a)
        if mode=='imported_paid':await c.execute('UPDATE referral_benefits SET imported_first_main_paid=true WHERE account_id=$1',a)
        if mode=='paid_history':await c.execute("INSERT INTO entitlements(account_id,kind,status,starts_at,ends_at,device_limit) VALUES($1,'paid','expired',now()-interval '40 days',now()-interval '1 day',2)",a)
        if mode=='pending':await c.execute("DELETE FROM referral_benefits WHERE account_id=$1",a)
        if mode=='low_price':s=replace(s,payment_price_rub_1=100)
        if mode in ('pending','low_price'):
            with pytest.raises(ApiError) as e:await billing_operation(c,s,Q,K)
            assert e.value.code==('REFERRAL_HISTORY_PENDING' if mode=='pending' else 'REFERRAL_PRICE_UNSUPPORTED')
            assert await c.fetchval('SELECT count(*) FROM s5_payment_quotes')==0
        else:
            q=await billing_operation(c,s,Q,K);assert q['amount']['amount_minor']==20000 and 'pricing' not in q
    finally:await c.close()

@pytest.mark.asyncio
async def test_owner_shapes_and_down_preserve_account_receipts(migrated_url,settings_factory):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        await eligible(c);q=await billing_operation(c,s,Q,K);qid=UUID(q['quote_id'])
        for sql in ("owner_kind='installation'","trusted_caller=NULL","trusted_owner_account_id=NULL","trusted_caller='browser'","owner_kind='other'"):
            with pytest.raises(asyncpg.CheckViolationError):
                async with c.transaction():await c.execute('UPDATE s5_payment_quotes SET '+sql+' WHERE id=$1',qid)
        with pytest.raises(asyncpg.ForeignKeyViolationError):
            async with c.transaction():await c.execute('UPDATE s5_payment_quotes SET trusted_owner_account_id=$2 WHERE id=$1',qid,uuid4())
        down=Path('server/terlimo_backend/migrations/versions/0041_account_quotes.down.sql').read_text()
        with pytest.raises(asyncpg.RaiseError):
            async with c.transaction():await c.execute(down)
        assert await c.fetchval('SELECT count(*) FROM s5_payment_quotes')==1
    finally:await c.close()

@pytest.mark.asyncio
@pytest.mark.parametrize('contract2',[False,True])
async def test_mobile_shared_quote_and_selection_replay(migrated_url,settings_factory,monkeypatch,contract2):
    s=_settings(settings_factory,migrated_url);db=Database(s);await db.connect()
    c=await connect(migrated_url)
    a=await owner(c,804,'Mobile804');iid=await install(c,a)
    binding=await c.fetchrow("SELECT * FROM account_bindings WHERE installation_id=$1",iid)
    context=SimpleNamespace(account_id=a,installation_id=iid,binding=binding)
    async def authenticated(*args):return context
    monkeypatch.setattr('terlimo_backend.s5_payments.authenticate_session',authenticated)
    app=web.Application();register_s5_payment_routes(app,s,db)
    client=TestClient(TestServer(app));await client.start_server()
    try:
        h={'Authorization':'Bearer local-test-context','Idempotency-Key':K}
        body={k:v for k,v in Q.items() if k not in ('operation','telegram_id')}
        path=QUOTES_PATH+('?payment_contract=2' if contract2 else '')
        r=await client.post(path,json=body,headers=h)
        assert r.status==200,await r.text()
        result=await r.json();assert result['amount']=={'amount_minor':20000,'currency':'RUB'}
        assert result['duration_code']=='days:30' and result['device_limit']==2 and result['method']=='sbp'
        assert ('product' in result)==contract2
        if contract2:assert result['product']['kind']=='subscription'
        replay=await (await client.post(path,json=body,headers=h)).json()
        for key in ('quote_id','amount','duration_code','device_limit','method','expires_at')+ (('product',) if contract2 else ()):assert replay[key]==result[key]
        conflict=await client.post(path,json={**body,'method':'card'},headers=h)
        assert conflict.status==409
        changed=await client.post(path,json={**body,'renew_extra_slot_ids':[str(uuid4())]},headers={**h,'Idempotency-Key':K+'new'})
        assert changed.status==400
        row=await c.fetchrow('SELECT * FROM s5_payment_quotes WHERE id=$1',UUID(result['quote_id']))
        assert row['owner_kind']=='installation' and row['installation_id']==iid and row['trusted_owner_account_id'] is None
        await c.execute("UPDATE account_bindings SET status='revoked' WHERE id=$1",binding['id'])
        denied=await client.post(path,json=body,headers=h)
        assert denied.status==403  # actual DB binding gate remains BEFORE replay
        assert await c.fetchval('SELECT count(*) FROM payment_orders')==0
    finally:await c.close();await client.close();await db.close()

@pytest.mark.asyncio
@pytest.mark.parametrize('months,price',[(3,48000),(6,84000)])
async def test_main_full_calendar_duration(migrated_url,settings_factory,months,price):
    from terlimo_backend.payments import paid_end
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        await eligible(c)
        result=await billing_operation(c,s,{**Q,'plan_id':f'terlimo-{months}m','duration_code':f'months:{months}'},K)
        assert result['amount']['amount_minor']==price-10000
        product=result['product']
        start=datetime.fromisoformat(product['valid_from'].replace('Z','+00:00'))
        end=datetime.fromisoformat(product['valid_until'].replace('Z','+00:00'))
        assert end==paid_end(start,{'unit':'months','value':months})
        await unchanged_effects(c)
    finally:await c.close()

@pytest.mark.asyncio
async def test_existing_paid_addon_and_selected_renewal_use_shared_rules(migrated_url,settings_factory):
    from terlimo_backend.payment_products import ADDON_PLAN
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        a=await eligible(c)
        e=await c.fetchval("""INSERT INTO entitlements(account_id,kind,status,starts_at,ends_at,device_limit,source_plan)
            VALUES($1,'paid','active',now()-interval '1 day',now()+interval '29 days',2,'{"duration":{"unit":"days","value":30}}') RETURNING id""",a)
        slot=await c.fetchval("INSERT INTO paid_extra_slots(entitlement_id,expires_at) VALUES($1,now()+interval '29 days') RETURNING id",e)
        plans=await billing_operation(c,s,{'operation':'plans','telegram_id':801})
        addon=next(x for x in plans['plans'] if x['plan_id']==ADDON_PLAN)
        q=await billing_operation(c,s,{**Q,'plan_id':ADDON_PLAN,'duration_code':addon['duration_code']},K)
        assert q['product']['kind']=='device_addon' and 'pricing' not in q
        renew=await billing_operation(c,s,{**Q,'renew_extra_slot_ids':[str(slot)]},K+'renew')
        assert renew['product']['renew_extra_slot_ids']==[str(slot)]
        assert renew['product']['device_limit']==3 and 'pricing' not in renew
        for selected in ([str(slot),str(slot)],[str(uuid4())]):
            with pytest.raises(ApiError) as error:await billing_operation(c,s,{**Q,'renew_extra_slot_ids':selected},K+'bad')
            assert error.value.code=='BAD_MESSAGE'
        assert await c.fetchval('SELECT count(*) FROM payment_orders')==0
        assert await c.fetchval('SELECT count(*) FROM entitlements')==1
        assert await c.fetchval('SELECT count(*) FROM paid_extra_slots')==1
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
        assert await c.fetchval('SELECT count(*) FROM referral_benefits WHERE reserved_order_id IS NOT NULL')==0
    finally:await c.close()
