"""Existing reward state machine, immutable typed delivery source; no RPC here."""
from datetime import timedelta
from . import delivery_plan as plan
from .mixed_delivery import readiness,queue


async def source_matches(connection,source):
    r=await connection.fetchrow('SELECT * FROM referral_rewards WHERE id=$1',source['source_reward_id'])
    snapshot=plan.obj(source['snapshot']);commercial=snapshot['commercial']
    return bool(r and r['fulfillment_kind']=='typed' and r['typed_source_id']==source['id']
        and r['state'] in ('APPLYING','APPLIED') and r['inviter_account_id']==source['account_id']
        and r['target_entitlement_id']==source['entitlement_id'] and r['target_revision']==source['source_revision']
        and plan.stamp(r['target_ends_at'])==snapshot['ends_at']
        and commercial.get('reward_id')==str(r['id']) and commercial.get('days')==r['days']
        and commercial.get('base_end')==plan.stamp(r['target_base_ends_at']))


async def examine(connection,reward):
    """Caller holds early owner SHARE -> paid-account -> reward; no late owner lock."""
    if reward['fulfillment_kind']=='typed':
        source=await connection.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',reward['typed_source_id'])
        if source and await source_matches(connection,source):
            # Same immutable attempt/event key; proof/membership events drive later checks.
            await queue(connection,source['id'],'reward-resume')
        return
    if reward['state']!='WAITING' or not await connection.fetchval("SELECT status='verified' FROM accounts WHERE id=$1",reward['inviter_account_id']):return
    # Historical partial targets must never be inferred/replayed as a new attempt.
    if any(reward[k] is not None for k in ('target_entitlement_id','target_revision','target_base_ends_at','target_ends_at')) or plan.obj(reward['grant_targets']):return
    entitlement=await connection.fetchrow("""SELECT * FROM entitlements WHERE account_id=$1
        AND kind IN ('paid','imported','trial') AND status='active'
        AND starts_at<=clock_timestamp() AND ends_at>clock_timestamp()
        ORDER BY CASE WHEN kind IN ('paid','imported') THEN 0 ELSE 1 END,ends_at DESC,id LIMIT 1 FOR UPDATE""",reward['inviter_account_id'])
    if entitlement is None:return
    source=await connection.fetchrow('''SELECT * FROM delivery_fulfillments WHERE account_id=$1
        AND entitlement_id=$2 AND source_revision=$3 AND source_sequence IS NOT NULL
        ORDER BY source_sequence DESC LIMIT 1''',reward['inviter_account_id'],entitlement['id'],entitlement['revision'])
    if source is None:return # no reconstruction, subscription, target or destination creation
    proof=await readiness(connection,source['id'])
    if not proof['transport_complete'] or not proof['all_observed_current_usable'] or proof['required_items']==0:return
    base_end=entitlement['ends_at'];target_end=base_end+timedelta(days=reward['days'])
    extended=await connection.fetchrow('''UPDATE entitlements SET ends_at=$2,revision=revision+1
        WHERE id=$1 RETURNING *''',entitlement['id'],target_end)
    original={**dict(reward),'target_base_ends_at':base_end}
    captured=await plan.capture_reward(connection,original,extended,proof['current_plan_id'])
    await connection.execute("""UPDATE referral_rewards SET fulfillment_kind='typed',typed_source_id=$2,
        state='APPLYING',target_entitlement_id=$3,target_revision=$4,target_base_ends_at=$5,
        target_ends_at=$6,updated_at=clock_timestamp() WHERE id=$1""",
        reward['id'],captured['id'],extended['id'],extended['revision'],base_end,target_end)


async def complete(connection,source):
    """Within exact-token/source/full-proof completion TX; no payment/recursive reward."""
    if not await source_matches(connection,source):raise ValueError('typed reward source mismatch')
    updated=await connection.fetchval("""UPDATE referral_rewards SET state='APPLIED',updated_at=clock_timestamp()
        WHERE id=$1 AND fulfillment_kind='typed' AND typed_source_id=$2 AND state='APPLYING' RETURNING id""",source['source_reward_id'],source['id'])
    if updated is None:raise ValueError('typed reward completion state mismatch')
