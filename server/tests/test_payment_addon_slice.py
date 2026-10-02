"""Only new product/selected-renewal/scoped HTTP cases; temporary DB and fake provider."""
import json
import uuid
from datetime import UTC, datetime, timedelta
import asyncpg
import pytest
from test_s4_payments import _app, FakePlategaProvider, _auth, _webhook_headers
from test_auth_flow import KeyMaterial, PoPClient, _challenge, _enroll, _session
from terlimo_backend.s5_payments import PLANS_PATH, QUOTES_PATH, PAYMENTS_PATH
from terlimo_backend.payments import WEBHOOK_PATH
from terlimo_backend.mobile_account import effective_device_limit
from terlimo_backend.payment_products import paid_limit, refresh_expired_limits


def hdr(token, *, v2=True):
    h={**_auth(token),'Idempotency-Key':'test-'+uuid.uuid4().hex}
    return h


async def identity(client,url,account_id):
    key=KeyMaterial();pop=PoPClient(key)
    assert (await _enroll(client,pop,await _challenge(client,key,'enrollment'))).status==200
    c=await asyncpg.connect(url)
    try:
        iid=await c.fetchval('SELECT id FROM installations WHERE public_key_fingerprint=$1',key.fingerprint)
        await c.execute("INSERT INTO accounts(id,status,telegram_id) VALUES ($1,'verified',$2)",account_id,account_id.int % 1000000000000)
        await c.execute("INSERT INTO account_bindings(account_id,installation_id,status) VALUES ($1,$2,'active')",account_id,iid)
    finally:await c.close()
    r=await _session(client,pop,await _challenge(client,key,'session'),['session:read','session:write'],'idem-'+uuid.uuid4().hex)
    assert r.status==200,await r.text()
    return iid,(await r.json())['session']['session_id']


async def paid(url,account, *, days=15):
    c=await asyncpg.connect(url)
    try:
        return await c.fetchrow("INSERT INTO entitlements(account_id,kind,status,starts_at,ends_at,device_limit,revision) VALUES ($1,'paid','active',$2,$3,2,7) RETURNING *",account,datetime.now(UTC)-timedelta(days=15),datetime.now(UTC)+timedelta(days=days))
    finally:await c.close()


async def invoice(client,token,plan,selected=None):
    body={'plan_id':plan['plan_id'],'duration_code':plan['duration_code'],'method':'sbp'}
    if selected is not None:body['renew_extra_slot_ids']=selected
    q=await client.post(QUOTES_PATH+'?payment_contract=2',headers=hdr(token),json=body)
    assert q.status==200,await q.text()
    q=await q.json();h=hdr(token)
    r=await client.post(PAYMENTS_PATH+'?payment_contract=2',headers=h,json={'quote_id':q['quote_id']})
    assert r.status==200,await r.text()
    return q,await r.json(),h


async def confirm(client,url,payment,amount,event=None):
    c=await asyncpg.connect(url)
    try:provider_id=await c.fetchval('SELECT provider_payment_id FROM payment_orders WHERE id=$1',uuid.UUID(payment['payment_id']))
    finally:await c.close()
    r=await client.post(WEBHOOK_PATH,headers=_webhook_headers(),json={'id':provider_id,'status':'CONFIRMED','amount':amount,'currency':'RUB','eventId':event or uuid.uuid4().hex})
    assert r.status==200,await r.text()
    return await r.json()


