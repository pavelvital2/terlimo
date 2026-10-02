"""Offline PostgreSQL checks for the replay/OFF and S5 expiry serialization boundary."""
import asyncio
import uuid
from dataclasses import replace

import pytest
from test_s4_payments import _app, _auth, _session_token, FakePlategaProvider
from test_step036_onboarding_hour_storage import _connect
from terlimo_backend.auth_api import ApiError
from terlimo_backend.payments import PAYMENT_PROVIDER_KEY, create_order
from terlimo_backend.s5_payments import PLANS_PATH, QUOTES_PATH, PAYMENTS_PATH

async def quote(client, token):
    plan=(await (await client.get(PLANS_PATH)).json())["plans"][0]
    result=await client.post(QUOTES_PATH, headers={**_auth(token),"Idempotency-Key":str(uuid.uuid4())},
        json={"plan_id":plan["plan_id"],"duration_code":"days:30","method":"sbp"})
    assert result.status==200
    return (await result.json())["quote_id"]

async def pay(client, token, q, k):
    return await client.post(PAYMENTS_PATH,headers={**_auth(token),"Idempotency-Key":k},json={"quote_id":q})

async def test_created_replay_off_expired_preserves_matches_and_rows(migrated_url,settings_factory):
    provider=FakePlategaProvider();client,settings,db=await _app(settings_factory,migrated_url,provider=provider)
    try:
        _,token=await _session_token(client);q=await quote(client,token);k=str(uuid.uuid4())
        first=await pay(client,token,q,k);assert first.status==200
        first_body=await first.json()
        c=await _connect(migrated_url)
        try:
            order=await c.fetchrow('SELECT * FROM payment_orders WHERE idempotency_key=$1',k)
            await c.execute("UPDATE s5_payment_quotes SET expires_at=now()-interval '1 second' WHERE id=$1",uuid.UUID(q))
            before=await c.fetchval('SELECT to_jsonb(o)::text FROM payment_orders o WHERE id=$1',order['id'])
            client.server.app[PAYMENT_PROVIDER_KEY]=None
            replay=await pay(client,token,q,k);assert replay.status==200
            assert (await replay.json())['payment_id']==first_body['payment_id']
            # Matching replay ignores current method availability, while immutable method
            # and owner/amount/quote identity remain checked even with provider OFF.
            args=dict(installation_id=order['installation_id'],months=1,idempotency_key=k,
                      quote_id=q,method='sbp',public_method='sbp')
            assert (await create_order(c,replace(settings,platega_methods=''),None,**args))['payment']['order_id']==str(order['id'])
            for changes in [dict(method='crypto',public_method='crypto'),dict(quote_id=str(uuid.uuid4()))]:
                with pytest.raises(ApiError) as caught:await create_order(c,settings,None,**{**args,**changes})
                assert caught.value.code=='ORDER_CONFLICT'
            with pytest.raises(ApiError) as caught:
                await create_order(c,replace(settings,payment_price_rub_1=201),None,**args)
            assert caught.value.code=='ORDER_CONFLICT'
            owner=await c.fetchval("INSERT INTO accounts(status) VALUES('verified') RETURNING id")
            await c.execute('UPDATE payment_orders SET checkout_owner_account_id=$1 WHERE id=$2',owner,order['id'])
            with pytest.raises(ApiError) as caught:
                await create_order(c,settings,None,**args,checkout_owner_account_id=uuid.uuid4())
            assert caught.value.code=='ORDER_CONFLICT'
            await c.execute('UPDATE payment_orders SET checkout_owner_account_id=NULL WHERE id=$1',order['id'])
            assert await c.fetchval('SELECT to_jsonb(o)::text FROM payment_orders o WHERE id=$1',order['id'])==before
            assert len(provider.create_calls)==1
        finally:await c.close()
    finally:await db.close();await client.close()

