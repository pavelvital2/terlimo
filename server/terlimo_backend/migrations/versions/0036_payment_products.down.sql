-- Never discard an addon purchase or pending review to roll code back.
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM paid_extra_slots WHERE source_order_id IS NOT NULL)
 OR EXISTS (SELECT 1 FROM s5_payment_quotes WHERE product IS NOT NULL)
 OR EXISTS (SELECT 1 FROM payment_orders WHERE months=0 OR credit_review_reason IS NOT NULL OR amount<>trunc(amount))
 OR EXISTS (SELECT 1 FROM payment_events WHERE amount<>trunc(amount))
 THEN RAISE EXCEPTION 'commercial activity exists: retain additive schema and ledger'; END IF;
END $$;
DROP TABLE paid_extra_slots;
ALTER TABLE s5_payment_quotes DROP COLUMN product;
ALTER TABLE payment_orders DROP COLUMN credit_review_reason;
ALTER TABLE payment_orders DROP COLUMN credited_product;
ALTER TABLE s5_payment_quotes DROP CONSTRAINT s5_payment_quotes_months_check;
ALTER TABLE s5_payment_quotes ADD CONSTRAINT s5_payment_quotes_months_check CHECK (months IN (1,3,6));
ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_months_check;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_months_check CHECK (months IN (1,3,6));

ALTER TABLE payment_orders ALTER COLUMN amount TYPE integer USING amount::integer;
ALTER TABLE payment_events ALTER COLUMN amount TYPE integer USING amount::integer;