async def test_scoped_http_plans_quote_create_snapshot(migrated_url,settings_factory):
    buyer=uuid.uuid4();other=uuid.uuid4();provider=FakePlategaProvider()
    client,s,d=await _app(settings_factory,migrated_url,provider=provider,s5_control_account_id=str(buyer),s5_control_price_rub_1=10,s5_control_price_rub_3=20)
    try:
        _,token=await identity(client,migrated_url,buyer);_,other_token=await identity(client,migrated_url,other)
        for t,price in ((token,10),(other_token,200)):
            r=await client.get(PLANS_PATH,headers=_auth(t));assert r.status==200
            plan=(await r.json())['plans'][0];assert plan['amount']['amount_minor']==price*100
            q=await client.post(QUOTES_PATH,headers=hdr(t,v2=False),json={'plan_id':plan['plan_id'],'duration_code':plan['duration_code'],'method':'sbp'})
            assert q.status==200,await q.text()
            q=await q.json();assert q['amount']['amount_minor']==price*100
            h=hdr(t,v2=False);r=await client.post(PAYMENTS_PATH,headers=h,json={'quote_id':q['quote_id']});assert r.status==200,await r.text()
            payment=await r.json();assert provider.create_calls[-1]['amount']==price
            c=await asyncpg.connect(migrated_url)
            try:
                order=await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',uuid.UUID(payment['payment_id']));assert order['amount']==price
                assert order['source_quote_id']==uuid.UUID(q['quote_id'])
                quote=await c.fetchrow('SELECT * FROM s5_payment_quotes WHERE id=$1',order['source_quote_id']);assert quote['amount_minor']==price*100
            finally:await c.close()
            r=await client.post(PAYMENTS_PATH,headers=h,json={'quote_id':q['quote_id']});assert r.status==200
        assert len(provider.create_calls)==2
        assert (await (await client.get(PLANS_PATH)).json())['plans'][0]['amount']['amount_minor']==20000
    finally:await d.close();await client.close()


async def test_addon_one_credit_same_end_duplicate_and_renew_selected(migrated_url,settings_factory):
    buyer=uuid.uuid4();provider=FakePlategaProvider()
    client,s,d=await _app(settings_factory,migrated_url,provider=provider,s5_control_account_id=str(buyer),s5_control_price_rub_1=10,s5_control_price_rub_extra=5)
    try:
        _,token=await identity(client,migrated_url,buyer);original=await paid(migrated_url,buyer)
        plans=(await (await client.get(PLANS_PATH+'?payment_contract=2',headers=hdr(token))).json())['plans'];addon=plans[-1]
        assert addon['plan_id']=='terlimo-extra-device';assert addon['amount']['amount_minor']==500
        for _ in range(2):
            q,p,h=await invoice(client,token,addon)
            assert q['device_limit'] in (3,4);assert q['product']['kind']=='device_addon';assert q['duration_code'].startswith('until:')
            assert q['product']['plan_id']=='terlimo-extra-device'
            assert q['product']['valid_until']==q['product']['target_valid_until']
            assert (await confirm(client,migrated_url,p,5))['result']=='succeeded'
            assert (await confirm(client,migrated_url,p,5))['result']=='duplicate'
            r=await client.post(PAYMENTS_PATH+'?payment_contract=2',headers=h,json={'quote_id':q['quote_id']});assert r.status==200
        c=await asyncpg.connect(migrated_url)
        try:
            e=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',original['id']);assert e['device_limit']==4 and e['revision']==9 and e['ends_at']==original['ends_at']
            extra=await c.fetch('SELECT * FROM paid_extra_slots WHERE entitlement_id=$1 ORDER BY id',e['id']);assert len(extra)==2
        finally:await c.close()
        q,p,h=await invoice(client,token,plans[0],selected=[str(extra[0]['id'])]);assert q['amount']['amount_minor']==1500
        assert q['device_limit']==3 and q['product']['extra_amount_minor']==500
        assert q['product']['plan_id']=='terlimo-30d'
        assert datetime.fromisoformat(q['product']['valid_until'].replace('Z','+00:00'))==datetime.fromisoformat(q['product']['target_valid_until'].replace('Z','+00:00'))+timedelta(days=30)
        await confirm(client,migrated_url,p,15);await confirm(client,migrated_url,p,15)
        state=await (await client.get(PAYMENTS_PATH+'/'+p['payment_id']+'?payment_contract=2',headers=hdr(token))).json()
        assert state['credit_state']=='applied'
        assert state['credited_product']['device_limit']==3
        assert state['credited_product']['current_device_limit']==4
        assert state['credited_product']['valid_until']==q['product']['valid_until']
        c=await asyncpg.connect(migrated_url)
        try:
            e=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',original['id']);assert e['ends_at']==original['ends_at']+timedelta(days=30) and e['revision']==10
            rows=await c.fetch('SELECT * FROM paid_extra_slots WHERE entitlement_id=$1 ORDER BY id',e['id']);assert rows[0]['expires_at']==e['ends_at'];assert rows[1]['expires_at']==original['ends_at']
            future=original['ends_at']+timedelta(seconds=1)
            assert await paid_limit(c,e,future)==3
            assert await effective_device_limit(c,buyer,now=future)==3
            # Narrow expiry materialization, no free inheritance for unselected slot.
            await c.execute('UPDATE paid_extra_slots SET expires_at=now()-interval \'1 second\' WHERE id=$1',rows[1]['id'])
            assert await refresh_expired_limits(c)==1
            assert await c.fetchval('SELECT device_limit FROM entitlements WHERE id=$1',e['id'])==3
        finally:await c.close()
        assert len(provider.create_calls)==3
    finally:await d.close();await client.close()


