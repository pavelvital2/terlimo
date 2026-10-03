"""Affected account reservation/FK regressions; isolated PG and fake provider."""
import asyncio
import uuid

import asyncpg
import pytest
from test_payment_addon_slice import confirm, hdr, identity
from test_referral_payments import create, quotation, referred
from test_s4_payments import FakePlategaProvider, _app

from terlimo_backend.payments import (
    ORDERS_PATH,
    apply_paid_entitlement,
    reconcile_payments,
    record_webhook,
)
from terlimo_backend.referral import initialize_account, stage_history


@pytest.mark.parametrize('preissued', [False, True])
async def test_ordinary_main_cannot_bypass_discount_reservation(migrated_url, settings_factory, preissued):
    provider = FakePlategaProvider()
    client, _settings, db = await _app(settings_factory, migrated_url, provider=provider)
    c = await asyncpg.connect(migrated_url)
    try:
        account, _iid, token = await referred(client, migrated_url)
        iid2, token2 = await identity(client, migrated_url, uuid.uuid4())
        await c.execute('UPDATE account_bindings SET account_id=$2 WHERE installation_id=$1', iid2, account)
        await c.execute('UPDATE sessions SET account_id=$2 WHERE installation_id=$1', iid2, account)
        if preissued:
            # Quote issued before referral attribution: immutable ordinary price.
            actual_inviter = await c.fetchval('SELECT referred_by_account_id FROM accounts WHERE id=$1', account)
            await c.execute('UPDATE accounts SET referred_by_account_id=NULL WHERE id=$1', account)
            ordinary = await quotation(client, token2)
            await c.execute('UPDATE accounts SET referred_by_account_id=$2 WHERE id=$1', account, actual_inviter)
        discounted = await quotation(client, token)
        key = uuid.uuid4().hex
        first = await create(client, token, discounted, key)
        assert first.status == 200, await first.text()
        payment = await first.json()
        if not preissued:
            ordinary = await quotation(client, token2)
        assert ordinary['amount']['amount_minor'] == 20000 and 'pricing' not in ordinary
        blocked_key = uuid.uuid4().hex
        blocked = await create(client, token2, ordinary, blocked_key)
        assert blocked.status == 409, await blocked.text()
        error = await blocked.json()
        assert error['details']['create_resolution'] == {
            'kind': 'no_order', 'quote_id': ordinary['quote_id'],
            'request_idempotency_key': blocked_key, 'reason': 'referral_discount_reserved'}
        assert len(provider.create_calls) == 1
        legacy = await client.post(ORDERS_PATH, headers=hdr(token2),
            json={'months':1, 'idempotency_key':uuid.uuid4().hex})
        assert legacy.status == 409, await legacy.text()
        legacy_error = await legacy.json()
        assert legacy_error['code'] == 'REFERRAL_DISCOUNT_RESERVED'
        assert legacy_error['retryable'] is True
        assert 'create_resolution' not in legacy_error.get('details', {})
        assert len(provider.create_calls) == 1
        assert (await create(client, token, discounted, key)).status == 200
        await confirm(client, migrated_url, payment, 100)
        oid = uuid.UUID(payment['payment_id'])
        entitlement = await c.fetchval('SELECT applied_entitlement_id FROM payment_orders WHERE id=$1', oid)
        assert (await c.fetchval('SELECT ends_at-starts_at FROM entitlements WHERE id=$1', entitlement)).days == 30
        assert (await create(client, token2, ordinary, blocked_key)).status == 409
        fresh = await quotation(client, token2)
        second = await create(client, token2, fresh, uuid.uuid4().hex)
        assert second.status == 200, await second.text()
        assert len(provider.create_calls) == 2
        assert [call['amount'] for call in provider.create_calls] == [100, 200]
        assert await c.fetchval('SELECT consumed_order_id FROM referral_benefits WHERE account_id=$1', account) == oid
    finally:
        await c.close()
        await db.close()
        await client.close()


