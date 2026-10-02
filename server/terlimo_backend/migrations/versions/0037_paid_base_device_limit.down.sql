-- Preserve commercial history and the base needed to interpret it.
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM paid_extra_slots WHERE source_order_id IS NOT NULL)
 OR EXISTS (SELECT 1 FROM s5_payment_quotes WHERE product IS NOT NULL)
 OR EXISTS (SELECT 1 FROM payment_orders WHERE months=0 OR credit_review_reason IS NOT NULL
            OR credited_product IS NOT NULL OR amount<>trunc(amount))
 OR EXISTS (SELECT 1 FROM payment_events WHERE amount<>trunc(amount))
 THEN RAISE EXCEPTION 'commercial activity exists: retain paid base capacity and ledger'; END IF;
END $$;
ALTER TABLE entitlements DROP COLUMN paid_base_device_limit;