@pytest.mark.parametrize('failure',['foreign','expired','rebound'])
async def test_paid_addon_foreign_or_expired_target_is_visible_review(migrated_url,settings_factory,failure):
    buyer=uuid.uuid4();client,s,d=await _app(settings_factory,migrated_url,provider=FakePlategaProvider())
    try:
        iid,token=await identity(client,migrated_url,buyer);original=await paid(migrated_url,buyer)
        addon=(await (await client.get(PLANS_PATH+'?payment_contract=2',headers=hdr(token))).json())['plans'][-1]
        q,p,h=await invoice(client,token,addon);amount=q['amount']['amount_minor']/100
        c=await asyncpg.connect(migrated_url)
        try:
            if failure=='expired':await c.execute('UPDATE entitlements SET ends_at=now()-interval \'1 second\' WHERE id=$1',original['id'])
            else:
                other=await c.fetchval("INSERT INTO accounts(status) VALUES ('verified') RETURNING id")
                if failure=='foreign':
                    await c.execute('UPDATE entitlements SET account_id=$2 WHERE id=$1',original['id'],other)
                else:
                    await c.execute("UPDATE account_bindings SET status='revoked' WHERE installation_id=$1",iid)
                    await c.execute("INSERT INTO account_bindings(account_id,installation_id,status) VALUES ($1,$2,'active')",other,iid)
        finally:await c.close()
        await confirm(client,migrated_url,p,amount)
        r=await client.get(PAYMENTS_PATH+'/'+p['payment_id']+'?payment_contract=2',headers=hdr(token));assert r.status==200,await r.text()
        value=await r.json();assert value['payment_status']=='paid' and value['credit_state']=='needs_review' and value['credited_entitlement_revision'] is None
        assert value['credit_review_reason'] in ('target_expired','target_unavailable','owner_changed')
        c=await asyncpg.connect(migrated_url)
        try:
            assert await c.fetchval('SELECT count(*) FROM paid_extra_slots')==0
            assert await c.fetchval('SELECT revision FROM entitlements WHERE id=$1',original['id'])==7
            assert await c.fetchval('SELECT count(*) FROM entitlements')==1
        finally:await c.close()
    finally:await d.close();await client.close()


async def test_addon_provider_unknown_never_creates_a_second_invoice(migrated_url,settings_factory):
    class UnknownProvider(FakePlategaProvider):
        async def create_payment(self,**kwargs):
            await super().create_payment(**kwargs)
            from terlimo_backend.payments import ProviderUnknown
            raise ProviderUnknown()
    buyer=uuid.uuid4();provider=UnknownProvider()
    client,s,d=await _app(settings_factory,migrated_url,provider=provider)
    try:
        _,token=await identity(client,migrated_url,buyer);await paid(migrated_url,buyer)
        addon=(await (await client.get(PLANS_PATH+'?payment_contract=2',headers=hdr(token))).json())['plans'][-1]
        r=await client.post(QUOTES_PATH+'?payment_contract=2',headers=hdr(token),json={'plan_id':addon['plan_id'],'duration_code':addon['duration_code'],'method':'sbp'})
        assert r.status==200
        q=await r.json();h=hdr(token)
        for header in (h,h,hdr(token)):
            r=await client.post(PAYMENTS_PATH+'?payment_contract=2',headers=header,json={'quote_id':q['quote_id']})
            assert r.status==503,await r.text()
            assert (await r.json())['code']=='PAYMENT_PROVIDER_UNKNOWN'
        assert len(provider.create_calls)==1
        c=await asyncpg.connect(migrated_url)
        try:
            assert await c.fetchval("SELECT count(*) FROM payment_orders WHERE provider_create_state='unknown'")==1
            assert await c.fetchval('SELECT count(*) FROM paid_extra_slots')==0
        finally:await c.close()
    finally:await d.close();await client.close()


