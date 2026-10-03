"""A1/A2 regressions only: row-lock barriers and pre-epoch legacy readiness."""
import asyncio
import uuid

import pytest

from terlimo_backend.auth_api import ApiError
from terlimo_backend.referral import attach_candidate, initialize_account, stage_history
from terlimo_backend.referral_trusted import trusted_operation
from terlimo_backend.telegram_binding import _account_advisory
from test_referral_trusted import connect, owner, install


class OwnersBarrier:
    def __init__(self):
        self.count=0
        self.ready=asyncio.Event()
    async def arrived(self):
        self.count+=1
        if self.count==2: self.ready.set()
        await asyncio.wait_for(self.ready.wait(),5)


class BarrierConnection:
    """Intercept AFTER the actual SQL has acquired this transaction's own row."""
    def __init__(self,c,barrier,channel):
        self.c=c;self.barrier=barrier;self.channel=channel;self.held=False
    def __getattr__(self,name): return getattr(self.c,name)
    async def fetchval(self,query,*args):
        value=await self.c.fetchval(query,*args)
        if self.channel=='trusted' and not self.held and 'telegram_id=$2' in query and 'FOR ' in query:
            self.held=True;await self.barrier.arrived()
        return value
    async def fetchrow(self,query,*args):
        row=await self.c.fetchrow(query,*args)
        owned = ('telegram_id=$2' in query if self.channel=='trusted'
                 else query.startswith('SELECT * FROM accounts WHERE id=$1 FOR '))
        if not self.held and owned:
            self.held=True;await self.barrier.arrived()
        return row


async def mobile_link(c,a,code):
    iid=await install(c,a)
    cid=await c.fetchval('INSERT INTO referral_candidates(installation_id,code) VALUES($1,$2) RETURNING id',iid,code)
    return await c.fetchrow("INSERT INTO registration_links(token_sha256,installation_id,environment,expires_at,referral_candidate_id,referral_idempotency_key) VALUES($1,$2,'test',now()+interval '1 hour',$3,'mobile-reciprocal-key') RETURNING *",uuid.uuid4().hex,iid,cid)


