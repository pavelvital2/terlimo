"""Protected operator scheduling and token-fenced direct delivery proof.

No runtime registration. Only injected strict Unix clients; canonical source owns intent.
"""
from dataclasses import asdict
from datetime import UTC, datetime
from uuid import uuid4

from . import delivery_plan as plan
from .direct_delivery_client import Request, Snapshot, DirectDeliveryClient
from .worker import FINALIZE_SQL

CLAIM = 'external_direct_claim'
APPLY = 'external_direct_apply'
DEPENDENCY_WAIT = 'physical_predecessor_pending'


class AcknowledgedReadbackRetry(RuntimeError):
    """Existing bounded outbox retry, always the original persisted wire."""
    def __init__(self):
        super().__init__('acknowledged_readback_pending')


def _wire(value):
    return plan.packed(value)


def _request(wire, base=None):
    return Request(wire.encode(), base)


def _snapshot(value):
    value = plan.obj(value)
    return Snapshot(value['state'], value['expires_at'])


def _metadata(response, request, *, now, applicable):
    # Deliberately no artifact accessor/storage or dataclasses.asdict(Response).
    receipt = None
    if response.receipt:
        r = response.receipt
        receipt = dict(operation_id=r.operation_id,digest=r.digest,
            external_key=r.target.external_key,grant_id=r.target.grant_id,fence_id=r.target.fence_id,
            revision=r.revision,desired=asdict(r.desired),applied=asdict(r.applied))
    return receipt, dict(operation_id=response.operation_id,body_digest=response.body_digest,
        accepted_revision=response.accepted_revision,current_revision=response.current_revision,
        current=asdict(response.current) if response.current else None,
        delivery_state=response.delivery_state,code=response.code,observed_at=now,
        historical_fulfilled=response.historical_fulfilled(request),
        observed_current_usable=applicable and response.current_usable(request,now=now),
        canonical_source_applicable=applicable)


def _claim_matches(physical, req):
    return (req.operation=='claim' and req.operation_id==str(physical['claim_operation_id']) and
        req.body.decode()==physical['claim_wire'] and req.digest==physical['claim_digest'] and
        req.revision==physical['initial_revision'] and req.desired==_snapshot(physical['initial_base']) and
        req.target.external_key==physical['external_key'] and req.target.grant_id==str(physical['grant_id']) and
        req.target.fence_id==str(physical['fence_id']))


def _intent_matches(source, item, physical, req, target):
    desired=plan.obj(item['desired'])
    value=plan.obj(req.body.decode())
    deadline=desired['expires_at']
    snap=Snapshot(desired['state'],int(plan.instant(deadline).timestamp()) if deadline else None)
    evidence=plan.obj(target['evidence'])
    return (req.operation=='apply' and req.operation_id==str(item['operation_id']) and
        req.revision==item['delivery_revision'] and req.digest==item['request_digest'] and
        value['expected_revision']==item['expected_revision'] and plan.obj(item['request'])==value and
        req.target.external_key==physical['external_key'] and req.target.grant_id==str(physical['grant_id']) and
        req.target.fence_id==str(physical['fence_id']) and req.desired==snap and
        desired['source_digest']==source['snapshot_digest'] and target['manifest_id']==source['manifest_id'] and
        target['account_id']==source['account_id']==physical['account_id'] and target['kind']=='direct' and
        target['deployment']==physical['deployment'] and target['external_key']==physical['external_key'] and
        evidence['grant_uuid']==str(physical['grant_id']) and evidence['fence']==str(physical['fence_id']))


async def _token(connection, operation):
    row=await connection.fetchrow('''SELECT * FROM outbox_operations
        WHERE id=$1 AND claim_token=$2 AND status='processing' FOR UPDATE''',
        operation['id'],operation['claim_token'])
    if row is None or any(row[key]!=operation[key] for key in ('operation_type','payload','idempotency_key','gateway_id','target_revision')):
        return None
    return row


