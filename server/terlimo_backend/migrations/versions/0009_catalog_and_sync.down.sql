-- Revert 0009. Fail-closed for operation linkage, catalog revision and receipt identity:
-- correlation ids, a populated catalog revision and durable account access receipts are
-- provenance and are never dropped silently while rows use them. No rows are deleted.
-- (Edited for correction1; up 0009 is unchanged.)

DO $$
DECLARE
    populated boolean;
BEGIN
    IF EXISTS (SELECT 1 FROM outbox_operations WHERE correlation_id IS NOT NULL) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0009: correlated operations exist';
    END IF;
    IF COALESCE((SELECT revision FROM catalog_state WHERE id), 1) > 1 THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0009: populated catalog revision exists';
    END IF;
    IF EXISTS (
        SELECT 1 FROM operation_receipts
        WHERE op = 'access.sync' AND environment IS NOT NULL AND account_ref IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0009: durable account access receipts exist';
    END IF;
    IF to_regclass('catalog_revisions') IS NOT NULL THEN
        EXECUTE 'SELECT EXISTS (SELECT 1 FROM catalog_revisions)' INTO populated;
        IF populated THEN
            RAISE EXCEPTION 'ROLLBACK_UNSAFE_0009: subject catalog revisions exist';
        END IF;
    END IF;
END $$;

DROP INDEX IF EXISTS operation_receipts_account_key_idx;
DROP INDEX IF EXISTS outbox_operations_correlation_idx;
ALTER TABLE outbox_operations DROP COLUMN IF EXISTS correlation_id;
DROP TABLE IF EXISTS catalog_state;
