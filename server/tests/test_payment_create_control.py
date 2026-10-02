"""Selective-create control only; isolated PostgreSQL and local fake provider."""
import uuid
from dataclasses import replace
import pytest
from test_s4_payments import _app, _session_token, FakePlategaProvider, _bind, _installation_id
from test_payment_recovery_off import quote, pay
from test_step036_onboarding_hour_storage import _connect
from terlimo_backend.config import load_settings
from terlimo_backend.auth_api import ApiError
from terlimo_backend.payments import create_order, record_webhook, reconcile_payments, build_provider, get_order

async def test_default_and_env_parsing(monkeypatch):
    monkeypatch.delenv('PLATEGA_CREATE_ENABLED',raising=False)
    assert load_settings(require_database=False).platega_create_enabled is True
    monkeypatch.setenv('PLATEGA_CREATE_ENABLED','false')
    assert load_settings(require_database=False).platega_create_enabled is False
    monkeypatch.setenv('PLATEGA_CREATE_ENABLED','true')
    assert load_settings(require_database=False).platega_create_enabled is True

async def test_false_new_s5_no_records_or_provider_session(migrated_url,settings_factory):
    provider=FakePlategaProvider()
    client,settings,db=await _app(settings_factory,migrated_url,provider=provider,platega_create_enabled=False)
    try:
        _,token=await _session_token(client);q=await quote(client,token)
        response=await pay(client,token,q,str(uuid.uuid4()))
        body=await response.json()
        assert response.status==503 and body['code']=='SERVICE_UNAVAILABLE'
        assert body.get('details',{}).get('reason')!='expired_quote_no_order'
        c=await _connect(migrated_url)
        try:
            assert await c.fetchval('select count(*) from payment_orders')==0
            assert await c.fetchval('select count(*) from payment_events')==0
        finally:await c.close()
        assert provider.create_calls==[]
        # Configured provider is still constructed for API maintenance, but no HTTP session
        # is opened by the rejected create.
        real=build_provider(replace(settings,platega_enabled=True))
        assert real is not None and real._session is None
        await real.close()
    finally:await db.close();await client.close()

async def test_true_create_false_replay_unknown_and_not_created_retry(migrated_url,settings_factory):
    provider=FakePlategaProvider();client,settings,db=await _app(settings_factory,migrated_url,provider=provider)
    try:
        pop,_=await _session_token(client);inst=await _installation_id(migrated_url,pop.key.fingerprint)
        c=await _connect(migrated_url)
        try:
            args=dict(installation_id=inst,months=1,idempotency_key=str(uuid.uuid4()),method='sbp')
            first=await create_order(c,settings,provider,**args)
            off=replace(settings,platega_create_enabled=False)
            before=await c.fetchval('select to_jsonb(o)::text from payment_orders o')
            assert await create_order(c,off,provider,**args)==first
            with pytest.raises(ApiError) as error:await create_order(c,off,provider,**{**args,'method':'crypto'})
            assert error.value.code=='ORDER_CONFLICT'
            assert await c.fetchval('select to_jsonb(o)::text from payment_orders o')==before
            for state in ['unknown','in_flight','not_created']:
                await c.execute('update payment_orders set provider_create_state=$1,provider_payment_id=NULL',state)
                before=await c.fetchval('select to_jsonb(o)::text from payment_orders o')
                with pytest.raises(ApiError) as error:await create_order(c,off,provider,**args)
                assert error.value.code==('PAYMENT_PROVIDER_UNAVAILABLE' if state=='not_created' else 'PAYMENT_PROVIDER_UNKNOWN')
                assert await c.fetchval('select to_jsonb(o)::text from payment_orders o')==before
                if state!='not_created':
                    with pytest.raises(ApiError) as other:await create_order(c,off,provider,**{**args,'idempotency_key':str(uuid.uuid4())})
                    assert other.value.code=='PAYMENT_PROVIDER_UNKNOWN'
            assert len(provider.create_calls)==1
            assert await c.fetchval('select count(*) from payment_orders')==1
            assert await c.fetchval('select count(*) from payment_events')==0
        finally:await c.close()
    finally:await db.close();await client.close()

async def test_false_preserves_callback_status_and_reconciliation(migrated_url,settings_factory):
    provider=FakePlategaProvider();client,settings,db=await _app(settings_factory,migrated_url,provider=provider)
    try:
        pop,_=await _session_token(client);inst=await _installation_id(migrated_url,pop.key.fingerprint)
        await _bind(migrated_url,inst,555001099)
        c=await _connect(migrated_url)
        try:
            order=(await create_order(c,settings,provider,installation_id=inst,months=1,idempotency_key=str(uuid.uuid4()),method='sbp'))['payment']
            off=replace(settings,platega_create_enabled=False)
            row=await c.fetchrow('select * from payment_orders')
            event={'id':row['provider_payment_id'],'status':'PENDING','amount':200,'currency':'RUB'}
            result=await record_webhook(c,off,merchant='merchant-1',secret='secret-1',body=event)
            assert result['result']=='pending'
            assert (await get_order(c,installation_id=inst,order_id=order['order_id']))['payment']['status']=='pending'
            provider.status_view={'amount':200,'currency':'RUB','status':'CONFIRMED'}
            first=await reconcile_payments(c,off,provider,limit=1)
            assert first['status_changed']==1
            assert await c.fetchval("select count(*) from entitlements where kind='paid'")==1
            before=await c.fetchval('select to_jsonb(o)::text from payment_orders o')
            assert (await get_order(c,installation_id=inst,order_id=order['order_id']))['payment']['status']=='succeeded'
            await reconcile_payments(c,off,provider,limit=1)
            assert await c.fetchval('select to_jsonb(o)::text from payment_orders o')==before
            assert await c.fetchval("select count(*) from entitlements where kind='paid'")==1
            assert len(provider.create_calls)==1
        finally:await c.close()
    finally:await db.close();await client.close()
