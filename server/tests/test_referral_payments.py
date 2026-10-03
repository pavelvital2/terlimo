"""Discount/reservation tests with isolated PostgreSQL and fake provider only."""
import asyncio
import json
import uuid
import asyncpg
from test_payment_addon_slice import identity, hdr, confirm
from test_s4_payments import _app, FakePlategaProvider
from terlimo_backend.s5_payments import QUOTES_PATH, PAYMENTS_PATH
from terlimo_backend.payments import reconcile_payments

async def referred(client,url):
    account=uuid.uuid4();iid,token=await identity(client,url,account)
    c=await asyncpg.connect(url)
    try:
        inviter=await c.fetchval("INSERT INTO accounts(status,telegram_id) VALUES ('verified',$1) RETURNING id",uuid.uuid4().int%1000000000000)
        await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1',account,inviter)
        await c.execute("INSERT INTO referral_benefits(account_id,history_state) VALUES ($1,'ready') ON CONFLICT(account_id) DO UPDATE SET history_state='ready'",account)
    finally:await c.close()
    return account,iid,token

async def quotation(client,token):
    r=await client.post(QUOTES_PATH+'?payment_contract=2',headers=hdr(token),json={'plan_id':'terlimo-30d','duration_code':'days:30','method':'sbp'})
    assert r.status==200,await r.text()
    return await r.json()

async def create(client,token,q,key):
    h=hdr(token);h['Idempotency-Key']=key
    return await client.post(PAYMENTS_PATH+'?payment_contract=2',headers=h,json={'quote_id':q['quote_id']})

async def test_discount_snapshot_late_paid_consumes_and_replays(migrated_url,settings_factory):
    provider=FakePlategaProvider();client,settings,db=await _app(settings_factory,migrated_url,provider=provider)
    try:
        account,iid,token=await referred(client,migrated_url);q=await quotation(client,token)
        assert q['amount']['amount_minor']==10000 and q['pricing']['base_amount_minor']==20000
        key=uuid.uuid4().hex;r=await create(client,token,q,key);assert r.status==200,await r.text()
        payment=await r.json();assert payment['pricing']==q['pricing']
        c=await asyncpg.connect(migrated_url)
        try:
            oid=uuid.UUID(payment['payment_id'])
            await c.execute("UPDATE payment_orders SET status='canceled' WHERE id=$1",oid)
            q2=await quotation(client,token)
            # New quote undiscounted while existing reservation is unresolved.
            assert q2['amount']['amount_minor']==20000
            provider.status_view={'status':'CONFIRMED','amount':100,'currency':'RUB'}
            result=await reconcile_payments(c,settings,provider)
            assert result['status_changed']==1
            benefit=await c.fetchrow('SELECT * FROM referral_benefits WHERE account_id=$1',account)
            assert benefit['consumed_order_id']==oid and benefit['reservation_state']=='consumed'
            order=await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',oid)
            credited=order['credited_product']
            while isinstance(credited,str):credited=json.loads(credited)
            assert credited['pricing']==q['pricing']
            assert (await c.fetchval('SELECT ends_at-starts_at FROM entitlements WHERE id=$1',order['applied_entitlement_id'])).days==30
            before=await c.fetchval('SELECT revision FROM entitlements WHERE id=$1',order['applied_entitlement_id'])
            await confirm(client,migrated_url,payment,100)
            assert await c.fetchval('SELECT revision FROM entitlements WHERE id=$1',order['applied_entitlement_id'])==before
            assert (await create(client,token,q,key)).status==200
            assert len(provider.create_calls)==1 and provider.create_calls[0]['amount']==100
        finally:await c.close()
    finally:await db.close();await client.close()

