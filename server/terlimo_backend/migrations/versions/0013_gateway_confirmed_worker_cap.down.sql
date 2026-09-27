-- Revert 0013. Fail-closed: a confirmed worker cap is admission provenance and is never
-- dropped silently while rows use it. No rows are deleted.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM gateways WHERE confirmed_max_workers IS NOT NULL) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0013: confirmed gateway worker caps exist';
    END IF;
END $$;

ALTER TABLE gateways DROP COLUMN IF EXISTS confirmed_max_workers;
