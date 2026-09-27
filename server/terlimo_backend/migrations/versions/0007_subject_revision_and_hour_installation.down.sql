-- Revert 0007. Fail-closed: installation-bound hours, accountless rows and populated subject
-- revisions cannot be represented by the pre-0007 schema, and monotonic client revisions must
-- never be silently reset. Nothing is deleted to force a rollback.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM entitlements WHERE installation_id IS NOT NULL) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0007: installation-bound hours exist';
    END IF;
    IF EXISTS (SELECT 1 FROM entitlements WHERE account_id IS NULL) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0007: entitlements with NULL account_id exist';
    END IF;
    IF EXISTS (SELECT 1 FROM subject_revisions) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0007: subject revisions exist';
    END IF;
END $$;

ALTER TABLE entitlements ALTER COLUMN account_id SET NOT NULL;

DROP TABLE IF EXISTS subject_revisions;
DROP INDEX IF EXISTS entitlements_hour_installation_idx;
ALTER TABLE entitlements DROP COLUMN IF EXISTS installation_id;
