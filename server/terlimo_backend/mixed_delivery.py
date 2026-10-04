"""Typed same-source generations, existing transports, exact full-set completion.

Capture/admission/grant hooks only enqueue existing outbox work. Reconcile acquires
order -> owner SHARE -> benefit -> paid-account -> source/plan -> grants. Admission
mutation acquires bind-account -> owner UPDATE before installation/binding locks.
Thus source/plan freeze uses compatible early owner fences, never a late bind lock.
"""
from datetime import datetime, UTC
from uuid import uuid4
from . import delivery_plan as plan
from .common_capacity import binding_capacity

RECONCILE='mixed_delivery.reconcile'


async def queue(connection, source_id, cause):
    key='mixed:'+str(source_id)+':'+plan.digest(cause)
    payload={'source_id':str(source_id)}
    # Never UPDATE a committed immutable duplicate: its worker may hold the
    # token row while waiting on the binding/grant held by this producer.
    await connection.execute('''INSERT INTO outbox_operations(operation_type,idempotency_key,payload)
        VALUES($1,$2,$3::jsonb) ON CONFLICT(idempotency_key) DO NOTHING''',RECONCILE,key,payload)
    existing=await connection.fetchrow('SELECT id,operation_type,payload FROM outbox_operations WHERE idempotency_key=$1',key)
    if existing is None or existing['operation_type']!=RECONCILE or plan.obj(existing['payload'])!=payload:
        raise ValueError('mixed immutable event mismatch')
    return existing['id']


async def schedule_latest(connection, account_id, cause):
    source=await connection.fetchval('SELECT id FROM delivery_fulfillments WHERE account_id=$1 AND source_sequence IS NOT NULL ORDER BY source_sequence DESC LIMIT 1',account_id)
    if source:await queue(connection,source,cause)


async def next_sequence(connection, account_id):
    return await connection.fetchval('''INSERT INTO delivery_source_counters(account_id,sequence) VALUES($1,1)
        ON CONFLICT(account_id) DO UPDATE SET sequence=delivery_source_counters.sequence+1 RETURNING sequence''',account_id)


async def membership_changed(connection, account_id):
    sequence=await next_sequence(connection,account_id)
    await connection.execute('INSERT INTO capacity_heads(account_id,membership_revision) VALUES($1,$2) ON CONFLICT(account_id) DO UPDATE SET membership_revision=$2',account_id,sequence)
    await schedule_latest(connection,account_id,['membership',sequence])


async def grant_source(connection, row):
    """Called by existing ensure_grant under its existing binding/entitlement fences.
    No new lock, proof or generation is inferred from an unrelated prior lease.
    """
    return await connection.fetchval('''SELECT f.id FROM delivery_fulfillments f
        JOIN capacity_scopes s ON s.account_id=f.account_id
        WHERE f.account_id=$1 AND f.entitlement_id=$2 AND f.source_revision=$3
        AND f.source_sequence=(SELECT max(source_sequence) FROM delivery_fulfillments WHERE account_id=$1)''',row['account_id'],row['entitlement_id'],row['entitlement_revision'])


async def grant_event(connection, grant_id, cause):
    g=await connection.fetchrow('SELECT delivery_source_id,desired_generation FROM grants WHERE id=$1',grant_id)
    if g and g['delivery_source_id']:await queue(connection,g['delivery_source_id'],['grant',str(grant_id),g['desired_generation'],cause])


async def members(connection, account_id):
    rows=await connection.fetch('''SELECT a.*,b.generation,b.installation_id FROM capacity_live_admissions a
        LEFT JOIN account_bindings b ON b.id=a.binding_id WHERE a.account_id=$1 ORDER BY a.rank''',account_id)
    actual=await connection.fetchval("SELECT count(*) FROM account_bindings WHERE account_id=$1 AND status='active'",account_id)
    if not rows or actual!=sum(r['kind']=='mobile' for r in rows):return None
    return [{'admission_id':str(r['id']),'kind':r['kind'],'physical_id':str(r['physical_id']) if r['physical_id'] else None,
        'binding_id':str(r['binding_id']) if r['binding_id'] else None,'binding_generation':r['generation'],
        'rank':int(r['rank'])} for r in rows]


async def source_for_item(connection, source, item):
    if item['plan_id'] is None:return source
    batch=await connection.fetchrow('SELECT * FROM delivery_plans WHERE id=$1 AND source_id=$2',item['plan_id'],source['id'])
    if batch is None:raise ValueError('foreign delivery generation')
    # Private validation view only. Original financial source stays immutable.
    return {**dict(source),'manifest_id':batch['manifest_id']}


