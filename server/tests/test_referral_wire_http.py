"""Actual authenticated HTTP responses versus the published closed native wire.

Uses only the existing isolated PostgreSQL fixture and fake provider. No Telegram
client or provider call is claimed by a server-side confirmation fixture.
"""
import json
import uuid
from pathlib import Path
from test_auth_flow import _challenge, _session
from test_s4_payments import _app, _auth, _session_token, _installation_id
from test_referral_identity import connection, account
from terlimo_backend.referral import stage_history
from terlimo_backend.telegram_binding import confirm_registration

FIXTURES = Path(__file__).resolve().parents[2]/'docs/TERLIMO_IMPLEMENTATION/referral_20261003'


def closed_shape(value, sample):
    if isinstance(sample,dict):
        assert isinstance(value,dict) and set(value)==set(sample)
        for key in sample:closed_shape(value[key],sample[key])
    elif sample is not None:
        assert type(value) is type(sample)


async def test_native_success_closed_wire_and_original_receipt_after_inviter_change(migrated_url,settings_factory):
    client, settings, database = await _app(settings_factory,migrated_url,disable_provider=True)
    c=await connection(migrated_url)
    try:
        inviter=await account(c,801,'Ab12')
        pop,token=await _session_token(client)
        iid=await _installation_id(migrated_url,pop.key.fingerprint)
        headers={**_auth(token),'Idempotency-Key':'candidate-wire-001'}
        response=await client.post('/api/mobile/v1/referral/candidate',headers=headers,json={'code':'Ab12'})
        assert response.status==200,await response.text()
        original=await response.json()
        closed_shape(original,json.loads((FIXTURES/'native-wire-fixtures.json').read_text())['referralPending'])
        await c.execute("UPDATE accounts SET referral_code='Other12' WHERE id=$1",inviter)
        replay=await client.post('/api/mobile/v1/referral/candidate',headers=headers,json={'code':'Ab12'})
        assert replay.status==200 and (await replay.json())['candidate']==original['candidate']
        changed=await client.post('/api/mobile/v1/referral/candidate',headers=headers,json={'code':'!bad'})
        assert changed.status==409 and (await changed.json())['code']=='IDEMPOTENCY_CONFLICT'
        malformed=await client.post('/api/mobile/v1/referral/candidate',headers={**headers,'Content-Type':'application/json'},data='{')
        assert malformed.status==400 and (await malformed.json())['retryable'] is True
        assert await c.fetchval("SELECT count(*) FROM referral_candidates WHERE installation_id=$1",iid)==1
        assert await c.fetchval("SELECT count(*) FROM payment_orders")==0
    finally:
        await c.close();await database.close();await client.close()


async def test_keyed_pending_registered_and_expired_ordinary_registration_wire(migrated_url,settings_factory):
    client,settings,database=await _app(settings_factory,migrated_url,disable_provider=True)
    c=await connection(migrated_url)
    try:
        await account(c,811,'Ab12')
        await stage_history(c,[{'telegram_id':812,'code':None,'referred_by_telegram_id':None,'proven_new':True,'trial_used':False,'first_main_paid':False}],source_sha256='d'*64)
        pop,token=await _session_token(client)
        candidate=await client.post('/api/mobile/v1/referral/candidate',headers={**_auth(token),'Idempotency-Key':'candidate-wire-002'},json={'code':'Ab12'})
        cid=(await candidate.json())['candidate']['id']
        kwargs={'headers':{**_auth(token),'Idempotency-Key':'registration-wire-002'},'json':{'referral_candidate_id':cid}}
        path='/api/mobile/v1/registration/telegram/link'
        pending_response=await client.post(path,**kwargs);assert pending_response.status==200,await pending_response.text()
        pending=await pending_response.json();fixtures=json.loads((FIXTURES/'registration-wire-fixtures.json').read_text())
        closed_shape(pending,fixtures['pending'])
        replay=await client.post(path,**kwargs);assert (await replay.json())['registration']==pending['registration']
        await confirm_registration(c,settings,token=pending['registration']['token'],telegram_id=812,telegram_username=None)
        fresh=await _session(client,pop,await _challenge(client,pop.key,'session'),['session:read','session:write'],'session-wire-registered')
        assert fresh.status==200,await fresh.text()
        bound_token=(await fresh.json())['session']['session_id']
        kwargs['headers']['Authorization']='Bearer '+bound_token
        registered=await client.post(path,**kwargs);assert registered.status==200,await registered.text()
        closed_shape(await registered.json(),fixtures['registered'])
        info=await client.get('/api/mobile/v1/referral',headers=_auth(bound_token));assert info.status==200,await info.text()
        shape=json.loads((FIXTURES/'native-wire-fixtures.json').read_text())['referralInfoFixture']
        # Actual attached attribution has a UUID receipt, while fixture illustrates none/null.
        actual=await info.json();closed_shape(actual,shape)
        assert actual['referral']['attribution']['state']=='attached'
        assert uuid.UUID(actual['referral']['attribution']['receipt_id'])
        ordinary=await client.post(path,headers=_auth(bound_token),json={})
        assert ordinary.status==200
        assert (await ordinary.json())['registration']=={'state':'registered'}
        assert await c.fetchval('SELECT count(*) FROM payment_orders')==0
    finally:
        await c.close();await database.close();await client.close()


def test_service_channel_only_fixed_referral_paths():
    from terlimo_backend.service_relay import service_path_allowed
    assert service_path_allowed('GET','/api/mobile/v1/referral')
    assert service_path_allowed('POST','/api/mobile/v1/referral/candidate')
    assert service_path_allowed('DELETE','/api/mobile/v1/referral/candidate')
    for method,path in [('GET','/api/mobile/v1/referral/candidate'),('POST','/api/mobile/v1/referral'),('PUT','/api/mobile/v1/referral/candidate'),('DELETE','/api/mobile/v1/referral'),('GET','/api/mobile/v1/referral?account_ref=foreign'),('GET','/api/mobile/v1/referral/'),('POST','/api/mobile/v1/referral/../candidate')]:
        assert not service_path_allowed(method,path)
