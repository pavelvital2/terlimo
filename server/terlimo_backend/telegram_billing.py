"""Trusted account plans/immutable quotes only; no invoice or access operations."""
import asyncpg
from aiohttp import web

from .auth_api import ApiError, _error_response, _json_body, random_hex
from .payments import _envelope
from .referral_trusted import CALLER, require_trusted_backend
from .telegram_binding import MOBILE_PREFIX, _account_advisory
from .s5_payments import (_KEY, _buyer_settings, _plans, _plans_revision, _quote_view,
                          account_plan_products, account_quote_snapshot, quote_body_digest)

PATH = f'{MOBILE_PREFIX}/internal/telegram/billing'


async def billing_operation(connection, settings, body, key=None):
    operation = body.get('operation')
    tg = body.get('telegram_id')
    if operation not in ('plans', 'quote') or type(tg) is not int or not 0 < tg < 2**63:
        raise ApiError('BAD_MESSAGE', http=400)
    if operation == 'plans':
        if set(body) != {'operation', 'telegram_id'}:
            raise ApiError('BAD_MESSAGE', http=400)
    else:
        if not isinstance(key, str) or not _KEY.fullmatch(key):
            raise ApiError('BAD_MESSAGE', http=400, details={'reason':'idempotency_key_required'})
        selection = {k:v for k,v in body.items() if k not in ('operation','telegram_id')}
        digest = quote_body_digest(selection, contract2=True)
    account_id = await connection.fetchval(
        "SELECT id FROM accounts WHERE telegram_id=$1 AND status='verified'", tg)
    if account_id is None:
        raise ApiError('REGISTRATION_REQUIRED', http=409, retryable=False)
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
                result = await billing_operation(connection,settings,body,request.headers.get('Idempotency-Key'))
            return _envelope(result)
        except ApiError as error:
            return _error_response(request_id,error)
        except (asyncpg.PostgresError,OSError):
            return _error_response(request_id,ApiError('SERVICE_UNAVAILABLE',http=503,retryable=True))
    app.router.add_post(PATH,handler)