async def test_discount_two_installations_atomic_and_noorder_tombstone(migrated_url,settings_factory):
    provider=FakePlategaProvider();client,settings,db=await _app(settings_factory,migrated_url,provider=provider)
    try:
        account,iid,token=await referred(client,migrated_url)
        # A second independently authenticated installation bound to the same account.
        other=uuid.uuid4();iid2,token2=await identity(client,migrated_url,other)
        c=await asyncpg.connect(migrated_url)
        try:
            await c.execute('UPDATE account_bindings SET account_id=$2 WHERE installation_id=$1',iid2,account)
            await c.execute('UPDATE sessions SET account_id=$2 WHERE installation_id=$1',iid2,account)
        finally:await c.close()
        q1=await quotation(client,token);q2=await quotation(client,token2)
        k1,k2=uuid.uuid4().hex,uuid.uuid4().hex
        responses=await asyncio.gather(create(client,token,q1,k1),create(client,token2,q2,k2))
        assert sorted(r.status for r in responses) in ([200,409],[200,503])
        loser=0 if responses[0].status!=200 else 1
        error=await responses[loser].json()
        q,k,t=(q1,k1,token) if loser==0 else (q2,k2,token2)
        if responses[loser].status==503:
            assert error['code']=='PAYMENT_PROVIDER_UNKNOWN' and 'create_resolution' not in error.get('details',{})
            error=await (await create(client,t,q,k)).json()
        assert error['details']['create_resolution']=={'kind':'no_order','quote_id':q['quote_id'],'request_idempotency_key':k,'reason':'referral_discount_reserved'}
        assert len(provider.create_calls)==1
        c=await asyncpg.connect(migrated_url)
        try:
            # Even eligibility becoming available later cannot revive exact rejected Q/K.
            await c.execute('UPDATE referral_benefits SET reserved_order_id=NULL,reservation_state=NULL WHERE account_id=$1',account)
            await c.execute('DELETE FROM payment_orders WHERE checkout_owner_account_id=$1',account)
        finally:await c.close()
        error2=await (await create(client,t,q,k)).json()
        assert error2['details']['create_resolution']==error['details']['create_resolution']
        assert len(provider.create_calls)==1
    finally:await db.close();await client.close()

async def test_unknown_keeps_reservation_and_needs_review_consumes(migrated_url,settings_factory):
    class Unknown(FakePlategaProvider):
        async def create_payment(self,**kw):
            await super().create_payment(**kw)
            raise TimeoutError('offline fake unknown')
    provider=Unknown();client,settings,db=await _app(settings_factory,migrated_url,provider=provider)
    try:
        account,iid,token=await referred(client,migrated_url);q=await quotation(client,token);key=uuid.uuid4().hex
        r=await create(client,token,q,key);assert r.status>=500
        assert (await (await create(client,token,q,key)).json())['code']=='PAYMENT_PROVIDER_UNKNOWN'
        c=await asyncpg.connect(migrated_url)
        try:
            oid=await c.fetchval('SELECT reserved_order_id FROM referral_benefits WHERE account_id=$1',account)
            assert oid and len(provider.create_calls)==1
            await c.execute("UPDATE payment_orders SET provider_create_state='created',provider_payment_id='offline-review',status='pending',credit_review_reason='owner_changed' WHERE id=$1",oid)
            from terlimo_backend.payments import record_webhook
            result=await record_webhook(c,settings,merchant=settings.platega_merchant_id,secret=settings.platega_secret,body={'id':'offline-review','status':'CONFIRMED','amount':100,'currency':'RUB'})
            assert result['result']=='succeeded'
            assert await c.fetchval('SELECT consumed_order_id FROM referral_benefits WHERE account_id=$1',account)==oid
            assert await c.fetchval('SELECT applied_entitlement_id FROM payment_orders WHERE id=$1',oid) is None
            assert await c.fetchval("SELECT count(*) FROM referral_rewards WHERE invitee_account_id=$1 AND event_kind='first_main_paid'",account)==0
        finally:await c.close()
    finally:await db.close();await client.close()

