-- Revert 0014. Fail-closed: a proven target identity/route is provenance and is never dropped
-- silently while rows use it. No rows are deleted or cleansed.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM grants
        WHERE target_node_id IS NOT NULL OR target_route IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0014: proven grant targets exist';
    END IF;
END $$;

ALTER TABLE grants DROP COLUMN IF EXISTS target_route;
ALTER TABLE grants DROP COLUMN IF EXISTS target_node_id;
