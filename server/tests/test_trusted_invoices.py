"""Real local route/PG + fake provider. No merchant or product service calls."""
import asyncio
import json
from dataclasses import replace
from datetime import timedelta
from types import SimpleNamespace
from uuid import UUID,uuid4

import pytest
from aiohttp import web
from aiohttp.test_utils import TestClient,TestServer

from test_referral_trusted import connect,owner,install
from test_trusted_quotes import _settings,Q,eligible,BOT_KEY
from terlimo_backend.auth_api import ApiError,rfc3339
from terlimo_backend.db import Database
from terlimo_backend.payments import PAYMENT_PROVIDER_KEY,ProviderPayment,ProviderUnknown,record_webhook,reconcile_payments
from terlimo_backend.telegram_billing import PATH,billing_operation,register_trusted_billing_routes
from terlimo_backend.s5_payments import register_s5_payment_routes,QUOTES_PATH,PAYMENTS_PATH

class FakeProvider:
    def __init__(self,c=None):self.calls=[];self.connection=c;self.fail=False;self.enter=None;self.release=None;self.status_view=None
    async def create_payment(self,**kw):
        self.calls.append(kw)
        if self.connection:
            assert not self.connection.is_in_transaction()
            assert not await self.connection.fetchval("SELECT count(*) FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory'")
            row=await self.connection.fetchrow('SELECT * FROM payment_orders WHERE id=$1',UUID(kw['order_ref']))
            assert row['provider_create_state']=='in_flight'
            if 'pricing' in json.loads(row['quote']):
                assert await self.connection.fetchval('SELECT reserved_order_id FROM referral_benefits WHERE account_id=$1',row['checkout_owner_account_id'])==row['id']
        if self.enter:self.enter.set();await self.release.wait()
        if self.fail:raise ProviderUnknown()
        return ProviderPayment(provider_payment_id='offline-'+kw['order_ref'],pay_url='https://checkout.invalid/'+kw['order_ref'],qr=None,variant=kw['method'])
    async def get_status(self,pid):return {**self.status_view,'id':pid} if self.status_view else None

async def quote(c,s,body=None):return await billing_operation(c,s,body or Q,uuid4().hex)
async def create(c,s,p,q,key=None,tg=801):return await billing_operation(c,s,{'operation':'create','telegram_id':tg,'quote_id':q['quote_id']},key or uuid4().hex,provider=p)
async def status(c,s,pid,tg=801):return await billing_operation(c,s,{'operation':'status','telegram_id':tg,'payment_id':str(pid)})
async def app(s,p):
    db=Database(s);await db.connect();a=web.Application();a[PAYMENT_PROVIDER_KEY]=p;register_trusted_billing_routes(a,s,db)
    client=TestClient(TestServer(a));await client.start_server();return client,db
async def paid(c,s,payment):
    row=await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',UUID(payment['payment_id']))
    return await record_webhook(c,s,merchant='offline-m',secret='offline-s',body={'id':row['provider_payment_id'],'status':'CONFIRMED','amount':float(row['amount']),'currency':'RUB'})
def settings(factory,url):return replace(_settings(factory,url),platega_merchant_id='offline-m',platega_secret='offline-s')

@pytest.mark.asyncio
async def test_real_route_strict_auth_identity_and_no_sideeffects(migrated_url,settings_factory):
    c=await connect(migrated_url);s=settings(settings_factory,migrated_url);p=FakeProvider();client,db=await app(s,p)
    try:
        await eligible(c);q=await quote(c,s);b={'operation':'create','telegram_id':801,'quote_id':q['quote_id']};h={'X-Telegram-Bot-Key':BOT_KEY,'Idempotency-Key':uuid4().hex}
        for headers in ({},{**h,'X-Telegram-Bot-Key':'wrong'}):assert (await client.post(PATH,json=b,headers=headers)).status==403
        for body in ({**b,'amount':100},{**b,'account_ref':str(uuid4())},{**b,'owner_kind':'telegram_account'},{**b,'telegram_id':True},{**b,'telegram_id':2**63},{**b,'quote_id':uuid4().hex},{**b,'caller':'telegram_backend'}):
            assert (await client.post(PATH,json=body,headers=h)).status==400
        assert (await client.post(PATH,json=b,headers={'X-Telegram-Bot-Key':BOT_KEY})).status==400
        assert (await client.post(PATH,json={**b,'telegram_id':999},headers=h)).status==409
        await c.execute("INSERT INTO accounts(status,telegram_id) VALUES('unlinked',999)")
        assert (await client.post(PATH,json={**b,'telegram_id':999},headers=h)).status==409
        assert (await client.post(PATH,json={**b,'telegram_id':802},headers=h)).status==404
        assert p.calls==[] and await c.fetchval('SELECT count(*) FROM payment_orders')==0
        response=await client.post(PATH,json=b,headers=h);assert response.status==200,await response.text()
        result=await response.json();assert result['provisioning_state']=='not_requested'
        for tg,pid in ((802,result['payment_id']),(801,str(uuid4()))):
            response=await client.post(PATH,json={'operation':'status','telegram_id':tg,'payment_id':pid},headers=h)
            assert response.status==404 and (await response.json())['code']=='PAYMENT_NOT_FOUND'
        for table in ('installations','account_bindings','entitlements','grants','referral_rewards'):assert await c.fetchval(f'SELECT count(*) FROM {table}')==0
    finally:await client.close();await db.close();await c.close()