async def _park_dependency(connection, operation):
    # Preparation already holds physical and exact outbox-token locks. Clearing
    # the token durably before returning fences a later worker finalization,
    # including one that arrives after predecessor proof has released this row.
    await connection.fetchval(FINALIZE_SQL,operation['id'],operation['claim_token'],
                              'failed',DEPENDENCY_WAIT,0.0)


async def _release_dependencies(connection, physical_id):
    # Called only inside authenticated application-proof commit while physical
    # row is locked. No release for accepted/proven gaps or unresolved commands.
    ready=await connection.fetchval("""SELECT accepted_revision=proven_revision AND NOT EXISTS(
        SELECT 1 FROM delivery_items WHERE physical_id=$1 AND dispatch_started
        AND application_receipt IS NULL) FROM delivery_physical_targets WHERE id=$1""",physical_id)
    if not ready:return
    await connection.execute("""UPDATE outbox_operations o SET status='pending',last_error=NULL,
        available_at=now(),updated_at=now()
        FROM delivery_items i WHERE i.outbox_id=o.id AND i.physical_id=$1
        AND NOT i.dispatch_started AND i.outcome='pending' AND i.application_receipt IS NULL
        AND o.operation_type=$2 AND o.status='failed' AND o.last_error=$3
        AND o.claim_token IS NULL""",physical_id,APPLY,DEPENDENCY_WAIT)


async def _enqueue(connection, kind, key, payload):
    # External revisions intentionally do not use outbox.target_revision (integer).
    return await connection.fetchval('''INSERT INTO outbox_operations(operation_type,idempotency_key,payload,gateway_id,target_revision)
        VALUES($1,$2,$3::jsonb,NULL,NULL) ON CONFLICT(idempotency_key) DO UPDATE
        SET idempotency_key=EXCLUDED.idempotency_key RETURNING id''',kind,key,payload)


def validate_writer_record(body, evidence):
    """Protected operator record only, not public caller proof or physical stop assertion."""
    if type(evidence) is not dict or set(evidence)!={'version','account_id','targets','legacy_writer_record_sha256'}:
        raise ValueError('closed retained writer record required')
    h=evidence['legacy_writer_record_sha256']
    if type(evidence['version']) is not int or evidence['version']!=1 or type(h) is not str or len(h)!=64 or any(c not in '0123456789abcdef' for c in h):
        raise ValueError('writer record invalid')
    if evidence['account_id']!=body['account_id'] or evidence['targets']!=body['targets'] or plan.digest(evidence)!=body['evidence_sha256']:
        raise ValueError('writer record does not match complete manifest')