async def test_new_off_and_expired_no_order_no_provider(migrated_url,settings_factory):
    provider=FakePlategaProvider();client,_,db=await _app(settings_factory,migrated_url,provider=provider,disable_provider=True)
    try:
        _,token=await _session_token(client);q=await quote(client,token);k=str(uuid.uuid4())
        off=await pay(client,token,q,k);assert off.status==503
        assert (await off.json())['code']=='SERVICE_UNAVAILABLE'
        c=await _connect(migrated_url)
        try:
            await c.execute("UPDATE s5_payment_quotes SET expires_at=now()-interval '1 second' WHERE id=$1",uuid.UUID(q))
            expired=await pay(client,token,q,k);assert expired.status==409
            assert (await expired.json())['code']=='QUOTE_EXPIRED'
            assert await c.fetchval('SELECT count(*) FROM payment_orders')==0
            assert await c.fetchval('SELECT count(*) FROM payment_events')==0
            assert provider.create_calls==[]
        finally:await c.close()
    finally:await db.close();await client.close()

@pytest.mark.parametrize('state',['unknown','in_flight'])
async def test_unknown_off_expired_stays_unknown(migrated_url,settings_factory,state):
    provider=FakePlategaProvider();client,_,db=await _app(settings_factory,migrated_url,provider=provider)
    try:
        _,token=await _session_token(client);q=await quote(client,token);k=str(uuid.uuid4())
        assert (await pay(client,token,q,k)).status==200
        c=await _connect(migrated_url)
        try:
            await c.execute('UPDATE payment_orders SET provider_create_state=$1,provider_payment_id=NULL WHERE idempotency_key=$2',state,k)
            await c.execute("UPDATE s5_payment_quotes SET expires_at=now()-interval '1 second' WHERE id=$1",uuid.UUID(q))
            before=await c.fetchval('SELECT to_jsonb(o)::text FROM payment_orders o WHERE idempotency_key=$1',k)
            client.server.app[PAYMENT_PROVIDER_KEY]=None
            result=await pay(client,token,q,k);assert result.status==503
            assert (await result.json())['code']=='PAYMENT_PROVIDER_UNKNOWN'
            assert await c.fetchval('SELECT to_jsonb(o)::text FROM payment_orders o WHERE idempotency_key=$1',k)==before
            assert len(provider.create_calls)==1
        finally:await c.close()
    finally:await db.close();await client.close()

async def test_s5_expired_replay_waits_for_active_create(migrated_url,settings_factory):
    class PausedProvider(FakePlategaProvider):
        def __init__(self):super().__init__();self.entered=asyncio.Event();self.release=asyncio.Event()
        async def create_payment(self,**kw):
            self.entered.set();await self.release.wait();return await super().create_payment(**kw)
    provider=PausedProvider();client,_,db=await _app(settings_factory,migrated_url,provider=provider)
    first=second=None
    try:
        _,token=await _session_token(client);q=await quote(client,token);k=str(uuid.uuid4())
        first=asyncio.create_task(pay(client,token,q,k));await asyncio.wait_for(provider.entered.wait(),5)
        c=await _connect(migrated_url)
        try:
            await c.execute("UPDATE s5_payment_quotes SET expires_at=now()-interval '1 second' WHERE id=$1",uuid.UUID(q))
            client.server.app[PAYMENT_PROVIDER_KEY]=None
            second=asyncio.create_task(pay(client,token,q,k))
            # Confirm that a different backend is really waiting on this advisory lock,
            # instead of inferring concurrency merely from an arbitrary sleep.
            for _ in range(100):
                waiting=await c.fetchval("SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted")
                if waiting:break
                await asyncio.sleep(.01)
            assert waiting and not second.done()
            provider.release.set()
            a,b=await asyncio.wait_for(asyncio.gather(first,second),5)
            assert a.status==b.status==200
            assert (await a.json())['payment_id']==(await b.json())['payment_id']
            assert len(provider.create_calls)==1
            assert await c.fetchval('SELECT count(*) FROM payment_orders')==1
            # Nested acquisition was released by both paths: no lock leaks after response.
            inst=await c.fetchval('SELECT installation_id FROM payment_orders WHERE idempotency_key=$1',k)
            assert await c.fetchval('SELECT pg_try_advisory_lock(hashtextextended($1,0))',f'payment-install:{inst}')
            await c.execute('SELECT pg_advisory_unlock(hashtextextended($1,0))',f'payment-install:{inst}')
        finally:await c.close()
    finally:
        provider.release.set()
        for task in [first,second]:
            if task and not task.done():task.cancel()
        await db.close();await client.close()
