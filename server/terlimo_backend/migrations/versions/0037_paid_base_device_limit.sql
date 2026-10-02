-- Preserve finite legacy base capacity independently of expiring extras.
-- NULL retains existing base2 semantics; zero remains zero. Negative legacy data is invalid.
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM entitlements WHERE kind='paid' AND ends_at IS NOT NULL AND device_limit < 0)
 THEN RAISE EXCEPTION 'invalid legacy finite paid device_limit: negative'; END IF;
END $$;
ALTER TABLE entitlements ADD COLUMN paid_base_device_limit integer NOT NULL DEFAULT 2
 CHECK (paid_base_device_limit BETWEEN 0 AND 2);
UPDATE entitlements SET paid_base_device_limit=LEAST(2,COALESCE(device_limit,2))
 WHERE kind='paid' AND ends_at IS NOT NULL;
-- Do not rewrite materialized device_limit/revision/ends/source_plan.
