"""Versioned migrations: repeatability, status on a fresh database, reversibility, drift."""

from __future__ import annotations

import json
import shutil

import asyncpg
import pytest

from terlimo_backend.migrations import runner


def _all_versions() -> list[str]:
    return [migration.version for migration in runner.discover_versions()]


async def _connect(database_url: str) -> asyncpg.Connection:
    return await asyncpg.connect(database_url, timeout=10)


async def _tables(connection: asyncpg.Connection) -> set[str]:
    rows = await connection.fetch(
        "SELECT tablename FROM pg_tables WHERE schemaname = 'public'"
    )
    return {row["tablename"] for row in rows}


async def test_migrations_repeatable_and_status(database_url):
    expected = _all_versions()
    assert expected == [
        "0001_core",
        "0002_outbox_claim_ownership",
        "0003_installation_session",
        "0004_receipt_environment_and_retention",
        "0005_gateway_control",
        "0006_operation_ownership",
        "0007_subject_revision_and_hour_installation",
        "0008_entitlement_account_or_hour",
        "0009_catalog_and_sync",
        "0010_subject_catalog_revision",
        "0011_registry_lifecycle",
        "0012_session_binding_fence",
        "0013_gateway_confirmed_worker_cap",
        "0014_grant_proven_target",
        "0015_gateway_vk_hashes",
        "0016_gateway_snapshot_epoch",
        "0017_onboarding_hour_storage",
        "0018_onboarding_sweep_cursors",
        "0019_explicit_connect_start",
        "0020_hour_grant_subject",
        "0021_telegram_registration",
        "0022_one_trial_per_account",
        "0023_payment_orders",
        "0024_s5_payment_quotes",
        "0025_order_credit_revision",
        "0026_entitlement_source_plan",
        "0027_order_source_quote",
        "0028_announcements",
        "0029_usage_pipeline",
        "0030_entitlement_reminders",
        "0031_reminder_stages",
        "0032_s5_card_method",
        "0033_device_list_revisions",
        "0034_checkout_owner_refs",
        "0035_checkout_sessions",
    ]
    connection = await _connect(database_url)
    try:
        status = await runner.migration_status(connection)
        assert status["applied"] == []
        assert status["pending"] == expected

        assert await runner.apply_migrations(connection) == expected
        assert await runner.apply_migrations(connection) == []

        status = await runner.migration_status(connection)
        assert status["applied"] == expected
        assert status["pending"] == []

        versions = await connection.fetch("SELECT id FROM schema_migrations ORDER BY id")
        assert [row["id"] for row in versions] == expected
    finally:
        await connection.close()


async def test_migration_down_and_up_roundtrip(database_url):
    expected = _all_versions()
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        assert "accounts" in await _tables(connection)
        assert "outbox_operations" in await _tables(connection)

        # Revert in reverse order (down scripts are per-version).
        for version in reversed(expected):
            await runner.rollback_migration(connection, version)

        remaining = await _tables(connection)
        for table in (
            "accounts",
            "installations",
            "sessions",
            "account_bindings",
            "entitlements",
            "gateways",
            "grants",
            "outbox_operations",
            "outbox_effect_log",
        ):
            assert table not in remaining
        assert (await runner.migration_status(connection))["pending"] == expected

        assert await runner.apply_migrations(connection) == expected
        columns = await connection.fetch(
            """
            SELECT column_name FROM information_schema.columns
            WHERE table_name = 'outbox_operations'
            """
        )
        assert {"claim_token", "lease_expires_at"} <= {row["column_name"] for row in columns}
        receipt_columns = await connection.fetch(
            """
            SELECT column_name FROM information_schema.columns
            WHERE table_name = 'operation_receipts'
            """
        )
        assert {"environment", "result_expires_at", "retain_until"} <= {
            row["column_name"] for row in receipt_columns
        }
    finally:
        await connection.close()


async def test_migration_checksum_drift_is_rejected(database_url, tmp_path):
    versions_dir = tmp_path / "versions"
    shutil.copytree(runner.VERSIONS_DIR, versions_dir)
    expected = _all_versions()
    connection = await _connect(database_url)
    try:
        assert await runner.apply_migrations(connection, versions_dir=versions_dir) == expected
        migration_file = versions_dir / "0001_core.sql"
        migration_file.write_text(
            migration_file.read_text(encoding="utf-8") + "\n-- drifted\n", encoding="utf-8"
        )
        with pytest.raises(runner.MigrationDriftError):
            await runner.apply_migrations(connection, versions_dir=versions_dir)
    finally:
        await connection.close()


