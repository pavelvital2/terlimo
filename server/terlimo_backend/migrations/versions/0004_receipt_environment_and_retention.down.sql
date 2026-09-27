-- Revert 0004_receipt_environment_and_retention.
--
-- Fail-closed rollback procedure: environment-aware receipts cannot be represented by the
-- legacy UNIQUE(installation_ref, op, idempotency_key) shape (the same key may legitimately
-- exist in two environments), and no idempotency data may be destroyed to satisfy a schema
-- rollback. If any environment-aware receipt exists this migration raises and the runner's
-- transaction rolls everything back, leaving receipts, indexes and schema_migrations intact.
-- Resolve/export those receipts explicitly before reverting 0004.
--
-- This file is the rollback procedure only; it does not change the applied up-migration
-- checksum (the runner tracks the up SQL). Legacy (environment IS NULL) receipts are
-- preserved by the rollback below; their raw result was already cleared by 0004 with a
-- documented decision.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM operation_receipts WHERE environment IS NOT NULL) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0004: environment-aware operation_receipts exist';
    END IF;
END $$;

DROP TABLE IF EXISTS public_endpoint_counters;
DROP INDEX IF EXISTS auth_challenges_retention_idx;
DROP INDEX IF EXISTS operation_receipts_retain_idx;
DROP INDEX IF EXISTS operation_receipts_env_key_idx;

ALTER TABLE operation_receipts
    ADD CONSTRAINT operation_receipts_installation_ref_op_idempotency_key_key
    UNIQUE (installation_ref, op, idempotency_key);

ALTER TABLE operation_receipts
    DROP COLUMN IF EXISTS retain_until,
    DROP COLUMN IF EXISTS result_expires_at,
    DROP COLUMN IF EXISTS environment;
