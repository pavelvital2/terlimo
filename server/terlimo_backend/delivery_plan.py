"""Private storage/planning boundary. No dispatch, ownership promotion or live admission.

Capture helpers require the original source transaction and its existing owner locks.
Stage/plan acquire owner SHARE first; never call them from a benefit-locked transaction.
No callable here writes delivery_mapping_proofs. Tests seed explicitly synthetic receipts.
"""
from __future__ import annotations

import hashlib
import json
import re
from datetime import datetime, timezone, timedelta
from uuid import UUID, uuid5


def obj(value):
    while isinstance(value, str):
        value = json.loads(value)
    return value


def packed(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=True)


def digest(value):
    return hashlib.sha256(packed(value).encode()).hexdigest()


def instant(value):
    if value is None:
        return None
    dt = datetime.fromisoformat(value.replace('Z', '+00:00')) if isinstance(value, str) else value
    if dt.tzinfo is None:
        raise ValueError('timezone required')
    return dt.astimezone(timezone.utc)


def stamp(value):
    return instant(value).isoformat() if value is not None else None


def uuid(value):
    parsed = UUID(str(value))
    if str(parsed) != str(value):
        raise ValueError('canonical UUID required')
    return parsed


async def _owner(connection, account_id):
    if not await connection.fetchval("SELECT status='verified' FROM accounts WHERE id=$1 FOR SHARE", account_id):
        raise ValueError('verified account required')


async def _snapshot(connection, entitlement, *, at, commercial):
    extras = await connection.fetch('SELECT id,expires_at FROM paid_extra_slots WHERE entitlement_id=$1 ORDER BY expires_at DESC,id', entitlement['id'])
    snapshot = {
        'version': 1, 'account_id': str(entitlement['account_id']),
        'entitlement_id': str(entitlement['id']), 'revision': entitlement['revision'],
        'kind': entitlement['kind'], 'status': entitlement['status'],
        'starts_at': stamp(entitlement['starts_at']), 'ends_at': stamp(entitlement['ends_at']),
        'captured_at': stamp(at), 'device_limit': entitlement['device_limit'],
        'base_limit': entitlement['paid_base_device_limit'],
        'extras': [{'slot_id': str(r['id']), 'expires_at': stamp(r['expires_at'])} for r in extras],
        'source_plan': obj(entitlement['source_plan']), 'commercial': commercial,
    }
    snapshot['rank_deadlines'] = [stamp(d) for d in capacity_deadlines(snapshot)]
    return snapshot


def capacity_deadlines(snapshot):
    """Absolute per-rank deadlines at original capture, never now+duration.

    Finite paid = base seats + descending still-live extra deadlines, capped by main
    end. Slot IDs are evidence, not device assignment. Trial/imported/nonfinite use
    the existing stored device limit (default two). No live admission caller yet.
    """
    end = instant(snapshot['ends_at'])
    if snapshot['kind'] == 'paid' and end is not None:
        extras = sorted((instant(e['expires_at']) for e in snapshot['extras']
                         if instant(e['expires_at']) > instant(snapshot['captured_at'])), reverse=True)
        return [end] * snapshot['base_limit'] + [min(end, e) for e in extras]
    return [end] * (snapshot['device_limit'] or 2)


async def capture_paid(connection, order_id):
    """Only original paid apply calls this before commit; never backfill old credits."""
    if not connection.is_in_transaction():
        raise ValueError('original transaction required')
    old = await connection.fetchrow('SELECT * FROM delivery_fulfillments WHERE source_order_id=$1', order_id)
    if old:
        return old
    order = await connection.fetchrow('SELECT * FROM payment_orders WHERE id=$1', order_id)
    if not order or not order['applied_entitlement_id']:
        raise ValueError('applied account order required')
    ent = await connection.fetchrow('SELECT * FROM entitlements WHERE id=$1', order['applied_entitlement_id'])
    owner_id=order['trusted_owner_account_id'] if order['owner_kind']=='telegram_account' else order['account_id']
    if order['owner_kind']!='telegram_account' and not await connection.fetchval('SELECT EXISTS(SELECT 1 FROM capacity_scopes WHERE account_id=$1)',owner_id):raise ValueError('mapped mobile original capture required')
    if ent['account_id'] != owner_id or ent['revision'] != order['credited_entitlement_revision']:
        raise ValueError('original revision required')
    snap = await _snapshot(connection, ent, at=await connection.fetchval('SELECT clock_timestamp()'), commercial={
        'order_id': str(order_id), 'quote': obj(order['quote']), 'credited_product': obj(order['credited_product']),
        'amount': str(order['amount']), 'currency': order['currency'], 'months': order['months']})
    return await _capture(connection, ent, 'paid', order_id, snap)


