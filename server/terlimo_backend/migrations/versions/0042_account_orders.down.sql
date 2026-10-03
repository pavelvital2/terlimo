DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM payment_orders WHERE owner_kind='telegram_account') THEN
   RAISE EXCEPTION 'cannot discard account-owned payment receipts';
 END IF;
END $$;
DROP TRIGGER payment_order_trusted_immutable ON payment_orders;
DROP FUNCTION payment_order_trusted_immutable();
ALTER TABLE payment_orders DROP CONSTRAINT payment_order_owner_shape;
ALTER TABLE payment_orders DROP COLUMN trusted_caller, DROP COLUMN trusted_owner_account_id, DROP COLUMN owner_kind;
ALTER TABLE payment_orders ALTER COLUMN installation_id SET NOT NULL;
