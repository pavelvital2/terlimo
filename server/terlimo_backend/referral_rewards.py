"""Durable referral rewards through ordinary entitlement/grant/readback writers.

Enqueue is part of the actual source activation transaction. WAITING never creates
an entitlement; extension is fenced by the account's existing paid writer lock.
APPLYING records the single core extension and survives failed gateway retries.
"""
from __future__ import annotations

import json
from datetime import timedelta
from typing import Any

from .auth_api import now_utc
from .gateway_control import ensure_grant


async def trial_bonus_days(connection, account_id: Any) -> int:
    return 3 if await connection.fetchval("""
        SELECT 1 FROM accounts a JOIN referral_benefits b ON b.account_id=a.id
        WHERE a.id=$1 AND a.referred_by_account_id IS NOT NULL AND b.history_state='ready' AND NOT b.imported_trial_used
        """, account_id) else 0


async def enqueue_reward(connection, *, account_id: Any, event_kind: str,
                         source_entitlement_id: Any, source_order_id: Any = None,
                         months: int | None = None) -> Any:
    if event_kind == 'trial':
        days = 7
    elif event_kind == 'first_main_paid' and months in (1, 3, 6):
        days = {1: 7, 3: 14, 6: 30}[months]
    else:
        raise ValueError('unsupported referral reward source')
    # Do not infer legacy eligibility from absence of local payment/trial history.
    return await connection.fetchval("""
        INSERT INTO referral_rewards
          (invitee_account_id, inviter_account_id,event_kind,source_entitlement_id,source_order_id,days,state)
        SELECT a.id,a.referred_by_account_id,$2,$3,$4,$5,'WAITING'
        FROM accounts a JOIN referral_benefits b ON b.account_id=a.id
        WHERE a.id=$1 AND a.referred_by_account_id IS NOT NULL AND b.history_state='ready'
          AND EXISTS(SELECT 1 FROM entitlements e WHERE e.id=$3 AND e.account_id=a.id AND e.status='active')
          AND ($2<>'trial' OR NOT b.imported_trial_used)
          AND ($2<>'first_main_paid' OR (NOT b.imported_first_main_paid
            AND b.first_paid_order_id IS NOT NULL AND b.first_paid_order_id=$4
            AND EXISTS(SELECT 1 FROM payment_orders source WHERE source.id=$4
                AND source.account_id=a.id AND source.status='succeeded'
                AND source.applied_entitlement_id=$3 AND source.months=$6)))
        ON CONFLICT(invitee_account_id,event_kind) DO NOTHING RETURNING id
        """, account_id, event_kind, source_entitlement_id, source_order_id, days, months)


def _confirmed(grant, *, generation: int | None = None, moment=None) -> bool:
    moment = moment or now_utc()
    data = grant['last_readback']
    if isinstance(data, str):
        try:
            data = json.loads(data)
        except ValueError:
            return False
    return (grant['state'] == 'applied'
            and grant['applied_generation'] == grant['desired_generation']
            and (generation is None or grant['applied_generation'] >= generation)
            and grant['applied_not_after'] is not None and grant['applied_not_after'] > moment
            and isinstance(data, dict) and data.get('runtime_applied') is True
            and data.get('revoked') is False)


async def record_trial_target(connection, *, entitlement_id, grant_id, generation):
    """Track the current ordinary generation for this same active trial source.

    A refresh can supersede a failed lease, but neither an older generation nor
    a commercial/foreign/revoked grant can become evidence for the trial.
    """
    await connection.execute("""
        INSERT INTO referral_trial_targets(source_entitlement_id,grant_id,generation)
        SELECT e.id,g.id,$3::integer FROM entitlements e
        JOIN account_bindings b ON b.account_id=e.account_id AND b.status='active'
        JOIN installations i ON i.id=b.installation_id AND i.state<>'revoked'
        JOIN grants g ON g.binding_id=b.id AND g.id=$2 AND g.hour_entitlement_id IS NULL
        WHERE e.id=$1 AND e.kind='trial' AND e.source_plan->>'referral_bonus_days'='3'
          AND e.status='active' AND e.starts_at<=now() AND e.ends_at>now()
          AND g.state<>'revoked' AND g.desired_generation=$3::integer
          AND NOT EXISTS(SELECT 1 FROM entitlements commercial WHERE commercial.account_id=e.account_id
            AND commercial.kind IN ('paid','imported') AND commercial.status='active'
            AND commercial.starts_at<=now() AND (commercial.ends_at IS NULL OR commercial.ends_at>now()))
        ON CONFLICT(source_entitlement_id,grant_id) DO UPDATE
          SET generation=EXCLUDED.generation
          WHERE referral_trial_targets.generation<EXCLUDED.generation
        """,entitlement_id,grant_id,generation)


