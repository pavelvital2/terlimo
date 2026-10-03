"""Explicit account-only trial for the existing trusted Telegram principal."""
from __future__ import annotations

import asyncpg
from aiohttp import web

from .auth_api import ApiError, _error_response, _json_body, now_utc, random_hex
from .referral_trusted import require_trusted_backend
from .trial_activation import (TRIAL_CHANNEL_KEY, BotApiChannelMembership,
    ChannelMembershipUnavailable, _trial_projection, trial_eligibility, insert_trial)

PATH = '/api/mobile/v1/internal/telegram/trial'


def projection(row, moment, *, replay=True):
    return {**_trial_projection(row, moment, replay=replay),
            'entitlement_id': str(row['id']) if row else None,
            'revision': row['revision'] if row else None,
            'provisioning_state': 'external_pending' if row else None}


async def owner(connection, telegram_id, *, account_id=None, locked=False):
    row = await connection.fetchrow(
        "SELECT id,status,telegram_id FROM accounts WHERE telegram_id=$1" +
        (" FOR SHARE" if locked else ""), telegram_id)
    if (row is None or row['status'] != 'verified' or
            (account_id is not None and row['id'] != account_id)):
        raise ApiError('REGISTRATION_REQUIRED', http=403)
    return row['id']


async def trusted_trial(connection, checker, body):
    if (type(body) is not dict or set(body) != {'operation','telegram_id'} or
            body['operation'] not in ('activate','status') or
            type(body['telegram_id']) is not int or not 0 < body['telegram_id'] < 2**63):
        raise ApiError('BAD_MESSAGE', http=400)
    tg = body['telegram_id']
    account_id = await owner(connection, tg)
    moment = now_utc()
    existing = await trial_eligibility(connection, account_id, tg, moment,
                                       trusted=True, locked=False)
    if existing is not None or body['operation'] == 'status':
        return {'trial': projection(existing, moment)}
    # No SQL transaction/row lock spans the mandatory external membership check.
    try:
        member = await checker.is_member(tg)
    except ChannelMembershipUnavailable as error:
        raise ApiError('TRIAL_CHECK_UNAVAILABLE', http=503, retryable=True) from error
    if not member:
        raise ApiError('CHANNEL_MEMBERSHIP_REQUIRED', http=403,
                       details={'reason':'subscribe_to_official_channel_then_retry'})
    async with connection.transaction():
        # Match account-owned paid credit: stabilize owner before paid/trial advisories.
        await owner(connection, tg, account_id=account_id, locked=True)
        for key in (f'paid-account:{account_id}', f'trial:{account_id}'):
            await connection.execute('SELECT pg_advisory_xact_lock(hashtextextended($1,0))',key)
        await owner(connection, tg, account_id=account_id)
        moment = now_utc()
        existing = await trial_eligibility(connection, account_id, tg, moment, trusted=True)
        if existing is not None:
            return {'trial': projection(existing, moment)}
        row = await insert_trial(connection, account_id, moment)
        return {'trial': projection(row, moment, replay=False), 'account_state':'ACTIVE_TRIAL'}


def register_trusted_trial_routes(app, settings, database):
    async def handler(request):
        request_id = random_hex(16)
        try:
            require_trusted_backend(request, settings)
            body = await _json_body(request)
            checker = request.app.get(TRIAL_CHANNEL_KEY)
            if checker is None:
                checker = BotApiChannelMembership(settings)
            async with database.acquire() as connection:
                result = await trusted_trial(connection, checker, body)
            return web.json_response({'request_id':request_id, 'status':'ok', **result})
        except ApiError as error:
            return _error_response(request_id,error)
        except (asyncpg.PostgresError, OSError):
            return _error_response(request_id,ApiError('SERVICE_UNAVAILABLE',http=503,retryable=True))
    app.router.add_post(PATH,handler)
