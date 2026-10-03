"""B3: actual HTTP expiry proofs with isolated PostgreSQL and a fake provider."""
import json
import uuid

import asyncpg
import pytest

from test_referral_payments import create, quotation, referred
from test_s4_payments import FakePlategaProvider, _app


def decoded(value):
    while isinstance(value, str):
        value = json.loads(value)
    return value


async def freeze(url, q, *, alter=None):
    connection = await asyncpg.connect(url)
    try:
        row = await connection.fetchrow('SELECT * FROM s5_payment_quotes WHERE id=$1', uuid.UUID(q['quote_id']))
        product, pricing = decoded(row['product']), decoded(row['pricing'])
        amount = row['amount_minor']
        if alter:
            amount = alter(product, pricing, amount)
        await connection.execute('''UPDATE s5_payment_quotes SET product=$2::jsonb, pricing=$3::jsonb,
            amount_minor=$4, expires_at=now()-interval '1 second' WHERE id=$1''',
            uuid.UUID(q['quote_id']), json.dumps(product), json.dumps(pricing), amount)
    finally:
        await connection.close()


@pytest.mark.parametrize('extras', [False, True])
async def test_expired_discount_quote_proves_no_order_and_replays(migrated_url, settings_factory, extras):
    provider = FakePlategaProvider()
    client, _, db = await _app(settings_factory, migrated_url, provider=provider)
    try:
        _, _, token = await referred(client, migrated_url)
        q = await quotation(client, token)
        def with_extras(product, pricing, amount):
            # A frozen MAIN+paid-extras stored selection. Expiry certifies the
            # snapshot; no current entitlement or provider execution is needed.
            product['extra_amount_minor'] = 5000
            pricing['base_amount_minor'] += 5000
            pricing['payable_amount_minor'] += 5000
            return amount + 5000
        await freeze(migrated_url, q, alter=with_extras if extras else None)
        key = uuid.uuid4().hex
        for _ in range(2):
            response = await create(client, token, q, key)
            body = await response.json()
            assert response.status == 409 and body['code'] == 'QUOTE_EXPIRED', body
            assert body['retryable'] is False
            assert body['details']['reason'] == 'expired_quote_no_order'
        connection = await asyncpg.connect(migrated_url)
        try:
            assert await connection.fetchval('SELECT count(*) FROM payment_orders') == 0
            assert await connection.fetchval('SELECT reserved_order_id FROM referral_benefits') is None
        finally:
            await connection.close()
        assert provider.create_calls == []
    finally:
        await db.close()
        await client.close()


async def test_existing_immutable_discount_order_outranks_quote_expiry(migrated_url, settings_factory):
    provider = FakePlategaProvider()
    client, _, db = await _app(settings_factory, migrated_url, provider=provider)
    try:
        _, _, token = await referred(client, migrated_url)
        q = await quotation(client, token)
        key = uuid.uuid4().hex
        original_response = await create(client, token, q, key)
        assert original_response.status == 200, await original_response.text()
        original = await original_response.json()
        await freeze(migrated_url, q)
        replay_response = await create(client, token, q, key)
        assert replay_response.status == 200, await replay_response.text()
        replay = await replay_response.json()
        assert replay['payment_id'] == original['payment_id']
        assert replay['pricing'] == original['pricing']
        conflict = await create(client, token, q, uuid.uuid4().hex)
        conflict_body = await conflict.json()
        assert conflict.status == 409 and conflict_body['code'] == 'ORDER_CONFLICT'
        assert conflict_body.get('details', {}).get('reason') != 'expired_quote_no_order'
        assert len(provider.create_calls) == 1
    finally:
        await db.close()
        await client.close()


@pytest.mark.parametrize('fault', [
    'base', 'discount', 'payable', 'currency', 'kind', 'terms',
    'bool', 'float', 'numeric_string', 'main_low', 'addon', 'extra_key',
])
async def test_malformed_discount_snapshot_never_proves_no_order(migrated_url, settings_factory, fault):
    provider = FakePlategaProvider()
    client, _, db = await _app(settings_factory, migrated_url, provider=provider)
    try:
        _, _, token = await referred(client, migrated_url)
        q = await quotation(client, token)
        def corrupt(product, pricing, amount):
            if fault == 'base': pricing['base_amount_minor'] += 1
            elif fault == 'discount': pricing['discount_minor'] = 9999
            elif fault == 'payable': pricing['payable_amount_minor'] += 1
            elif fault == 'currency': pricing['currency'] = 'USD'
            elif fault == 'kind': pricing['discount_kind'] = 'other'
            elif fault == 'terms': pricing['terms_version'] = 'other'
            elif fault == 'bool': pricing['discount_minor'] = True
            elif fault == 'float': pricing['discount_minor'] = 10000.0
            elif fault == 'numeric_string': pricing['discount_minor'] = '10000'
            elif fault == 'extra_key': pricing['unknown'] = 'field'
            elif fault == 'main_low':
                product['base_amount_minor'], product['extra_amount_minor'] = 10000, 10000
            elif fault == 'addon': product['kind'] = 'device_addon'
            return amount
        await freeze(migrated_url, q, alter=corrupt)
        response = await create(client, token, q, uuid.uuid4().hex)
        body = await response.json()
        assert response.status == 409 and body['code'] == 'QUOTE_EXPIRED', body
        assert body.get('details', {}).get('reason') != 'expired_quote_no_order'
        assert 'create_resolution' not in body.get('details', {})
        assert provider.create_calls == []
        connection = await asyncpg.connect(migrated_url)
        try:
            assert await connection.fetchval('SELECT count(*) FROM payment_orders') == 0
        finally:
            await connection.close()
    finally:
        await db.close()
        await client.close()


