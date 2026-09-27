-- Revert 0016. Fail-closed while any epoch advanced; no rows are deleted.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM gateways WHERE snapshot_epoch <> 0) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0016: gateway snapshot epochs advanced';
    END IF;
END $$;

ALTER TABLE gateways DROP COLUMN IF EXISTS snapshot_epoch;