async def current_item(connection, source, item):
    if item['plan_id'] is None:
        return not await connection.fetchval('SELECT EXISTS(SELECT 1 FROM delivery_plans WHERE account_id=$1 AND dispatch_sequence>$2)',source['account_id'],source['source_sequence'])
    batch=await connection.fetchrow('SELECT * FROM delivery_plans WHERE id=$1',item['plan_id'])
    revision=await connection.fetchval('SELECT membership_revision FROM capacity_heads WHERE account_id=$1',source['account_id'])
    newest=await connection.fetchval('SELECT max(dispatch_sequence) FROM delivery_plans WHERE account_id=$1',source['account_id'])
    return batch is not None and batch['membership_revision']==revision and batch['dispatch_sequence']==newest


async def freeze(connection, source, settings):
    """Caller holds early owner SHARE and paid-account; no bind advisory or I/O."""
    from .external_delivery import _applicable,APPLY,_enqueue
    if not await _applicable(connection,source):return None,'source_inapplicable_or_unsequenced'
    scope=await connection.fetchrow('SELECT * FROM capacity_scopes WHERE account_id=$1',source['account_id'])
    if scope is None:return None,'scope_not_active'
    identities=await members(connection,source['account_id'])
    if identities is None:return None,'untracked_or_empty_membership'
    revision=await connection.fetchval('SELECT membership_revision FROM capacity_heads WHERE account_id=$1',source['account_id'])
    snapshot=plan.obj(source['snapshot']);deadlines=[plan.instant(x) for x in snapshot['rank_deadlines']]
    entitlement=await connection.fetchrow('SELECT * FROM entitlements WHERE id=$1',source['entitlement_id'])
    required={}
    for m in identities:
        if m['rank']>len(deadlines):return None,'source_capacity_incomplete'
        if m['kind']=='mobile':
            # Existing authorized grants are destinations, never all registry gateways.
            grants=await connection.fetch("SELECT * FROM grants WHERE binding_id=$1 AND hour_entitlement_id IS NULL AND state<>'revoked' ORDER BY gateway_id,id",plan.uuid(m['binding_id']))
            from .gateway_control import ensure_grant
            for g in grants:
                await ensure_grant(connection,binding_id=g['binding_id'],gateway_id=g['gateway_id'],entitlement_id=source['entitlement_id'],max_lease_seconds=settings.gateway_max_lease_seconds)
            grants=await connection.fetch("SELECT * FROM grants WHERE binding_id=$1 AND hour_entitlement_id IS NULL AND state<>'revoked' AND delivery_source_id=$2 AND delivery_binding_generation=$3 ORDER BY gateway_id,id",plan.uuid(m['binding_id']),source['id'],m['binding_generation'])
            required[m['admission_id']]=[{'grant_id':str(g['id']),'generation':int(g['desired_generation']),
                'not_after':int(g['not_after'].timestamp()),'gateway_id':str(g['gateway_id'])} for g in grants]
    destinations=plan.digest(required)
    old=await connection.fetchrow('SELECT * FROM delivery_plans WHERE source_id=$1 AND membership_revision=$2 AND destination_digest=$3',source['id'],revision,destinations)
    if old:return old,None
    sequence=await next_sequence(connection,source['account_id'])
    batch=await connection.fetchrow('''INSERT INTO delivery_plans(source_id,account_id,manifest_id,membership_revision,members,destination_digest,dispatch_sequence)
        VALUES($1,$2,$3,$4,$5::jsonb,$6,$7) RETURNING *''',source['id'],source['account_id'],scope['manifest_id'],revision,identities,destinations,sequence)
    now=await connection.fetchval('SELECT clock_timestamp()')
    for m in identities:
        deadline=deadlines[m['rank']-1]
        if m['kind']=='mobile':
            await connection.execute('''INSERT INTO delivery_mobile_items(plan_id,admission_id,binding_id,binding_generation,rank,deadline,required_grants)
                VALUES($1,$2,$3,$4,$5,$6,$7::jsonb)''',batch['id'],plan.uuid(m['admission_id']),plan.uuid(m['binding_id']),m['binding_generation'],m['rank'],deadline,required[m['admission_id']])
        else:
            p=await connection.fetchrow('SELECT * FROM delivery_physical_targets WHERE id=$1',plan.uuid(m['physical_id']))
            target=await connection.fetchrow('''SELECT * FROM delivery_targets WHERE manifest_id=$1 AND deployment=$2 AND external_key=$3''',scope['manifest_id'],p['deployment'],p['external_key'])
            if p['account_id']!=source['account_id'] or p['claim_receipt'] is None or target is None:raise ValueError('claimed target authority lost')
            desired=plan.desired_for_rank(snapshot,m['rank'],deadline.replace(microsecond=0) if deadline else None,now,source['snapshot_digest'])
            i=await connection.fetchrow('''INSERT INTO delivery_items(fulfillment_id,target_id,desired,physical_id,plan_id,delivery_order)
                VALUES($1,$2,$3::jsonb,$4,$5,$6) RETURNING *''',source['id'],target['id'],desired,p['id'],batch['id'],sequence)
            oid=await _enqueue(connection,APPLY,'direct-apply:'+str(i['operation_id']),{'item_id':str(i['id'])})
            await connection.execute('UPDATE delivery_items SET outbox_id=$2 WHERE id=$1',i['id'],oid)
    return batch,None


