"""Trusted account billing; external delivery remains a separate pending boundary."""
import uuid
import asyncpg
from aiohttp import web

from .auth_api import ApiError, _error_response, _json_body, random_hex
from .payments import (_envelope, PAYMENT_PROVIDER_KEY, TrustedPaymentOwner, create_order, _order_view)
from .referral_trusted import CALLER, require_trusted_backend
from .telegram_binding import MOBILE_PREFIX, _account_advisory
from .s5_payments import (_KEY, _buyer_settings, _plans, _plans_revision, _quote_view,
                          account_plan_products, account_quote_snapshot, quote_body_digest,
                          _payment_view, _with_product, S5_METHOD_PROVIDER)

PATH = f'{MOBILE_PREFIX}/internal/telegram/billing'


async def billing_operation(connection, settings, body, key=None, *, provider=None):
    operation = body.get('operation')
    tg = body.get('telegram_id')
    if operation not in ('plans', 'quote', 'create', 'status') or type(tg) is not int or not 0 < tg < 2**63:
        raise ApiError('BAD_MESSAGE', http=400)
    if operation == 'plans':
        if set(body) != {'operation', 'telegram_id'}:
            raise ApiError('BAD_MESSAGE', http=400)
    elif operation in ('create','status'):
        field = 'quote_id' if operation == 'create' else 'payment_id'
        if set(body) != {'operation','telegram_id',field}:
            raise ApiError('BAD_MESSAGE',http=400)
        raw = body[field]
        try:
            identity = uuid.UUID(raw) if isinstance(raw,str) and len(raw)==36 else None
        except ValueError:
            identity = None
        if identity is None or str(identity) != raw:
            raise ApiError('BAD_MESSAGE',http=400)
        if operation == 'create' and (not isinstance(key,str) or not _KEY.fullmatch(key)):
            raise ApiError('BAD_MESSAGE',http=400,details={'reason':'idempotency_key_required'})
    else:
        if not isinstance(key, str) or not _KEY.fullmatch(key):
            raise ApiError('BAD_MESSAGE', http=400, details={'reason':'idempotency_key_required'})
        selection = {k:v for k,v in body.items() if k not in ('operation','telegram_id')}
        digest = quote_body_digest(selection, contract2=True)
    account_id = await connection.fetchval(
        "SELECT id FROM accounts WHERE telegram_id=$1 AND status='verified'", tg)
    if account_id is None:
        raise ApiError('REGISTRATION_REQUIRED', http=409, retryable=False)
    if operation in ('create','status'):
        # No account row/advisory transaction around create: its common core owns
        # preparation order and commits in_flight before external provider I/O.
        if operation == 'create':
            row = await connection.fetchrow(
                "SELECT * FROM s5_payment_quotes WHERE id=$1 AND owner_kind='telegram_account' "
                "AND trusted_owner_account_id=$2 AND trusted_caller=$3",identity,account_id,CALLER)
            if row is None:
                raise ApiError('NOT_FOUND',http=404)
            result = await create_order(connection,settings,provider,installation_id=None,
                months=int(row['months']),idempotency_key=key,quote_id=str(identity),
                method=S5_METHOD_PROVIDER.get(row['method'],row['method']),public_method=row['method'],
                _trusted_owner=TrustedPaymentOwner(account_id,tg))
            identity = uuid.UUID(result['payment']['order_id'])
        # Same read-only status contract as mobile. Provider reconcile/callback
        # use the existing workers, not hidden invoice creation/status I/O here.
        order = await connection.fetchrow(
            "SELECT p.* FROM payment_orders p JOIN accounts a ON a.id=p.trusted_owner_account_id "
            "WHERE p.id=$1 AND p.owner_kind='telegram_account' AND p.trusted_owner_account_id=$2 "
            "AND p.trusted_caller=$3 AND a.status='verified' AND a.telegram_id=$4",
            identity,account_id,CALLER,tg)
        if order is None:
            raise ApiError('PAYMENT_NOT_FOUND',http=404)
        result = _with_product(_payment_view(_order_view(order),
            credited_revision=order['credited_entitlement_revision'],needs_grant=order['needs_grant'],
            require_checkout=operation=='create'),order,True)
        # Accounting applied does not prove a working external VPN target.
        result['provisioning_state'] = 'external_pending' if order['applied_entitlement_id'] else 'not_requested'
        return result
    async with connection.transaction():
        await _account_advisory(connection, account_id)
        account = await connection.fetchrow(
            'SELECT status,telegram_id FROM accounts WHERE id=$1 FOR NO KEY UPDATE', account_id)
        if account is None or account['status'] != 'verified' or account['telegram_id'] != tg:
            raise ApiError('REGISTRATION_REQUIRED', http=409, retryable=False)
        if operation == 'quote':
            row = await connection.fetchrow(
                "SELECT * FROM s5_payment_quotes WHERE owner_kind='telegram_account' "
                'AND trusted_owner_account_id=$1 AND trusted_caller=$2 AND idempotency_key=$3',
                account_id, CALLER, key)
            if row is not None:
                if row['request_digest'] != digest:
                    raise ApiError('IDEMPOTENCY_CONFLICT', http=409)
                return _quote_view(row, contract2=True)
        # No absent-history => undiscounted fallback. Replay above is immutable even
        # if readiness/tariffs changed. This read must not initialize or grant anything.
        if await connection.fetchval('SELECT history_state FROM referral_benefits WHERE account_id=$1',account_id) != 'ready':
            raise ApiError('REFERRAL_HISTORY_PENDING', http=503, retryable=True)
        if operation == 'plans':
            buyer = _buyer_settings(settings, account_id)
            listed = _plans(buyer)
            await account_plan_products(connection,buyer,account_id,listed)
            return {'plans':listed,'plans_revision':_plans_revision(listed)}
        snapshot = await account_quote_snapshot(connection,settings,account_id,selection,contract2=True)
        row = await connection.fetchrow('''
            INSERT INTO s5_payment_quotes
            (owner_kind,trusted_owner_account_id,trusted_caller,installation_id,
             idempotency_key,request_digest,plan_id,months,duration_code,method,
             amount_minor,currency,tariff_key,plans_revision,expires_at,product,pricing)
            VALUES ('telegram_account',$1,$2,NULL,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::jsonb,$15::jsonb)
            RETURNING *''',account_id,CALLER,key,digest,selection['plan_id'],snapshot[0],
            selection['duration_code'],selection['method'],*snapshot[1:])
        return _quote_view(row,contract2=True)


def register_trusted_billing_routes(app,settings,database):
    async def handler(request):
        request_id = random_hex(16)
        try:
            require_trusted_backend(request,settings)
            body = await _json_body(request)
            async with database.acquire() as connection:
                result = await billing_operation(connection,settings,body,request.headers.get('Idempotency-Key'),provider=app.get(PAYMENT_PROVIDER_KEY))
            return _envelope(result)
        except ApiError as error:
            return _error_response(request_id,error)
        except (asyncpg.PostgresError,OSError):
            return _error_response(request_id,ApiError('SERVICE_UNAVAILABLE',http=503,retryable=True))
    app.router.add_post(PATH,handler)
