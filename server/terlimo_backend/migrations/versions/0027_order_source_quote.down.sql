DROP INDEX IF EXISTS payment_orders_source_quote_uniq;
ALTER TABLE payment_orders DROP COLUMN IF EXISTS source_quote_id;