async def test_selective_renewal_gap_is_server_priced_and_quotes_store_purchased_period(migrated_url,settings_factory):
    # Owner-reviewed full days and nearest whole RUB rounding (half up), including selected-slot gap.
    buyer=uuid.uuid4();client,s,d=await _app(settings_factory,migrated_url,provider=FakePlategaProvider())
    try:
        _,token=await identity(client,migrated_url,buyer);e=await paid(migrated_url,buyer)
        c=await asyncpg.connect(migrated_url)
        try:
            sid=await c.fetchval('INSERT INTO paid_extra_slots(entitlement_id,expires_at) VALUES ($1,$2) RETURNING id',e['id'],e['ends_at']-timedelta(days=10,hours=1))
        finally:await c.close()
        plan=(await (await client.get(PLANS_PATH+'?payment_contract=2',headers=hdr(token))).json())['plans'][0]
        assert plan['product']['extra_slots'][0]['renew_amount_minor']==13300
        q,p,h=await invoice(client,token,plan,selected=[str(sid)])
        assert q['amount']['amount_minor']==33300 and q['product']['extra_amount_minor']==13300
        await confirm(client,migrated_url,p,333.00)
        c=await asyncpg.connect(migrated_url)
        try:
            assert await c.fetchval('SELECT expires_at FROM paid_extra_slots WHERE id=$1',sid)==e['ends_at']+timedelta(days=30)
        finally:await c.close()
    finally:await d.close();await client.close()


@pytest.mark.parametrize('seconds,months,expected',[(0,1,0),(0.5,1,0),(86399.999999,1,0),(86400,1,300),(2*86400,1,700),(10*86400+5*3600,1,3300),(15*86400,1,5000),(20*86400,1,6700),(30*86400,1,10000),(31*86400,1,10000),(60*86400,3,20000),(100*86400,3,30000)])
def test_whole_days_prorata_boundary(seconds,months,expected):
    from terlimo_backend.payment_products import prorata_minor
    now=datetime(2026,10,2,15,0,tzinfo=UTC)
    assert prorata_minor(now+timedelta(seconds=seconds),now-timedelta(days=1),now,months)==expected
    if seconds==86400:
        assert prorata_minor(now+timedelta(days=1),now,now+timedelta(microseconds=1),1)==0
        assert prorata_minor(now+timedelta(days=2),now+timedelta(days=1),now,1)==300


