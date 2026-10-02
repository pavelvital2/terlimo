"""New definitive-reason guards only: isolated PG and offline provider."""
import asyncio,json,uuid
from contextlib import asynccontextmanager
import pytest
from test_payment_addon_slice import identity,hdr
from test_s4_payments import _app,FakePlategaProvider
from test_step036_onboarding_hour_storage import _connect,_seed_installation
from terlimo_backend.payments import PAYMENT_PROVIDER_KEY
from terlimo_backend.s5_payments import PLANS_PATH,QUOTES_PATH,PAYMENTS_PATH

async def setup(client,url):
    account=uuid.uuid4();iid,token=await identity(client,url,account)
    plan=(await (await client.get(PLANS_PATH+'?payment_contract=2',headers=hdr(token))).json())['plans'][0]
    r=await client.post(QUOTES_PATH+'?payment_contract=2',headers=hdr(token),json={'plan_id':plan['plan_id'],'duration_code':plan['duration_code'],'method':'sbp'})
    assert r.status==200
    return account,iid,token,(await r.json())['quote_id'],str(uuid.uuid4())
async def pay(client,token,q,key):
    h=hdr(token);h['Idempotency-Key']=key
    return await client.post(PAYMENTS_PATH+'?payment_contract=2',headers=h,json={'quote_id':q})
def reason(body):return body.get('details',{}).get('reason')
async def expire(c,q):await c.execute("UPDATE s5_payment_quotes SET expires_at=now()-interval '1 second' WHERE id=$1",uuid.UUID(q))
async def insert_order(c,iid,q,key,state='not_created',account=None):
    row=await c.fetchrow('SELECT * FROM s5_payment_quotes WHERE id=$1',uuid.UUID(q));product=row['product']
    while isinstance(product,str):product=json.loads(product)
    return await c.fetchval("""INSERT INTO payment_orders
      (installation_id,idempotency_key,quote,amount,currency,months,tariff_key,source_quote_id,
       provider_create_state,provider_payment_id,provider_payment_url,checkout_owner_account_id)
      VALUES($1,$2,$3::jsonb,200,'RUB',1,$4,$5,$6,$7,$8,$9) RETURNING id""",iid,key,
      {'method':'sbp','product':product},row['tariff_key'],uuid.UUID(q),state,
      'offline-known' if state=='created' else None,'https://pay.test/known' if state=='created' else None,account)

async def test_positive_expired_noorder_and_late_same_keys(migrated_url,settings_factory):
    provider=FakePlategaProvider();client,_,db=await _app(settings_factory,migrated_url,provider=provider,disable_provider=True)
    try:
        _,_,token,q,k=await setup(client,migrated_url);c=await _connect(migrated_url)
        try:
            await expire(c,q);r=await pay(client,token,q,k);body=await r.json()
            assert r.status==409 and body['code']=='QUOTE_EXPIRED' and body['retryable'] is False
            assert reason(body)=='expired_quote_no_order' and body['schema_version']=='1.0'
            assert not any(x in body for x in ['payment_id','payment','credited_product'])
            # Legacy consumers see unchanged failure fields, never a paid/terminal receipt.
            assert {x:body[x] for x in ['code','retryable']}=={'code':'QUOTE_EXPIRED','retryable':False}
            client.server.app[PAYMENT_PROVIDER_KEY]=provider
            r=await pay(client,token,q,k);assert r.status==409 and reason(await r.json())=='expired_quote_no_order'
            assert await c.fetchval('SELECT count(*) FROM payment_orders')==0
            assert await c.fetchval('SELECT count(*) FROM payment_events')==0 and provider.create_calls==[]
        finally:await c.close()
    finally:await db.close();await client.close()

