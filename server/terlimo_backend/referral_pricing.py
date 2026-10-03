"""Referral price snapshots and account reservation, using the existing payment ledger.

Lock order: installation session -> account KEY SHARE -> referral-discount xact -> benefit row -> quote/order insertion.
Paid writers hold order -> owner/current credit/inviter account KEY SHARE ->
referral-discount/benefit -> paid-account. They never acquire installation locks. Create
never locks an existing order under referral-discount, avoiding that inversion.
"""
import json

from .auth_api import ApiError

TERMS = 'referral-20261003-v1'
DISCOUNT_MINOR = 10000


def pricing_of(row):
    value = row.get('pricing') if row else None
    while isinstance(value,str):
        value = json.loads(value)
    return value


async def eligible(connection, account_id):
    if account_id is None:
        return False
    return bool(await connection.fetchval('''
        SELECT 1 FROM referral_benefits b JOIN accounts a ON a.id=b.account_id
        WHERE b.account_id=$1 AND b.history_state='ready'
          AND a.referred_by_account_id IS NOT NULL
          AND b.consumed_order_id IS NULL AND b.reserved_order_id IS NULL
          AND NOT b.imported_first_main_paid
          AND NOT EXISTS (SELECT 1 FROM payment_orders p
              WHERE p.checkout_owner_account_id=b.account_id AND p.months IN (1,3,6)
                AND p.status='succeeded')
          AND NOT EXISTS (SELECT 1 FROM entitlements e
              WHERE e.account_id=b.account_id AND e.kind='paid')
        ''', account_id))


async def quote_pricing(connection, account_id, base_minor, *, main, main_base_minor=None):
    if not main or not await eligible(connection, account_id):
        return None
    if (main_base_minor if main_base_minor is not None else base_minor) <= DISCOUNT_MINOR:
        raise ApiError("REFERRAL_PRICE_UNSUPPORTED",http=409,retryable=False)
    return {'base_amount_minor':base_minor, 'discount_minor':DISCOUNT_MINOR,
            'payable_amount_minor':base_minor-DISCOUNT_MINOR, 'currency':'RUB',
            'discount_kind':'referral_first_main', 'terms_version':TERMS}


async def benefit_lock(connection, account_id):
    await connection.execute('SELECT pg_advisory_xact_lock(hashtextextended($1,0))',
                             f'referral-discount:{account_id}')


def no_order_error(quote_id, key, reason):
    return ApiError('REFERRAL_DISCOUNT_RESERVED' if reason == 'referral_discount_reserved' else 'QUOTE_EXPIRED',
                    http=409, retryable=False,
                    details={'reason':reason, 'create_resolution':{
                        'kind':'no_order','quote_id':str(quote_id),
                        'request_idempotency_key':key,'reason':reason}})


async def reserve_check(connection, account_id, quote_id, key, *, discounted=True):
    """Under the account lock after global K/source Q/unknown checks.

    Caller commits a returned rejection before raising it. Quote invalidation is
    durable: relaxing eligibility cannot resurrect this Q with any later key.
    """
    if await connection.fetchval('''SELECT 1 FROM payment_orders p LEFT JOIN account_bindings b
        ON b.installation_id=p.installation_id AND b.status='active'
        WHERE coalesce(p.checkout_owner_account_id,p.account_id,b.account_id)=$1
        AND p.months IN (1,3,6) AND p.provider_create_state IN ('in_flight','unknown') LIMIT 1''',account_id):
        raise ApiError('PAYMENT_PROVIDER_UNKNOWN',http=503,retryable=False)
    benefit = await connection.fetchrow('SELECT * FROM referral_benefits WHERE account_id=$1 FOR UPDATE',account_id)
    reason = None
    other_invoice = await connection.fetchval('''SELECT 1 FROM payment_orders p LEFT JOIN account_bindings b
        ON b.installation_id=p.installation_id AND b.status='active'
        WHERE coalesce(p.checkout_owner_account_id,p.account_id,b.account_id)=$1 AND p.months IN (1,3,6)
          AND p.status IN ('pending','canceled','expired','failed')
          AND p.provider_create_state IN ('in_flight','unknown','created') LIMIT 1''',account_id)
    if (discounted and other_invoice) or (benefit and benefit['reserved_order_id'] is not None and benefit['consumed_order_id'] is None):
        reason = 'referral_discount_reserved'
    elif discounted and not await eligible(connection, account_id):
        reason = 'referral_quote_changed'
    if reason:
        if quote_id is None:
            return ApiError('REFERRAL_DISCOUNT_RESERVED', http=409, retryable=True, details={'reason':reason})
        await connection.execute('''UPDATE s5_payment_quotes SET expires_at=LEAST(expires_at,now()),
                                    referral_create_resolution_reason=$2 WHERE id=$1''',quote_id,reason)
        return no_order_error(quote_id,key,reason)
    return None


async def paid_account_locks(connection, order):
    """Acquire owner, current credit and inviter FK locks before benefit writes.

    This runs inside the existing paid transaction, without external I/O.
    Attribution writers take account FOR UPDATE, so the owner KEY SHARE
    stabilizes their inviter until commit.
    """
    binding_account = await connection.fetchval(
        "SELECT account_id FROM account_bindings WHERE installation_id=$1 AND status='active'",
        order['installation_id'])
    accounts = {aid for aid in (order['checkout_owner_account_id'], order['account_id'], binding_account) if aid is not None}
    for aid in sorted(accounts, key=str):
        # Stabilize verified status for account-only credit BEFORE the benefit lock,
        # including callback/reconcile callers which consume before applying credit.
        lock = "SHARE" if order.get('owner_kind') == 'telegram_account' else "KEY SHARE"
        await connection.fetchval(f"SELECT id FROM accounts WHERE id=$1 FOR {lock}", aid)
    inviters = set()
    for aid in accounts:
        inviter = await connection.fetchval("SELECT referred_by_account_id FROM accounts WHERE id=$1", aid)
        if inviter is not None:
            inviters.add(inviter)
    for aid in sorted(inviters - accounts, key=str):
        await connection.fetchval("SELECT id FROM accounts WHERE id=$1 FOR KEY SHARE", aid)
    return binding_account


async def consume(connection, order):
    """Anchor the first matching main payment, even while credit needs review.

    An eventual replay of that order may earn its original period reward; a later
    applied order cannot substitute a different paid period for this durable fact.
    """
    binding_account = await paid_account_locks(connection, order)
    if int(order['months']) not in (1,3,6):
        return
    quote = order['quote']
    while isinstance(quote,str):
        quote = json.loads(quote)
    account_id = order['checkout_owner_account_id'] or order['account_id']
    if account_id is None:
        account_id = binding_account
    if account_id is None:
        return
    await benefit_lock(connection,account_id)
    await connection.execute("""UPDATE referral_benefits SET first_paid_order_id=$2,revision=revision+1
        WHERE account_id=$1 AND first_paid_order_id IS NULL""",account_id,order['id'])
    if quote.get('pricing'):
        await connection.execute("""UPDATE referral_benefits SET consumed_order_id=$2,
            reservation_state='consumed',revision=revision+1
            WHERE account_id=$1 AND reserved_order_id=$2 AND consumed_order_id IS NULL""",account_id,order['id'])


async def mark_reconciling(connection, order):
    await connection.execute('''UPDATE referral_benefits SET reservation_state='reconciling'
        WHERE reserved_order_id=$1 AND consumed_order_id IS NULL''',order['id'])