async def test_minor_rounding_http_db_callback_status_and_wire_fixtures(migrated_url,settings_factory):
    from decimal import Decimal
    from pathlib import Path
    from terlimo_backend.payments import _exact_amount,reconcile_payments
    from terlimo_backend.db import Database
    buyer=uuid.uuid4();other=uuid.uuid4();third=uuid.uuid4();provider=FakePlategaProvider()
    client,s,d=await _app(settings_factory,migrated_url,provider=provider,s5_control_account_id=str(buyer),s5_control_price_rub_1=10)
    fixtures={'origin':'actual HTTP serialized endpoints over temporary PostgreSQL + Fakeprovider; synthetic identities; no secrets/live traffic'}
    try:
        _,buyer_token=await identity(client,migrated_url,buyer)
        plans=(await (await client.get(PLANS_PATH+'?payment_contract=2',headers=hdr(buyer_token))).json())
        q,p,h=await invoice(client,buyer_token,plans['plans'][0])
        assert q['amount']['amount_minor']==1000 and provider.create_calls[-1]['amount']==10
        fixtures['subscription']={'plans_envelope':plans,'quote_envelope':q,'created_envelope':p}
        _,token=await identity(client,migrated_url,other);e=await paid(migrated_url,other,days=1.5)
        c=await asyncpg.connect(migrated_url)
        try:await c.execute('UPDATE entitlements SET source_plan=$2::jsonb WHERE id=$1',e['id'],json.dumps({'plan_id':'terlimo-30d','duration_code':'days:30','title':'30 дней'}))
        finally:await c.close()
        plans=(await (await client.get(PLANS_PATH+'?payment_contract=2',headers=hdr(token))).json());plan=plans['plans'][-1]
        q,p,h=await invoice(client,token,plan)
        assert q['amount']['amount_minor']==300 and 'extra_price_basis' not in q['product']
        assert provider.create_calls[-1]['amount']==Decimal('3.00')
        mismatch=await confirm(client,migrated_url,p,3.33)
        assert mismatch['result']=='amount_mismatch'
        c=await asyncpg.connect(migrated_url)
        try:
            await Database._init_connection(c)
            row=await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',uuid.UUID(p['payment_id']));assert row['amount']==Decimal('3.00')
            assert row['status']=='pending'
            # Same immutable amount/currency guard in provider-status reconciliation.
            provider.status_view={'id':row['provider_payment_id'],'status':'CONFIRMED','amount':3.33,'currency':'RUB'}
            r=await reconcile_payments(c,s,provider);assert r['applied']==0
            assert await c.fetchval('SELECT revision FROM entitlements WHERE id=$1',e['id'])==7
            provider.status_view=None
        finally:await c.close()
        assert (await confirm(client,migrated_url,p,3.00))['result']=='succeeded'
        assert (await confirm(client,migrated_url,p,3.00))['result']=='duplicate'
        paid_view=await (await client.get(PAYMENTS_PATH+'/'+p['payment_id']+'?payment_contract=2',headers=hdr(token))).json()
        assert paid_view['credit_state']=='applied' and paid_view['credited_product']['device_limit']==3
        fixtures['addon']={'plans_envelope':plans,'quote_envelope':q,'created_envelope':p,'paid_envelope':paid_view}
        fixtures['callback_amount_mismatch']=mismatch
        c=await asyncpg.connect(migrated_url)
        try:
            slot=await c.fetchval('SELECT id FROM paid_extra_slots WHERE source_order_id=$1',uuid.UUID(p['payment_id']))
            await c.execute('UPDATE paid_extra_slots SET expires_at=$2 WHERE id=$1',slot,e['ends_at']-timedelta(hours=12))
            await c.execute('INSERT INTO paid_extra_slots(entitlement_id,expires_at) VALUES ($1,$2)',e['id'],e['ends_at'])
        finally:await c.close()
        plans=(await (await client.get(PLANS_PATH+'?payment_contract=2',headers=hdr(token))).json());plan=plans['plans'][0]
        q,p,h=await invoice(client,token,plan,selected=[str(slot)])
        assert q['amount']['amount_minor']==30000 and q['product']['extra_amount_minor']==10000
        assert len(q['product']['extra_slots'])==2 and q['product']['renew_extra_slot_ids']==[str(slot)]
        assert provider.create_calls[-1]['amount']==Decimal('300.00')
        c=await asyncpg.connect(migrated_url)
        try:
            await Database._init_connection(c)
            row=await c.fetchrow('SELECT * FROM payment_orders WHERE id=$1',uuid.UUID(p['payment_id']))
            provider.status_view={'id':row['provider_payment_id'],'status':'CONFIRMED','amount':300.00,'currency':'RUB'}
            await reconcile_payments(c,s,provider)
            assert await c.fetchval('SELECT amount FROM payment_orders WHERE id=$1',row['id'])==Decimal('300.00')
            provider.status_view=None
        finally:await c.close()
        view=await (await client.get(PAYMENTS_PATH+'/'+p['payment_id']+'?payment_contract=2',headers=hdr(token))).json()
        assert view['credit_state']=='applied' and view['credited_product']['device_limit']==3 and view['credited_product']['current_device_limit']==4, {'quote_product':q['product'],'order_product':view['product'],'receipt':view['credited_product']}
        fixtures['selected_renewal']={'plans_envelope':plans,'quote_envelope':q,'created_envelope':p,'paid_envelope':view}
        _,third_token=await identity(client,migrated_url,third);target=await paid(migrated_url,third,days=1.5)
        plans=(await (await client.get(PLANS_PATH+'?payment_contract=2',headers=hdr(third_token))).json())
        q,p,h=await invoice(client,third_token,plans['plans'][-1])
        c=await asyncpg.connect(migrated_url)
        try:await c.execute("UPDATE entitlements SET ends_at=now()-interval '1 second' WHERE id=$1",target['id'])
        finally:await c.close()
        await confirm(client,migrated_url,p,3.00)
        view=await (await client.get(PAYMENTS_PATH+'/'+p['payment_id']+'?payment_contract=2',headers=hdr(third_token))).json()
        assert view['payment_status']=='paid' and view['credit_state']=='needs_review' and view['credited_product'] is None
        fixtures['paid_needs_review']={'quote_envelope':q,'created_envelope':p,'status_envelope':view}
        rejected=await client.post(QUOTES_PATH+'?payment_contract=2',headers=hdr(third_token),json={'plan_id':q['product']['plan_id'],'duration_code':q['duration_code'],'method':'sbp'})
        assert rejected.status==409
        fixtures['expired_target_quote_error']={'http_status':rejected.status,'body':await rejected.json()}
        assert _exact_amount(3.341) is None and _exact_amount(True) is None
        output=Path(__file__).resolve().parents[2]/'wire-fixtures-v2.safe.json'
        output.write_text(json.dumps(fixtures,ensure_ascii=False,indent=2)+'\n');output.chmod(0o600)
    finally:await d.close();await client.close()