async def capture_trial(connection, row, moment):
    """Only trusted insert calls this; mobile/racing preexisting trials are not backfilled."""
    if not connection.is_in_transaction() or row['kind'] != 'trial' or row['revision'] != 1:
        raise ValueError('original trial insert transaction required')
    old = await connection.fetchrow("SELECT * FROM delivery_fulfillments WHERE source_kind='trial' AND entitlement_id=$1", row['id'])
    if old:
        return old
    return await _capture(connection, row, 'trial', None,
                          await _snapshot(connection, row, at=moment, commercial=None))


async def _capture(connection, ent, kind, order_id, snap):
    # Caller already owns order/account/benefit/paid-account locks. Insert FKs use
    # compatible KEY SHARE; no late account/advisory lock introduced here.
    sequence = await connection.fetchval('''INSERT INTO delivery_source_counters(account_id,sequence)
        VALUES($1,1) ON CONFLICT(account_id) DO UPDATE
        SET sequence=delivery_source_counters.sequence+1 RETURNING sequence''', ent['account_id'])
    source=await connection.fetchrow('''INSERT INTO delivery_fulfillments
        (account_id,source_kind,source_order_id,entitlement_id,source_revision,snapshot,snapshot_digest,source_sequence)
        VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,$8) RETURNING *''', ent['account_id'], kind,
        order_id, ent['id'], ent['revision'], snap, digest(snap), sequence)
    if await connection.fetchval('SELECT EXISTS(SELECT 1 FROM capacity_scopes WHERE account_id=$1)',ent['account_id']):
        from .mixed_delivery import queue
        await queue(connection,source['id'],'capture')
    return source


def validate_manifest(body, allowlist):
    """Allowlist is protected operator input: deployment -> key -> exact permitted account/grant.
    No endpoint, secrets, arbitrary proof boolean or ownership status accepted.
    """
    if type(body) is not dict or set(body) != {'id','account_id','evidence_sha256','targets'}:
        raise ValueError('closed manifest required')
    uuid(body['id']); uuid(body['account_id'])
    h = body['evidence_sha256']
    if type(h) is not str or len(h) != 64 or any(c not in '0123456789abcdef' for c in h):
        raise ValueError('evidence hash required')
    if type(body['targets']) is not list or not 1 <= len(body['targets']) <= 256:
        raise ValueError('nonempty bounded target set required')
    keys = set()
    for t in body['targets']:
        if type(t) is not dict:
            raise ValueError('target object required')
        if t.get('kind') == 'mobile':
            if set(t) != {'kind','binding_id','bound_at'}:
                raise ValueError('mobile shape')
            uuid(t['binding_id']); instant(t['bound_at'])
            key = ('mobile',t['binding_id'])
        elif t.get('kind') == 'direct':
            if set(t) != {'kind','deployment','external_key','grant_uuid','fence','expected_base','device_index'}:
                raise ValueError('direct shape')
            uuid(t['grant_uuid']); uuid(t['fence'])
            if type(t['device_index']) is not int or t['device_index'] < 1:
                raise ValueError('device index')
            if (type(t['deployment']) is not str or not re.fullmatch(r'[A-Za-z0-9._-]{1,64}',t['deployment'])
                    or type(t['external_key']) is not str or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9:._-]{0,127}',t['external_key'])):
                raise ValueError('target identity')
            if str(uuid5(UUID('97ab0503-5926-51b2-9a62-741f65f11845'),t['external_key'])) != t['grant_uuid']:
                raise ValueError('deterministic grant mismatch')
            if allowlist.get(t['deployment'], {}).get(t['external_key']) != {'account_id':body['account_id'],'grant_uuid':t['grant_uuid']}:
                raise ValueError('deployment/key/account/grant not allowlisted')
            base = t['expected_base']
            if type(base) is not dict or set(base) != {'state','expires_at','revision'} or base['state'] not in ('active','inactive','absent'):
                raise ValueError('expected base')
            if type(base['revision']) is not int or not 0 <= base['revision'] < 2**63:
                raise ValueError('base revision')
            if base['expires_at'] is not None:
                raw = base['expires_at']
                if type(raw) is not str or len(raw)>40:
                    raise ValueError('base deadline')
                parsed = datetime.fromisoformat(raw.replace('Z','+00:00'))
                if parsed.tzinfo is None or parsed.utcoffset()!=timedelta(0) or parsed.microsecond or parsed.timestamp()<0:
                    raise ValueError('base deadline must be UTC whole seconds')
            if base['state'] != 'absent' and base['expires_at'] is None:
                raise ValueError('existing base deadline required')
            if base['state'] == 'absent' and base['expires_at'] is not None:
                raise ValueError('absent deadline')
            key = ('direct',t['deployment'],t['external_key'])
        else:
            raise ValueError('target kind')
        if key in keys:
            raise ValueError('duplicate target')
        keys.add(key)
    return body


