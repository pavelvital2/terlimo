"""Additive schema roundtrip and refusal to erase accepted durable receipts."""
import asyncpg
import pytest
from test_referral_identity import connection, install
from terlimo_backend.auth_api import ApiError
from terlimo_backend.migrations.runner import rollback_migration, apply_migrations
from terlimo_backend.referral import change_candidate


async def test_unused_migration_roundtrip(migrated_url):
    c=await asyncpg.connect(migrated_url)
    try:
        await rollback_migration(c,'0038_referral')
        assert await c.fetchval("SELECT to_regclass('public.referral_operations')") is None
        assert await c.fetchval("SELECT count(*) FROM information_schema.columns WHERE table_name='accounts' AND column_name='referral_code'")==0
        await apply_migrations(c)
        assert await c.fetchval("SELECT to_regclass('public.referral_operations')")=='referral_operations'
    finally:await c.close()


async def test_definitive_no_mutation_receipt_prevents_destructive_down(migrated_url):
    c=await connection(migrated_url)
    try:
        iid=await install(c,'migration-proof')
        with pytest.raises(ApiError,match='REFERRAL_CODE_INVALID'):
            await change_candidate(c,installation_id=iid,code='MissingCode',idempotency_key='migration-proof-key')
        with pytest.raises(asyncpg.PostgresError,match='retain additive schema'):
            await rollback_migration(c,'0038_referral')
        assert await c.fetchval('SELECT count(*) FROM referral_operations')==1
        assert await c.fetchval("SELECT count(*) FROM schema_migrations WHERE id='0038_referral'")==1
        with pytest.raises(ApiError,match='REFERRAL_CODE_INVALID'):
            await change_candidate(c,installation_id=iid,code='MissingCode',idempotency_key='migration-proof-key')
        assert await c.fetchval('SELECT count(*) FROM referral_candidates')==0
    finally:await c.close()
