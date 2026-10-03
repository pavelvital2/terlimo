"""H1: real referral_info boundary for already registered authenticated accounts."""
from dataclasses import replace
from datetime import datetime, UTC
import uuid

import asyncpg
import pytest

from terlimo_backend.auth_api import ApiError
from terlimo_backend.referral import referral_info
from terlimo_backend.session_auth import SessionContext
from test_referral_history_coverage import account, installation, member, manifest, reward, load


async def context(c, owner, tg):
    iid, bid = await installation(c, owner, tg)
    binding = await c.fetchrow('SELECT * FROM account_bindings WHERE id=$1',bid)
    return SessionContext(session_id=uuid.uuid4(),session_generation=1,account_id=owner,
        installation_id=iid,installation_ref='fixture',environment='test',scopes=frozenset(),
        account_state='VERIFIED_NO_ENTITLEMENT',management_only=False,binding=binding,
        binding_status='active',entitlement=None,active_entitlement=None,onboarding_hour=None,
        slots_used=1,evaluated_at=datetime.now(UTC))


@pytest.mark.asyncio
async def test_get_initializes_existing_legacy_and_proven_absent_without_registration(migrated_url):
    c=await asyncpg.connect(migrated_url)
    try:
        parent=await account(c,201);child=await account(c,202);fresh=await account(c,203)
        pc=await context(c,parent,201);cc=await context(c,child,202);nc=await context(c,fresh,203)
        await c.execute("INSERT INTO referral_benefits(account_id) VALUES($1),($2),($3)",parent,child,fresh)
        with pytest.raises(ApiError,match='REFERRAL_HISTORY_PENDING'): await referral_info(c,nc)
        m=manifest([member(201,'Parent'),member(202,'Kept',201,trial=True,paid=True,trial_reward='earned')],
                   [reward(202,201,'APPLIED')])
        await load(c,m,activate=True)
        # No manual initialize_account calls and no new registration links.
        child_info=await referral_info(c,cc)
        assert child_info['code']=='Kept' and child_info['terms_version']=='referral-20261003-v1'
        assert child_info['benefits']['discount']['state']=='consumed'
        assert child_info['benefits']['trial_bonus_days']==0
        assert child_info['attribution']['state']=='attached'
        before=await c.fetchrow('SELECT * FROM referral_rewards WHERE invitee_account_id=$1',child)
        assert before['state']=='APPLIED' and before['import_evidence'] is not None
        assert await referral_info(c,cc)==child_info
        parent_info=await referral_info(c,pc)
        assert parent_info['rewards']=={'waiting_days':0,'applied_days':7}
        new_info=await referral_info(c,nc)
        assert new_info['code'] and await referral_info(c,nc)==new_info
        assert await c.fetchval("SELECT count(*) FROM referral_benefits WHERE history_state='ready'")==3
        assert await c.fetchval('SELECT count(*) FROM registration_links')==0
        assert await c.fetchval('SELECT count(*) FROM entitlements')==0
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==1
        assert await c.fetchrow('SELECT * FROM referral_rewards WHERE invitee_account_id=$1',child)==before
    finally: await c.close()


@pytest.mark.asyncio
@pytest.mark.parametrize('mode',['incomplete','unactivated','unresolved','PENDING_REMOTE','REMOTE_APPLIED'])
async def test_get_preserves_pending_for_incomplete_or_ambiguous_history(migrated_url,mode):
    c=await asyncpg.connect(migrated_url)
    try:
        parent=await account(c,211);child=await account(c,212)
        ctx=await context(c,child,212)
        remote=mode in ('PENDING_REMOTE','REMOTE_APPLIED')
        m=manifest([member(211,'Parent'),member(212,'Child',211,trial=True,
            trial_reward='earned' if remote else 'unresolved' if mode=='unresolved' else 'not_earned')],
            [reward(212,211,mode)] if remote else [],complete=mode!='incomplete')
        await load(c,m,activate=mode not in ('incomplete','unactivated'))
        for _ in range(2):
            with pytest.raises(ApiError,match='REFERRAL_HISTORY_PENDING') as error:
                await referral_info(c,ctx)
            assert error.value.retryable
        assert await c.fetchval('SELECT referral_code FROM accounts WHERE id=$1',child) is None
        assert await c.fetchval('SELECT history_state FROM referral_benefits WHERE account_id=$1',child)=='history_pending'
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
        if remote or mode=='unresolved':
            assert await c.fetchval('SELECT imported_trial_used FROM referral_benefits WHERE account_id=$1',child)
    finally: await c.close()


@pytest.mark.asyncio
@pytest.mark.parametrize('mode',['foreign','inactive_db','inactive_context','unverified','anonymous'])
async def test_get_rejects_bad_ownership_before_any_initialization(migrated_url,mode):
    c=await asyncpg.connect(migrated_url)
    try:
        owner=await account(c,221);other=await account(c,222)
        ctx=await context(c,owner,221)
        await load(c,manifest(),activate=True)
        if mode=='foreign': ctx=replace(ctx,account_id=other)
        elif mode=='inactive_db':
            await c.execute("UPDATE account_bindings SET status='revoked' WHERE id=$1",ctx.binding['id'])
        elif mode=='inactive_context': ctx=replace(ctx,binding={'status':'revoked'})
        elif mode=='anonymous': ctx=replace(ctx,account_id=None,binding=None)
        elif mode=='unverified':
            # Retain a real active binding, but remove the verified identity proof.
            await c.execute('UPDATE accounts SET telegram_id=NULL WHERE id=$1',owner)
        before=await c.fetch('SELECT * FROM accounts ORDER BY id')
        with pytest.raises(ApiError,match='ACCESS_DENIED'):
            await referral_info(c,ctx)
        assert await c.fetch('SELECT * FROM accounts ORDER BY id')==before
        assert await c.fetchval('SELECT count(*) FROM referral_benefits')==0
        assert await c.fetchval('SELECT count(*) FROM referral_rewards')==0
    finally: await c.close()
