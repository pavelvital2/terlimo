-- Additive immutable commercial snapshot and individually expiring extra slots.
ALTER TABLE s5_payment_quotes ADD COLUMN product jsonb;
ALTER TABLE payment_orders ADD COLUMN credit_review_reason text;
ALTER TABLE payment_orders ADD COLUMN credited_product jsonb;
ALTER TABLE s5_payment_quotes DROP CONSTRAINT s5_payment_quotes_months_check;
ALTER TABLE s5_payment_quotes ADD CONSTRAINT s5_payment_quotes_months_check CHECK (months IN (0,1,3,6));
ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_months_check;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_months_check CHECK (months IN (0,1,3,6));
CREATE TABLE paid_extra_slots (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 entitlement_id uuid NOT NULL REFERENCES entitlements(id),
 source_order_id uuid UNIQUE REFERENCES payment_orders(id),
 expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX paid_extra_slots_entitlement_idx ON paid_extra_slots(entitlement_id,expires_at);
-- Preserve finite historical extra limits until their original end; no free renewal.
INSERT INTO paid_extra_slots(entitlement_id,expires_at)
SELECT e.id,e.ends_at FROM entitlements e CROSS JOIN LATERAL generate_series(1,GREATEST(0,COALESCE(e.device_limit,2)-2)) n
WHERE e.kind='paid' AND e.ends_at IS NOT NULL;

-- Preserve old RUB values exactly; permit kopecks in the same order/event ledger.
ALTER TABLE payment_orders ALTER COLUMN amount TYPE numeric(18,2) USING amount::numeric;
ALTER TABLE payment_events ALTER COLUMN amount TYPE numeric(18,2) USING amount::numeric;