async def test_foreign_owner_expired_discount_never_proves_no_order(migrated_url, settings_factory):
    provider = FakePlategaProvider()
    client, _, db = await _app(settings_factory, migrated_url, provider=provider)
    try:
        _, _, token = await referred(client, migrated_url)
        q = await quotation(client, token)
        def foreign(product, pricing, amount):
            product['owner_account_id'] = str(uuid.uuid4())
            return amount
        await freeze(migrated_url, q, alter=foreign)
        response = await create(client, token, q, uuid.uuid4().hex)
        body = await response.json()
        assert response.status == 404 and body['code'] == 'PAYMENT_NOT_FOUND', body
        assert 'create_resolution' not in body.get('details', {})
        assert provider.create_calls == []
    finally:
        await db.close()
        await client.close()


async def test_unknown_invoice_outranks_discount_expiry_proof(migrated_url, settings_factory):
    class Unknown(FakePlategaProvider):
        async def create_payment(self, **kwargs):
            await super().create_payment(**kwargs)
            from terlimo_backend.payments import ProviderUnknown
            raise ProviderUnknown()
    provider = Unknown()
    client, _, db = await _app(settings_factory, migrated_url, provider=provider)
    try:
        _, _, token = await referred(client, migrated_url)
        q = await quotation(client, token)
        key = uuid.uuid4().hex
        first = await create(client, token, q, key)
        assert first.status == 503, await first.text()
        assert (await first.json())['code'] == 'PAYMENT_PROVIDER_UNKNOWN'
        await freeze(migrated_url, q)
        for next_key in (key, uuid.uuid4().hex):
            response = await create(client, token, q, next_key)
            body = await response.json()
            if next_key == key:
                assert response.status == 503 and body['code'] == 'PAYMENT_PROVIDER_UNKNOWN', body
            else:
                # A different K cannot reuse the already-owned source Q.
                assert response.status == 409 and body['code'] == 'ORDER_CONFLICT', body
                assert body['details']['reason'] == 'quote_already_used'
            assert body.get('details', {}).get('reason') != 'expired_quote_no_order'
            assert 'create_resolution' not in body.get('details', {})
        assert len(provider.create_calls) == 1
    finally:
        await db.close()
        await client.close()


async def test_invalid_pricing_encoding_and_product_numbers_fail_closed(migrated_url, settings_factory):
    provider = FakePlategaProvider()
    client, _, db = await _app(settings_factory, migrated_url, provider=provider)
    try:
        _, _, token = await referred(client, migrated_url)
        q = await quotation(client, token)
        await freeze(migrated_url, q)
        connection = await asyncpg.connect(migrated_url)
        try:
            row = await connection.fetchrow('SELECT * FROM s5_payment_quotes WHERE id=$1', uuid.UUID(q['quote_id']))
            pricing, product = decoded(row['pricing']), decoded(row['product'])
            for malformed in ({}, 'invalid-json', [], False):
                await connection.execute('UPDATE s5_payment_quotes SET pricing=$2::jsonb WHERE id=$1',
                                         uuid.UUID(q['quote_id']), json.dumps(malformed))
                response = await create(client, token, q, uuid.uuid4().hex)
                body = await response.json()
                assert response.status == 409 and body['code'] == 'QUOTE_EXPIRED', body
                assert body.get('details', {}).get('reason') != 'expired_quote_no_order'
                assert 'create_resolution' not in body.get('details', {})
            await connection.execute('UPDATE s5_payment_quotes SET pricing=$2::jsonb WHERE id=$1',
                                     uuid.UUID(q['quote_id']), json.dumps(pricing))
            for field, value in (('base_amount_minor', True), ('extra_amount_minor', -1)):
                invalid_product = {**product, field: value}
                await connection.execute('UPDATE s5_payment_quotes SET product=$2::jsonb WHERE id=$1',
                                         uuid.UUID(q['quote_id']), json.dumps(invalid_product))
                response = await create(client, token, q, uuid.uuid4().hex)
                body = await response.json()
                assert response.status == 409 and body['code'] == 'QUOTE_EXPIRED', body
                assert body.get('details', {}).get('reason') != 'expired_quote_no_order'
            assert await connection.fetchval('SELECT count(*) FROM payment_orders') == 0
        finally:
            await connection.close()
        assert provider.create_calls == []
    finally:
        await db.close()
        await client.close()


@pytest.mark.parametrize('price', [10, 20])
async def test_legacy_no_discount_quote_retains_expiry_proof(migrated_url, settings_factory, price):
    from test_payment_addon_slice import identity
    buyer = uuid.uuid4()
    provider = FakePlategaProvider()
    client, _, db = await _app(settings_factory, migrated_url, provider=provider,
                               s5_control_account_id=str(buyer), s5_control_price_rub_1=price)
    try:
        _, token = await identity(client, migrated_url, buyer)
        q = await quotation(client, token)
        assert q['amount']['amount_minor'] == price * 100 and 'pricing' not in q
        connection = await asyncpg.connect(migrated_url)
        try:
            await connection.execute("UPDATE s5_payment_quotes SET expires_at=now()-interval '1 second' WHERE id=$1",
                                     uuid.UUID(q['quote_id']))
        finally:
            await connection.close()
        response = await create(client, token, q, uuid.uuid4().hex)
        body = await response.json()
        assert response.status == 409 and body['code'] == 'QUOTE_EXPIRED', body
        assert body['details']['reason'] == 'expired_quote_no_order'
        assert 'create_resolution' not in body['details']
        assert provider.create_calls == []
    finally:
        await db.close()
        await client.close()