async def schedule_claims(connection, manifest_id, allowlist, writer_record, *, dry_run=True):
    """Local protected operator seam. Requires already-staged complete direct manifest.

    Never executed automatically on payment, read or startup. Claim wire committed
    before outbox; receipt writer is solely ExternalDeliveryHandlers.
    """
    async with connection.transaction():
        m=await connection.fetchrow('SELECT * FROM delivery_manifests WHERE id=$1',manifest_id)
        if not m:raise ValueError('staged manifest required')
        body=plan.obj(m['body']);plan.validate_manifest(body,allowlist);validate_writer_record(body,writer_record)
        await plan._owner(connection,m['account_id'])
        if m['state']!='staged' or any(t['kind']!='direct' for t in body['targets']):
            raise ValueError('complete direct-only staged manifest required')
        targets=await connection.fetch('SELECT * FROM delivery_targets WHERE manifest_id=$1 ORDER BY ordinal',manifest_id)
        if not targets or [plan.obj(t['evidence']) for t in targets]!=body['targets']:
            raise ValueError('complete stored target set required')
        physical=[]
        for t in sorted(body['targets'],key=lambda t:(t['deployment'],t['external_key'])):
            await connection.execute('SELECT pg_advisory_xact_lock(hashtextextended($1,0))','delivery-target:'+t['deployment']+':'+t['external_key'])
            old=await connection.fetchrow('SELECT * FROM delivery_physical_targets WHERE deployment=$1 AND external_key=$2 FOR UPDATE',t['deployment'],t['external_key'])
            base=t['expected_base'];snap={'state':base['state'],'expires_at':int(plan.instant(base['expires_at']).timestamp()) if base['expires_at'] else None}
            if old:
                if (old['account_id']!=m['account_id'] or str(old['grant_id'])!=t['grant_uuid'] or str(old['fence_id'])!=t['fence'] or old['initial_revision']!=base['revision'] or plan.obj(old['initial_base'])!=snap):
                    raise ValueError('physical identity/fence/base transfer requires review')
                physical.append(old)
            elif not dry_run:
                op=uuid4();value=dict(version=3,operation='claim',external_key=t['external_key'],grant_id=t['grant_uuid'],fence_id=t['fence'],operation_id=str(op),revision=base['revision'],expected_state=base['state'],expected_expires_at=base['expires_at'])
                wire=_wire(value);req=_request(wire)
                physical.append(await connection.fetchrow('''INSERT INTO delivery_physical_targets
                    (deployment,external_key,account_id,grant_id,fence_id,claim_operation_id,claim_wire,claim_digest,initial_base,initial_revision,accepted_revision,proven_revision,proven_snapshot)
                    VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$10,$10,$9::jsonb) RETURNING *''',t['deployment'],t['external_key'],m['account_id'],plan.uuid(t['grant_uuid']),plan.uuid(t['fence']),op,wire,req.digest,snap,base['revision']))
        if dry_run:return {'dry_run':True,'required_targets':len(targets),'mapping_proven':False}
        await connection.execute('''INSERT INTO delivery_claim_schedules(manifest_id,evidence,evidence_digest)
            VALUES($1,$2::jsonb,$3) ON CONFLICT(manifest_id) DO NOTHING''',manifest_id,writer_record,plan.digest(writer_record))
        queued=[]
        for p in physical:
            # One unconfirmed physical claim stream. Later manifests join it.
            oid=p['claim_outbox_id']
            if oid is None:
                oid=await _enqueue(connection,CLAIM,'direct-claim:'+str(p['id']),{'physical_id':str(p['id'])})
                await connection.execute('UPDATE delivery_physical_targets SET claim_outbox_id=$2 WHERE id=$1',p['id'],oid)
            elif p['claim_receipt'] is not None:
                # Authenticated handler runs aggregation without issuing another claim.
                oid=await _enqueue(connection,CLAIM,'direct-map:'+str(manifest_id)+':'+str(p['id']),{'physical_id':str(p['id'])})
            queued.append(str(oid))
        return {'dry_run':False,'outbox_ids':queued,'mapping_proven':False}


async def _mapping_proofs(connection, account_id):
    """Only token-validated claim handler calls this, under account/physical locks."""
    await connection.execute('SELECT pg_advisory_xact_lock(hashtextextended($1,0))','delivery-proof:'+str(account_id))
    manifests=await connection.fetch('''SELECT m.* FROM delivery_manifests m JOIN delivery_claim_schedules s ON s.manifest_id=m.id
        WHERE m.account_id=$1 AND m.state='staged' ORDER BY m.id''',account_id)
    for m in manifests:
        targets=await connection.fetch('''SELECT t.*,p.claim_receipt,p.id physical_id,p.fence_id,p.grant_id,p.account_id physical_account
            FROM delivery_targets t LEFT JOIN delivery_physical_targets p ON p.deployment=t.deployment AND p.external_key=t.external_key
            WHERE t.manifest_id=$1 ORDER BY t.ordinal''',m['id'])
        if not targets or any(t['kind']!='direct' or t['claim_receipt'] is None or t['physical_account']!=account_id or str(t['fence_id'])!=plan.obj(t['evidence'])['fence'] or str(t['grant_id'])!=plan.obj(t['evidence'])['grant_uuid'] for t in targets):continue
        evidence={'version':1,'claim_receipts':[{'physical_id':str(t['physical_id']),'receipt':plan.obj(t['claim_receipt'])} for t in targets]}
        await connection.execute('''INSERT INTO delivery_mapping_proofs(manifest_id,receipt_id,manifest_digest,target_digest,evidence)
            VALUES($1,$2,$3,$4,$5::jsonb) ON CONFLICT(manifest_id) DO NOTHING''',m['id'],uuid4(),m['digest'],plan.digest([plan.obj(t['evidence']) for t in targets]),evidence)


