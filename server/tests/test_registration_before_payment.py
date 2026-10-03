"""Addition24 only: trusted Telegram before mobile purchase, late registration stays open."""
import uuid
import pytest
from test_s4_payments import (_app,_session_token,_installation_id,_auth,FakePlategaProvider,
    BOT_KEY,_create_order)
from test_auth_flow import _session,_challenge
from test_step036_onboarding_hour_storage import _connect,_seed_installation
from terlimo_backend.telegram_binding import LINK_PATH,CONFIRM_PATH,BOT_KEY_HEADER
from terlimo_backend.s5_payments import PLANS_PATH,QUOTES_PATH,PAYMENTS_PATH
from terlimo_backend.payments import QUOTE_PATH,ORDERS_PATH

ME='/api/mobile/v1/me'
def hdr(token):return {**_auth(token),'Idempotency-Key':'reg-first-'+uuid.uuid4().hex}
async def refreshed(client,pop):
    r=await _session(client,pop,await _challenge(client,pop.key,'session'),['session:read','session:write'],'refresh-'+uuid.uuid4().hex)
    assert r.status==200,await r.text()
    return (await r.json())['session']['session_id']

@pytest.mark.parametrize('existing',[False,True])
async def test_late_new_and_existing_telegram_unlocks_purchase_not_hour_or_trial(migrated_url,settings_factory,existing):
    provider=FakePlategaProvider();client,s,db=await _app(settings_factory,migrated_url,provider=provider)
    c=await _connect(migrated_url)
    try:
        pop,token=await _session_token(client);iid=await _installation_id(migrated_url,pop.key.fingerprint)
        await c.execute("INSERT INTO entitlements(installation_id,kind,status,starts_at,ends_at) VALUES($1,'onboarding_hour','expired',now()-interval '2 hours',now()-interval '1 hour')",iid)
        hour=await c.fetchrow("SELECT * FROM entitlements WHERE installation_id=$1 AND kind='onboarding_hour'",iid)
        me=await (await client.get(ME,headers=_auth(token))).json()
        assert me['account_state']=='UNLINKED' and not me['telegram_linked'] and 'account_ref' not in me
        assert me['registration']['state']=='none' and me['registration']['purchase_available'] is False
        assert me['onboarding']['state']=='expired' and me['grant_resolution']['control_available'] is True
        assert me['grant_resolution']['data_access']=='none'
        plan=(await (await client.get(PLANS_PATH)).json())['plans'][0]
        qbody={'plan_id':plan['plan_id'],'duration_code':plan['duration_code'],'method':'sbp'}
        for version in ('','?payment_contract=2'):
            r=await client.post(QUOTES_PATH+version,headers=hdr(token),json=qbody)
            assert r.status==403 and (await r.json())['code']=='ACCESS_DENIED'
            r=await client.post(PAYMENTS_PATH+version,headers=hdr(token),json={'quote_id':str(uuid.uuid4())})
            assert r.status==403 and (await r.json())['code']=='ACCESS_DENIED'
        # Legacy mobile endpoints cannot bypass the account-bound release policy.
        r=await client.post(QUOTE_PATH,headers=_auth(token),json={'months':1});assert r.status==403
        r=await _create_order(client,token,1);assert r.status==403
        assert provider.create_calls==[] and await c.fetchval('SELECT count(*) FROM payment_orders')==0
        tg=700000000+uuid.uuid4().int%100000000
        acc=await c.fetchval("INSERT INTO accounts(status,telegram_id) VALUES('verified',$1) RETURNING id",tg) if existing else None
        link=await client.post(LINK_PATH,headers=_auth(token),json={});assert link.status==200
        linktoken=(await link.json())['registration']['token']
        pending=await (await client.get(ME,headers=_auth(token))).json()
        assert pending['registration']['state']=='pending' and not pending['registration']['purchase_available']
        unauthorized=await client.post(CONFIRM_PATH,json={'token':linktoken,'telegram_id':tg});assert unauthorized.status==403
        confirm=await client.post(CONFIRM_PATH,headers={BOT_KEY_HEADER:BOT_KEY},json={'token':linktoken,'telegram_id':tg})
        assert confirm.status==200,await confirm.text();body=await confirm.json()
        assert body['trial_available'] is False and body['trial_reason']=='hour_expired' and body['purchase_available'] is True
        if existing:
            assert await c.fetchval('SELECT id FROM accounts WHERE telegram_id=$1',tg)==acc
            repeated=await client.post(CONFIRM_PATH,headers={BOT_KEY_HEADER:BOT_KEY},json={'token':linktoken,'telegram_id':tg})
            assert repeated.status==200 and (await repeated.json())['purchase_available'] is True
            login=await client.post(LINK_PATH,headers=_auth(token),json={})
            assert login.status==200 and (await login.json())['registration']['state']=='registered'
        # Registration projection may be confirmed while the old bearer is still UNLINKED.
        stale=await (await client.get(ME,headers=_auth(token))).json()
        assert not stale['registration']['purchase_available']
        fresh=await refreshed(client,pop)
        me=await (await client.get(ME,headers=_auth(fresh))).json()
        assert me['account_ref'] and me['telegram_linked'] and me['binding_status']=='active'
        assert me['registration']['state']=='registered' and me['registration']['purchase_available'] is True
        assert me['registration']['trial_available'] is False and me['onboarding']['state']=='expired'
        assert await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',hour['id'])==hour
        assert await c.fetchval("SELECT count(*) FROM entitlements WHERE kind IN ('paid','trial')")==0
        # The ordinary linked quote path works in both existing DTO versions; provider kept fake.
        for version in ('','?payment_contract=2'):
            r=await client.post(QUOTES_PATH+version,headers=hdr(fresh),json=qbody);assert r.status==200,await r.text()
            quote=await r.json();assert quote['amount']['amount_minor']==20000
            h=hdr(fresh);r=await client.post(PAYMENTS_PATH+version,headers=h,json={'quote_id':quote['quote_id']})
            assert r.status==200,await r.text();p=await r.json()
            replay=await client.post(PAYMENTS_PATH+version,headers=h,json={'quote_id':quote['quote_id']})
            assert replay.status==200 and (await replay.json())['payment_id']==p['payment_id']
        assert len(provider.create_calls)==2
    finally:await c.close();await db.close();await client.close()

