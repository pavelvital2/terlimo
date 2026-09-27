-- Revert 0011. Fail-closed: an environment registry lifecycle marker is provenance and is
-- never dropped silently. No rows are deleted.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM registry_environments) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0011: environment registry lifecycle markers exist';
    END IF;
END $$;

DROP TABLE IF EXISTS registry_environments;
