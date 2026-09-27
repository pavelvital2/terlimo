-- Revert 0020_hour_grant_subject. Never lossy: refuse while any hour-owned grant or any outbox
-- operation for an hour grant exists, because dropping hour_entitlement_id would silently
-- re-attribute or orphan live data rights.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM grants WHERE hour_entitlement_id IS NOT NULL) THEN
        RAISE EXCEPTION '0020 down refused: hour-owned grants exist';
    END IF;
    IF EXISTS (
        SELECT 1 FROM outbox_operations
        WHERE payload->>'hour_entitlement_id' IS NOT NULL
    ) THEN
        RAISE EXCEPTION '0020 down refused: hour outbox operations exist';
    END IF;
END;
$$;

DROP TRIGGER IF EXISTS grants_hour_owner_check ON grants;
DROP FUNCTION IF EXISTS grants_hour_owner_check();
ALTER TABLE grants DROP CONSTRAINT IF EXISTS grants_hour_owner_required;
ALTER TABLE grants DROP COLUMN IF EXISTS hour_entitlement_id;