@pytest.mark.asyncio
@pytest.mark.parametrize('channels',[('trusted','trusted'),('mobile','trusted'),('mobile','mobile')],ids=['trusted_trusted','mobile_trusted','mobile_mobile'])
@pytest.mark.parametrize('pending',[False,True],ids=['ready','pending_existing_code'])
async def test_reciprocal_attach_after_real_owned_row_barrier(migrated_url,channels,pending):
    c1=await connect(migrated_url);c2=await connect(migrated_url)
    try:
        a=await owner(c1,401,'CodeA');b=await owner(c1,402,'CodeB')
        if pending:
            await stage_history(c1,[dict(telegram_id=tg,code=code,referred_by_telegram_id=None,proven_new=False,trial_used=False,first_main_paid=False) for tg,code in [(401,'CodeA'),(402,'CodeB')]],source_sha256='e'*64)
            await c1.execute("UPDATE referral_benefits SET history_state='history_pending'")
        links=[await mobile_link(c1,a,'CodeB'),await mobile_link(c1,b,'CodeA')]
        barrier=OwnersBarrier()
        wrappers=[BarrierConnection(c1,barrier,channels[0]),BarrierConnection(c2,barrier,channels[1])]
        async def attach(c,channel,aid,tg,code,link):
            if channel=='trusted':
                return (await trusted_operation(c,{'operation':'attach','telegram_id':tg,'code':code},'reciprocal-key-001'))['attribution']
            async with c.transaction():
                await _account_advisory(c,aid)
                # Actual mobile confirmation ordering: initializer BEFORE candidate attach.
                await initialize_account(c,aid)
                return await attach_candidate(c,link,aid)
        receipts=await asyncio.wait_for(asyncio.gather(
            attach(wrappers[0],channels[0],a,401,'CodeB',links[0]),
            attach(wrappers[1],channels[1],b,402,'CodeA',links[1]),return_exceptions=True),8)
        assert barrier.count==2 and all(w.held for w in wrappers)
        if pending:
            # Each inviter is still pending in the other transaction's snapshot.
            # This must remain transient, with no durable rejection or retry loop.
            assert all(isinstance(r,ApiError) and r.code=='REFERRAL_HISTORY_PENDING' for r in receipts)
            assert await c1.fetchval('SELECT count(*) FROM referral_trusted_operations')==0
            assert await c1.fetchval('SELECT count(*) FROM accounts WHERE referred_by_account_id IS NOT NULL')==0
            assert await c1.fetchval('SELECT count(*) FROM registration_links WHERE referral_attribution IS NOT NULL')==0
            assert await c1.fetchval("SELECT count(*) FROM referral_benefits WHERE history_state='history_pending'")==2
            assert await c1.fetchval('SELECT count(*) FROM referral_rewards')==0
            assert await c1.fetchval('SELECT count(*) FROM entitlements')==0
            return
        assert all(not isinstance(r,BaseException) and r['state']=='attached' for r in receipts)
        assert await c1.fetchval('SELECT referred_by_account_id FROM accounts WHERE id=$1',a)==b
        assert await c1.fetchval('SELECT referred_by_account_id FROM accounts WHERE id=$1',b)==a
        assert await c1.fetchval('SELECT count(*) FROM referral_trusted_operations')==channels.count('trusted')
        assert await c1.fetchval("SELECT count(*) FROM registration_links WHERE referral_attribution IS NOT NULL")==channels.count('mobile')
        for aid,receipt in zip((a,b),receipts):
            assert str(await c1.fetchval('SELECT referral_attribution_receipt_id FROM accounts WHERE id=$1',aid))==receipt['receipt_id']
        assert await c1.fetchval('SELECT count(*) FROM referral_rewards')==0
        assert await c1.fetchval('SELECT count(*) FROM entitlements')==0
    finally: await c1.close();await c2.close()


@pytest.mark.asyncio
async def test_legacy_stage_read_attach_new_key_does_not_reinitialize_owner(migrated_url):
    c=await connect(migrated_url)
    try:
        p=await owner(c,411,'Parent');a=await owner(c,412)
        await stage_history(c,[dict(telegram_id=412,code='Known',referred_by_telegram_id=None,
            proven_new=False,trial_used=True,first_main_paid=True)],source_sha256='d'*64)
        read={'operation':'read','telegram_id':412}
        assert (await trusted_operation(c,read))['referral']['code']=='Known'
        assert await c.fetchval('SELECT history_epoch_id FROM referral_benefits WHERE account_id=$1',a) is None
        body={'operation':'attach','telegram_id':412,'code':'Parent'}
        first=await trusted_operation(c,body,'legacy-attach-key-1')
        assert first['attribution']['state']=='attached'
        before=await c.fetchrow('SELECT * FROM accounts WHERE id=$1',a)
        second=await trusted_operation(c,body,'legacy-attach-key-2')
        assert second['attribution']['reason']=='already_attributed'
        assert await c.fetchrow('SELECT * FROM accounts WHERE id=$1',a)==before
        assert await trusted_operation(c,body,'legacy-attach-key-1')==first
        assert (await trusted_operation(c,read))['referral']['attribution']['receipt_id']==first['attribution']['receipt_id']
        # Shared mobile initializer must likewise not reset/re-evaluate accepted ready legacy state.
        await initialize_account(c,a)
        flags=await c.fetchrow('SELECT * FROM referral_benefits WHERE account_id=$1',a)
        assert flags['history_state']=='ready' and flags['imported_trial_used'] and flags['imported_first_main_paid']
        assert await c.fetchrow('SELECT * FROM accounts WHERE id=$1',a)==before
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
        assert await c.fetchval('SELECT count(*) FROM entitlements')==0
    finally: await c.close()
