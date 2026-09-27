-- 0006_operation_ownership: server-side owner of an outbox operation for the account
-- operation-status route. Additive and reversible; no existing up migration rewritten.

ALTER TABLE outbox_operations
    ADD COLUMN account_id uuid REFERENCES accounts (id) ON DELETE CASCADE,
    ADD COLUMN binding_id uuid REFERENCES account_bindings (id) ON DELETE CASCADE,
    ADD COLUMN requested_by_installation uuid REFERENCES installations (id) ON DELETE SET NULL;

CREATE INDEX outbox_operations_owner_idx ON outbox_operations (account_id, created_at DESC);