async def test_0008_refuses_accountless_non_hour_and_keeps_state(database_url, tmp_path):
    early_dir = tmp_path / "versions_early"
    early_dir.mkdir()
    for path in sorted(runner.VERSIONS_DIR.glob("*.sql")):
        if path.name.startswith(("0008", "0009", "0010", "0011", "0012", "0013", "0014", "0015", "0016", "0017", "0018", "0019", "0020", "0021", "0022", "0023", "0024", "0025", "0026", "0027", "0028", "0029", "0030", "0031", "0032", "0033", "0034", "0035")):
            continue
        (early_dir / path.name).write_bytes(path.read_bytes())
    connection = await _connect(database_url)
    try:
        assert await runner.apply_migrations(connection, versions_dir=early_dir) == [
            "0001_core",
            "0002_outbox_claim_ownership",
            "0003_installation_session",
            "0004_receipt_environment_and_retention",
            "0005_gateway_control",
            "0006_operation_ownership",
            "0007_subject_revision_and_hour_installation",
        ]
        await connection.execute(
            """
            INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at)
            VALUES (NULL, 'paid', 'active', now(), now() + interval '1 day')
            """
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="MIGRATION_UNSAFE_0008"):
            await runner.apply_migrations(connection)
        assert "0008_entitlement_account_or_hour" not in (
            await runner.migration_status(connection)
        )["applied"]
        assert await connection.fetchval(
            "SELECT count(*) FROM entitlements WHERE account_id IS NULL"
        ) == 1
        assert await connection.fetchval(
            """
            SELECT count(*) FROM pg_constraint
            WHERE conname = 'entitlements_account_or_hour_check'
            """
        ) == 0
    finally:
        await connection.close()


