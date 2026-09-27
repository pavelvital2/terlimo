-- Revert 0010. Fail-closed: any persisted subject catalog revision (even revision 1) and any
-- durable account access receipt are identity/provenance and are never dropped silently.
-- No rows are deleted. (Edited for correction2; up 0010 is unchanged.)

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM catalog_revisions) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0010: subject catalog revisions exist';
    END IF;
    IF EXISTS (
        SELECT 1 FROM operation_receipts
        WHERE op = 'access.sync' AND environment IS NOT NULL AND account_ref IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0010: durable account access receipts exist';
    END IF;
END $$;

DROP TABLE IF EXISTS catalog_revisions;