@pytest.mark.asyncio
async def test_frozen_replay_late_callback_fullperiod_pending(migrated_url,settings_factory):
    c=await connect(migrated_url);s=settings(settings_factory,migrated_url);p=FakeProvider(c)
    try:
        a=await eligible(c);q=await quote(c,s);k=uuid4().hex;result=await create(c,s,p,q,k)
        assert p.calls[0]['amount']==100 and result['pricing']['base_amount_minor']==20000
        assert result['credit_state']=='unapplied'
        await c.execute("UPDATE s5_payment_quotes SET expires_at=now()-interval '1 hour' WHERE id=$1",UUID(q['quote_id']))
        changed=replace(s,payment_price_rub_1=900,platega_methods='',payment_tariff_key='changed',payment_currency='USD',platega_create_enabled=False)
        assert await create(c,changed,p,q,k)==result and len(p.calls)==1
        other=await quote(c,s)
        with pytest.raises(ApiError) as e:await create(c,s,p,other,k)
        assert e.value.code=='ORDER_CONFLICT'
        with pytest.raises(ApiError) as e:await create(c,s,p,q)
        assert e.value.code=='ORDER_CONFLICT'
        await c.execute("UPDATE payment_orders SET status='canceled' WHERE id=$1",UUID(result['payment_id']))
        assert (await status(c,s,result['payment_id']))['referral_discount_state']=='reconciling'
        assert await paid(c,s,result)=={'result':'succeeded'}
        view=await status(c,s,result['payment_id']);assert view['credit_state']=='applied' and view['provisioning_state']=='external_pending'
        assert view['access_application_state']=='retryable_failure' and view['referral_discount_state']=='consumed'
        assert view['credited_product']['pricing']==q['pricing']
        assert (await c.fetchval('SELECT ends_at-starts_at FROM entitlements WHERE account_id=$1',a))==timedelta(days=30)
        assert await paid(c,s,result)=={'result':'duplicate'}
        assert await create(c,changed,p,q,k)==view and len(p.calls)==1
        for t in ('installations','account_bindings','grants','outbox_operations','referral_rewards'):assert await c.fetchval(f'SELECT count(*) FROM {t}')==0
    finally:await c.close()

@pytest.mark.asyncio
async def test_unknown_and_concurrent_original_no_second_post(migrated_url,settings_factory):
    c=await connect(migrated_url);c2=await connect(migrated_url);s=settings(settings_factory,migrated_url);p=FakeProvider(c)
    try:
        a=await eligible(c);q=await quote(c,s);q2=await quote(c,s);k=uuid4().hex
        p.enter=asyncio.Event();p.release=asyncio.Event();p.fail=True
        task=asyncio.create_task(create(c,s,p,q,k));await asyncio.wait_for(p.enter.wait(),3)
        for candidate,key in ((q,k),(q2,uuid4().hex)):
            with pytest.raises(ApiError) as e:await create(c2,s,p,candidate,key)
            assert e.value.code=='PAYMENT_PROVIDER_UNKNOWN'
        p.release.set()
        with pytest.raises(ApiError):await task
        with pytest.raises(ApiError) as e:await create(c2,s,p,q,k)
        assert e.value.code=='PAYMENT_PROVIDER_UNKNOWN'
        assert len(p.calls)==1
        assert await c.fetchval('SELECT provider_create_state FROM payment_orders')=='unknown'
        assert await c.fetchval('SELECT reserved_order_id FROM referral_benefits WHERE account_id=$1',a)
        assert await c.fetchval('SELECT referral_create_resolution_reason FROM s5_payment_quotes WHERE id=$1',UUID(q2['quote_id'])) is None
    finally:await c.close();await c2.close()