async def mobile_receipt(connection, source, item):
    required=plan.obj(item['required_grants'])
    if not required:return None
    b=await connection.fetchrow('''SELECT b.*,i.state AS installation_state,i.public_key_fingerprint FROM account_bindings b JOIN installations i ON i.id=b.installation_id WHERE b.id=$1''',item['binding_id'])
    if not b or b['account_id']!=source['account_id'] or b['status']!='active' or b['generation']!=item['binding_generation'] or b['installation_state']=='revoked':return None
    e=await connection.fetchrow('SELECT * FROM entitlements WHERE id=$1',source['entitlement_id'])
    if not e or not (await binding_capacity(connection,e,b['id']))[0]:return None
    receipt=[];now=datetime.now(UTC)
    for r in required:
        g=await connection.fetchrow('SELECT * FROM grants WHERE id=$1',plan.uuid(r['grant_id']))
        if not g or g['binding_id']!=b['id'] or g['gateway_id']!=plan.uuid(r['gateway_id']) or g['hour_entitlement_id'] is not None or g['delivery_source_id']!=source['id'] or g['delivery_binding_generation']!=b['generation']:return None
        data=plan.obj(g['last_readback'] or {})
        if (g['state']!='applied' or g['desired_generation']!=r['generation'] or g['applied_generation']!=r['generation'] or g['applied_not_after'] is None or g['applied_not_after']<=now
            or int(g['applied_not_after'].timestamp())!=r['not_after'] or int(g['not_after'].timestamp())!=r['not_after']
            or item['deadline'] is not None and r['not_after']>int(item['deadline'].timestamp())
            or data.get('runtime_applied') is not True or data.get('revoked') is not False
            or data.get('grant_id')!=str(g['opaque_id']) or data.get('registration_id')!=b['public_key_fingerprint'] or data.get('node_id')!=g['target_node_id']
            or data.get('generation')!=str(g['gateway_generation']) or data.get('lease_seq')!=str(g['lease_seq']) or data.get('expires_at')!=r['not_after']):return None
        receipt.append({'grant_id':str(g['id']),'generation':r['generation'],'not_after':r['not_after'],'lease_seq':g['lease_seq'],'gateway_generation':g['gateway_generation']})
    return receipt


async def check_batch(connection, source, batch, *, store=False):
    from .external_delivery import _applicable
    current=await _applicable(connection,source)
    identities=await members(connection,source['account_id'])
    revision=await connection.fetchval('SELECT membership_revision FROM capacity_heads WHERE account_id=$1',source['account_id'])
    latest=await connection.fetchval('SELECT max(dispatch_sequence) FROM delivery_plans WHERE account_id=$1',source['account_id'])
    current=current and identities==plan.obj(batch['members']) and revision==batch['membership_revision'] and latest==batch['dispatch_sequence']
    directs=await connection.fetch('''SELECT i.*,p.proven_revision,p.proven_snapshot FROM delivery_items i JOIN delivery_physical_targets p ON p.id=i.physical_id WHERE i.plan_id=$1''',batch['id'])
    mobiles=await connection.fetch('SELECT * FROM delivery_mobile_items WHERE plan_id=$1',batch['id'])
    proofs=[];ready=bool(directs or mobiles) and current
    for i in directs:
        observation=plan.obj(i['observation'] or {});snapshot=plan.obj(i['proven_snapshot'])
        usable=i['application_receipt'] is not None and i['proven_revision']==i['delivery_revision'] and observation.get('observed_current_usable') is True and snapshot['state']=='active' and snapshot['expires_at'] is not None and snapshot['expires_at']>int(datetime.now(UTC).timestamp())
        ready=ready and usable
        if usable:proofs.append({'kind':'direct','item_id':str(i['id']),'operation_id':str(i['operation_id']),'revision':i['delivery_revision']})
    for i in mobiles:
        receipt=await mobile_receipt(connection,source,i) if current else None
        ready=ready and receipt is not None
        if receipt is not None:
            if store:await connection.execute('INSERT INTO delivery_mobile_proofs(item_id,receipt) VALUES($1,$2::jsonb) ON CONFLICT(item_id) DO NOTHING',i['id'],receipt)
            proofs.append({'kind':'mobile','item_id':str(i['id']),'receipts':receipt})
    ready=ready and len(directs)+len(mobiles)==len(plan.obj(batch['members']))
    return bool(ready),proofs,len(directs)+len(mobiles)


