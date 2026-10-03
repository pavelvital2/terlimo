DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM s5_payment_quotes WHERE owner_kind='telegram_account') THEN
  RAISE EXCEPTION 'Cannot discard durable account quote receipts';
 END IF;
END $$;
DROP INDEX s5_account_quote_key;
ALTER TABLE s5_payment_quotes DROP CONSTRAINT s5_quote_owner_shape;
ALTER TABLE s5_payment_quotes DROP COLUMN trusted_caller;
ALTER TABLE s5_payment_quotes DROP COLUMN trusted_owner_account_id;
ALTER TABLE s5_payment_quotes DROP COLUMN owner_kind;
ALTER TABLE s5_payment_quotes ALTER COLUMN installation_id SET NOT NULL;
