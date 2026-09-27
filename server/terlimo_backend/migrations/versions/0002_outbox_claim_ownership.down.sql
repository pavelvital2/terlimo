-- Revert 0002_outbox_claim_ownership.

DROP INDEX IF EXISTS outbox_operations_lease_idx;

ALTER TABLE outbox_operations
    DROP COLUMN IF EXISTS lease_expires_at,
    DROP COLUMN IF EXISTS claim_token;