@pytest.mark.asyncio
async def test_no_order_durable_expiry_and_reservation(migrated_url,settings_factory):
    c=await connect(migrated_url);s=settings(settings_factory,migrated_url);p=FakeProvider(c)
    try:
        a=await eligible(c);expired=await quote(c,s)
        await c.execute("UPDATE s5_payment_quotes SET expires_at=now()-interval '1 second' WHERE id=$1",UUID(expired['quote_id']))
        k=uuid4().hex
        with pytest.raises(ApiError) as e:await create(c,s,p,expired,k)
        assert e.value.details['create_resolution']=={'kind':'no_order','quote_id':expired['quote_id'],'request_idempotency_key':k,'reason':'expired_quote_no_order'}
        assert await c.fetchval('SELECT referral_create_resolution_reason FROM s5_payment_quotes WHERE id=$1',UUID(expired['quote_id']))=='expired_quote_no_order'
        q=await quote(c,s);await create(c,s,p,q)
        ordinary=await quote(c,s);assert ordinary['amount']['amount_minor']==20000
        k2=uuid4().hex
        with pytest.raises(ApiError) as e:await create(c,s,p,ordinary,k2)
        assert e.value.details['create_resolution']['reason']=='referral_discount_reserved'
        assert await c.fetchval('SELECT referral_create_resolution_reason FROM s5_payment_quotes WHERE id=$1',UUID(ordinary['quote_id']))=='referral_discount_reserved'
        assert len(p.calls)==1
        # Later eligibility cannot revive original terminal Q.
        await c.execute('UPDATE referral_benefits SET reserved_order_id=NULL WHERE account_id=$1',a)
        with pytest.raises(ApiError) as e:await create(c,s,p,ordinary,k2)
        assert e.value.details['create_resolution']['reason']=='referral_discount_reserved' and len(p.calls)==1
    finally:await c.close()

async def mobile_app(c,s,p,monkeypatch,a):
    iid=await install(c,a)
    async def authenticated(*args):
        binding=await c.fetchrow("SELECT * FROM account_bindings WHERE installation_id=$1 AND status='active'",iid)
        return SimpleNamespace(account_id=a,installation_id=iid,binding=binding)
    monkeypatch.setattr('terlimo_backend.s5_payments.authenticate_session',authenticated)
    db=Database(s);await db.connect();app=web.Application();app[PAYMENT_PROVIDER_KEY]=p;register_s5_payment_routes(app,s,db)
    client=TestClient(TestServer(app));await client.start_server();return client,db,iid
async def mobile_quote(client):
    r=await client.post(QUOTES_PATH+'?payment_contract=2',json={k:v for k,v in Q.items() if k not in ('operation','telegram_id')},headers={'Authorization':'Bearer fixture','Idempotency-Key':uuid4().hex})
    assert r.status==200,await r.text();return await r.json()
async def mobile_create(client,q,key=None):return await client.post(PAYMENTS_PATH+'?payment_contract=2',json={'quote_id':q['quote_id']},headers={'Authorization':'Bearer fixture','Idempotency-Key':key or uuid4().hex})

@pytest.mark.asyncio
@pytest.mark.parametrize('first',['account','mobile'])
async def test_cross_channel_ordinary_main_cannot_bypass_reservation(migrated_url,settings_factory,monkeypatch,first):
    c=await connect(migrated_url);s=settings(settings_factory,migrated_url);p=FakeProvider()
    a=await eligible(c);client,db,iid=await mobile_app(c,s,p,monkeypatch,a)
    try:
        parent=await c.fetchval('SELECT referred_by_account_id FROM accounts WHERE id=$1',a)
        await c.execute('UPDATE accounts SET referred_by_account_id=NULL WHERE id=$1',a)
        old_account=await quote(c,s);old_mobile=await mobile_quote(client)
        assert old_account['amount']['amount_minor']==old_mobile['amount']['amount_minor']==20000
        await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',a,parent)
        if first=='account':
            q=await quote(c,s);await create(c,s,p,q)
            for ordinary in (old_mobile,await mobile_quote(client)):
                r=await mobile_create(client,ordinary);assert r.status==409,await r.text()
                assert (await r.json())['details']['create_resolution']['reason']=='referral_discount_reserved'
        else:
            q=await mobile_quote(client);k=uuid4().hex;r=await mobile_create(client,q,k);assert r.status==200,await r.text()
            original=await r.json()
            assert 'provisioning_state' not in original and original['credit_state']=='unapplied'
            replay=await mobile_create(client,q,k);assert replay.status==200
            assert {k:v for k,v in (await replay.json()).items() if k!='request_id'}=={k:v for k,v in original.items() if k!='request_id'}
            for ordinary in (old_account,await quote(c,s)):
                with pytest.raises(ApiError) as e:await create(c,s,p,ordinary)
                assert e.value.details['create_resolution']['reason']=='referral_discount_reserved'
            # Real binding gate remains active; same mobile request no longer authorized.
            await c.execute("UPDATE account_bindings SET status='revoked' WHERE installation_id=$1",iid)
            r=await mobile_create(client,q,k);assert r.status==403
        assert len(p.calls)==1 and p.calls[0]['amount']==100
    finally:await client.close();await db.close();await c.close()