async def stage_manifest(connection, body, allowlist, *, dry_run=True):
    validate_manifest(body, allowlist)
    aid, mid = uuid(body['account_id']), uuid(body['id'])
    async with connection.transaction():
        await _owner(connection, aid)
        # Serialize only this private registry; source hooks don't acquire this lock.
        await connection.execute('SELECT pg_advisory_xact_lock(hashtextextended($1,0))', 'delivery-stage:'+str(aid))
        old = await connection.fetchrow('SELECT * FROM delivery_manifests WHERE id=$1', mid)
        if old:
            if old['digest'] != digest(body) or old['account_id'] != aid:
                raise ValueError('manifest conflict')
            return {'id':str(mid),'state':old['state'],'conflicts':obj(old['conflicts']),'replay':True}
        # Stable physical identity cannot move to another account in a new manifest.
        # All target locks are sorted after owner SHARE/private stage lock; no source
        # writer takes them. Same-account manifests may retain the same identity.
        direct_keys = sorted((t['deployment'],t['external_key']) for t in body['targets'] if t['kind']=='direct')
        for deployment,key in direct_keys:
            await connection.execute('SELECT pg_advisory_xact_lock(hashtextextended($1,0))', 'delivery-target:'+deployment+':'+key)
            if await connection.fetchval('SELECT EXISTS(SELECT 1 FROM delivery_targets WHERE deployment=$1 AND external_key=$2 AND account_id<>$3)',deployment,key,aid):
                raise ValueError('mapping owner conflict')
        bindings = await connection.fetch("SELECT id,bound_at FROM account_bindings WHERE account_id=$1 AND status='active' ORDER BY bound_at,id", aid)
        wanted = [t for t in body['targets'] if t['kind']=='mobile']
        actual = [{'kind':'mobile','binding_id':str(b['id']),'bound_at':stamp(b['bound_at'])} for b in bindings]
        # Foreign/inactive identifiers are errors, not a permitted mapping conflict.
        for t in wanted:
            if not any(b['binding_id']==t['binding_id'] and instant(b['bound_at'])==instant(t['bound_at']) for b in actual):
                raise ValueError('foreign/inactive binding or stale bound_at')
        conflicts = []
        if [t['binding_id'] for t in wanted] != [t['binding_id'] for t in actual]:
            conflicts.append('incomplete_or_reordered_mobile_set')
        direct = [t for t in body['targets'] if t['kind']=='direct']
        if direct and wanted:
            conflicts.append('mixed_order_requires_cutover')
        if [t['device_index'] for t in direct] != list(range(1,len(direct)+1)):
            conflicts.append('direct_order_conflict')
        ent = await connection.fetchrow("""SELECT * FROM entitlements WHERE account_id=$1
            AND kind IN ('paid','trial','imported') AND status='active'
            AND (starts_at IS NULL OR starts_at<=now()) AND (ends_at IS NULL OR ends_at>now())
            ORDER BY created_at DESC LIMIT 1""",aid)
        if ent:
            snap = await _snapshot(connection, ent, at=await connection.fetchval('SELECT now()'), commercial=None)
            if len(body['targets']) > len(capacity_deadlines(snap)):
                conflicts.append('overcapacity')
        else:
            conflicts.append('no_current_capacity')
        result = {'id':str(mid),'state':'conflict' if conflicts else 'staged','conflicts':conflicts,'replay':False}
        if not dry_run:
            await connection.execute('INSERT INTO delivery_manifests(id,account_id,digest,body,state,conflicts) VALUES($1,$2,$3,$4::jsonb,$5,$6::jsonb)',mid,aid,digest(body),body,result['state'],conflicts)
            for i,t in enumerate(body['targets'],1):
                await connection.execute('''INSERT INTO delivery_targets(manifest_id,account_id,ordinal,kind,binding_id,deployment,external_key,evidence)
                    VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb)''',mid,aid,i,t['kind'],uuid(t['binding_id']) if t['kind']=='mobile' else None,t.get('deployment'),t.get('external_key'),t)
        return result


