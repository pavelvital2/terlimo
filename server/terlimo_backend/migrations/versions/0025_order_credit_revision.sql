-- The order keeps the exact paid-entitlement revision produced by its own credit.
-- Existing orders remain NULL; a later /me revision must not be guessed for them.
ALTER TABLE payment_orders
ADD COLUMN credited_entitlement_revision integer
    CHECK (credited_entitlement_revision IS NULL OR credited_entitlement_revision > 0);

ALTER TABLE payment_orders
ADD CONSTRAINT payment_orders_credit_requires_entitlement
CHECK (credited_entitlement_revision IS NULL OR applied_entitlement_id IS NOT NULL);