async def confirm_trial_target(connection, *, grant_id):
    """Called atomically with gateway readback publication, not on a VPN UI event."""
    if await connection.fetchval('SELECT EXISTS(SELECT 1 FROM grants g JOIN account_bindings b ON b.id=g.binding_id JOIN capacity_scopes s ON s.account_id=b.account_id WHERE g.id=$1)',grant_id):return 0
    rows = await connection.fetch("""
        SELECT e.id,e.account_id,t.generation FROM referral_trial_targets t
        JOIN entitlements e ON e.id=t.source_entitlement_id
        JOIN grants g ON g.id=t.grant_id AND g.hour_entitlement_id IS NULL AND g.state<>'revoked'
        JOIN account_bindings b ON b.id=g.binding_id AND b.account_id=e.account_id AND b.status='active'
        JOIN installations i ON i.id=b.installation_id AND i.state<>'revoked'
        WHERE t.grant_id=$1 AND e.kind='trial' AND e.status='active' AND e.starts_at<=now() AND e.ends_at>now()
          AND NOT EXISTS(SELECT 1 FROM entitlements commercial WHERE commercial.account_id=e.account_id
            AND commercial.kind IN ('paid','imported') AND commercial.status='active'
            AND commercial.starts_at<=now() AND (commercial.ends_at IS NULL OR commercial.ends_at>now()))
        """,grant_id)
    grant = await connection.fetchrow("SELECT * FROM grants WHERE id=$1",grant_id)
    if grant is None:
        return 0
    created = 0
    for row in rows:
        # Exact latest correlated trial generation; a paid apply cannot prove it.
        if grant['desired_generation'] == row['generation'] and _confirmed(grant,generation=row['generation']):
            created += bool(await enqueue_reward(connection,account_id=row['account_id'],event_kind='trial',source_entitlement_id=row['id']))
    return created


async def sweep_trial_rewards(connection, *, limit: int = 100) -> int:
    rows = await connection.fetch("""SELECT DISTINCT t.grant_id FROM referral_trial_targets t
        JOIN entitlements e ON e.id=t.source_entitlement_id
        WHERE NOT EXISTS(SELECT 1 FROM referral_rewards r WHERE r.invitee_account_id=e.account_id AND r.event_kind='trial')
        ORDER BY t.grant_id LIMIT $1""",limit)
    created = 0
    for row in rows:
        async with connection.transaction():
            created += await confirm_trial_target(connection,grant_id=row['grant_id'])
    return created