@pytest.mark.asyncio
async def test_addon_and_renewal_frozen_selection_reconcile(migrated_url,settings_factory):
    c=await connect(migrated_url);s=settings(settings_factory,migrated_url);p=FakeProvider(c)
    try:
        a=await eligible(c);initial=await create(c,s,p,await quote(c,s));await paid(c,s,initial)
        end=await c.fetchval('SELECT ends_at FROM entitlements WHERE account_id=$1',a)
        addonq=await quote(c,s,{**Q,'plan_id':'terlimo-extra-device','duration_code':'until:'+rfc3339(end)})
        addon=await create(c,s,p,addonq);assert 'pricing' not in addon
        await paid(c,s,addon)
        slot=await c.fetchrow('SELECT * FROM paid_extra_slots')
        assert slot['expires_at']==end
        renewalq=await quote(c,s,{**Q,'renew_extra_slot_ids':[str(slot['id'])]})
        renewal=await create(c,s,p,renewalq);assert 'pricing' not in renewal
        p.status_view={'status':'CONFIRMED','amount':float(p.calls[-1]['amount']),'currency':'RUB'}
        await reconcile_payments(c,s,p)
        view=await status(c,s,renewal['payment_id'])
        assert view['credit_state']=='applied' and view['product']['renew_extra_slot_ids']==[str(slot['id'])]
        assert view['credited_product']['device_limit']==3 and view['provisioning_state']=='external_pending'
        assert await c.fetchval('SELECT ends_at FROM entitlements WHERE account_id=$1',a)==end+timedelta(days=30)
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
        assert await c.fetchval('SELECT count(*) FROM grants')==0
    finally:await c.close()

@pytest.mark.asyncio
async def test_history_and_global_foreign_key_precedence(migrated_url,settings_factory):
    c=await connect(migrated_url);s=settings(settings_factory,migrated_url);p=FakeProvider(c)
    try:
        a=await eligible(c);q=await quote(c,s);k=uuid4().hex
        await c.execute("UPDATE referral_benefits SET history_state='history_pending' WHERE account_id=$1",a)
        with pytest.raises(ApiError) as e:await create(c,s,p,q,k)
        assert e.value.code=='REFERRAL_HISTORY_PENDING' and e.value.retryable
        assert await c.fetchval('SELECT referral_create_resolution_reason FROM s5_payment_quotes WHERE id=$1',UUID(q['quote_id'])) is None
        await c.execute("UPDATE referral_benefits SET history_state='ready' WHERE account_id=$1",a)
        first=await create(c,s,p,q,k)
        foreignq=await quote(c,s,{**Q,'telegram_id':802})
        with pytest.raises(ApiError) as e:await create(c,s,p,foreignq,k,tg=802)
        assert e.value.code=='ORDER_CONFLICT' and len(p.calls)==1
        assert await c.fetchval('SELECT count(*) FROM payment_orders')==1
        # Existing explicit account order cannot be adopted by a mobile installation.
        from terlimo_backend.payments import create_order
        i=await install(c,a)
        with pytest.raises(ApiError) as e:await create_order(c,s,p,installation_id=i,months=1,idempotency_key=k)
        assert e.value.code=='ORDER_CONFLICT' and len(p.calls)==1
    finally:await c.close()