async def freeze_and_enqueue(connection, fulfillment_id, manifest_id):
    """Canonical protected planner seam; no send, ownership invention or reward."""
    async with connection.transaction():
        source=await connection.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',fulfillment_id)
        if source and await connection.fetchval('SELECT EXISTS(SELECT 1 FROM capacity_scopes WHERE account_id=$1)',source['account_id']):
            from .mixed_delivery import queue
            await queue(connection,source['id'],'explicit-plan')
            return source
        source=await plan.freeze_plan(connection,fulfillment_id,manifest_id)
        items=await connection.fetch('''SELECT i.*,t.kind,t.deployment,t.external_key,t.evidence FROM delivery_items i
            JOIN delivery_targets t ON t.id=i.target_id WHERE i.fulfillment_id=$1 ORDER BY t.deployment,t.external_key''',fulfillment_id)
        if not items or any(i['kind']!='direct' for i in items):raise ValueError('direct-only nonempty batch required')
        for i in items:
            p=await connection.fetchrow('SELECT * FROM delivery_physical_targets WHERE deployment=$1 AND external_key=$2 FOR UPDATE',i['deployment'],i['external_key'])
            if not p or p['claim_receipt'] is None or p['account_id']!=source['account_id'] or str(p['fence_id'])!=plan.obj(i['evidence'])['fence']:
                raise ValueError('authenticated physical claim required')
            if i['physical_id'] is not None and i['physical_id']!=p['id']:raise ValueError('physical stream mismatch')
            oid=await _enqueue(connection,APPLY,'direct-apply:'+str(i['operation_id']),{'item_id':str(i['id'])})
            await connection.execute('UPDATE delivery_items SET physical_id=$2,outbox_id=$3 WHERE id=$1',i['id'],p['id'],oid)
        return source


async def _applicable(connection, source):
    account=await connection.fetchval("SELECT status='verified' FROM accounts WHERE id=$1",source['account_id'])
    current=await connection.fetchrow('SELECT * FROM entitlements WHERE id=$1',source['entitlement_id'])
    newest=await connection.fetchval('SELECT max(source_sequence) FROM delivery_fulfillments WHERE account_id=$1',source['account_id'])
    now=await connection.fetchval('SELECT clock_timestamp()')
    return bool(account and source['source_sequence'] is not None and source['source_sequence']==newest and current and current['account_id']==source['account_id'] and current['revision']==source['source_revision'] and current['status']=='active' and (current['starts_at'] is None or current['starts_at']<=now) and (current['ends_at'] is None or current['ends_at']>now))