async def sweep_rewards(connection, settings, *, limit: int = 100) -> int:
    """Extend one existing active finite right, then prove its ordinary grant targets.

    No registration, no new slots, no resurrection. Missing active right or binding
    leaves WAITING. APPLYING retries enqueue/readback without extending a second time.
    """
    # updated_at is the last examination time, including unavailable WAITING
    # receipts. Rotating each inspected receipt makes a bounded batch fair without
    # expiring rewards, adding a cursor table, or scanning the complete backlog.
    rows = await connection.fetch("SELECT id,inviter_account_id FROM referral_rewards WHERE state IN ('WAITING','APPLYING') ORDER BY updated_at,id LIMIT $1",limit)
    applied = 0
    for source in rows:
        async with connection.transaction():
            from .gateway_control import grant_owner_lock
            await grant_owner_lock(connection,source['inviter_account_id'])
            await connection.execute("SELECT pg_advisory_xact_lock(hashtextextended($1,0))",f"paid-account:{source['inviter_account_id']}")
            reward = await connection.fetchrow("SELECT * FROM referral_rewards WHERE id=$1 FOR UPDATE",source['id'])
            if reward['state'] == 'APPLIED':
                continue
            await connection.execute("UPDATE referral_rewards SET updated_at=clock_timestamp() WHERE id=$1",reward['id'])
            if reward['state'] == 'WAITING':
                entitlement = await connection.fetchrow("""
                    SELECT * FROM entitlements WHERE account_id=$1 AND kind IN ('paid','imported','trial')
                      AND status='active' AND starts_at<=now() AND ends_at>now()
                    ORDER BY CASE WHEN kind IN ('paid','imported') THEN 0 ELSE 1 END,ends_at DESC,id
                    LIMIT 1 FOR UPDATE
                    """,reward['inviter_account_id'])
                if entitlement is None:
                    continue
                grants = await connection.fetch("""
                    SELECT g.* FROM grants g JOIN account_bindings b ON b.id=g.binding_id
                    WHERE b.account_id=$1 AND b.status='active' AND g.hour_entitlement_id IS NULL AND g.state<>'revoked'
                    ORDER BY g.binding_id,g.gateway_id
                    """,reward['inviter_account_id'])
                eligible = []
                for grant in grants:
                    if not _confirmed(grant):
                        continue
                    outcome = await ensure_grant(connection,binding_id=grant['binding_id'],gateway_id=grant['gateway_id'],
                        entitlement_id=entitlement['id'],max_lease_seconds=settings.gateway_max_lease_seconds)
                    if outcome in ('enqueued','unchanged','pending'):
                        eligible.append(grant)
                grants = eligible
                if not grants:
                    continue
                target_end = entitlement['ends_at'] + timedelta(days=reward['days'])
                revision = await connection.fetchval("UPDATE entitlements SET ends_at=$2,revision=revision+1 WHERE id=$1 RETURNING revision",entitlement['id'],target_end)
                await connection.execute("""UPDATE referral_rewards SET state='APPLYING',target_entitlement_id=$2,
                    target_base_ends_at=$3,target_ends_at=$4,target_revision=$5,updated_at=now() WHERE id=$1""",
                    reward['id'],entitlement['id'],entitlement['ends_at'],target_end,revision)
            else:
                entitlement = await connection.fetchrow("SELECT * FROM entitlements WHERE id=$1",reward['target_entitlement_id'])
                if entitlement is None or entitlement['status'] != 'active' or entitlement['ends_at'] <= now_utc():
                    continue
                grants = await connection.fetch("""SELECT g.* FROM grants g JOIN account_bindings b ON b.id=g.binding_id
                    WHERE b.account_id=$1 AND b.status='active' AND g.hour_entitlement_id IS NULL AND g.state<>'revoked'
                    ORDER BY g.binding_id,g.gateway_id""",reward['inviter_account_id'])
            frozen = reward['grant_targets']
            if isinstance(frozen,str):
                frozen = json.loads(frozen)
            targets = dict(frozen or {})
            if targets:
                grants = [g for g in grants if str(g['id']) in targets]
            confirmed = bool(grants) and (not targets or len(grants) == len(targets))
            for grant in grants:
                outcome = await ensure_grant(connection,binding_id=grant['binding_id'],gateway_id=grant['gateway_id'],
                    entitlement_id=entitlement['id'],max_lease_seconds=settings.gateway_max_lease_seconds)
                current = await connection.fetchrow("SELECT * FROM grants WHERE id=$1",grant['id'])
                targets.setdefault(str(grant['id']),current['desired_generation'])
                confirmed = confirmed and outcome in ('enqueued','unchanged','pending') and _confirmed(current,generation=targets[str(grant['id'])])
            await connection.execute("UPDATE referral_rewards SET grant_targets=$2::jsonb,state=$3,updated_at=now() WHERE id=$1",
                                     reward['id'],json.dumps(targets),'APPLIED' if confirmed else 'APPLYING')
            applied += bool(confirmed)
    return applied


def validate_import_evidence(entry):
    """Protected legacy receipt, including intermediate states that must NOT be replayed."""
    import re
    from datetime import datetime
    fields = {'source_id','invitee_telegram_id','inviter_telegram_id','event_kind','days','state','evidence'}
    if not isinstance(entry,dict) or set(entry) != fields:
        raise ValueError('invalid reward evidence shape')
    if (not isinstance(entry['source_id'],str) or not 1 <= len(entry['source_id']) <= 256
        or type(entry['days']) is not int or entry['days'] <= 0
        or entry['event_kind'] not in ('trial','first_main_paid')
        or entry['state'] not in ('WAITING','APPLIED','PENDING_REMOTE','REMOTE_APPLIED')
        or any(type(entry[k]) is not int or not 0 < entry[k] < 2**63 for k in ('invitee_telegram_id','inviter_telegram_id'))
        or entry['invitee_telegram_id'] == entry['inviter_telegram_id']):
        raise ValueError('invalid reward evidence')
    proof = entry['evidence']
    if not isinstance(proof,dict) or set(proof) != {'source_reference','source_sha256','target'}:
        raise ValueError('invalid source evidence')
    if (not isinstance(proof['source_reference'],str) or not 1 <= len(proof['source_reference']) <= 256
        or not isinstance(proof['source_sha256'],str) or not re.fullmatch('[0-9a-f]{64}',proof['source_sha256'])):
        raise ValueError('invalid source evidence')
    target = proof['target']
    if entry['state'] == 'WAITING':
        if target is not None:
            raise ValueError('WAITING cannot discard an existing remote target')
    else:
        if not isinstance(target,dict) or set(target) != {'reference','sha256','base_ends_at','target_ends_at'}:
            raise ValueError('missing target evidence')
        if (not isinstance(target['reference'],str) or not 1 <= len(target['reference']) <= 256
            or not isinstance(target['sha256'],str) or not re.fullmatch('[0-9a-f]{64}',target['sha256'])):
            raise ValueError('invalid target evidence')
        base = datetime.fromisoformat(target['base_ends_at'])
        end = datetime.fromisoformat(target['target_ends_at'])
        if base.tzinfo is None or end.tzinfo is None or end-base != timedelta(days=entry['days']):
            raise ValueError('invalid target period')


