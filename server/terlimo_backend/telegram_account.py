"""Account ensure for the trusted telegram_backend principal, not browser identity."""
from __future__ import annotations

import asyncpg
from aiohttp import web

from .auth_api import ApiError, _error_response, _json_body, random_hex
from .referral import initialize_account
from .referral_trusted import require_trusted_backend
from .telegram_binding import MOBILE_PREFIX, _account_advisory

PATH = f'{MOBILE_PREFIX}/internal/telegram/account'


async def ensure_account(connection, body):
    if (set(body) != {'operation', 'telegram_id'} or body['operation'] != 'ensure'
            or type(body['telegram_id']) is not int or not 0 < body['telegram_id'] < 2**63):
        raise ApiError('BAD_MESSAGE', http=400)
    tg = body['telegram_id']
    async with connection.transaction():
        account_id = await connection.fetchval('SELECT id FROM accounts WHERE telegram_id=$1', tg)
        if account_id is None:
            # No installation/row lock before insertion. A concurrent mobile insert
            # may win; DO NOTHING never changes its status or updated_at. Roll back
            # this attempt rather than acquire its advisory after an insert conflict.
            account_id = await connection.fetchval(
                "INSERT INTO accounts(status,telegram_id) VALUES('verified',$1) "
                'ON CONFLICT (telegram_id) DO NOTHING RETURNING id', tg)
            if account_id is None:
                raise ApiError('REVISION_CONFLICT', http=409, retryable=True)
            # New UUID is private to this transaction, as in mobile confirmation.
        await _account_advisory(connection, account_id)
        account = await connection.fetchrow(
            'SELECT id,status,telegram_id FROM accounts WHERE id=$1 FOR NO KEY UPDATE', account_id)
        if account is None or account['status'] != 'verified' or account['telegram_id'] != tg:
            raise ApiError('REGISTRATION_REQUIRED', http=409, retryable=False)
        await initialize_account(connection, account_id)
        state = await connection.fetchval(
            'SELECT history_state FROM referral_benefits WHERE account_id=$1', account_id)
        return {'account': {'account_ref': str(account_id), 'status': 'verified',
                            'history_state': 'ready' if state == 'ready' else 'history_pending'}}


def register_trusted_account_routes(app, settings, database):
    async def handler(request):
        request_id = random_hex(16)
        try:
            require_trusted_backend(request, settings)
            body = await _json_body(request)
            async with database.acquire() as connection:
                result = await ensure_account(connection, body)
            return web.json_response({'request_id': request_id, **result})
        except ApiError as error:
            return _error_response(request_id, error)
        except (asyncpg.PostgresError, OSError):
            return _error_response(request_id, ApiError('SERVICE_UNAVAILABLE', http=503, retryable=True))
    app.router.add_post(PATH, handler)