async def freeze_plan(connection, fulfillment_id, manifest_id):
    """Private future-proof consumer. No proof writer exists in this slice.

    Tests insert synthetic proof rows explicitly. Staged rows alone always fail.
    All items stay unprepared; queuehead request preparation needs a later worker.
    """
    async with connection.transaction():
        aid = await connection.fetchval('SELECT account_id FROM delivery_fulfillments WHERE id=$1',fulfillment_id)
        if aid is None:
            raise ValueError('unknown source')
        from .common_capacity import admission_lock, plan_membership
        await admission_lock(connection,aid)
        await _owner(connection,aid)
        membership = await plan_membership(connection,aid,manifest_id)
        if not membership['complete']:raise ValueError('incomplete_account_plan')
        source = await connection.fetchrow('SELECT * FROM delivery_fulfillments WHERE id=$1 FOR UPDATE',fulfillment_id)
        if source['state']=='planned':
            if source['manifest_id']!=manifest_id:
                raise ValueError('frozen target set conflict')
            return source
        manifest = await connection.fetchrow('SELECT * FROM delivery_manifests WHERE id=$1',manifest_id)
        proof = await connection.fetchrow('SELECT * FROM delivery_mapping_proofs WHERE manifest_id=$1',manifest_id)
        if not manifest or manifest['account_id']!=aid or manifest['state']!='staged' or not proof or proof['manifest_digest']!=manifest['digest']:
            raise ValueError('complete authenticated mapping proof required')
        targets = await connection.fetch('SELECT * FROM delivery_targets WHERE manifest_id=$1 ORDER BY ordinal',manifest_id)
        evidence = [obj(t['evidence']) for t in targets]
        if not targets or proof['target_digest']!=digest(evidence):
            raise ValueError('target proof mismatch')
        snap = obj(source['snapshot']); deadlines = [instant(d) for d in snap['rank_deadlines']]
        if len(targets)>len(deadlines):
            raise ValueError('source capacity conflict')
        moment = await connection.fetchval('SELECT clock_timestamp()')
        for i,t in enumerate(targets):
            rank=membership['ranks'].get(t['id'],i+1)
            if rank>len(deadlines):raise ValueError('source capacity conflict')
            deadline=deadlines[rank-1]
            # v3 accepts whole UTC seconds only. Floor once, never round access up.
            if t['kind']=='direct' and deadline is not None:
                deadline=deadline.replace(microsecond=0)
            desired = desired_for_rank(snap,rank,deadline,moment,source['snapshot_digest'])
            await connection.execute('INSERT INTO delivery_items(fulfillment_id,target_id,desired) VALUES($1,$2,$3::jsonb)',fulfillment_id,t['id'],desired)
        return await connection.fetchrow("UPDATE delivery_fulfillments SET state='planned',manifest_id=$2,target_digest=$3 WHERE id=$1 RETURNING *",fulfillment_id,manifest_id,digest(evidence))


def desired_for_rank(snapshot, rank, deadline, moment, source_digest):
    """Late planning preserves the original deadline, never issues a new duration."""
    started = snapshot['starts_at'] is None or instant(snapshot['starts_at']) <= moment
    active = snapshot['status']=='active' and started and (deadline is None or deadline>moment)
    return {'rank':rank,'state':'active' if active else 'inactive',
            'starts_at':snapshot['starts_at'],'expires_at':stamp(deadline),
            'source_digest':source_digest}