async def import_reward_receipts(connection, entries, source_sha256: str, *, require_evidence=False) -> int:
    """Operator-only protected history import; never an API/absence based award.

    Input provenance is the pinned SHA of the reviewed staging source. Ownership is
    resolved solely via verified Telegram accounts. Existing first-event receipts
    must match exactly; any conflict rolls back the complete staging batch.
    """
    import re
    if not isinstance(source_sha256,str) or not re.fullmatch(r'[0-9a-f]{64}',source_sha256):
        raise ValueError('invalid reward history provenance')
    count = 0
    async with connection.transaction():
        for entry in entries:
            evidence = entry.get('evidence') if isinstance(entry,dict) else None
            if require_evidence or evidence is not None:
                validate_import_evidence(entry)
                if entry['state'] in ('PENDING_REMOTE','REMOTE_APPLIED'):
                    # Preserved in the immutable coverage manifest; no runnable reward.
                    if await connection.fetchval('''SELECT 1 FROM referral_rewards r JOIN accounts a
                        ON a.id=r.invitee_account_id WHERE a.telegram_id=$1 AND r.event_kind=$2''',
                        entry['invitee_telegram_id'],entry['event_kind']):
                        raise ValueError('ambiguous receipt conflicts with canonical reward')
                    continue
            if not isinstance(entry,dict):
                raise ValueError('invalid reward history receipt')
            entry = {k:v for k,v in entry.items() if k != 'evidence'}
            if not isinstance(entry,dict) or set(entry) != {'source_id','invitee_telegram_id','inviter_telegram_id','event_kind','days','state'}:
                raise ValueError('invalid reward history receipt')
            if (not isinstance(entry['source_id'],str) or not entry['source_id']
                or type(entry['days']) is not int or entry['days']<=0
                or entry['event_kind'] not in ('trial','first_main_paid')
                or entry['state'] not in ('WAITING','APPLIED')
                or type(entry['invitee_telegram_id']) is not int
                or type(entry['inviter_telegram_id']) is not int):
                raise ValueError('invalid reward history receipt')
            invitee = await connection.fetchval("SELECT id FROM accounts WHERE telegram_id=$1 AND status='verified'",entry['invitee_telegram_id'])
            inviter = await connection.fetchval("SELECT id FROM accounts WHERE telegram_id=$1 AND status='verified'",entry['inviter_telegram_id'])
            if invitee is None or inviter is None or invitee == inviter:
                raise ValueError('reward history ownership unresolved')
            await connection.execute("SELECT pg_advisory_xact_lock(hashtextextended($1,0))",f"referral-history:{invitee}")
            existing = await connection.fetchrow("SELECT * FROM referral_rewards WHERE invitee_account_id=$1 AND event_kind=$2 FOR UPDATE",invitee,entry['event_kind'])
            if existing is not None:
                if not (existing['imported'] and existing['legacy_source_id']==entry['source_id']
                    and existing['import_source_sha256']==source_sha256 and existing['inviter_account_id']==inviter
                    and existing['days']==entry['days']):
                    raise ValueError('reward history receipt conflict')
                stored_evidence = existing['import_evidence']
                if isinstance(stored_evidence,str):
                    stored_evidence = json.loads(stored_evidence)
                if stored_evidence != evidence:
                    raise ValueError('reward evidence conflict')
                # A WAITING import legitimately progresses through the existing worker;
                # re-import never resets its APPLYING/APPLIED state or extends twice.
                if entry['state']=='APPLIED' and existing['state']!='APPLIED':
                    raise ValueError('reward history receipt conflict')
                continue
            await connection.execute("""INSERT INTO referral_rewards(invitee_account_id,inviter_account_id,event_kind,
                source_entitlement_id,days,state,imported,legacy_source_id,import_source_sha256,import_evidence)
                VALUES($1,$2,$3,NULL,$4,$5,true,$6,$7,$8::jsonb)""",invitee,inviter,entry['event_kind'],entry['days'],entry['state'],entry['source_id'],source_sha256,json.dumps(evidence) if evidence is not None else None)
            count += 1
    return count
