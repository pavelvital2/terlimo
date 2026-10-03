"""Internal read/attach adapter for the existing trusted Telegram backend.

Never reachable by treating a browser Telegram ID as authentication. The existing
shared backend key attests the bot update / WebApp session verification upstream.
"""
from __future__ import annotations

import hmac
import json

import asyncpg
from aiohttp import web

from .auth_api import ApiError, _error_response, _json_body, random_hex
from .referral import account_referral_info, attach_account, digest, initialize_account, normalize_code, validate_key
from .telegram_binding import BOT_KEY_HEADER, MOBILE_PREFIX, _account_advisory

PATH = f'{MOBILE_PREFIX}/internal/telegram/referral'
# Identity is server-controlled. Bot and site share the existing credential/trust principal.
CALLER = 'telegram_backend'


async def trusted_operation(connection, body, key=None):
    operation = body.get('operation')
    expected = {'operation','telegram_id'} | ({'code'} if operation == 'attach' else set())
    if set(body) != expected or operation not in ('read','attach'):
        raise ApiError('BAD_MESSAGE',http=400)
    tg = body['telegram_id']
    if type(tg) is not int or not 0 < tg < 2**63:
        raise ApiError('BAD_MESSAGE',http=400)
    if operation == 'attach':
        validate_key(key)
    account_id = await connection.fetchval("SELECT id FROM accounts WHERE telegram_id=$1 AND status='verified'",tg)
    if account_id is None:
        raise ApiError('REGISTRATION_REQUIRED',http=409,retryable=False)
    async with connection.transaction():
        # Same account advisory as mobile registration, before any account/receipt rows.
        await _account_advisory(connection,account_id)
        current = await connection.fetchrow("SELECT a.id,a.referral_code,b.history_state FROM accounts a LEFT JOIN referral_benefits b ON b.account_id=a.id WHERE a.id=$1 AND a.telegram_id=$2 AND a.status='verified' FOR NO KEY UPDATE OF a",account_id,tg)
        if current is None:
            raise ApiError('REGISTRATION_REQUIRED',http=409,retryable=False)
        if operation == 'read':
            return {'referral':await account_referral_info(connection,account_id)}
        body_digest = digest(body)
        prior = await connection.fetchrow('''SELECT digest,result FROM referral_trusted_operations
            WHERE account_id=$1 AND caller=$2 AND operation='attach' AND idempotency_key=$3''',account_id,CALLER,key)
        if prior is not None:
            if prior['digest'] != body_digest:
                raise ApiError('IDEMPOTENCY_CONFLICT',http=409,retryable=False)
            result = prior['result']
            return json.loads(result) if isinstance(result,str) else result
        # Replay precedes mutable code/history/eligibility. No random/default key.
        code = normalize_code(body['code'])
        if current['history_state'] in (None,'history_pending') or current['referral_code'] is None:
            await initialize_account(connection,account_id)
        receipt = await attach_account(connection,account_id,code)
        receipt['idempotency_key'] = key
        result = {'attribution':receipt}
        await connection.execute('''INSERT INTO referral_trusted_operations
            (account_id,caller,operation,idempotency_key,digest,result)
            VALUES($1,$2,'attach',$3,$4,$5::text::jsonb)''',account_id,CALLER,key,body_digest,json.dumps(result))
        return result


def register_trusted_referral_routes(app,settings,database):
    async def handler(request):
        request_id = random_hex(16)
        try:
            supplied = request.headers.get(BOT_KEY_HEADER,'')
            expected = settings.telegram_bot_key
            if not supplied or not expected or not hmac.compare_digest(supplied.encode(),expected.encode()):
                raise ApiError('REGISTRATION_AUTH',http=403,retryable=False)
            body = await _json_body(request)
            async with database.acquire() as connection:
                result = await trusted_operation(connection,body,request.headers.get('Idempotency-Key'))
            return web.json_response({'request_id':request_id,**result})
        except ApiError as error:
            return _error_response(request_id,error)
        except (asyncpg.PostgresError,OSError):
            return _error_response(request_id,ApiError('SERVICE_UNAVAILABLE',http=503,retryable=True))
    app.router.add_post(PATH,handler)
