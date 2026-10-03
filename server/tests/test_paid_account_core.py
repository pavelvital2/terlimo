"""Synthetic trusted order proof, real temporary PG, no invoice/provider or grant writes."""
import asyncio
import json
from datetime import timedelta
from decimal import Decimal
from pathlib import Path
from uuid import UUID,uuid4

import asyncpg
import pytest

from test_referral_trusted import connect,owner,install
from test_trusted_quotes import _settings,Q,K,eligible
from terlimo_backend.telegram_billing import billing_operation
from terlimo_backend.payments import apply_paid_entitlement,_enqueue_paid_grant,_duration_spec
from terlimo_backend.payment_products import product_of
from terlimo_backend.auth_api import rfc3339


def obj(v):
    while isinstance(v,str):v=json.loads(v)
    return v

async def order(c,s,*,body=None,change=None,mobile=False):
    """Test-only trusted create stand-in: use actual authenticated-account quote core.
    Production create/identity authorization is deliberately not implemented here.
    """
    q=await billing_operation(c,s,body or Q,uuid4().hex)
    r=await c.fetchrow('SELECT * FROM s5_payment_quotes WHERE id=$1',UUID(q['quote_id']))
    a=r['trusted_owner_account_id'];p=product_of(r)
    snapshot={'product':p,'method':r['method'],'duration':_duration_spec(r['months']) if r['months'] else {'unit':'until','value':p['valid_until']},
        'plan':{'plan_id':r['plan_id'],'duration_code':r['duration_code'],'title':'Frozen title','tariff_key':r['tariff_key'],'origin':'paid'}}
    if r['pricing']:snapshot['pricing']=obj(r['pricing'])
    amount=Decimal(r['amount_minor'])/100
    if change:change(snapshot)
    i=await install(c,a) if mobile else None
    b=await c.fetchval('SELECT id FROM account_bindings WHERE installation_id=$1',i) if mobile else None
    oid=await c.fetchval('''INSERT INTO payment_orders(installation_id,owner_kind,trusted_owner_account_id,trusted_caller,
        checkout_owner_account_id,checkout_owner_binding_id,idempotency_key,source_quote_id,quote,amount,currency,months,tariff_key,
        status,provider_payment_id,provider_create_state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12,$13,'succeeded',$14,'created') RETURNING id''',
        i,'installation' if mobile else 'telegram_account',None if mobile else a,None if mobile else 'telegram_backend',a,b,
        uuid4().hex,r['id'],json.dumps(snapshot),amount,r['currency'],r['months'],r['tariff_key'],str(uuid4()))
    return oid,r

async def no_delivery(c):
    for t in ('grants','outbox_operations','outbox_effect_log','referral_rewards'):
        assert await c.fetchval(f'SELECT count(*) FROM {t}')==0

@pytest.mark.asyncio
async def test_full_period_discount_replay_and_pending(migrated_url,settings_factory):
    c=await connect(migrated_url);c2=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        a=await eligible(c);oid,q=await order(c,s)
        await c.execute("UPDATE referral_benefits SET reserved_order_id=$2,reservation_state='reserved' WHERE account_id=$1",a,oid)
        ids=await asyncio.gather(apply_paid_entitlement(c,s,order_id=oid),apply_paid_entitlement(c2,s,order_id=oid))
        assert ids[0]==ids[1] and ids[0]
        e=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',ids[0]);assert e['ends_at']-e['starts_at']==timedelta(days=30)
        assert e['revision']==1 and e['device_limit']==2
        o=await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',oid)
        assert o['account_id']==a and o['binding_id'] is None and o['needs_grant']
        assert obj(o['credited_product'])['pricing']['discount_minor']==10000
        assert obj(o['credited_product'])['device_limit']==2
        b=await c.fetchrow('SELECT * FROM referral_benefits WHERE account_id=$1',a)
        assert b['first_paid_order_id']==oid and b['consumed_order_id']==oid and b['reservation_state']=='consumed'
        assert not await _enqueue_paid_grant(c,s,order_id=oid)
        assert await apply_paid_entitlement(c,s,order_id=oid)==ids[0]
        assert o==await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',oid)
        for t in ('installations','account_bindings'):assert await c.fetchval(f'SELECT count(*) FROM {t}')==0
        await no_delivery(c)
    finally:await c.close();await c2.close()

