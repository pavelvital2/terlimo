-- Revert 0012. Fail-closed: an account-bound session or any binding fence residue (including a
-- NULL binding_id left behind by ON DELETE SET NULL while binding_generation remains) is
-- provenance and is never dropped silently. No rows are deleted or cleansed.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM sessions
        WHERE account_id IS NOT NULL
           OR binding_id IS NOT NULL
           OR binding_generation IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0012: session account/binding fences exist';
    END IF;
END $$;

ALTER TABLE sessions DROP COLUMN IF EXISTS binding_generation;
ALTER TABLE sessions DROP COLUMN IF EXISTS binding_id;
