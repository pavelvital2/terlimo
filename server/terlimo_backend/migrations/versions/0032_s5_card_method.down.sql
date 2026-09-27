-- Reverting 0032 is fail-closed: card quote snapshots are NOT deleted (they are durable,
-- idempotency-relevant snapshots and a "non-financial" label does not authorise data loss).
-- The schema may stay expanded while the code is rolled back, because the extra CHECK value is
-- harmless for the old code. If any card quote exists the down script refuses and keeps the
-- expanded schema; only an empty card set allows the strict 0024 constraint to be restored.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM s5_payment_quotes WHERE method = 'card') THEN
        RAISE EXCEPTION 'MIGRATION_UNSAFE_0032_DOWN: card quotes exist; keep the expanded schema';
    END IF;
END $$;
ALTER TABLE s5_payment_quotes DROP CONSTRAINT s5_payment_quotes_method_check;
ALTER TABLE s5_payment_quotes
    ADD CONSTRAINT s5_payment_quotes_method_check CHECK (method IN ('sbp', 'crypto'));
