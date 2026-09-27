-- Revert 0006_operation_ownership.

DROP INDEX IF EXISTS outbox_operations_owner_idx;

ALTER TABLE outbox_operations
    DROP COLUMN IF EXISTS requested_by_installation,
    DROP COLUMN IF EXISTS binding_id,
    DROP COLUMN IF EXISTS account_id;