async def test_new_low_tariff_rejected_and_legacy_history_denied(migrated_url,settings_factory):
    provider=FakePlategaProvider();client,settings,db=await _app(settings_factory,migrated_url,provider=provider,payment_price_rub_1=20)
    try:
        account,iid,token=await referred(client,migrated_url)
        r=await client.post(QUOTES_PATH+'?payment_contract=2',headers=hdr(token),json={'plan_id':'terlimo-30d','duration_code':'days:30','method':'sbp'})
        assert r.status==409 and (await r.json())['code']=='REFERRAL_PRICE_UNSUPPORTED'
        c=await asyncpg.connect(migrated_url)
        try:await c.execute('UPDATE referral_benefits SET imported_first_main_paid=true WHERE account_id=$1',account)
        finally:await c.close()
        q=await quotation(client,token)
        assert q['amount']['amount_minor']==2000 and 'pricing' not in q
    finally:await db.close();await client.close()

async def test_legacy_unknown_globalkey_sourcequote_outrank_noorder(migrated_url,settings_factory):
    provider=FakePlategaProvider();client,settings,db=await _app(settings_factory,migrated_url,provider=provider)
    try:
        account,iid,token=await referred(client,migrated_url);q=await quotation(client,token)
        c=await asyncpg.connect(migrated_url)
        try:
            from test_step036_onboarding_hour_storage import _seed_installation
            old_iid=await _seed_installation(migrated_url)
            oldkey=uuid.uuid4().hex
            old=await c.fetchval("""INSERT INTO payment_orders(installation_id,idempotency_key,quote,amount,currency,months,tariff_key,provider_create_state,checkout_owner_account_id)
                VALUES($1,$2,'{}',200,'RUB',1,'legacy-200','unknown',$3) RETURNING id""",old_iid,oldkey,account)
            key=uuid.uuid4().hex
            r=await create(client,token,q,key);error=await r.json()
            assert error['code']=='PAYMENT_PROVIDER_UNKNOWN' and 'create_resolution' not in error.get('details',{})
            assert await c.fetchval('SELECT referral_create_resolution_reason FROM s5_payment_quotes WHERE id=$1',uuid.UUID(q['quote_id'])) is None
            r=await create(client,token,q,oldkey);assert (await r.json())['code']=='ORDER_CONFLICT'
            await c.execute("UPDATE payment_orders SET provider_create_state='created',provider_payment_id='legacy-existing',status='canceled' WHERE id=$1",old)
            r=await create(client,token,q,key);error=await r.json()
            assert error['details']['create_resolution']['reason']=='referral_discount_reserved'
            assert len(provider.create_calls)==0
        finally:await c.close()
    finally:await db.close();await client.close()

async def test_simultaneous_global_key_outranks_reservation_after_lock_wait(migrated_url,settings_factory):
    provider=FakePlategaProvider();client,settings,db=await _app(settings_factory,migrated_url,provider=provider)
    try:
        account,iid,token=await referred(client,migrated_url)
        other=uuid.uuid4();iid2,token2=await identity(client,migrated_url,other)
        c=await asyncpg.connect(migrated_url)
        try:
            await c.execute('UPDATE account_bindings SET account_id=$2 WHERE installation_id=$1',iid2,account)
            await c.execute('UPDATE sessions SET account_id=$2 WHERE installation_id=$1',iid2,account)
            q1,q2=await quotation(client,token),await quotation(client,token2)
            key=uuid.uuid4().hex
            rs=await asyncio.gather(create(client,token,q1,key),create(client,token2,q2,key))
            assert sorted(r.status for r in rs)==[200,409]
            loser=0 if rs[0].status==409 else 1
            error=await rs[loser].json()
            assert error['code']=='ORDER_CONFLICT' and 'create_resolution' not in error.get('details',{})
            assert len(provider.create_calls)==1
            q=q1 if loser==0 else q2
            assert await c.fetchval('SELECT referral_create_resolution_reason FROM s5_payment_quotes WHERE id=$1',uuid.UUID(q['quote_id'])) is None
        finally:await c.close()
    finally:await db.close();await client.close()