@pytest.mark.asyncio
async def test_cancelled_provider_wait_keeps_inflight_original(migrated_url,settings_factory):
    c=await connect(migrated_url);s=settings(settings_factory,migrated_url);p=FakeProvider(c)
    try:
        a=await eligible(c);q=await quote(c,s);k=uuid4().hex
        p.enter=asyncio.Event();p.release=asyncio.Event()
        task=asyncio.create_task(create(c,s,p,q,k));await asyncio.wait_for(p.enter.wait(),3)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):await task
        row=await c.fetchrow('SELECT * FROM payment_orders')
        assert row['provider_create_state']=='in_flight'
        assert await c.fetchval('SELECT reserved_order_id FROM referral_benefits WHERE account_id=$1',a)==row['id']
        with pytest.raises(ApiError) as e:await create(c,s,p,q,k)
        assert e.value.code=='PAYMENT_PROVIDER_UNKNOWN' and len(p.calls)==1
        view=await status(c,s,row['id']);assert view['payment_status']=='pending' and view['checkout_reference'] is None
    finally:await c.close()

@pytest.mark.asyncio
async def test_recheck_key_after_real_account_barrier(migrated_url,settings_factory):
    from terlimo_backend.referral_pricing import benefit_lock
    c=await connect(migrated_url);c2=await connect(migrated_url);s=settings(settings_factory,migrated_url);p=FakeProvider(c2)
    try:
        a=await eligible(c);q=await quote(c,s);k=uuid4().hex
        async with c.transaction():
            await benefit_lock(c,a)
            task=asyncio.create_task(create(c2,s,p,q,k))
            for _ in range(100):
                waiting=await c.fetchval("SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1",c2.get_server_pid())
                if waiting:break
                await asyncio.sleep(.01)
            assert waiting and not task.done()
            # Simulate a different already-authorized installation claiming the global K
            # while account create waited. No provider is invoked by this fixture.
            i=await install(c,a)
            await c.execute("""INSERT INTO payment_orders(installation_id,checkout_owner_account_id,idempotency_key,quote,amount,currency,months,tariff_key,provider_create_state)
                VALUES($1,$2,$3,'{}',200,'RUB',1,'fixture','unknown')""",i,a,k)
        with pytest.raises(ApiError) as e:await asyncio.wait_for(task,3)
        assert e.value.code=='ORDER_CONFLICT' and p.calls==[]
        assert await c.fetchval('SELECT referral_create_resolution_reason FROM s5_payment_quotes WHERE id=$1',UUID(q['quote_id'])) is None
    finally:await c.close();await c2.close()

@pytest.mark.asyncio
async def test_authorization_rechecked_after_prepare_wait(migrated_url,settings_factory):
    from terlimo_backend.payments import payment_install_lock
    c=await connect(migrated_url);c2=await connect(migrated_url);s=settings(settings_factory,migrated_url);p=FakeProvider(c2)
    try:
        a=await eligible(c);q=await quote(c,s)
        async with payment_install_lock(c,f'telegram-account:{a}'):
            task=asyncio.create_task(create(c2,s,p,q))
            for _ in range(100):
                waiting=await c.fetchval("SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1",c2.get_server_pid())
                if waiting:break
                await asyncio.sleep(.01)
            assert waiting
            await c.execute("UPDATE accounts SET status='unlinked' WHERE id=$1",a)
        with pytest.raises(ApiError) as e:await asyncio.wait_for(task,3)
        assert e.value.code=='REGISTRATION_REQUIRED' and p.calls==[]
        assert await c.fetchval('SELECT count(*) FROM payment_orders')==0
    finally:await c.close();await c2.close()

@pytest.mark.asyncio
async def test_unknown_addon_cannot_evade_with_new_account_key(migrated_url,settings_factory):
    c=await connect(migrated_url);s=settings(settings_factory,migrated_url);p=FakeProvider(c)
    try:
        a=await eligible(c);initial=await create(c,s,p,await quote(c,s));await paid(c,s,initial)
        end=await c.fetchval('SELECT ends_at FROM entitlements WHERE account_id=$1',a)
        addonq=await quote(c,s,{**Q,'plan_id':'terlimo-extra-device','duration_code':'until:'+rfc3339(end)})
        p.fail=True
        with pytest.raises(ApiError):await create(c,s,p,addonq)
        another=await quote(c,s,{**Q,'plan_id':'terlimo-extra-device','duration_code':'until:'+rfc3339(end)})
        with pytest.raises(ApiError) as e:await create(c,s,p,another)
        assert e.value.code=='PAYMENT_PROVIDER_UNKNOWN' and len(p.calls)==2
        assert await c.fetchval('SELECT referral_create_resolution_reason FROM s5_payment_quotes WHERE id=$1',UUID(another['quote_id'])) is None
    finally:await c.close()
