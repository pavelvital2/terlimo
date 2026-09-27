-- 0004_receipt_environment_and_retention: environment-aware idempotency receipts, bounded
-- raw-result retention, expired-challenge cleanup and the atomic public issuance counter.
-- Additive and reversible. Legacy receipts are ambiguous and are never matched by a guessed
-- environment: they keep no raw result and are removed by the normal cleanup.

ALTER TABLE operation_receipts
    ADD COLUMN environment text,
    ADD COLUMN result_expires_at timestamptz,
    ADD COLUMN retain_until timestamptz;

-- Legacy rows: no environment can be derived without guessing. Clear the raw response so it
-- can never leak through a fallback, and let the cleanup remove the tombstone.
UPDATE operation_receipts
SET result = NULL,
    retain_until = COALESCE(retain_until, now() + interval '7 days')
WHERE environment IS NULL;

ALTER TABLE operation_receipts
    DROP CONSTRAINT IF EXISTS operation_receipts_installation_ref_op_idempotency_key_key;

CREATE UNIQUE INDEX operation_receipts_env_key_idx
    ON operation_receipts (environment, installation_ref, op, idempotency_key)
    WHERE environment IS NOT NULL;

CREATE INDEX operation_receipts_retain_idx ON operation_receipts (retain_until);
CREATE INDEX auth_challenges_retention_idx ON auth_challenges (expires_at);

-- Atomic per-minute counter shared by all API processes (single PostgreSQL, no Redis).
CREATE TABLE public_endpoint_counters (
    scope text NOT NULL,
    window_start timestamptz NOT NULL,
    count bigint NOT NULL,
    PRIMARY KEY (scope, window_start)
);
