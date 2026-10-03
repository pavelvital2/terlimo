"""Account referral identity and installation-scoped durable candidate receipts.

No legacy absence implies eligibility. Only explicit verified imports or a protected
proof of a new Telegram identity may enter ready history state.
"""

from __future__ import annotations

import hashlib
import json
import re
import secrets
import string
from uuid import UUID, uuid4

from .auth_api import ApiError

TERMS_VERSION = "referral-20261003-v1"


def validate_key(value):
    if (
        not isinstance(value, str)
        or not 8 <= len(value) <= 128
        or not value.isascii()
        or any(ord(c) < 33 for c in value)
    ):
        raise ApiError("BAD_MESSAGE", http=400)
    return value


def candidate_uuid(value):
    try:
        return UUID(value) if isinstance(value, str) else UUID(str(value))
    except (ValueError, TypeError, AttributeError):
        raise ApiError("BAD_MESSAGE", http=400) from None


def normalize_code(code):
    # Legacy Mini Shop codes preserve case/spelling. URL ref_u prefix is not an input code.
    if not isinstance(code, str) or not re.fullmatch(r"[A-Za-z0-9]{1,32}", code):
        raise ApiError("BAD_MESSAGE", http=400)
    return code


def digest(body):
    return hashlib.sha256(
        json.dumps(body, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()


async def _initialize_account(connection, account_id, *, new_account=False):
    # Account -> benefit order is unchanged. Ordinary attribution updates no
    # referenced key: NO KEY UPDATE preserves serialization while allowing the
    # other account's FK KEY SHARE in reciprocal mobile/trusted attachments.
    account = await connection.fetchrow("SELECT * FROM accounts WHERE id=$1 FOR NO KEY UPDATE", account_id)
    if account is None or account["status"] != "verified" or account["telegram_id"] is None:
        return
    await connection.execute(
        "INSERT INTO referral_benefits(account_id) VALUES($1) ON CONFLICT DO NOTHING", account_id
    )
    benefit = await connection.fetchrow("SELECT * FROM referral_benefits WHERE account_id=$1", account_id)
    if benefit['history_state'] == 'ready' and account['referral_code'] is not None:
        return  # Includes accepted pre-epoch staging: never replay it over live attribution.
    staged = await connection.fetchrow(
        "SELECT * FROM referral_history_staging WHERE telegram_id=$1", account["telegram_id"]
    )
    epoch = None
    absent = staged is None
    if staged is None:
        epoch = await connection.fetchrow('SELECT * FROM referral_history_epochs WHERE active AND complete')
        if epoch is None:
            return
        # Absence is proved only by full immutable membership plus explicit operator activation.
        staged = dict(code=None, code_absent_verified=True, proven_new=True,
                      referred_by_telegram_id=None, trial_used=False, first_main_paid=False,
                      reward_dispositions={'trial':'not_earned','first_main_paid':'not_earned'},
                      source_sha256=epoch['manifest_sha256'])
        if account['referred_by_account_id'] is not None or account['referral_code'] is not None:
            return  # Existing canonical ownership needs reconciliation, not an absence rewrite.
    elif staged['epoch_id'] is not None:
        epoch = await connection.fetchrow('SELECT * FROM referral_history_epochs WHERE id=$1 AND active AND complete', staged['epoch_id'])
        if epoch is None:
            return
    trial_used = staged['trial_used'] or bool(await connection.fetchval(
        "SELECT 1 FROM entitlements WHERE account_id=$1 AND kind='trial' LIMIT 1",account_id))
    paid_used = staged['first_main_paid'] or bool(await connection.fetchval(
        """SELECT 1 FROM payment_orders p LEFT JOIN account_bindings b ON b.id=p.binding_id
           WHERE coalesce(p.checkout_owner_account_id,p.account_id,b.account_id)=$1
             AND p.months IN (1,3,6) AND p.status='succeeded' LIMIT 1""",account_id))
    paid_used = paid_used or bool(await connection.fetchval(
        "SELECT 1 FROM entitlements WHERE account_id=$1 AND kind='paid' LIMIT 1",account_id))
    # Used benefits stay blocked even if attribution/remote reward reconciliation
    # keeps this member pending. Pending must never re-enable a legacy trial.
    await connection.execute(
        """UPDATE referral_benefits SET imported_trial_used=imported_trial_used OR $2,
        imported_first_main_paid=imported_first_main_paid OR $3 WHERE account_id=$1""",
        account_id,trial_used,paid_used)
    dispositions = staged['reward_dispositions']
    if isinstance(dispositions,str):
        dispositions = json.loads(dispositions)
    if dispositions is not None and 'unresolved' in dispositions.values():
        return
    inviter = None
    if staged["referred_by_telegram_id"] is not None:
        inviter = await connection.fetchval(
            "SELECT id FROM accounts WHERE telegram_id=$1 AND status='verified'",
            staged["referred_by_telegram_id"],
        )
        if inviter is None:
            return
    if account['referred_by_account_id'] not in (None,inviter):
        raise ApiError('REFERRAL_HISTORY_PENDING',http=503)
    for used,event in ((staged['trial_used'],'trial'),(staged['first_main_paid'],'first_main_paid')):
        earned = dispositions[event] == 'earned' if dispositions is not None else used and inviter is not None
        if earned and not await connection.fetchval(
            "SELECT 1 FROM referral_rewards WHERE invitee_account_id=$1 AND inviter_account_id=$2 AND event_kind=$3 AND imported AND import_source_sha256=$4",
            account_id,inviter,event,staged['source_sha256']):
            return  # Includes ambiguous remote evidence: never enqueue a second extension.
    if staged['code'] is not None:
        normalize_code(staged['code'])
        if account['referral_code'] not in (None,staged['code']):
            raise ApiError('REFERRAL_HISTORY_PENDING',http=503)
        code = staged['code']
    elif staged['proven_new'] or staged['code_absent_verified']:
        code = account['referral_code']
        if code is None:
            import asyncpg
            for _ in range(8):
                code = ''.join(secrets.choice(string.ascii_letters + string.digits) for _ in range(12))
                if await connection.fetchval('SELECT 1 FROM referral_history_staging WHERE code=$1 AND telegram_id<>$2',code,account['telegram_id']):
                    continue
                try:
                    async with connection.transaction():
                        await connection.execute('UPDATE accounts SET referral_code=$2 WHERE id=$1 AND referral_code IS NULL',account_id,code)
                    break
                except asyncpg.UniqueViolationError:
                    continue
            else:
                raise ApiError('SERVICE_UNAVAILABLE',http=503)
    else:
        return
    # Assign a missing unique code separately. Do not list the unique column in
    # ordinary updates: even SET referral_code=the_same_value upgrades the row
    # lock to FOR UPDATE and would defeat FK-compatible ownership above.
    if account['referral_code'] is None and staged['code'] is not None:
        await connection.execute('UPDATE accounts SET referral_code=$2 WHERE id=$1',account_id,code)
    await connection.execute(
        "UPDATE accounts SET referred_by_account_id=coalesce(referred_by_account_id,$2),referral_attributed_at=CASE WHEN referred_by_account_id IS NULL AND $2::uuid IS NOT NULL THEN now() ELSE referral_attributed_at END,referral_attribution_receipt_id=CASE WHEN $2::uuid IS NOT NULL THEN coalesce(referral_attribution_receipt_id,gen_random_uuid()) ELSE referral_attribution_receipt_id END,referral_terms_version=$3 WHERE id=$1",
        account_id,inviter,TERMS_VERSION)
    await connection.execute(
        """UPDATE referral_benefits SET history_state='ready',
        imported_trial_used=imported_trial_used OR $2,imported_first_main_paid=imported_first_main_paid OR $3,
        history_epoch_id=$4,history_proof=$5 WHERE account_id=$1""",
        account_id,trial_used,paid_used,epoch['id'] if epoch else None,
        ('absent' if absent else 'member') if epoch else None)


async def initialize_account(connection, account_id, *, new_account=False):
    async with connection.transaction():
        await _initialize_account(connection, account_id, new_account=new_account)


async def stage_history(connection, entries, *, source_sha256):
    """Protected operator import primitive, never exposed through API.

    Caller must reconcile complete original rewards into referral_rewards before
    staging redeemed histories; source digest identifies that accepted manifest.
    Insert-only exact replay prevents overwriting first attribution/code/history.
    """
    if not re.fullmatch(r"[0-9a-f]{64}", source_sha256):
        raise ValueError("invalid source digest")
    async with connection.transaction():
        for entry in entries:
            if set(entry) != {
                "telegram_id",
                "code",
                "referred_by_telegram_id",
                "proven_new",
                "trial_used",
                "first_main_paid",
            }:
                raise ValueError("incomplete history proof")
            if entry["code"] is not None:
                normalize_code(entry["code"])
            values = [
                entry[k]
                for k in (
                    "telegram_id",
                    "code",
                    "referred_by_telegram_id",
                    "proven_new",
                    "trial_used",
                    "first_main_paid",
                )
            ]
            previous = await connection.fetchrow(
                "SELECT * FROM referral_history_staging WHERE telegram_id=$1", entry["telegram_id"]
            )
            if previous is not None:
                if (
                    any(previous[k] != entry[k] for k in entry)
                    or previous["source_sha256"] != source_sha256
                ):
                    raise ValueError("history proof conflict")
                continue
            await connection.execute(
                "INSERT INTO referral_history_staging(telegram_id,code,referred_by_telegram_id,proven_new,trial_used,first_main_paid,source_sha256) VALUES($1,$2,$3,$4,$5,$6,$7)",
                *values,
                source_sha256,
            )


async def trial_bonus_days(connection, account_id):
    return (
        3
        if await connection.fetchval(
            "SELECT 1 FROM accounts a JOIN referral_benefits b ON b.account_id=a.id WHERE a.id=$1 AND a.referred_by_account_id IS NOT NULL AND b.history_state='ready' AND NOT b.imported_trial_used",
            account_id,
        )
        else 0
    )


async def change_candidate(
    connection, *, installation_id, code, idempotency_key, clear=False, body=...
):
    key = validate_key(idempotency_key)
    operation = "candidate_clear" if clear else "candidate_set"
    # Original key/body replay precedes mutable lookup or validation.
    raw_body = body if body is not ... else {"code": code}
    body_digest = digest(raw_body)
    failure = None
    result = None
    async with connection.transaction():
        installation = await connection.fetchrow(
            "SELECT id,state FROM installations WHERE id=$1 FOR UPDATE", installation_id
        )
        if installation is None or installation["state"] == "revoked":
            raise ApiError("DEVICE_REVOKED", http=403)
        receipt = await connection.fetchrow(
            "SELECT digest,result FROM referral_operations WHERE installation_id=$1 AND operation=$2 AND idempotency_key=$3",
            installation_id,
            operation,
            key,
        )
        if receipt:
            if receipt["digest"] != body_digest:
                raise ApiError("IDEMPOTENCY_CONFLICT", http=409)
            result = receipt["result"]
            if "error" in result:
                raise ApiError(result["error"], http=result["http"], retryable=False)
            return result
        normalized = None
        try:
            if not clear:
                if not isinstance(raw_body, dict) or set(raw_body) != {"code"}:
                    raise ApiError("BAD_MESSAGE", http=400)
                normalized = normalize_code(raw_body["code"])
        except ApiError as error:
            failure = error
        if failure is None:
            if await connection.fetchval(
                "SELECT 1 FROM registration_links WHERE installation_id=$1 AND referral_candidate_id IS NOT NULL AND status='pending' AND expires_at>now()",
                installation_id,
            ):
                raise ApiError("REFERRAL_CANDIDATE_LOCKED", http=409)
            if normalized is not None:
                inviter = await connection.fetchrow(
                    "SELECT a.referral_code,b.history_state FROM accounts a LEFT JOIN referral_benefits b ON b.account_id=a.id WHERE a.referral_code=$1 AND a.status='verified' AND a.telegram_id IS NOT NULL",
                    normalized,
                )
                if inviter is None:
                    failure = ApiError("REFERRAL_CODE_INVALID", http=404, retryable=False)
                elif inviter["history_state"] != "ready":
                    raise ApiError("REFERRAL_HISTORY_PENDING", http=503, retryable=True)
                else:
                    normalized = inviter["referral_code"]
        if failure is None:
            await connection.execute(
                "UPDATE referral_candidates SET state='cleared' WHERE installation_id=$1 AND state='pending'",
                installation_id,
            )
            result = {"candidate": {"state": "cleared"}}
            if not clear:
                row = await connection.fetchrow(
                    "INSERT INTO referral_candidates(installation_id,code) VALUES($1,$2) RETURNING id",
                    installation_id,
                    normalized,
                )
                result = {
                    "candidate": {"id": str(row["id"]), "state": "pending", "code": normalized}
                }
        else:
            result = {"error": failure.code, "http": failure.http}
        await connection.execute(
            "INSERT INTO referral_operations(installation_id,operation,idempotency_key,digest,result) VALUES($1,$2,$3,$4,$5)",
            installation_id,
            operation,
            key,
            body_digest,
            result,
        )
    if failure is not None:
        raise failure
    return result


async def attach_account(connection, account_id, code):
    """Shared mutation seam; caller owns transaction + existing bind-account lock.

    No installation/registration fabrication. Caller has already authorized this
    verified account; the row lock serializes mobile and trusted attribution.
    """
    account = await connection.fetchrow(
        "SELECT a.*,b.history_state FROM accounts a LEFT JOIN referral_benefits b ON b.account_id=a.id WHERE a.id=$1 AND a.status='verified' AND a.telegram_id IS NOT NULL FOR NO KEY UPDATE OF a",
        account_id,
    )
    if account is None:
        raise ApiError("ACCESS_DENIED", http=403)
    inviter = None if code is None else await connection.fetchrow(
        "SELECT a.id,b.history_state FROM accounts a LEFT JOIN referral_benefits b ON b.account_id=a.id WHERE a.referral_code=$1 AND a.status='verified' AND a.telegram_id IS NOT NULL",
        code,
    )
    if account['history_state'] in (None,'history_pending') or (
        inviter is not None and inviter['history_state'] in (None,'history_pending')
    ):
        raise ApiError('REFERRAL_HISTORY_PENDING',http=503,retryable=True)
    reason = None
    if inviter is None or inviter['history_state'] != 'ready':
        reason = 'invalid'
    elif inviter['id'] == account_id:
        reason = 'self'
    elif account['referred_by_account_id'] is not None:
        reason = 'already_attributed'
    elif account['history_state'] != 'ready':
        reason = 'ineligible'
    elif await connection.fetchval(
        "SELECT 1 FROM entitlements WHERE account_id=$1 AND kind IN ('paid','trial','imported') "
        "AND status='active' AND (starts_at IS NULL OR starts_at<=now()) "
        "AND (ends_at IS NULL OR ends_at>now()) LIMIT 1",account_id
    ):
        reason = 'ineligible'
    receipt = {'receipt_id':str(uuid4()),'account_ref':str(account_id),
               'state':'attached' if reason is None else 'rejected','reason':reason}
    if reason is None:
        await connection.execute(
            "UPDATE accounts SET referred_by_account_id=$2,referral_attributed_at=now(),referral_terms_version=$3,referral_attribution_receipt_id=$4 WHERE id=$1",
            account_id,inviter['id'],TERMS_VERSION,UUID(receipt['receipt_id']))
    return receipt


async def attach_candidate(connection, link, account_id):
    candidate_id = link['referral_candidate_id']
    if candidate_id is None:
        return None
    if link['referral_attribution'] is not None:
        return link['referral_attribution']
    candidate = await connection.fetchrow(
        'SELECT * FROM referral_candidates WHERE id=$1 FOR UPDATE',candidate_id)
    code = candidate['code'] if candidate is not None and candidate['installation_id'] == link['installation_id'] else None
    receipt = await attach_account(connection,account_id,code)
    receipt.update(candidate_id=str(candidate_id),registration_id=str(link['id']),
                   idempotency_key=link['referral_idempotency_key'])
    await connection.execute('UPDATE registration_links SET referral_attribution=$2 WHERE id=$1',link['id'],receipt)
    await connection.execute('UPDATE referral_candidates SET state=$2 WHERE id=$1',candidate_id,receipt['state'])
    return receipt


async def referral_info(connection, context):
    if (
        context.account_id is None
        or context.binding is None
        or context.binding["status"] != "active"
        or context.installation_id is None
    ):
        raise ApiError("ACCESS_DENIED", http=403)
    return await account_referral_info(connection,context.account_id,installation_id=context.installation_id)


async def account_referral_info(connection, account_id, *, installation_id=None):
    """Common projection for already-authorized account callers, never a public ID API.

    Mobile supplies its installation for the unchanged active-owner check; trusted
    backend resolves a verified Telegram identity and does not need a data grant.
    """
    projection_sql = "SELECT a.*,b.history_state,b.imported_first_main_paid,b.reserved_order_id,b.consumed_order_id,b.reservation_state FROM accounts a LEFT JOIN referral_benefits b ON b.account_id=a.id WHERE a.id=$1 AND a.status='verified' AND a.telegram_id IS NOT NULL AND ($2::uuid IS NULL OR EXISTS(SELECT 1 FROM account_bindings own WHERE own.account_id=a.id AND own.installation_id=$2 AND own.status='active'))"
    account = await connection.fetchrow(projection_sql, account_id, installation_id)
    if account is None:
        raise ApiError("ACCESS_DENIED", http=403)
    if account["history_state"] in (None, "history_pending") or account["referral_code"] is None:
        # Existing registered users need no re-registration after protected coverage
        # activation. Authorization must precede any initializer mutation; reread
        # the same authoritative owner projection after its transaction commits.
        await initialize_account(connection, account_id)
        account = await connection.fetchrow(projection_sql, account_id, installation_id)
        if account is None:
            raise ApiError("ACCESS_DENIED", http=403)
    if account["history_state"] in (None, "history_pending") or account["referral_code"] is None:
        raise ApiError("REFERRAL_HISTORY_PENDING", http=503, retryable=True)
    receipt = await connection.fetchval(
        "SELECT referral_attribution FROM registration_links WHERE referral_attribution->>'account_ref'=$1 AND referral_attribution IS NOT NULL ORDER BY confirmed_at DESC LIMIT 1",
        str(account_id),
    )
    if isinstance(receipt,str):
        receipt = json.loads(receipt)
    if account['referral_attribution_receipt_id'] is not None and (
        receipt is None or receipt['receipt_id'] != str(account['referral_attribution_receipt_id'])
    ):
        receipt = None  # Project the canonical attached receipt instead of an older rejection.
    state = "ineligible"
    if account["history_state"] == "ready" and account["referred_by_account_id"] is not None:
        state = (
            "consumed"
            if account["consumed_order_id"] or account["imported_first_main_paid"]
            else "reserved"
            if account["reserved_order_id"]
            else "eligible"
        )
    from .referral_pricing import eligible

    if state == "eligible" and not await eligible(connection, account_id):
        state = "ineligible"
    rewards = await connection.fetchrow(
        "SELECT coalesce(sum(days) FILTER(WHERE state IN ('WAITING','APPLYING')),0) waiting,coalesce(sum(days) FILTER(WHERE state='APPLIED'),0) applied FROM referral_rewards WHERE inviter_account_id=$1",
        account_id,
    )
    code = account["referral_code"]
    return {
        "account_ref": str(account_id),
        "code": code,
        "links": {
            "telegram": f"https://t.me/terlimo_vpn_wdtt_bot?start=ref_u{code}",
            "web": f"https://terlimo.xyz/?ref=u{code}",
        },
        "attribution": {
            "state": receipt["state"]
            if receipt
            else "attached"
            if account["referred_by_account_id"]
            else "none",
            "receipt_id": receipt["receipt_id"]
            if receipt
            else str(account["referral_attribution_receipt_id"])
            if account["referral_attribution_receipt_id"]
            else None,
            "reason": receipt["reason"] if receipt else None,
        },
        "benefits": {
            "trial_bonus_days": await trial_bonus_days(connection, account_id),
            "discount": {"currency": "RUB", "amount_minor": 10000, "state": state},
        },
        "rewards": {
            "waiting_days": int(rewards["waiting"]),
            "applied_days": int(rewards["applied"]),
        },
        "terms_version": TERMS_VERSION,
    }


async def keyed_registration_link(
    connection, settings, *, installation_id, candidate_id, idempotency_key
):
    from .auth_api import now_utc
    from .telegram_binding import _account_advisory, create_registration_link

    key = validate_key(idempotency_key)
    body_digest = digest({"referral_candidate_id": str(candidate_id)})
    failure = None
    async with connection.transaction():
        account_id = await connection.fetchval(
            "SELECT account_id FROM account_bindings WHERE installation_id=$1 AND status='active'",
            installation_id,
        )
        if account_id is not None:
            await _account_advisory(connection, account_id)
        installation = await connection.fetchrow(
            "SELECT id,state FROM installations WHERE id=$1 FOR UPDATE", installation_id
        )
        if installation is None or installation["state"] == "revoked":
            raise ApiError("DEVICE_REVOKED", http=403)
        receipt = await connection.fetchrow(
            "SELECT * FROM referral_operations WHERE installation_id=$1 AND operation='registration' AND idempotency_key=$2",
            installation_id,
            key,
        )
        if receipt is not None:
            if receipt["digest"] != body_digest:
                raise ApiError("IDEMPOTENCY_CONFLICT", http=409)
            original = receipt["result"]
            link = await connection.fetchrow(
                "SELECT * FROM registration_links WHERE id=$1 FOR UPDATE",
                candidate_uuid(original["referral_registration"]["registration_id"]),
            )
            if link["status"] == "confirmed":
                attribution = link["referral_attribution"]
                owner = await connection.fetchval(
                    "SELECT id FROM accounts WHERE telegram_id=$1 AND status='verified'",
                    link["telegram_id"],
                )
                current = await connection.fetchval(
                    "SELECT account_id FROM account_bindings WHERE installation_id=$1 AND status='active'",
                    installation_id,
                )
                if (
                    attribution is None
                    or owner is None
                    or str(owner) != attribution["account_ref"]
                    or (current is not None and current != owner)
                ):
                    raise ApiError("REGISTRATION_CONFLICT", http=409)
                result = {
                    "state": "registered",
                    "referral_attribution": link["referral_attribution"],
                }
            elif link["status"] == "expired" or link["expires_at"] <= now_utc():
                await connection.execute(
                    "UPDATE registration_links SET status='expired' WHERE id=$1", link["id"]
                )
                failure = ApiError(
                    "REGISTRATION_EXPIRED",
                    http=410,
                    retryable=False,
                    details={
                        "state": "expired",
                        "referral_registration": original["referral_registration"],
                    },
                )
                result = None
            else:
                result = original
        else:
            candidate_id = candidate_uuid(candidate_id)
            candidate = await connection.fetchrow(
                "SELECT * FROM referral_candidates WHERE id=$1", candidate_id
            )
            if (
                candidate is None
                or candidate["installation_id"] != installation_id
                or candidate["state"] != "pending"
            ):
                raise ApiError("REFERRAL_CANDIDATE_INVALID", http=409)
            if await connection.fetchval(
                "SELECT 1 FROM registration_links WHERE installation_id=$1 AND status='pending' AND referral_candidate_id IS NOT NULL AND expires_at>now()",
                installation_id,
            ):
                raise ApiError("REFERRAL_CANDIDATE_LOCKED", http=409)
            # New candidate cannot be attached after account proof has already bound this install.
            if account_id is not None:
                raise ApiError("REFERRAL_CANDIDATE_LOCKED", http=409)
            result = await create_registration_link(
                connection, settings, installation_id=installation_id, _keyed=True
            )
            link = await connection.fetchrow(
                "UPDATE registration_links SET referral_candidate_id=$2,referral_idempotency_key=$3 WHERE token_sha256=$1 RETURNING id",
                hashlib.sha256(result["token"].encode()).hexdigest(),
                candidate_id,
                key,
            )
            result["referral_registration"] = {
                "candidate_id": str(candidate_id),
                "registration_id": str(link["id"]),
                "idempotency_key": key,
            }
            await connection.execute(
                "INSERT INTO referral_operations(installation_id,operation,idempotency_key,digest,result) VALUES($1,'registration',$2,$3,$4)",
                installation_id,
                key,
                body_digest,
                result,
            )
    if failure is not None:
        raise failure
    return result
