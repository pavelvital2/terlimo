-- 0027_order_source_quote: durable link from a paid order to the exact S5 quote that proved the
-- selected plan. Unique so one quote can never fund two orders; the plan snapshot is taken only
-- from this proven quote at order creation.
ALTER TABLE payment_orders ADD COLUMN source_quote_id uuid REFERENCES s5_payment_quotes(id);
CREATE UNIQUE INDEX payment_orders_source_quote_uniq
    ON payment_orders (source_quote_id)
    WHERE source_quote_id IS NOT NULL;