@pytest.mark.asyncio
@pytest.mark.parametrize('mode',['foreign_product','nonverified','amount_snapshot','duration_snapshot','foreign_quote'])
async def test_owner_and_snapshot_denied(migrated_url,settings_factory,mode):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        a=await eligible(c)
        change=None
        if mode=='foreign_product':change=lambda x:x['product'].update(owner_account_id=str(uuid4()))
        if mode=='duration_snapshot':change=lambda x:x['duration'].update(value=999)
        oid,q=await order(c,s,change=change)
        if mode=='nonverified':await c.execute("UPDATE accounts SET status='unlinked' WHERE id=$1",a)
        if mode in ('amount_snapshot','foreign_quote'):
            # Quote-side corruption is not authorized production mutation; exercise fail-closed proof.
            if mode=='amount_snapshot':await c.execute('UPDATE s5_payment_quotes SET amount_minor=amount_minor+1 WHERE id=$1',q['id'])
            else:await c.execute('UPDATE s5_payment_quotes SET trusted_owner_account_id=$2 WHERE id=$1',q['id'],await owner(c,899,'Foreign'))
        assert await apply_paid_entitlement(c,s,order_id=oid) is None
        assert await c.fetchval('SELECT credit_review_reason FROM payment_orders WHERE id=$1',oid) in ('owner_changed','owner_not_verified','snapshot_conflict')
        assert await c.fetchval('SELECT count(*) FROM entitlements')==0
        await no_delivery(c)
    finally:await c.close()

@pytest.mark.asyncio
async def test_addon_renewal_preserve_target_and_slots(migrated_url,settings_factory):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        await eligible(c);first,_=await order(c,s);eid=await apply_paid_entitlement(c,s,order_id=first)
        end=await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',eid)
        addon,_=await order(c,s,body={**Q,'plan_id':'terlimo-extra-device','duration_code':'until:'+rfc3339(end)})
        assert await apply_paid_entitlement(c,s,order_id=addon)==eid
        slot=await c.fetchrow('SELECT * FROM paid_extra_slots WHERE entitlement_id=$1',eid)
        assert slot['expires_at']==end
        renewal,_=await order(c,s,body={**Q,'renew_extra_slot_ids':[str(slot['id'])]})
        assert await apply_paid_entitlement(c,s,order_id=renewal)==eid
        e=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',eid)
        assert e['ends_at']==end+timedelta(days=30) and e['revision']==3 and e['device_limit']==3
        assert await c.fetchval('SELECT expires_at FROM paid_extra_slots WHERE id=$1',slot['id'])==e['ends_at']
        assert await apply_paid_entitlement(c,s,order_id=addon)==eid
        assert await c.fetchval('SELECT count(*) FROM paid_extra_slots')==1
        await no_delivery(c)
    finally:await c.close()

@pytest.mark.asyncio
async def test_mobile_wrapper_binding_receipt_and_reward_unchanged(migrated_url,settings_factory):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        a=await eligible(c);oid,q=await order(c,s,mobile=True)
        eid=await apply_paid_entitlement(c,s,order_id=oid);assert eid
        o=await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',oid)
        assert o['binding_id']==o['checkout_owner_binding_id'] and o['account_id']==a
        assert o['credited_entitlement_revision']==1 and obj(o['credited_product'])['pricing']==obj(q['pricing'])
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==1
        second,_=await order(c,s,mobile=True)
        i=await c.fetchval('SELECT installation_id FROM payment_orders WHERE id=$1',second)
        await c.execute("UPDATE account_bindings SET status='revoked' WHERE installation_id=$1",i)
        assert await apply_paid_entitlement(c,s,order_id=second) is None
        assert await c.fetchval('SELECT credit_review_reason FROM payment_orders WHERE id=$1',second)=='owner_unbound'
        assert await c.fetchval('SELECT revision FROM entitlements WHERE id=$1',eid)==1
    finally:await c.close()