class ExternalDeliveryHandlers:
    def __init__(self, clients):
        # Protected deployment->accepted strict DirectDeliveryClient configuration.
        self.clients=dict(clients)
        if any(not isinstance(client,DirectDeliveryClient) for client in self.clients.values()):
            raise ValueError('accepted strict Unix client required')

    def as_handlers(self):return {CLAIM:self.claim,APPLY:self.apply}

    async def claim(self, connection, operation):
        payload=plan.obj(operation['payload'])
        if set(payload)!={'physical_id'}:return ('failed','invalid_claim_payload')
        pid=plan.uuid(payload['physical_id'])
        p=await connection.fetchrow('SELECT * FROM delivery_physical_targets WHERE id=$1',pid)
        if not p:return ('failed','physical_target_missing')
        async with connection.transaction():
            await plan._owner(connection,p['account_id'])
            p=await connection.fetchrow('SELECT * FROM delivery_physical_targets WHERE id=$1 FOR UPDATE',pid)
            if not await _token(connection,operation):return ('failed','claim_token_lost')
            if p['claim_receipt'] is not None:
                await _mapping_proofs(connection,p['account_id']);return None
            if p['claim_outbox_id']!=operation['id']:return ('failed','claim_stream_pending')
            if p['deployment'] not in self.clients:return ('failed','deployment_not_configured')
            req=_request(p['claim_wire'])
            if not _claim_matches(p,req):return ('failed','claim_wire_mismatch')
            await connection.execute('UPDATE delivery_physical_targets SET claim_started=true WHERE id=$1',pid)
        # No SQL transaction/row/advisory lock spans protected Unix I/O.
        response=await self.clients[p['deployment']].claim(req)
        receipt,observation=_metadata(response,req,now=int(datetime.now(UTC).timestamp()),applicable=False)
        async with connection.transaction():
            await plan._owner(connection,p['account_id'])
            current=await connection.fetchrow('SELECT * FROM delivery_physical_targets WHERE id=$1 FOR UPDATE',pid)
            if not await _token(connection,operation):return ('failed','claim_token_lost')
            if receipt is None or response.delivery_state!='applied' or response.code is not None:return ('failed',response.code or response.delivery_state)
            if not _claim_matches(current,req):return ('failed','claim_intent_changed')
            if receipt['revision']!=current['initial_revision'] or receipt['applied']!=plan.obj(current['initial_base']):return ('failed','claim_proof_mismatch')
            await connection.execute('UPDATE delivery_physical_targets SET claim_receipt=$2::jsonb,accepted_revision=greatest(accepted_revision,$3) WHERE id=$1 AND claim_receipt IS NULL',pid,receipt,response.accepted_revision)
            await _mapping_proofs(connection,p['account_id'])
        return None

    async def _prepare(self,connection,operation,item_id):
        preliminary=await connection.fetchrow('''SELECT f.* FROM delivery_fulfillments f JOIN delivery_items i ON i.fulfillment_id=f.id WHERE i.id=$1''',item_id)
        if not preliminary:return None,'item_missing'
        async with connection.transaction():
            from .common_capacity import admission_lock, plan_membership
            await admission_lock(connection,preliminary['account_id'])
            if not await connection.fetchval("SELECT status='verified' FROM accounts WHERE id=$1 FOR SHARE",preliminary['account_id']):return None,'account_unverified'
            source=await connection.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1 FOR UPDATE',preliminary['id'])
            i=await connection.fetchrow('SELECT * FROM delivery_items WHERE id=$1',item_id)
            p=await connection.fetchrow('SELECT * FROM delivery_physical_targets WHERE id=$1 FOR UPDATE',i['physical_id'])
            if not await _token(connection,operation):return None,'claim_token_lost'
            i=await connection.fetchrow('SELECT * FROM delivery_items WHERE id=$1 FOR UPDATE',item_id)
            if i['outbox_id']!=operation['id'] or not p or p['account_id']!=source['account_id'] or p['claim_receipt'] is None:return None,'mapping_missing'
            if p['deployment'] not in self.clients:return None,'deployment_not_configured'
            if i['outcome']=='superseded':return None,'obsolete_source'
            from .mixed_delivery import source_for_item, current_item
            source=await source_for_item(connection,source,i)
            delivery_order=i['delivery_order'] or source['source_sequence']
            # Prepared/possibly-issued original must never be rewritten or superseded.
            if i['dispatch_started']:
                req=_request(i['wire'],_snapshot(p['proven_snapshot']))
                target=await connection.fetchrow('SELECT * FROM delivery_targets WHERE id=$1',i['target_id'])
                if not _intent_matches(source,i,p,req,target):return None,'wire_source_identity_mismatch'
                return (source,i,p,req),None
            if i['plan_id'] is None:
                membership=await plan_membership(connection,source['account_id'],source['manifest_id'])
                if not membership['complete']:return None,'incomplete_account_plan'
            if source['source_sequence'] is None:return None,'source_order_unproven'
            if not await current_item(connection,source,i) or delivery_order<=p['source_frontier'] or not await _applicable(connection,source):
                await connection.execute("UPDATE delivery_items SET outcome='superseded' WHERE id=$1",item_id)
                return None,'obsolete_or_inapplicable_source'
            # Explicitly supersede only older unsent sources, before any dispatch marker.
            await connection.execute('''UPDATE delivery_items x SET outcome='superseded'
                FROM delivery_fulfillments f WHERE x.fulfillment_id=f.id AND x.physical_id=$1
                AND x.id<>$2 AND NOT x.dispatch_started AND x.application_receipt IS NULL
                AND coalesce(x.delivery_order,f.source_sequence)<$3''',p['id'],item_id,delivery_order)
            blocking=await connection.fetchval('''SELECT EXISTS(SELECT 1 FROM delivery_items x
                JOIN delivery_fulfillments f ON f.id=x.fulfillment_id WHERE x.physical_id=$1 AND x.id<>$2
                AND (x.dispatch_started AND x.application_receipt IS NULL OR
                NOT x.dispatch_started AND x.outcome NOT IN ('blocked','superseded') AND coalesce(x.delivery_order,f.source_sequence)<$3))''',p['id'],item_id,delivery_order)
            if blocking or p['accepted_revision']!=p['proven_revision']:
                await _park_dependency(connection,operation)
                return None,DEPENDENCY_WAIT
            desired=plan.obj(i['desired'])
            if desired['source_digest']!=source['snapshot_digest']:return None,'source_digest_mismatch'
            if desired['expires_at'] is None:
                await connection.execute("UPDATE delivery_items SET outcome='blocked' WHERE id=$1",item_id)
                return None,'unsupported_nonfinite_expiry'
            expiry=plan.instant(desired['expires_at'])
            if desired['state']=='active' and expiry<=await connection.fetchval('SELECT clock_timestamp()'):
                await connection.execute("UPDATE delivery_items SET outcome='blocked' WHERE id=$1",item_id)
                return None,'source_expiry_elapsed'
            if p['accepted_revision']==2**63-1:return None,'revision_exhausted'
            revision=p['accepted_revision']+1
            value=dict(version=3,operation='apply',external_key=p['external_key'],grant_id=str(p['grant_id']),fence_id=str(p['fence_id']),operation_id=str(i['operation_id']),revision=revision,expected_revision=p['accepted_revision'],desired_state=desired['state'],expires_at=desired['expires_at'])
            wire=_wire(value);req=_request(wire,_snapshot(p['proven_snapshot']))
            await connection.execute('''UPDATE delivery_items SET preparation='prepared',expected_revision=$2,request=$3::jsonb,request_digest=$4,wire=$5,delivery_revision=$6,dispatch_started=true WHERE id=$1''',item_id,p['accepted_revision'],value,req.digest,wire,revision)
            await connection.execute('UPDATE delivery_physical_targets SET accepted_revision=$2,source_frontier=$3 WHERE id=$1',p['id'],revision,delivery_order)
            i=await connection.fetchrow('SELECT * FROM delivery_items WHERE id=$1',item_id)
            return (source,i,p,req),None

    async def apply(self,connection,operation):
        payload=plan.obj(operation['payload'])
        if set(payload)!={'item_id'}:return ('failed','invalid_apply_payload')
        prepared,error=await self._prepare(connection,operation,plan.uuid(payload['item_id']))
        if error:return ('failed',error)
        source,item,p,req=prepared
        response=await self.clients[p['deployment']].apply(req)
        async with connection.transaction():
            await connection.fetchval('SELECT id FROM accounts WHERE id=$1 FOR SHARE',source['account_id'])
            source=await connection.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1 FOR UPDATE',source['id'])
            physical=await connection.fetchrow('SELECT * FROM delivery_physical_targets WHERE id=$1 FOR UPDATE',p['id'])
            # Exact claim token locked in the SAME transaction as proof/revision writes.
            if not await _token(connection,operation):return ('failed','claim_token_lost')
            current=await connection.fetchrow('SELECT * FROM delivery_items WHERE id=$1 FOR UPDATE',item['id'])
            target=await connection.fetchrow('SELECT * FROM delivery_targets WHERE id=$1',current['target_id'])
            from .mixed_delivery import source_for_item, queue
            source=await source_for_item(connection,source,current)
            if current['wire']!=req.body.decode() or not _intent_matches(source,current,physical,req,target):return ('failed','intent_changed')
            await connection.execute('UPDATE delivery_physical_targets SET accepted_revision=greatest(accepted_revision,$2) WHERE id=$1',p['id'],response.accepted_revision)
            receipt,observation=_metadata(response,req,now=int(datetime.now(UTC).timestamp()),applicable=await _applicable(connection,source))
            historical=response.historical_fulfilled(req)
            if historical:
                if current['application_receipt'] is not None and plan.obj(current['application_receipt'])!=receipt:return ('failed','immutable_proof_mismatch')
                await connection.execute('''UPDATE delivery_items SET application_receipt=$2::jsonb,observation=$3::jsonb,outcome='applied' WHERE id=$1''',item['id'],receipt,observation)
                if req.revision>physical['proven_revision']:
                    # Historical receipt never lowers latest physical projection.
                    await connection.execute('UPDATE delivery_physical_targets SET proven_revision=$2,proven_snapshot=$3::jsonb WHERE id=$1',p['id'],req.revision,receipt['applied'])
            else:
                await connection.execute('UPDATE delivery_items SET observation=$2::jsonb,outcome=$3 WHERE id=$1',item['id'],observation,'conflict' if response.delivery_state=='conflict' else 'pending')
            if historical:
                await _release_dependencies(connection,p['id'])
                if await connection.fetchval('SELECT EXISTS(SELECT 1 FROM capacity_scopes WHERE account_id=$1)',source['account_id']):
                    await queue(connection,source['id'],['direct-proof',str(current['id'])])
            recoverable_readback=(not historical and response.delivery_state=='pending' and response.code=='wdtt_readback_failed')
            if not historical or response.delivery_state!='applied':
                if not recoverable_readback:return ('failed',response.code or response.delivery_state)
        if recoverable_readback:
            raise AcknowledgedReadbackRetry()
        return None