async def test_first_paid_needsreview_anchor_preserves_original_reward_period(migrated_url,settings_factory):
    provider=FakePlategaProvider();client,settings,db=await _app(settings_factory,migrated_url,provider=provider)
    try:
        account,iid,token=await referred(client,migrated_url);q=await quotation(client,token)
        first=await (await create(client,token,q,uuid.uuid4().hex)).json()
        firstid=uuid.UUID(first['payment_id'])
        c=await asyncpg.connect(migrated_url)
        try:
            await c.execute("UPDATE payment_orders SET credit_review_reason='owner_changed' WHERE id=$1",firstid)
            await confirm(client,migrated_url,first,100)
            assert await c.fetchval('SELECT first_paid_order_id FROM referral_benefits WHERE account_id=$1',account)==firstid
            r=await client.post(QUOTES_PATH+'?payment_contract=2',headers=hdr(token),json={'plan_id':'terlimo-3m','duration_code':'months:3','method':'sbp'})
            assert r.status==200,await r.text()
            laterq=await r.json();later=await (await create(client,token,laterq,uuid.uuid4().hex)).json()
            await confirm(client,migrated_url,later,480)
            assert await c.fetchval('SELECT applied_entitlement_id FROM payment_orders WHERE id=$1',uuid.UUID(later['payment_id']))
            assert await c.fetchval("SELECT count(*) FROM referral_rewards WHERE invitee_account_id=$1 AND event_kind='first_main_paid'",account)==0
            await c.execute('UPDATE payment_orders SET credit_review_reason=NULL WHERE id=$1',firstid)
            from terlimo_backend.payments import apply_paid_entitlement
            # Use actual API DB codec used by maintenance and callback, not raw SQL helper.
            async with db.acquire() as connection:
                eid=await apply_paid_entitlement(connection,settings,order_id=firstid)
                assert eid
                assert await apply_paid_entitlement(connection,settings,order_id=firstid)==eid
            reward=await c.fetchrow("SELECT * FROM referral_rewards WHERE invitee_account_id=$1 AND event_kind='first_main_paid'",account)
            assert reward['source_order_id']==firstid and reward['days']==7
            assert await c.fetchval("SELECT count(*) FROM referral_rewards WHERE invitee_account_id=$1 AND event_kind='first_main_paid'",account)==1
        finally:await c.close()
    finally:await db.close();await client.close()

async def test_actual_api_prior_addon_does_not_anchor_main_reward(migrated_url,settings_factory):
    from test_payment_addon_slice import paid
    provider=FakePlategaProvider();client,settings,db=await _app(settings_factory,migrated_url,provider=provider)
    try:
        account,iid,token=await referred(client,migrated_url)
        # An existing fixture right with known imported history (no first MAIN paid).
        ent=await paid(migrated_url,account)
        from terlimo_backend.auth_api import rfc3339
        r=await client.post(QUOTES_PATH+'?payment_contract=2',headers=hdr(token),json={'plan_id':'terlimo-extra-device','duration_code':'until:'+rfc3339(ent['ends_at']),'method':'sbp'})
        assert r.status==200,await r.text()
        addonq=await r.json();addonr=await create(client,token,addonq,uuid.uuid4().hex)
        assert addonr.status==200,await addonr.text()
        addon=await addonr.json();await confirm(client,migrated_url,addon,addonq['amount']['amount_minor']/100)
        c=await asyncpg.connect(migrated_url)
        try:
            assert await c.fetchval('SELECT first_paid_order_id FROM referral_benefits WHERE account_id=$1',account) is None
            assert await c.fetchval("SELECT count(*) FROM referral_rewards WHERE invitee_account_id=$1 AND event_kind='first_main_paid'",account)==0
            mainq=await quotation(client,token);mainr=await create(client,token,mainq,uuid.uuid4().hex)
            assert mainr.status==200,await mainr.text()
            main=await mainr.json();await confirm(client,migrated_url,main,200)
            reward=await c.fetchrow("SELECT * FROM referral_rewards WHERE invitee_account_id=$1 AND event_kind='first_main_paid'",account)
            assert reward['source_order_id']==uuid.UUID(main['payment_id']) and reward['days']==7
            assert await c.fetchval('SELECT first_paid_order_id FROM referral_benefits WHERE account_id=$1',account)==uuid.UUID(main['payment_id'])
        finally:await c.close()
    finally:await db.close();await client.close()