async def test_0008_invariant_and_reversible(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        # A legitimate unlinked onboarding hour stays allowed.
        await connection.execute(
            """
            INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at)
            VALUES (NULL, 'onboarding_hour', 'active', now(), now() + interval '30 minutes')
            """
        )
        with pytest.raises(asyncpg.exceptions.CheckViolationError):
            await connection.execute(
                """
                INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at)
                VALUES (NULL, 'trial', 'active', now(), now() + interval '1 day')
                """
            )
        await runner.rollback_migration(connection, "0008_entitlement_account_or_hour")
        assert await connection.fetchval(
            "SELECT count(*) FROM entitlements WHERE kind = 'onboarding_hour'"
        ) == 1
        assert await runner.apply_migrations(connection) == ["0008_entitlement_account_or_hour"]
    finally:
        await connection.close()


async def test_down_0009_refuses_when_correlated_operations_exist(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        await connection.execute(
            """
            INSERT INTO outbox_operations (operation_type, idempotency_key, correlation_id)
            VALUES ('gateway.apply_grant', 'down-0009-key', gen_random_uuid())
            """
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0009"):
            await runner.rollback_migration(connection, "0009_catalog_and_sync")
        assert "0009_catalog_and_sync" in (await runner.migration_status(connection))["applied"]
        await connection.execute(
            "UPDATE outbox_operations SET correlation_id = NULL WHERE correlation_id IS NOT NULL"
        )
        await runner.rollback_migration(connection, "0009_catalog_and_sync")
        assert "0009_catalog_and_sync" not in (
            await runner.migration_status(connection)
        )["applied"]
    finally:
        await connection.close()


async def test_0010_converts_existing_access_receipts_without_loss(database_url, tmp_path):
    early_dir = tmp_path / "versions_before_0010"
    early_dir.mkdir()
    for path in sorted(runner.VERSIONS_DIR.glob("*.sql")):
        if path.name.startswith(("0010", "0011", "0012", "0013", "0014", "0015", "0016", "0017", "0018", "0019", "0020", "0021", "0022", "0023", "0024", "0025", "0026", "0027", "0028", "0029", "0030", "0031", "0032", "0033", "0034", "0035")):
            continue
        (early_dir / path.name).write_bytes(path.read_bytes())
    connection = await _connect(database_url)
    try:
        applied = await runner.apply_migrations(connection, versions_dir=early_dir)
        assert applied[-1] == "0009_catalog_and_sync"
        await connection.execute(
            """
            INSERT INTO operation_receipts
                (environment, installation_ref, account_ref, op, idempotency_key,
                 business_digest, result, result_expires_at, retain_until)
            VALUES ('test', 'inst', 'acct', 'access.sync', 'migrate-key',
                    'digest', '{"operation_id": "op-1"}'::jsonb,
                    now() + interval '1 day', now() + interval '7 days'),
                   ('test', 'inst', 'acct', 'session.enroll', 'enroll-key',
                    'digest', '{"session_id": "s-1"}'::jsonb,
                    now() + interval '1 day', now() + interval '7 days')
            """
        )
        assert await runner.apply_migrations(connection) == [
            "0010_subject_catalog_revision",
            "0011_registry_lifecycle",
            "0012_session_binding_fence",
            "0013_gateway_confirmed_worker_cap",
            "0014_grant_proven_target",
            "0015_gateway_vk_hashes",
            "0016_gateway_snapshot_epoch",
            "0017_onboarding_hour_storage",
            "0018_onboarding_sweep_cursors",
            "0019_explicit_connect_start",
        "0020_hour_grant_subject",
        "0021_telegram_registration",
        "0022_one_trial_per_account",
        "0023_payment_orders",
        "0024_s5_payment_quotes",
        "0025_order_credit_revision",
        "0026_entitlement_source_plan",
        "0027_order_source_quote",
        "0028_announcements",
        "0029_usage_pipeline",
        "0030_entitlement_reminders",
        "0031_reminder_stages",
        "0032_s5_card_method",
            "0033_device_list_revisions",
        "0034_checkout_owner_refs",
        "0035_checkout_sessions",
        ]
        access = await connection.fetchrow(
            "SELECT * FROM operation_receipts WHERE op = 'access.sync'"
        )
        enroll = await connection.fetchrow(
            "SELECT * FROM operation_receipts WHERE op = 'session.enroll'"
        )
        assert json.loads(access["result"]) == {"operation_id": "op-1"}
        assert access["result_expires_at"] is None and access["retain_until"] is None
        assert json.loads(enroll["result"]) == {"session_id": "s-1"}
        assert enroll["result_expires_at"] is not None and enroll["retain_until"] is not None
    finally:
        await connection.close()


async def test_0011_backfills_only_from_real_registry_rows(database_url, tmp_path):
    early_dir = tmp_path / "versions_before_0011"
    early_dir.mkdir()
    for path in sorted(runner.VERSIONS_DIR.glob("*.sql")):
        if path.name.startswith(("0011", "0012", "0013", "0014", "0015", "0016", "0017", "0018", "0019", "0020", "0021", "0022", "0023", "0024", "0025", "0026", "0027", "0028", "0029", "0030", "0031", "0032", "0033", "0034", "0035")):
            continue
        (early_dir / path.name).write_bytes(path.read_bytes())
    connection = await _connect(database_url)
    try:
        applied = await runner.apply_migrations(connection, versions_dir=early_dir)
        assert applied[-1] == "0010_subject_catalog_revision"
        await connection.execute(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, registry_state)
            VALUES ('bf-test-1', 'test', '{}'::jsonb, 'registered'),
                   ('bf-test-2', 'test', '{}'::jsonb, 'disabled'),
                   ('bf-prod-1', 'production', '{}'::jsonb, 'registered')
            """
        )
        assert await runner.apply_migrations(connection) == [
            "0011_registry_lifecycle",
            "0012_session_binding_fence",
            "0013_gateway_confirmed_worker_cap",
            "0014_grant_proven_target",
            "0015_gateway_vk_hashes",
            "0016_gateway_snapshot_epoch",
            "0017_onboarding_hour_storage",
            "0018_onboarding_sweep_cursors",
            "0019_explicit_connect_start",
        "0020_hour_grant_subject",
        "0021_telegram_registration",
        "0022_one_trial_per_account",
        "0023_payment_orders",
        "0024_s5_payment_quotes",
        "0025_order_credit_revision",
        "0026_entitlement_source_plan",
        "0027_order_source_quote",
        "0028_announcements",
        "0029_usage_pipeline",
        "0030_entitlement_reminders",
        "0031_reminder_stages",
        "0032_s5_card_method",
            "0033_device_list_revisions",
        "0034_checkout_owner_refs",
        "0035_checkout_sessions",
        ]
        rows = await connection.fetch(
            "SELECT environment, initialized_at FROM registry_environments ORDER BY environment"
        )
        assert [row["environment"] for row in rows] == ["production", "test"]
        assert all(row["initialized_at"] is not None for row in rows)
        assert await connection.fetchval(
            "SELECT count(*) FROM registry_environments WHERE environment = 'staging'"
        ) == 0
    finally:
        await connection.close()


async def _seed_0012_subject(connection) -> tuple:
    account_id = await connection.fetchval(
        "INSERT INTO accounts (status) VALUES ('verified') RETURNING id"
    )
    installation_id = await connection.fetchval(
        """
        INSERT INTO installations
            (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
        VALUES ('test', 'android', gen_random_uuid()::text, 'spki', 'technical') RETURNING id
        """
    )
    binding_id = await connection.fetchval(
        """
        INSERT INTO account_bindings (account_id, installation_id, status)
        VALUES ($1, $2, 'active') RETURNING id
        """,
        account_id,
        installation_id,
    )
    return account_id, installation_id, binding_id


async def test_down_0012_refuses_fk_delete_generation_residue(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        account_id, installation_id, binding_id = await _seed_0012_subject(connection)
        await connection.execute(
            """
            INSERT INTO sessions
                (account_id, installation_id, scopes, generation, expires_at, token_sha256,
                 binding_id, binding_generation)
            VALUES ($1, $2, ARRAY['session:read'], 1, now() + interval '1 hour',
                    'down-0012-residue', $3, 3)
            """,
            account_id,
            installation_id,
            binding_id,
        )
        # ON DELETE SET NULL leaves the generation residue behind.
        await connection.execute("DELETE FROM account_bindings WHERE id = $1", binding_id)
        row = await connection.fetchrow(
            "SELECT binding_id, binding_generation FROM sessions WHERE token_sha256 = 'down-0012-residue'"
        )
        assert row["binding_id"] is None and row["binding_generation"] == 3

        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0012"):
            await runner.rollback_migration(connection, "0012_session_binding_fence")
        assert "0012_session_binding_fence" in (
            await runner.migration_status(connection)
        )["applied"]
        assert await connection.fetchval(
            "SELECT count(*) FROM schema_migrations WHERE id = '0012_session_binding_fence'"
        ) == 1
        columns = await connection.fetch(
            """
            SELECT column_name FROM information_schema.columns
            WHERE table_name = 'sessions' AND column_name IN ('binding_id', 'binding_generation')
            """
        )
        assert {item["column_name"] for item in columns} == {"binding_id", "binding_generation"}
        preserved = await connection.fetchrow(
            "SELECT binding_id, binding_generation FROM sessions WHERE token_sha256 = 'down-0012-residue'"
        )
        assert preserved["binding_id"] is None and preserved["binding_generation"] == 3
    finally:
        await connection.close()


async def test_down_0012_refuses_legacy_account_bound_null_fences(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        account_id, installation_id, _binding_id = await _seed_0012_subject(connection)
        # A legacy account-bound row without a provable fence still blocks the downgrade.
        await connection.execute(
            """
            INSERT INTO sessions
                (account_id, installation_id, scopes, generation, expires_at, token_sha256)
            VALUES ($1, $2, ARRAY['session:read'], 1, now() + interval '1 hour',
                    'down-0012-legacy')
            """,
            account_id,
            installation_id,
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0012"):
            await runner.rollback_migration(connection, "0012_session_binding_fence")
        assert "0012_session_binding_fence" in (
            await runner.migration_status(connection)
        )["applied"]
        assert await connection.fetchval(
            "SELECT count(*) FROM sessions WHERE token_sha256 = 'down-0012-legacy'"
        ) == 1
    finally:
        await connection.close()


async def test_down_0012_safe_when_only_unlinked_rows(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        installation_id = await connection.fetchval(
            """
            INSERT INTO installations
                (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test', 'android', gen_random_uuid()::text, 'spki', 'technical') RETURNING id
            """
        )
        await connection.execute(
            """
            INSERT INTO sessions
                (account_id, installation_id, scopes, generation, expires_at, token_sha256)
            VALUES (NULL, $1, ARRAY['enrollment'], 1, now() + interval '1 hour',
                    'down-0012-unlinked')
            """,
            installation_id,
        )
        await runner.rollback_migration(connection, "0012_session_binding_fence")
        assert "0012_session_binding_fence" not in (
            await runner.migration_status(connection)
        )["applied"]
        assert await connection.fetchval(
            "SELECT count(*) FROM sessions WHERE token_sha256 = 'down-0012-unlinked'"
        ) == 1
        assert await runner.apply_migrations(connection) == ["0012_session_binding_fence"]
    finally:
        await connection.close()


async def test_down_0013_refuses_when_confirmed_cap_exists(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        await connection.execute(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, registry_state,
                                  confirmed_max_workers)
            VALUES ('cap-gw', 'test', '{"node_id":"cap-gw","target_workers":36}'::jsonb,
                    'registered', 36)
            """
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0013"):
            await runner.rollback_migration(connection, "0013_gateway_confirmed_worker_cap")
        assert "0013_gateway_confirmed_worker_cap" in (
            await runner.migration_status(connection)
        )["applied"]
        assert await connection.fetchval(
            "SELECT count(*) FROM gateways WHERE confirmed_max_workers IS NOT NULL"
        ) == 1
    finally:
        await connection.close()


async def test_down_0014_refuses_when_proven_target_exists(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        account_id = await connection.fetchval(
            "INSERT INTO accounts (status) VALUES ('verified') RETURNING id"
        )
        installation_id = await connection.fetchval(
            """
            INSERT INTO installations
                (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test', 'android', 'down-0014-fp', 'spki', 'technical') RETURNING id
            """
        )
        binding_id = await connection.fetchval(
            """
            INSERT INTO account_bindings (account_id, installation_id, status)
            VALUES ($1, $2, 'active') RETURNING id
            """,
            account_id,
            installation_id,
        )
        gateway_id = await connection.fetchval(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, registry_state)
            VALUES ('down-0014-gw', 'test', '{"node_id":"down-0014-gw"}'::jsonb, 'registered')
            RETURNING id
            """
        )
        await connection.execute(
            """
            INSERT INTO grants
                (binding_id, gateway_id, desired_generation, not_after, target_node_id)
            VALUES ($1, $2, 1, now() + interval '1 hour', 'down-0014-gw')
            """,
            binding_id,
            gateway_id,
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0014"):
            await runner.rollback_migration(connection, "0014_grant_proven_target")
        assert "0014_grant_proven_target" in (
            await runner.migration_status(connection)
        )["applied"]
        assert await connection.fetchval(
            "SELECT count(*) FROM grants WHERE target_node_id IS NOT NULL"
        ) == 1
    finally:
        await connection.close()


async def test_down_0015_refuses_when_vk_snapshot_exists(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        await connection.execute(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, registry_state, vk_hashes)
            VALUES ('vk-gw', 'test', '{"node_id":"vk-gw"}'::jsonb, 'registered', '["h1"]'::jsonb)
            """
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0015"):
            await runner.rollback_migration(connection, "0015_gateway_vk_hashes")
        assert "0015_gateway_vk_hashes" in (
            await runner.migration_status(connection)
        )["applied"]
        assert await connection.fetchval(
            "SELECT count(*) FROM gateways WHERE vk_hashes IS NOT NULL"
        ) == 1
    finally:
        await connection.close()


async def test_down_0016_refuses_when_epoch_advanced(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        await connection.execute(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, registry_state, snapshot_epoch)
            VALUES ('epoch-gw', 'test', '{"node_id":"epoch-gw"}'::jsonb, 'registered', 3)
            """
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0016"):
            await runner.rollback_migration(connection, "0016_gateway_snapshot_epoch")
        assert "0016_gateway_snapshot_epoch" in (
            await runner.migration_status(connection)
        )["applied"]
    finally:
        await connection.close()


async def test_down_0011_refuses_when_marker_exists(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        await connection.execute(
            "INSERT INTO registry_environments (environment) VALUES ('test')"
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0011"):
            await runner.rollback_migration(connection, "0011_registry_lifecycle")
        assert "0011_registry_lifecycle" in (
            await runner.migration_status(connection)
        )["applied"]
        assert await connection.fetchval(
            "SELECT count(*) FROM registry_environments WHERE environment = 'test'"
        ) == 1
        assert await connection.fetchval(
            "SELECT count(*) FROM schema_migrations WHERE id = '0011_registry_lifecycle'"
        ) == 1
    finally:
        await connection.close()


async def test_down_0009_refuses_subject_catalog_revision_or_access_receipt(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        installation_id = await connection.fetchval(
            """
            INSERT INTO installations
                (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test', 'android', 'down-0009-fp', 'spki', 'technical') RETURNING id
            """
        )
        await connection.execute(
            "INSERT INTO catalog_revisions (installation_id, revision, fingerprint) VALUES ($1, 1, 'fp')",
            installation_id,
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0009"):
            await runner.rollback_migration(connection, "0009_catalog_and_sync")
        assert "0009_catalog_and_sync" in (await runner.migration_status(connection))["applied"]

        await connection.execute("DELETE FROM catalog_revisions")
        await connection.execute(
            """
            INSERT INTO operation_receipts
                (environment, installation_ref, account_ref, op, idempotency_key, business_digest)
            VALUES ('test', 'inst', 'acct', 'access.sync', 'down-0009-access-key', 'digest')
            """
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0009"):
            await runner.rollback_migration(connection, "0009_catalog_and_sync")
        assert "0009_catalog_and_sync" in (await runner.migration_status(connection))["applied"]
    finally:
        await connection.close()


async def test_down_0010_refuses_any_subject_catalog_revision(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        installation_id = await connection.fetchval(
            """
            INSERT INTO installations
                (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test', 'android', 'down-0010-fp', 'spki', 'technical') RETURNING id
            """
        )
        # Even revision 1 is identity: the table must not be dropped under a populated revision.
        await connection.execute(
            "INSERT INTO catalog_revisions (installation_id, revision, fingerprint) VALUES ($1, 1, 'fp')",
            installation_id,
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0010"):
            await runner.rollback_migration(connection, "0010_subject_catalog_revision")
        assert "0010_subject_catalog_revision" in (
            await runner.migration_status(connection)
        )["applied"]
        assert await connection.fetchval(
            "SELECT count(*) FROM catalog_revisions WHERE installation_id = $1", installation_id
        ) == 1
        assert await _tables(connection)
        assert "catalog_revisions" in await _tables(connection)
        assert await connection.fetchval(
            "SELECT count(*) FROM schema_migrations WHERE id = $1",
            "0010_subject_catalog_revision",
        ) == 1
    finally:
        await connection.close()


async def test_down_0010_refuses_durable_access_receipt(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        await connection.execute(
            """
            INSERT INTO operation_receipts
                (environment, installation_ref, account_ref, op, idempotency_key, business_digest)
            VALUES ('test', 'inst', 'acct', 'access.sync', 'down-0010-access-key', 'digest')
            """
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0010"):
            await runner.rollback_migration(connection, "0010_subject_catalog_revision")
        assert "0010_subject_catalog_revision" in (
            await runner.migration_status(connection)
        )["applied"]
        assert await connection.fetchval(
            "SELECT count(*) FROM operation_receipts WHERE op = 'access.sync'"
        ) == 1
    finally:
        await connection.close()


async def test_down_0010_safe_when_empty(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        await runner.rollback_migration(connection, "0010_subject_catalog_revision")
        assert "0010_subject_catalog_revision" not in (
            await runner.migration_status(connection)
        )["applied"]
        assert await runner.apply_migrations(connection) == ["0010_subject_catalog_revision"]
    finally:
        await connection.close()


async def test_down_0007_refuses_when_provenance_or_revisions_exist(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        await connection.execute(
            """
            INSERT INTO entitlements (account_id, kind, status, starts_at, ends_at)
            VALUES (NULL, 'onboarding_hour', 'active', now(), now() + interval '30 minutes')
            """
        )
        with pytest.raises(asyncpg.exceptions.RaiseError, match="ROLLBACK_UNSAFE_0007"):
            await runner.rollback_migration(
                connection, "0007_subject_revision_and_hour_installation"
            )
        status = await runner.migration_status(connection)
        assert "0007_subject_revision_and_hour_installation" in status["applied"]
        assert await connection.fetchval(
            """
            SELECT count(*) FROM information_schema.columns
            WHERE table_name = 'entitlements' AND column_name = 'installation_id'
            """
        ) == 1
        assert await connection.fetchval(
            """
            SELECT count(*) FROM information_schema.tables
            WHERE table_name = 'subject_revisions'
            """
        ) == 1
        assert await connection.fetchval("SELECT count(*) FROM entitlements") == 1
    finally:
        await connection.close()


async def test_down_0007_safe_only_without_new_data(database_url):
    connection = await _connect(database_url)
    try:
        await runner.apply_migrations(connection)
        await runner.rollback_migration(
            connection, "0007_subject_revision_and_hour_installation"
        )
        assert await connection.fetchval(
            """
            SELECT count(*) FROM information_schema.tables
            WHERE table_name = 'subject_revisions'
            """
        ) == 0
        assert await connection.fetchval(
            """
            SELECT count(*) FROM information_schema.columns
            WHERE table_name = 'entitlements' AND column_name = 'installation_id'
            """
        ) == 0
        assert "0007_subject_revision_and_hour_installation" in (
            await runner.migration_status(connection)
        )["pending"]
        assert await runner.apply_migrations(connection) == [
            "0007_subject_revision_and_hour_installation"
        ]
    finally:
        await connection.close()