async def test_negative_durable_owner_selection_missing_legacy_guards(migrated_url,settings_factory):
    provider=FakePlategaProvider();client,_,db=await _app(settings_factory,migrated_url,provider=provider,disable_provider=True)
    try:
        account,iid,token,q,k=await setup(client,migrated_url);c=await _connect(migrated_url)
        try:
            r=await pay(client,token,q,k);assert r.status==503 and reason(await r.json())!='expired_quote_no_order'
            await expire(c,q)
            for state in ['not_created','unknown','in_flight','created']:
                oid=await insert_order(c,iid,q,k,state,account);r=await pay(client,token,q,k);body=await r.json()
                assert reason(body)!='expired_quote_no_order'
                if state=='created':assert r.status==200 and body['payment_id']==str(oid)
                elif state in ['unknown','in_flight']:assert body['code']=='PAYMENT_PROVIDER_UNKNOWN'
                else:assert r.status==503
                await c.execute('DELETE FROM payment_orders WHERE id=$1',oid)
            other=await _seed_installation(migrated_url);oid=await insert_order(c,other,q,k)
            assert (await (await pay(client,token,q,k)).json())['code']=='ORDER_CONFLICT'
            await c.execute('DELETE FROM payment_orders WHERE id=$1',oid)
            oid=await insert_order(c,iid,q,str(uuid.uuid4()))
            assert (await (await pay(client,token,q,k)).json())['code']=='ORDER_CONFLICT'
            await c.execute('DELETE FROM payment_orders WHERE id=$1',oid)
            oid=await insert_order(c,iid,q,str(uuid.uuid4()),'unknown')
            await c.execute('UPDATE payment_orders SET source_quote_id=NULL WHERE id=$1',oid)
            assert (await (await pay(client,token,q,k)).json())['code']=='PAYMENT_PROVIDER_UNKNOWN'
            await c.execute('DELETE FROM payment_orders WHERE id=$1',oid)
            product=await c.fetchval('SELECT product FROM s5_payment_quotes WHERE id=$1',uuid.UUID(q))
            while isinstance(product,str):product=json.loads(product)
            for changed,status in [({**product,'owner_account_id':str(uuid.uuid4())},404),({**product,'plan_id':'wrong-plan'},409)]:
                await c.execute('UPDATE s5_payment_quotes SET product=$1::jsonb WHERE id=$2',json.dumps(changed),uuid.UUID(q))
                r=await pay(client,token,q,k);assert r.status==status and reason(await r.json())!='expired_quote_no_order'
            await c.execute('UPDATE s5_payment_quotes SET product=NULL WHERE id=$1',uuid.UUID(q))
            r=await pay(client,token,q,k);assert r.status==409 and reason(await r.json())!='expired_quote_no_order'
            await c.execute('DELETE FROM s5_payment_quotes WHERE id=$1',uuid.UUID(q))
            r=await pay(client,token,q,k);assert r.status==404 and reason(await r.json())!='expired_quote_no_order'
            assert reason(await (await pay(client,token,'not-a-uuid',k)).json())!='expired_quote_no_order'
            assert await c.fetchval('SELECT count(*) FROM payment_orders')==0 and provider.create_calls==[]
        finally:await c.close()
    finally:await db.close();await client.close()

@pytest.mark.parametrize('change',['binding_generation','quote_deleted'])
async def test_wait_then_refresh_identity_and_quote(migrated_url,settings_factory,change):
    client,_,db=await _app(settings_factory,migrated_url,disable_provider=True);task=None;c=None
    try:
        _,iid,token,q,k=await setup(client,migrated_url);c=await _connect(migrated_url);await expire(c,q)
        lock=f'payment-install:{iid}';await c.execute('SELECT pg_advisory_lock(hashtextextended($1,0))',lock)
        task=asyncio.create_task(pay(client,token,q,k))
        for _ in range(100):
            waiting=await c.fetchval("SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted")
            if waiting:break
            await asyncio.sleep(.01)
        assert waiting and not task.done()
        if change=='binding_generation':await c.execute('UPDATE account_bindings SET generation=generation+1 WHERE installation_id=$1',iid)
        else:await c.execute('DELETE FROM s5_payment_quotes WHERE id=$1',uuid.UUID(q))
        await c.execute('SELECT pg_advisory_unlock(hashtextextended($1,0))',lock)
        r=await asyncio.wait_for(task,5);body=await r.json()
        assert r.status==(401 if change=='binding_generation' else 404) and reason(body)!='expired_quote_no_order'
        assert await c.fetchval('SELECT count(*) FROM payment_orders')==0
    finally:
        if task and not task.done():task.cancel()
        if c:await c.close()
        await db.close();await client.close()

async def test_db_failure_cannot_certify_no_order(migrated_url,settings_factory,monkeypatch):
    client,_,db=await _app(settings_factory,migrated_url,disable_provider=True)
    try:
        _,_,token,q,k=await setup(client,migrated_url);c=await _connect(migrated_url)
        try:await expire(c,q)
        finally:await c.close()
        original=db.acquire
        class FailingLookup:
            def __init__(self,connection):self.connection=connection
            def __getattr__(self,name):return getattr(self.connection,name)
            async def fetchrow(self,sql,*args,**kw):
                if sql=='SELECT * FROM payment_orders WHERE idempotency_key=$1':raise RuntimeError('offline injected DB lookup failure')
                return await self.connection.fetchrow(sql,*args,**kw)
        @asynccontextmanager
        async def acquire():
            async with original() as connection:yield FailingLookup(connection)
        monkeypatch.setattr(db,'acquire',acquire)
        r=await pay(client,token,q,k);assert r.status==500 and 'expired_quote_no_order' not in await r.text()
    finally:await db.close();await client.close()
