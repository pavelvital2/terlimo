"""Private activated capacity. No entitlement ledger, remote I/O or public proof API.

Mutation callers own bind-account advisory BEFORE installation/link/binding locks.
Paid/benefit/grant writers only read live ranks; they never acquire a late advisory.
"""
from datetime import datetime, UTC
from . import delivery_plan as plan


async def admission_lock(connection, account_id, *, activation=False):
    await connection.execute('SELECT pg_advisory_xact_lock(hashtextextended($1,0))','bind-account:'+str(account_id))
    if activation or await connection.fetchval('SELECT EXISTS(SELECT 1 FROM capacity_scopes WHERE account_id=$1)',account_id):
        await connection.fetchval('SELECT id FROM accounts WHERE id=$1 FOR UPDATE',account_id)


async def current_entitlement(connection, account_id, now=None):
    return await connection.fetchrow("""SELECT * FROM entitlements WHERE account_id=$1
        AND kind IN ('paid','trial','imported') AND status='active'
        AND (starts_at IS NULL OR starts_at<=$2) AND (ends_at IS NULL OR ends_at>$2)
        ORDER BY created_at DESC LIMIT 1""",account_id,now or datetime.now(UTC))


async def live_deadlines(connection, entitlement, now=None):
    """Canonical live commercial ranks; slot UUID never belongs to a device."""
    now=now or datetime.now(UTC)
    end=entitlement['ends_at']
    if entitlement['kind']=='paid' and end is not None:
        from .payment_products import slots
        extra=sorted((r['expires_at'] for r in await slots(connection,entitlement['id'],now)),reverse=True)
        return [end]*int(entitlement['paid_base_device_limit'])+[min(end,e) for e in extra]
    return [end]*int(entitlement['device_limit'] or 2)


async def occupied_count(connection, account_id):
    # Count ALL real active bindings, including an untracked imported binding. Such
    # a binding is denied a mapped rank; never silently ignored for admission.
    return int(await connection.fetchval("""SELECT
        (SELECT count(*) FROM account_bindings WHERE account_id=$1 AND status='active')+
        (SELECT count(*) FROM capacity_admissions WHERE account_id=$1 AND kind='direct')""",account_id))


async def binding_capacity(connection, entitlement, binding_id, now=None):
    now=now or datetime.now(UTC)
    rank=await connection.fetchval('SELECT rank FROM capacity_mobile_ranks WHERE id=$1 AND account_id=$2',binding_id,entitlement['account_id'])
    return await rank_capacity(connection,entitlement,rank,now)


async def rank_capacity(connection, entitlement, rank, now=None):
    now=now or datetime.now(UTC)
    deadlines=await live_deadlines(connection,entitlement,now)
    if rank is None or rank>len(deadlines):return False,now
    deadline=deadlines[int(rank)-1]
    return deadline is None or deadline>now,deadline


async def physical_capacity(connection, entitlement, physical_id, now=None):
    """Concrete typed direct rank/deadline seam for the next aggregate planner."""
    rank=await connection.fetchval("SELECT rank FROM capacity_live_admissions WHERE account_id=$1 AND physical_id=$2 AND kind='direct'",entitlement['account_id'],physical_id)
    return await rank_capacity(connection,entitlement,rank,now)


async def append_mobile(connection, account_id, binding_id):
    """After actual authorized binding insert/reactivation in the same transaction.

    Caller already holds the admission lock; admission count check precedes binding
    mutation. Each reactivation appends a new incarnation, never steals an old rank.
    """
    if not await connection.fetchval('SELECT EXISTS(SELECT 1 FROM capacity_scopes WHERE account_id=$1)',account_id):return
    if await connection.fetchval('SELECT EXISTS(SELECT 1 FROM capacity_admissions WHERE binding_id=$1 AND released_at IS NULL)',binding_id):return
    await connection.execute("""INSERT INTO capacity_admissions(account_id,admission_order,kind,binding_id)
        SELECT $1,coalesce(max(admission_order),0)+1,'mobile',$2 FROM capacity_admissions WHERE account_id=$1""",account_id,binding_id)
    from .mixed_delivery import membership_changed
    await membership_changed(connection,account_id)


async def release_mobile(connection, binding_id):
    # Same transaction and already-held bind-account lock as authorized revoke.
    account_id=await connection.fetchval("UPDATE capacity_admissions SET released_at=now() WHERE binding_id=$1 AND kind='mobile' AND released_at IS NULL RETURNING account_id",binding_id)
    if account_id:
        from .mixed_delivery import membership_changed
        await membership_changed(connection,account_id)


