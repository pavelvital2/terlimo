-- Revert 0005_gateway_control. No gateway-facing data is stored elsewhere; dropping these
-- technical columns does not touch entitlements, bindings or receipts.

DROP INDEX IF EXISTS grants_state_not_after_idx;

ALTER TABLE grants
    DROP COLUMN IF EXISTS last_readback,
    DROP COLUMN IF EXISTS applied_at,
    DROP COLUMN IF EXISTS applied_not_after,
    DROP COLUMN IF EXISTS gateway_generation,
    DROP COLUMN IF EXISTS lease_seq,
    DROP COLUMN IF EXISTS gateway_credential;