def test_public_product_omits_internal_basis_without_mutating_snapshot():
    from terlimo_backend.payment_products import public_product
    snapshot={'kind':'device_addon','device_delta':1,'owner_account_id':'synthetic','extra_price_basis':{'period_months':3,'remaining_microseconds':123},'renew_extra_slot_ids':[]}
    assert public_product(snapshot)=={'kind':'device_addon','device_delta':1,'renew_extra_slot_ids':[]}
    assert snapshot['extra_price_basis']['remaining_microseconds']==123 and snapshot['owner_account_id']=='synthetic'

async def test_paid_capacity_expiry_policy_three_bindings(migrated_url,settings_factory,monkeypatch):
    from pathlib import Path
    from terlimo_backend.db import Database
    from terlimo_backend.payment_products import binding_paid_capacity,cap_existing_extra_grants
    from terlimo_backend.gateway_control import ensure_grant,GatewayControlHandlers
    from terlimo_backend.session_auth import authenticate_session
    from terlimo_backend.mobile_catalog import CatalogService,SUBJECT_BINDING
    from terlimo_backend.auth_api import ApiError,_error_response
    owner=uuid.uuid4();client,s,d=await _app(settings_factory,migrated_url,provider=FakePlategaProvider())
    try:
        identities=[await identity(client,migrated_url,a) for a in (owner,uuid.uuid4(),uuid.uuid4())]
        e=await paid(migrated_url,owner,days=30);c=await asyncpg.connect(migrated_url)
        try:
            await Database._init_connection(c)
            now=datetime.now(UTC);deadline=now+timedelta(seconds=60);bindings=[]
            for i,(iid,_token) in enumerate(identities):
                bindings.append(await c.fetchval("UPDATE account_bindings SET account_id=$2,bound_at=$3 WHERE installation_id=$1 RETURNING id",iid,owner,now-timedelta(days=3-i)))
                await c.execute('UPDATE sessions SET account_id=$2 WHERE installation_id=$1',iid,owner)
            slot=await c.fetchval('INSERT INTO paid_extra_slots(entitlement_id,expires_at) VALUES ($1,$2) RETURNING id',e['id'],deadline)
            gateway=await c.fetchval("INSERT INTO gateways(gateway_key,environment,endpoints) VALUES ($1,'test',$2::jsonb) RETURNING id",'capacity-'+uuid.uuid4().hex,{'node_id':'synthetic-node','target_workers':1})
            for bid in bindings:
                await c.execute("INSERT INTO grants(binding_id,gateway_id,not_after,state,desired_generation,applied_generation,lease_seq,gateway_credential) VALUES ($1,$2,$3,'applied',1,1,1,'synthetic-offline')",bid,gateway,now+timedelta(seconds=900))
            before=[await binding_paid_capacity(c,e,bid,now) for bid in bindings]
            assert [x[0] for x in before]==[True,True,True] and before[2][1]==deadline
            assert await cap_existing_extra_grants(c,max_lease_seconds=900)==1
            by_binding={g['binding_id']:g for g in await c.fetch('SELECT * FROM grants WHERE gateway_id=$1',gateway)}
            assert by_binding[bindings[2]]['not_after']==deadline and by_binding[bindings[2]]['desired_generation']==2
            assert all(by_binding[b]['desired_generation']==1 and by_binding[b]['not_after']==now+timedelta(seconds=900) for b in bindings[:2])
            assert [(await binding_paid_capacity(c,e,b,deadline))[0] for b in bindings]==[True,True,False]
            generation=by_binding[bindings[2]]['desired_generation']
            assert await ensure_grant(c,binding_id=bindings[2],gateway_id=gateway,entitlement_id=e['id'],max_lease_seconds=900,now=deadline)=='device_limit_reached'
            assert await c.fetchval('SELECT desired_generation FROM grants WHERE binding_id=$1 AND gateway_id=$2',bindings[2],gateway)==generation
            await c.execute('UPDATE paid_extra_slots SET expires_at=$2 WHERE id=$1',slot,now-timedelta(seconds=1))
            context=await authenticate_session(c,s,identities[2][1]);assert not context.data_access_allowed and context.binding_status=='active'
            assert context.account_state=='ACTIVE_PAID' and context.metadata['device_capacity_exceeded']
            with pytest.raises(ApiError) as caught:
                CatalogService(s,d)._require_data_subject(context,'f'*32,(SUBJECT_BINDING,bindings[2]))
            assert caught.value.code=='DEVICE_LIMIT_REACHED' and caught.value.http==409
            error=json.loads(_error_response('f'*32,caught.value).body)
            assert error['details']=={'slots_used':3,'device_limit':2}
            from terlimo_backend.mobile_account import _sync_parent_progress
            with pytest.raises(ApiError) as progress_error:
                await _sync_parent_progress(c,{},'f'*32,context)
            assert progress_error.value.code=='DEVICE_LIMIT_REACHED'
            handler=GatewayControlHandlers(s,client_factory=lambda *a: (_ for _ in ()).throw(AssertionError('node RPC forbidden')))
            async def loaded(*args):return by_binding[bindings[2]]
            monkeypatch.setattr(handler,'_load',loaded)
            operation={'payload':{'grant_id':str(by_binding[bindings[2]]['opaque_id']),'generation':str(generation)}}
            assert await handler.apply_grant(c,operation)==('failed','DEVICE_LIMIT_REACHED')
            await c.execute('INSERT INTO paid_extra_slots(entitlement_id,expires_at) VALUES ($1,$2)',e['id'],e['ends_at'])
            assert (await authenticate_session(c,s,identities[2][1])).data_access_allowed
            assert await ensure_grant(c,binding_id=bindings[2],gateway_id=gateway,entitlement_id=e['id'],max_lease_seconds=900)=='enqueued'
            assert await c.fetchval('SELECT status FROM account_bindings WHERE id=$1',bindings[2])=='active'
            assert await c.fetchval('SELECT generation FROM account_bindings WHERE id=$1',bindings[2])==1
            assert await c.fetchval('SELECT ends_at FROM entitlements WHERE id=$1',e['id'])==e['ends_at']
            await c.execute('UPDATE account_bindings SET bound_at=$2 WHERE account_id=$1',owner,now-timedelta(days=1))
            await c.execute('UPDATE paid_extra_slots SET expires_at=$2 WHERE entitlement_id=$1',e['id'],now-timedelta(seconds=1))
            assert [(await binding_paid_capacity(c,e,b,now))[0] for b in sorted(bindings)]==[True,True,False]
            receipt={'scope':'temporary PostgreSQL + offline no RPC; only established expiry gap','policy':'first N active bindings by bound_at,id; N=2+unexpired extras','before_expiry_eligible':[True,True,True],'at_expiry_eligible':[True,True,False],'maintenance_existing_excess_not_after':'old paid extra deadline; retained grants unchanged','queued_apply_after_expiry':'failed DEVICE_LIMIT_REACHED before RPC','restored_capacity':'same active binding/generation1; normal refresh enqueued','base_period_unchanged':True,'tie_break':'id verified','node_runtime_PASS':False,'wire_error':{'http_status':409,'body':error}}
            output=Path(__file__).resolve().parents[2]/'capacity-expiry-receipt.safe.json';output.write_text(json.dumps(receipt,ensure_ascii=False,indent=2)+'\n');output.chmod(0o600)
        finally:await c.close()
    finally:await d.close();await client.close()


