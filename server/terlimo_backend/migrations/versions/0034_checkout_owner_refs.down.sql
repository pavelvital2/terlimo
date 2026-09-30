DROP INDEX IF EXISTS payment_orders_checkout_owner_idx;
ALTER TABLE payment_orders
    DROP COLUMN IF EXISTS checkout_owner_binding_id,
    DROP COLUMN IF EXISTS checkout_owner_account_id;