async def activate(connection, body, allowlist, *, dry_run=True):
    """Protected activation, separate from stage/claim; initial direct-only scope."""
    plan.validate_manifest(body,allowlist)
    aid,mid=plan.uuid(body['account_id']),plan.uuid(body['id'])
    async with connection.transaction():
        await admission_lock(connection,aid,activation=True)
        await plan._owner(connection,aid)
        m=await connection.fetchrow('SELECT * FROM delivery_manifests WHERE id=$1',mid)
        proof=await connection.fetchrow('SELECT * FROM delivery_mapping_proofs WHERE manifest_id=$1',mid)
        if not m or m['account_id']!=aid or m['digest']!=plan.digest(body) or m['state']!='staged' or not proof or proof['manifest_digest']!=m['digest']:
            raise ValueError('complete current authenticated mapping proof required')
        targets=await connection.fetch('SELECT * FROM delivery_targets WHERE manifest_id=$1 ORDER BY ordinal',mid)
        if not targets or any(t['kind']!='direct' for t in targets) or proof['target_digest']!=plan.digest([plan.obj(t['evidence']) for t in targets]):
            raise ValueError('complete direct-only proof required')
        physical=[]
        for index,t in enumerate(targets,1):
            evidence=plan.obj(t['evidence'])
            p=await connection.fetchrow('SELECT * FROM delivery_physical_targets WHERE deployment=$1 AND external_key=$2',t['deployment'],t['external_key'])
            if evidence['device_index']!=index or not p or p['account_id']!=aid or p['claim_receipt'] is None or str(p['grant_id'])!=evidence['grant_uuid'] or str(p['fence_id'])!=evidence['fence']:
                raise ValueError('physical claim/owner/order mismatch')
            physical.append(p['id'])
        old=await connection.fetchrow('SELECT * FROM capacity_scopes WHERE account_id=$1',aid)
        if old:
            retained=await connection.fetch("SELECT physical_id FROM capacity_admissions WHERE account_id=$1 AND kind='direct' ORDER BY admission_order",aid)
            if old['manifest_id']!=mid or old['manifest_digest']!=m['digest'] or [r['physical_id'] for r in retained]!=physical:
                raise ValueError('activated scope conflict; reviewed cutover required')
            return {'dry_run':dry_run,'active':True,'replay':True,'direct_seats':len(physical)}
        if await connection.fetchval("SELECT EXISTS(SELECT 1 FROM account_bindings WHERE account_id=$1 AND status='active')",aid):
            raise ValueError('mixed activation requires reviewed cutover')
        now=await connection.fetchval('SELECT clock_timestamp()')
        entitlement=await current_entitlement(connection,aid,now)
        if entitlement is None or len(physical)>len(await live_deadlines(connection,entitlement,now)):
            raise ValueError('no current capacity or overcapacity')
        if not dry_run:
            await connection.execute('INSERT INTO capacity_scopes(account_id,manifest_id,manifest_digest) VALUES($1,$2,$3)',aid,mid,m['digest'])
            await connection.execute('INSERT INTO capacity_heads(account_id) VALUES($1)',aid)
            for index,pid in enumerate(physical,1):
                await connection.execute("INSERT INTO capacity_admissions(account_id,admission_order,kind,physical_id) VALUES($1,$2,'direct',$3)",aid,index,pid)
        if not dry_run:
            from .mixed_delivery import schedule_latest
            await schedule_latest(connection,aid,'activation:'+str(mid))
        return {'dry_run':dry_run,'active':not dry_run,'replay':False,'direct_seats':len(physical)}


async def plan_membership(connection, account_id, manifest_id):
    """Typed seam for next aggregate slice. A single DB statement snapshots scope,
    current real membership and corresponding manifest ranks. No scope = legacy.
    Mixed/untracked/omitted targets remain explicit incomplete, never full proof.
    """
    rows=await connection.fetch("""SELECT s.account_id,
        (SELECT count(*) FROM account_bindings WHERE account_id=s.account_id AND status='active') AS mobile_count,
        a.kind,a.physical_id,a.binding_id,a.rank,t.id AS target_id
        FROM capacity_scopes s LEFT JOIN capacity_live_admissions a ON a.account_id=s.account_id
        LEFT JOIN delivery_physical_targets p ON p.id=a.physical_id
        LEFT JOIN delivery_targets t ON t.manifest_id=$2 AND t.account_id=s.account_id AND t.kind='direct'
             AND t.deployment=p.deployment AND t.external_key=p.external_key
        WHERE s.account_id=$1 ORDER BY a.rank""",account_id,manifest_id)
    if not rows:return {'activated':False,'complete':True,'ranks':{},'reason':None}
    complete=bool(rows) and all(r['kind']=='direct' and r['target_id'] is not None and r['mobile_count']==0 for r in rows)
    manifest_count=await connection.fetchval('SELECT count(*) FROM delivery_targets WHERE manifest_id=$1',manifest_id)
    complete=complete and manifest_count==len(rows)
    return {'activated':True,'complete':bool(complete),'reason':None if complete else 'incomplete_account_plan',
        'ranks':{r['target_id']:int(r['rank']) for r in rows if r['target_id'] is not None},
        'members':[{'kind':r['kind'],'physical_id':str(r['physical_id']) if r['physical_id'] else None,'binding_id':str(r['binding_id']) if r['binding_id'] else None,'rank':int(r['rank']) if r['rank'] else None} for r in rows]}
