-- 0009_catalog_and_sync: stable catalog revision, access/sync operation linkage and the
-- account-bound receipt identity. Additive; 0001-0008 are not rewritten.

CREATE TABLE catalog_state (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision >= 1),
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO catalog_state (id) VALUES (true) ON CONFLICT (id) DO NOTHING;

-- One access/sync effect may fan out to several gateways; the correlation id is the stable
-- operation identity returned to the client and resolved by GET /operations/{id}.
ALTER TABLE outbox_operations
    ADD COLUMN correlation_id uuid;

CREATE INDEX outbox_operations_correlation_idx
    ON outbox_operations (correlation_id)
    WHERE correlation_id IS NOT NULL;

-- Account-bound receipts carry the account identity in the key scope
-- (account, installation, operation, idempotency_key); non-account receipts keep the
-- existing environment/installation key.
CREATE UNIQUE INDEX operation_receipts_account_key_idx
    ON operation_receipts (environment, account_ref, installation_ref, op, idempotency_key)
    WHERE environment IS NOT NULL AND account_ref IS NOT NULL;