async def readiness(connection, source_id):
    source=await connection.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',source_id)
    batch=await connection.fetchrow('SELECT * FROM delivery_plans WHERE source_id=$1 ORDER BY dispatch_sequence DESC LIMIT 1',source_id)
    historical=await connection.fetchval('SELECT EXISTS(SELECT 1 FROM delivery_completions WHERE source_id=$1)',source_id)
    ready,proofs,count=await check_batch(connection,source,batch) if batch else (False,[],0)
    return {'transport_complete':ready,'all_observed_current_usable':ready,'required_items':count,'historical_application_items':len(proofs),
        'business_completed':bool(historical),'current_plan_id':str(batch['id']) if batch else None,'plan_status':'ready' if ready else 'typed_delivery_pending'}


class MixedDeliveryHandlers:
    def __init__(self, settings):self.settings=settings
    def as_handlers(self):return {RECONCILE:self.reconcile}

    async def reconcile(self, connection, operation):
        from .external_delivery import _token
        payload=plan.obj(operation['payload'])
        if set(payload)!={'source_id'}:return ('failed','invalid_mixed_payload')
        source=await connection.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',plan.uuid(payload['source_id']))
        if not source:return ('failed','source_missing')
        async with connection.transaction():
            order=None
            if source['source_order_id']:
                order=await connection.fetchrow('SELECT * FROM payment_orders WHERE id=$1 FOR UPDATE',source['source_order_id'])
            await plan._owner(connection,source['account_id'])
            from .referral_pricing import benefit_lock
            await benefit_lock(connection,source['account_id'])
            await connection.fetchrow('SELECT account_id FROM referral_benefits WHERE account_id=$1 FOR UPDATE',source['account_id'])
            await connection.execute('SELECT pg_advisory_xact_lock(hashtextextended($1,0))','paid-account:'+str(source['account_id']))
            source=await connection.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1 FOR UPDATE',source['id'])
            if not await _token(connection,operation):return ('failed','claim_token_lost')
            batch,error=await freeze(connection,source,self.settings)
            if error:return ('failed',error)
            ready,proofs,count=await check_batch(connection,source,batch,store=True)
            if not ready:return None # durable proof/admission/destination events schedule next examination
            if order and (order['status']!='succeeded' or order['account_id']!=source['account_id'] or order['applied_entitlement_id']!=source['entitlement_id'] or order['credited_entitlement_revision']!=source['source_revision']):return ('failed','order_source_mismatch')
            created=await connection.fetchval('''INSERT INTO delivery_completions(source_id,plan_id,receipt) VALUES($1,$2,$3::jsonb) ON CONFLICT(source_id) DO NOTHING RETURNING source_id''',source['id'],batch['id'],{'source_digest':source['snapshot_digest'],'required_items':count,'typed_proofs':proofs})
            if created:
                from .referral_rewards import enqueue_reward
                if order:
                    await connection.execute('UPDATE payment_orders SET needs_grant=false,updated_at=now() WHERE id=$1',order['id'])
                    if order['months'] in (1,3,6):await enqueue_reward(connection,account_id=source['account_id'],event_kind='first_main_paid',source_entitlement_id=source['entitlement_id'],source_order_id=order['id'],months=order['months'])
                elif source['source_kind']=='trial':
                    snapshot=plan.obj(source['snapshot'])
                    if plan.obj(snapshot['source_plan']).get('referral_bonus_days')==3:
                        await enqueue_reward(connection,account_id=source['account_id'],event_kind='trial',source_entitlement_id=source['entitlement_id'])
        return None