@pytest.mark.parametrize('writer', ['callback', 'reconcile', 'deferred'])
async def test_paid_account_fk_precedes_benefit_with_history_barrier(migrated_url, settings_factory, writer):
    provider = FakePlategaProvider()
    client, settings, db = await _app(settings_factory, migrated_url, provider=provider)
    history = await asyncpg.connect(migrated_url)
    paid = await asyncpg.connect(migrated_url)
    task = None
    try:
        account, _iid, token = await referred(client, migrated_url)
        q = await quotation(client, token)
        response = await create(client, token, q, uuid.uuid4().hex)
        assert response.status == 200, await response.text()
        payment = await response.json()
        oid = uuid.UUID(payment['payment_id'])
        order = await paid.fetchrow('SELECT * FROM payment_orders WHERE id=$1', oid)
        tg = await history.fetchval('SELECT telegram_id FROM accounts WHERE id=$1', account)
        # Valid protected history makes initialize_account update both account and benefit.
        await stage_history(history, [{'telegram_id': tg, 'code': None,
            'referred_by_telegram_id': None, 'proven_new': True,
            'trial_used': False, 'first_main_paid': False}], source_sha256='c'*64)
        if writer == 'deferred':
            await paid.execute("UPDATE payment_orders SET status='succeeded' WHERE id=$1", oid)
        provider.status_view = {'status': 'CONFIRMED', 'amount': 100, 'currency': 'RUB'}
        async def run_paid():
            if writer == 'callback':
                return await record_webhook(paid, settings, merchant=settings.platega_merchant_id,
                    secret=settings.platega_secret, body={'id': order['provider_payment_id'],
                    'status': 'CONFIRMED', 'amount': 100, 'currency': 'RUB'})
            if writer == 'reconcile':
                return await reconcile_payments(paid, settings, provider)
            return await apply_paid_entitlement(paid, settings, order_id=oid)
        async with history.transaction():
            await history.fetchval('SELECT id FROM accounts WHERE id=$1 FOR UPDATE', account)
            task = asyncio.create_task(run_paid())
            # Barrier proves real PG lock wait rather than relying on scheduling/sleep.
            async with asyncio.timeout(5):
                while not await history.fetchval('SELECT $1=ANY(pg_blocking_pids($2))', history.get_server_pid(), paid.get_server_pid()):
                    await asyncio.sleep(.01)
            assert not task.done()
            # Paid must wait for account BEFORE it writes/locks benefits.
            assert await history.fetchval('SELECT first_paid_order_id FROM referral_benefits WHERE account_id=$1', account) is None
            await asyncio.wait_for(initialize_account(history, account), timeout=3)
        await asyncio.wait_for(task, timeout=5)
        row = await history.fetchrow('SELECT * FROM payment_orders WHERE id=$1', oid)
        assert row['status'] == 'succeeded' and row['applied_entitlement_id'] is not None
        assert await history.fetchval('SELECT first_paid_order_id FROM referral_benefits WHERE account_id=$1', account) == oid
        assert await history.fetchval('SELECT consumed_order_id FROM referral_benefits WHERE account_id=$1', account) == oid
        assert await history.fetchval("SELECT count(*) FROM referral_rewards WHERE invitee_account_id=$1 AND event_kind='first_main_paid'", account) == 1
        assert await history.fetchval('SELECT count(*) FROM entitlements WHERE account_id=$1 AND kind=\'paid\'', account) == 1
    finally:
        if task is not None and not task.done():
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
        await paid.close()
        await history.close()
        await db.close()
        await client.close()


async def test_legacy_ordinary_invoice_fences_later_discounted_create(migrated_url, settings_factory):
    provider = FakePlategaProvider()
    client, _settings, db = await _app(settings_factory, migrated_url, provider=provider)
    c = await asyncpg.connect(migrated_url)
    try:
        account, _iid, token = await referred(client, migrated_url)
        iid2, token2 = await identity(client, migrated_url, uuid.uuid4())
        await c.execute('UPDATE account_bindings SET account_id=$2 WHERE installation_id=$1', iid2, account)
        await c.execute('UPDATE sessions SET account_id=$2 WHERE installation_id=$1', iid2, account)
        q = await quotation(client, token2)
        legacy = await client.post(ORDERS_PATH, headers=hdr(token), json={'months':1,'idempotency_key':uuid.uuid4().hex})
        assert legacy.status == 200, await legacy.text()
        legacy_payment = (await legacy.json())['payment']
        oid = uuid.UUID(legacy_payment['order_id'])
        assert await c.fetchval('SELECT checkout_owner_account_id FROM payment_orders WHERE id=$1', oid) is None
        blocked = await create(client, token2, q, uuid.uuid4().hex)
        assert blocked.status == 409, await blocked.text()
        assert (await blocked.json())['details']['create_resolution']['reason'] == 'referral_discount_reserved'
        assert len(provider.create_calls) == 1 and provider.create_calls[0]['amount'] == 200
        assert await c.fetchval('SELECT amount FROM payment_orders WHERE id=$1', oid) == 200
    finally:
        await c.close()
        await db.close()
        await client.close()