@pytest.mark.asyncio
async def test_whole_days_snapshot_and_zero_offer_boundary(settings_factory,monkeypatch):
    """Offline quote arithmetic plus real HTTP serialization; no PG/provider/node."""
    from contextlib import asynccontextmanager
    from types import SimpleNamespace
    from copy import deepcopy
    from aiohttp import web
    from aiohttp.test_utils import TestClient,TestServer
    from dataclasses import replace
    from terlimo_backend import s5_payments as surface
    from terlimo_backend.payment_products import quote_product,public_product,product_of
    from terlimo_backend.auth_api import rfc3339
    now=datetime(2026,10,2,15,0,tzinfo=UTC);owner=uuid.uuid4();slot=uuid.uuid4()
    target={'id':uuid.uuid4(),'starts_at':now-timedelta(days=20),'ends_at':now+timedelta(days=10,hours=5),'source_plan':{'duration_code':'days:30'}}
    class Connection:
        existing=None
        async def fetchrow(self,sql,*args):
            if 'FROM entitlements' in sql:return target
            if 'FROM s5_payment_quotes' in sql:return self.existing
            raise AssertionError('No writes or provider path allowed')
        async def fetch(self,sql,*args):
            assert 'paid_extra_slots' in sql
            return [{'id':slot,'expires_at':target['ends_at']-timedelta(days=1,hours=5)}]
    c=Connection();settings=settings_factory('postgresql://offline/test',payment_price_rub_1=200,payment_price_rub_3=480,payment_price_rub_6=840)
    amount,_,product=await quote_product(c,settings,owner,addon=True,selected=[],months=0,base_amount_minor=0,now=now)
    assert amount==3300 and product['valid_until']==rfc3339(target['ends_at'])
    basis=product['extra_price_basis'];assert basis['remaining_day_policy']=='floor_full_86400_seconds' and basis['remaining_full_days']==10
    assert basis['rounding']=='nearest_rub_half_up'
    assert basis['charged_days']==10 and basis['period_cap_days']==30 and basis['quoted_amount_minor']==3300
    assert 'extra_price_basis' not in public_product(product)
    total,_,renew=await quote_product(c,settings,owner,addon=False,selected=[str(slot)],months=3,base_amount_minor=48000,now=now)
    assert total==48000+30000+300 and renew['extra_price_basis']['selected_gap_basis'][0]['remaining_full_days']==1
    legacy_product=deepcopy(product)
    legacy_product['extra_amount_minor']=3334
    legacy_product['extra_price_basis']['rounding']='ceil_minor'
    legacy_product['extra_price_basis']['quoted_amount_minor']=3334
    frozen=deepcopy({'product':json.dumps(legacy_product),'amount_minor':3334})
    target['ends_at']=datetime.now(UTC)+timedelta(hours=5)
    zero,_,zero_product=await quote_product(c,settings,owner,addon=True,selected=[],months=0,base_amount_minor=0)
    assert zero==0 and zero_product['extra_price_basis']['remaining_full_days']==0
    assert product_of(frozen)==legacy_product and frozen['amount_minor']==3334
    control=replace(settings,s5_control_account_id=str(owner),s5_control_price_rub_extra=5)
    assert (await quote_product(c,control,owner,addon=True,selected=[],months=0,base_amount_minor=0))[0]==500
    class Database:
        @asynccontextmanager
        async def acquire(self):yield c
    async def auth(*args):return SimpleNamespace(account_id=owner,installation_id=uuid.uuid4())
    monkeypatch.setattr(surface,'authenticate_session',auth)
    app=web.Application();surface.register_s5_payment_routes(app,settings,Database())
    client=TestClient(TestServer(app));await client.start_server()
    try:
        plans=await client.get(PLANS_PATH+'?payment_contract=2',headers={'Authorization':'Bearer offline'})
        assert plans.status==200
        values=await plans.json();assert len(values['plans'])==3 and all(x['plan_id']!='terlimo-extra-device' for x in values['plans'])
        body={'plan_id':'terlimo-extra-device','duration_code':'until:'+rfc3339(target['ends_at']),'method':'sbp'}
        headers={'Authorization':'Bearer offline','Idempotency-Key':'offline-whole-days-boundary'}
        error=await client.post(QUOTES_PATH+'?payment_contract=2',headers=headers,json=body)
        assert error.status==409 and (await error.json())['code']=='PAYMENT_STATE_INVALID'
        # Existing quote remains frozen and replayable even when a new offer is now unpayable.
        import hashlib
        c.existing={'id':uuid.uuid4(),'amount_minor':3334,'currency':'RUB','duration_code':body['duration_code'],'method':'sbp','expires_at':datetime.now(UTC)+timedelta(minutes=5),'product':json.dumps(legacy_product),'request_digest':hashlib.sha256(json.dumps(body,sort_keys=True,separators=(',',':')).encode()).hexdigest()}
        replay=await client.post(QUOTES_PATH+'?payment_contract=2',headers=headers,json=body)
        assert replay.status==200
        replayed=await replay.json();assert replayed['amount']['amount_minor']==3334 and replayed['product']==public_product(legacy_product)
    finally:await client.close()