async def fulfillment_proof(connection, fulfillment_id):
    """Evidence only; recheck lifecycle/expiry before any current-observation projection."""
    source=await connection.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1',fulfillment_id)
    if source and await connection.fetchval('SELECT EXISTS(SELECT 1 FROM capacity_scopes WHERE account_id=$1)',source['account_id']):
        from .mixed_delivery import readiness
        return await readiness(connection,fulfillment_id)
    rows=await connection.fetch("""SELECT i.*,p.proven_revision,p.proven_snapshot FROM delivery_items i
        LEFT JOIN delivery_physical_targets p ON p.id=i.physical_id WHERE i.fulfillment_id=$1""",fulfillment_id)
    complete=bool(rows) and all(r['application_receipt'] is not None for r in rows)
    from .common_capacity import plan_membership
    membership=await plan_membership(connection,source['account_id'],source['manifest_id']) if source and source['manifest_id'] else {'complete':True,'reason':None}
    complete=complete and membership['complete']
    now=int((await connection.fetchval('SELECT clock_timestamp()')).timestamp())
    current=complete and source is not None and await _applicable(connection,source)
    for r in rows:
        snap=plan.obj(r['proven_snapshot'] or {})
        current=current and plan.obj(r['observation'] or {}).get('observed_current_usable') is True and r['proven_revision']==r['delivery_revision'] and snap.get('state')=='active' and snap.get('expires_at') is not None and snap['expires_at']>now
    return {'transport_complete':complete,'plan_status':membership.get('reason') or 'complete_target_set','required_items':len(rows),
        'historical_application_items':sum(r['application_receipt'] is not None for r in rows),
        'all_observed_current_usable':bool(current),'business_completed':False}
