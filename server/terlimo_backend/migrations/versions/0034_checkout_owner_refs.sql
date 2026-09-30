-- 0034_checkout_owner_refs: immutable checkout ownership captured once at S5 pending-order
-- creation. Kept separate from the mutable credit-target columns account_id/binding_id, which
-- the provider webhook rewrites to the currently active binding at credit time (rebinding).
-- Historical rows stay NULL and are never backfilled or captured on replay.
ALTER TABLE payment_orders
    ADD COLUMN checkout_owner_account_id uuid REFERENCES accounts(id) ON DELETE SET NULL,
    ADD COLUMN checkout_owner_binding_id uuid REFERENCES account_bindings(id) ON DELETE SET NULL;
CREATE INDEX payment_orders_checkout_owner_idx
    ON payment_orders (checkout_owner_account_id)
    WHERE checkout_owner_account_id IS NOT NULL;