@pytest.mark.asyncio
async def test_closed_immutable_owners_and_global_keys(migrated_url,settings_factory):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        await eligible(c);oid,q=await order(c,s)
        for update in ("trusted_caller='browser'","owner_kind='installation'","trusted_owner_account_id=NULL","binding_id=gen_random_uuid()","amount=amount+1","quote='{}'::jsonb"):
            with pytest.raises(asyncpg.IntegrityConstraintViolationError):await c.execute(f'UPDATE payment_orders SET {update} WHERE id=$1',oid)
        for field in ('idempotency_key','provider_payment_id'):
            other,_=await order(c,s)
            with pytest.raises(asyncpg.UniqueViolationError):await c.execute(f'UPDATE payment_orders SET {field}=(SELECT {field} FROM payment_orders WHERE id=$1) WHERE id=$2',oid,other)
        down=Path('server/terlimo_backend/migrations/versions/0042_account_orders.down.sql').read_text()
        with pytest.raises(asyncpg.RaiseError):await c.execute(down)
        assert await c.fetchval('SELECT owner_kind FROM payment_orders WHERE id=$1',oid)=='telegram_account'
    finally:await c.close()

@pytest.mark.asyncio
async def test_preexisting_rows_migrate_unchanged_and_down(migrated_url):
    c=await connect(migrated_url)
    path=Path('server/terlimo_backend/migrations/versions')
    try:
        await c.execute((path/'0042_account_orders.down.sql').read_text())
        a=await owner(c,899);i=await install(c,a)
        oid=await c.fetchval("""INSERT INTO payment_orders(installation_id,idempotency_key,quote,amount,currency,months,tariff_key)
            VALUES($1,'old-global-key','{"duration":{"unit":"days","value":30}}',200,'RUB',1,'old') RETURNING id""",i)
        before=dict(await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',oid))
        await c.execute((path/'0042_account_orders.sql').read_text())
        after=dict(await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',oid))
        assert after.pop('owner_kind')=='installation'
        assert after.pop('trusted_caller') is None and after.pop('trusted_owner_account_id') is None
        assert before==after
        await c.execute((path/'0042_account_orders.down.sql').read_text())
        assert before==dict(await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',oid))
    finally:await c.close()

@pytest.mark.asyncio
async def test_two_orders_account_serialization_and_status_lock(migrated_url,settings_factory):
    c=await connect(migrated_url);c2=await connect(migrated_url);c3=await connect(migrated_url)
    s=_settings(settings_factory,migrated_url)
    try:
        a=await eligible(c);o1,_=await order(c,s);o2,_=await order(c,s)
        async with c.transaction():
            eid=await apply_paid_entitlement(c,s,order_id=o1)
            task=asyncio.create_task(apply_paid_entitlement(c2,s,order_id=o2))
            revoke=asyncio.create_task(c3.execute("UPDATE accounts SET status='unlinked' WHERE id=$1",a))
            # Real lock barrier: neither status mutation nor second benefit writer can pass.
            for _ in range(100):
                waiting=await c.fetchval('SELECT count(*) FROM pg_stat_activity WHERE pid=ANY($1::int[]) AND wait_event_type=\'Lock\'', [c2.get_server_pid(),c3.get_server_pid()])
                if waiting==2:break
                await asyncio.sleep(.01)
            assert waiting==2 and not task.done() and not revoke.done()
        result=await asyncio.wait_for(task,3);await asyncio.wait_for(revoke,3)
        # Depending on lock scheduling, revoke may win after first commit. Both outcomes
        # must be safe: second credit uses same target, or explicitly needs review.
        assert result in (eid,None)
        assert await c.fetchval('SELECT count(*) FROM entitlements')==1
        e=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',eid)
        assert e['revision']==(2 if result else 1)
        assert e['ends_at']-e['starts_at']==timedelta(days=60 if result else 30)
        await no_delivery(c)
    finally:await c.close();await c2.close();await c3.close()

@pytest.mark.asyncio
async def test_applied_external_pending_not_mobile_reconcile_work(migrated_url,settings_factory):
    from terlimo_backend.payments import reconcile_payments
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    class NoProviderCalls:
        async def get_status(self,*args):raise AssertionError('no provider I/O for applied credit')
    try:
        await eligible(c);oid,_=await order(c,s);await apply_paid_entitlement(c,s,order_id=oid)
        result=await reconcile_payments(c,s,NoProviderCalls())
        assert result=={'checked':0,'applied':0,'status_changed':0}
        assert await c.fetchval('SELECT needs_grant FROM payment_orders WHERE id=$1',oid)
        await no_delivery(c)
    finally:await c.close()

@pytest.mark.asyncio
@pytest.mark.parametrize('months',[3,6])
async def test_calendar_full_period(migrated_url,settings_factory,months):
    from terlimo_backend.payments import paid_end
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        await eligible(c)
        oid,_=await order(c,s,body={**Q,'plan_id':f'terlimo-{months}m','duration_code':f'months:{months}'})
        eid=await apply_paid_entitlement(c,s,order_id=oid)
        e=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',eid)
        assert e['ends_at']==paid_end(e['starts_at'],{'unit':'months','value':months})
        await no_delivery(c)
    finally:await c.close()

@pytest.mark.asyncio
async def test_stale_addon_target_does_not_extend_or_award(migrated_url,settings_factory):
    c=await connect(migrated_url);s=_settings(settings_factory,migrated_url)
    try:
        await eligible(c);first,_=await order(c,s);eid=await apply_paid_entitlement(c,s,order_id=first)
        end=await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',eid)
        addon,_=await order(c,s,body={**Q,'plan_id':'terlimo-extra-device','duration_code':'until:'+rfc3339(end)})
        renewal,_=await order(c,s);assert await apply_paid_entitlement(c,s,order_id=renewal)==eid
        assert await apply_paid_entitlement(c,s,order_id=addon) is None
        assert await c.fetchval('SELECT credit_review_reason FROM payment_orders WHERE id=$1',addon)=='target_period_changed'
        assert await c.fetchval('SELECT count(*) FROM paid_extra_slots')==0
        assert await c.fetchval('SELECT revision FROM entitlements WHERE id=$1',eid)==2
        await no_delivery(c)
    finally:await c.close()

@pytest.mark.asyncio
async def test_callback_claim_consume_credit_atomic_pending(migrated_url,settings_factory):
    from dataclasses import replace
    from terlimo_backend.payments import record_webhook
    c=await connect(migrated_url)
    s=replace(_settings(settings_factory,migrated_url),platega_merchant_id='offline-merchant',platega_secret='offline-secret')
    try:
        a=await eligible(c);oid,_=await order(c,s)
        await c.execute("UPDATE payment_orders SET status='pending' WHERE id=$1",oid)
        await c.execute("UPDATE referral_benefits SET reserved_order_id=$2,reservation_state='reserved' WHERE account_id=$1",a,oid)
        o=await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',oid)
        body={'id':o['provider_payment_id'],'status':'CONFIRMED','amount':float(o['amount']),'currency':'RUB'}
        assert await record_webhook(c,s,merchant='offline-merchant',secret='offline-secret',body=body)=={'result':'succeeded'}
        after=await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',oid)
        assert after['applied_entitlement_id'] and after['needs_grant'] and after['binding_id'] is None
        assert await c.fetchval('SELECT consumed_order_id FROM referral_benefits WHERE account_id=$1',a)==oid
        assert await record_webhook(c,s,merchant='offline-merchant',secret='offline-secret',body=body)=={'result':'duplicate'}
        assert after==await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',oid)
        await no_delivery(c)
    finally:await c.close()
