"""One regression slice: old schema -> paid base -> real fake-provider credit/expiry."""
import hashlib
import json
import shutil
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path

import asyncpg
import pytest
from terlimo_backend.migrations import runner
from terlimo_backend.db import Database
from terlimo_backend.payment_products import paid_limit,refresh_expired_limits,binding_paid_capacity,cap_existing_extra_grants
from terlimo_backend.gateway_control import ensure_grant
from terlimo_backend.mobile_account import effective_device_limit
from terlimo_backend.session_auth import authenticate_session
from terlimo_backend.maintenance import sweep_once
from test_payment_addon_slice import identity,invoice,confirm,hdr
from test_s4_payments import _app,FakePlategaProvider


@pytest.mark.asyncio
async def test_legacy_base_migration_credit_expiry_and_renewal(database_url,settings_factory,tmp_path):
    old=tmp_path/'old';old.mkdir()
    for f in runner.VERSIONS_DIR.glob('*.sql'):
        if f.name[:4]<'0036':shutil.copy2(f,old/f.name)
    c=await asyncpg.connect(database_url)
    try:
        await runner.apply_migrations(c,old)
        now=datetime.now(UTC);original={};owners={}
        for label,limit,kind,finite in [('zero',0,'paid',True),('legacy',1,'paid',True),('base2',2,'paid',True),('extra',4,'paid',True),('null',None,'paid',True),('indefinite',1,'paid',False),('trial',1,'trial',True)]:
            owner=await c.fetchval("INSERT INTO accounts(status) VALUES('verified') RETURNING id");owners[label]=owner
            e=await c.fetchrow("INSERT INTO entitlements(account_id,kind,status,starts_at,ends_at,device_limit,revision,source_plan) VALUES($1,$2,'active',$3,$4,$5,7,$6::jsonb) RETURNING *",owner,kind,now-timedelta(days=10),now+timedelta(days=30) if finite else None,limit,json.dumps({'duration_code':'days:30'}))
            original[label]=dict(e)
        for f in runner.VERSIONS_DIR.glob('0036*.sql'):shutil.copy2(f,old/f.name)
        assert await runner.apply_migrations(c,old)==['0036_payment_products']
        checksum36=await c.fetchval("SELECT checksum FROM schema_migrations WHERE id='0036_payment_products'")
        bad=await c.fetchval("INSERT INTO entitlements(account_id,kind,status,ends_at,device_limit) VALUES($1,'paid','active',$2,-1) RETURNING id",owners['legacy'],now+timedelta(days=1))
        with pytest.raises(asyncpg.PostgresError,match='invalid legacy'):
            await runner.apply_migrations(c)
        assert await c.fetchval("SELECT count(*) FROM schema_migrations WHERE id='0037_paid_base_device_limit'")==0
        await c.execute('DELETE FROM entitlements WHERE id=$1',bad)
        assert await runner.apply_migrations(c)==['0037_paid_base_device_limit']
        assert await c.fetchval("SELECT checksum FROM schema_migrations WHERE id='0036_payment_products'")==checksum36
        # Old ledger cannot be dropped out of order while durable base is present.
        with pytest.raises(asyncpg.PostgresError,match='paid base migration'):
            await runner.rollback_migration(c,'0036_payment_products')
        # Safe down/reapply before commercial writes; existing rows unchanged.
        await runner.rollback_migration(c,'0037_paid_base_device_limit')
        assert await runner.apply_migrations(c)==['0037_paid_base_device_limit']
        expected={'zero':0,'legacy':1,'base2':2,'extra':2,'null':2,'indefinite':2,'trial':2}
        for label,oldrow in original.items():
            row=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',oldrow['id'])
            assert row['paid_base_device_limit']==expected[label]
            assert all(row[k]==v for k,v in oldrow.items()),label
            want={'zero':0,'legacy':1,'base2':2,'extra':4,'null':2,'indefinite':1,'trial':1}[label]
            assert await paid_limit(c,row)==want
        with pytest.raises(KeyError):
            await paid_limit(c,original['legacy']) # Missing base projection must fail, not become2.
        settings=settings_factory(database_url,platega_enabled=False)
        for _ in range(2):await sweep_once(c,settings)
        for label,oldrow in original.items():
            row=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',oldrow['id'])
            assert row['device_limit']==oldrow['device_limit'] and row['revision']==7,label
        assert await runner.pending_versions(c)==[]
    finally:await c.close()
    provider=FakePlategaProvider();client,settings,database=await _app(settings_factory,database_url,provider=provider)
    c=await asyncpg.connect(database_url)
    try:
        await Database._init_connection(c)
        owner=owners['legacy'];e=original['legacy'];identities=[await identity(client,database_url,uuid.uuid4()) for _ in range(2)]
        bindings=[]
        for i,(iid,token) in enumerate(identities):
            bindings.append(await c.fetchval('UPDATE account_bindings SET account_id=$2,bound_at=$3 WHERE installation_id=$1 RETURNING id',iid,owner,now-timedelta(days=3-i)))
            await c.execute('UPDATE sessions SET account_id=$2 WHERE installation_id=$1',iid,owner)
        token=identities[0][1]
        me=await client.get('/api/mobile/v1/me',headers=hdr(token));assert me.status==200
        mebody=await me.json();assert mebody['entitlement']['effective_device_limit']==1
        assert await effective_device_limit(c,owner)==1
        assert (await authenticate_session(c,settings,identities[1][1])).data_access_allowed is False
        gateway=await c.fetchval("INSERT INTO gateways(gateway_key,environment,endpoints) VALUES($1,'test',$2::jsonb) RETURNING id",'legacy-base-'+uuid.uuid4().hex,{'node_id':'offline-only','target_workers':1})
        assert await ensure_grant(c,binding_id=bindings[1],gateway_id=gateway,entitlement_id=e['id'],max_lease_seconds=900)=='device_limit_reached'
        assert await c.fetchval('SELECT count(*) FROM grants WHERE binding_id=$1',bindings[1])==0
        plans=(await (await client.get('/api/mobile/v1/plans?payment_contract=2',headers=hdr(token))).json())['plans']
        addon=next(p for p in plans if p['plan_id']=='terlimo-extra-device');assert addon['product']['device_limit']==2 and addon['product']['device_delta']==1
        q,payment,h=await invoice(client,token,addon)
        amount=q['amount']['amount_minor']/100
        assert q['product']['device_limit']==2
        assert (await confirm(client,database_url,payment,amount))['result']=='succeeded'
        paidrow=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',e['id'])
        assert paidrow['device_limit']==2 and paidrow['paid_base_device_limit']==1 and paidrow['revision']==8 and paidrow['ends_at']==e['ends_at']
        assert (await confirm(client,database_url,payment,amount))['result']=='duplicate'
        assert await c.fetchval('SELECT revision FROM entitlements WHERE id=$1',e['id'])==8
        slot=await c.fetchrow('SELECT * FROM paid_extra_slots WHERE entitlement_id=$1',e['id'])
        assert slot['expires_at']==e['ends_at']
        # Real base-aware SQL rank/ensure projection: second binding gets only extra deadline.
        deadline=datetime.now(UTC)+timedelta(seconds=60)
        await c.execute('UPDATE paid_extra_slots SET expires_at=$2 WHERE id=$1',slot['id'],deadline)
        assert (await binding_paid_capacity(c,paidrow,bindings[0]))[1]==e['ends_at']
        assert (await binding_paid_capacity(c,paidrow,bindings[1]))[1]==deadline
        await c.execute("INSERT INTO grants(binding_id,gateway_id,not_after,state,desired_generation,applied_generation,lease_seq,gateway_credential) VALUES($1,$2,$3,'applied',1,1,1,'offline-only')",bindings[1],gateway,datetime.now(UTC)+timedelta(seconds=900))
        assert await cap_existing_extra_grants(c,max_lease_seconds=900)==1
        assert await c.fetchval('SELECT not_after FROM grants WHERE binding_id=$1',bindings[1])==deadline
        await c.execute('UPDATE paid_extra_slots SET expires_at=$2 WHERE id=$1',slot['id'],datetime.now(UTC)-timedelta(seconds=1))
        assert await refresh_expired_limits(c)==1 and await refresh_expired_limits(c)==0
        expired=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',e['id'])
        assert expired['paid_base_device_limit']==1 and expired['device_limit']==1 and expired['ends_at']==e['ends_at']
        assert await effective_device_limit(c,owner)==1
        assert await ensure_grant(c,binding_id=bindings[1],gateway_id=gateway,entitlement_id=e['id'],max_lease_seconds=900)=='device_limit_reached'
        # Normal confirmed subscription credit resets base atomically; replay cannot reset twice.
        plans=(await (await client.get('/api/mobile/v1/plans?payment_contract=2',headers=hdr(token))).json())['plans']
        plan=next(p for p in plans if p['plan_id']=='terlimo-30d')
        q,payment,h=await invoice(client,token,plan,selected=[])
        oldrev=expired['revision']
        assert (await confirm(client,database_url,payment,q['amount']['amount_minor']/100))['result']=='succeeded'
        renewed=await c.fetchrow('SELECT * FROM entitlements WHERE id=$1',e['id'])
        assert renewed['paid_base_device_limit']==2 and renewed['device_limit']==2 and renewed['revision']==oldrev+1
        assert renewed['ends_at']==e['ends_at']+timedelta(days=30)
        assert (await confirm(client,database_url,payment,q['amount']['amount_minor']/100))['result']=='duplicate'
        assert await c.fetchval('SELECT revision FROM entitlements WHERE id=$1',e['id'])==oldrev+1
        assert (await binding_paid_capacity(c,renewed,bindings[1]))[0] is True
        for version in ['0037_paid_base_device_limit','0036_payment_products']:
            with pytest.raises(asyncpg.PostgresError,match='commercial activity'):
                await runner.rollback_migration(c,version)
        assert await c.fetchval("SELECT checksum FROM schema_migrations WHERE id='0036_payment_products'")==checksum36
        receipt={'isolated_PG':True,'legacy_backfill_bases':expected,'maintenance_twice_no_old_limit_revision_change':True,'read_me_effective_limit':1,'second_binding_before_addon':'device_limit_reached; no grant','addon_once':'1->2, base1, same end, revision+1; duplicate unchanged','expiry':'2->1, second grant denied; base1 persisted','normal_confirmed_renewal':'base2/current2 atomically; duplicate unchanged','NULL_zero_invalid_guard':'PASS','down_before_money':'0037 down/reapply PASS','down_after_money':'0037 and0036 guards reject;0036 credited_product guard retained even for v1 receipts','0036_checksum_unchanged':True,'node_provider_network':False}
        out=Path(__file__).resolve().parents[3]/'regression.safe.json';out.write_text(json.dumps(receipt,indent=2)+'\n');out.chmod(0o600)
    finally:await c.close();await database.close();await client.close()
