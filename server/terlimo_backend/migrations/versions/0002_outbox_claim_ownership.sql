-- 0002_outbox_claim_ownership: unique claim token and bounded lease for the
-- durable outbox. Finalizations are fenced by the token; a stale owner can no
-- longer overwrite a newer result. Additive and reversible.

ALTER TABLE outbox_operations
    ADD COLUMN claim_token uuid,
    ADD COLUMN lease_expires_at timestamptz;

-- Backfill rows that were claimed before this migration (running workers from
-- the previous revision are not guaranteed; treat them as expired by the
-- configured lease after their last lock time).
UPDATE outbox_operations
SET lease_expires_at = COALESCE(locked_at, now()) + interval '30 seconds'
WHERE status = 'processing';

CREATE INDEX outbox_operations_lease_idx
    ON outbox_operations (status, lease_expires_at);