async def test_capacity_proof_does_not_advertise_purchase_without_binding(migrated_url,settings_factory):
    provider=FakePlategaProvider();client,s,db=await _app(settings_factory,migrated_url,provider=provider)
    c=await _connect(migrated_url)
    try:
        pop,token=await _session_token(client);tg=800000000+uuid.uuid4().int%100000000
        acc=await c.fetchval("INSERT INTO accounts(status,telegram_id) VALUES('verified',$1) RETURNING id",tg)
        for _ in range(2):
            iid=await _seed_installation(migrated_url)
            await c.execute("INSERT INTO account_bindings(account_id,installation_id,status) VALUES($1,$2,'active')",acc,iid)
        link=(await (await client.post(LINK_PATH,headers=_auth(token),json={})).json())['registration']['token']
        r=await client.post(CONFIRM_PATH,headers={BOT_KEY_HEADER:BOT_KEY},json={'token':link,'telegram_id':tg})
        assert r.status==409 and (await r.json())['code']=='DEVICE_LIMIT_REACHED'
        me=await (await client.get(ME,headers=_auth(token))).json()
        assert me['registration']['state']=='registered' and me['registration']['purchase_available'] is False
        assert me['binding_status']=='none'
        r=await client.post(QUOTES_PATH,headers=hdr(token),json={'plan_id':'terlimo-30d','duration_code':'days:30','method':'sbp'})
        assert r.status==403 and provider.create_calls==[]
    finally:await c.close();await db.close();await client.close()

async def test_existing_managed_login_and_unverified_or_revoked_binding(migrated_url,settings_factory):
    from test_payment_addon_slice import identity
    provider=FakePlategaProvider();client,s,db=await _app(settings_factory,migrated_url,provider=provider)
    c=await _connect(migrated_url)
    try:
        acc=uuid.uuid4();iid,token=await identity(client,migrated_url,acc)
        me=await (await client.get(ME,headers=_auth(token))).json()
        assert me['telegram_linked'] and me['account_ref']==str(acc) and me['binding_status']=='active'
        assert me['registration']['state']=='none' and me['registration']['purchase_available'] is True
        body={'plan_id':'terlimo-30d','duration_code':'days:30','method':'sbp'}
        r=await client.post(QUOTES_PATH,headers=hdr(token),json=body);assert r.status==200
        await c.execute("UPDATE accounts SET status='unlinked' WHERE id=$1",acc)
        me=await (await client.get(ME,headers=_auth(token))).json();assert not me['registration']['purchase_available']
        r=await client.post(QUOTES_PATH,headers=hdr(token),json=body);assert r.status==403
        await c.execute("UPDATE accounts SET status='verified' WHERE id=$1",acc)
        await c.execute("UPDATE account_bindings SET status='revoked' WHERE installation_id=$1",iid)
        me=await (await client.get(ME,headers=_auth(token))).json();assert not me['registration']['purchase_available']
        r=await client.post(QUOTES_PATH,headers=hdr(token),json=body);assert r.status==403
        assert provider.create_calls==[]
    finally:await c.close();await db.close();await client.close()
