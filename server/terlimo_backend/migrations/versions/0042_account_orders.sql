-- Explicit trusted ownership; no API in this migration creates account orders.
ALTER TABLE payment_orders ALTER COLUMN installation_id DROP NOT NULL;
ALTER TABLE payment_orders ADD COLUMN owner_kind text NOT NULL DEFAULT 'installation';
ALTER TABLE payment_orders ADD COLUMN trusted_owner_account_id uuid REFERENCES accounts(id);
ALTER TABLE payment_orders ADD COLUMN trusted_caller text;
ALTER TABLE payment_orders ADD CONSTRAINT payment_order_owner_shape CHECK (
 (owner_kind='installation' AND installation_id IS NOT NULL AND trusted_owner_account_id IS NULL AND trusted_caller IS NULL)
 OR (owner_kind='telegram_account' AND installation_id IS NULL
     AND trusted_owner_account_id IS NOT NULL AND trusted_caller IS NOT NULL AND trusted_caller='telegram_backend'
     AND checkout_owner_account_id IS NOT NULL AND checkout_owner_account_id=trusted_owner_account_id
     AND checkout_owner_binding_id IS NULL AND binding_id IS NULL
     AND (account_id IS NULL OR account_id=trusted_owner_account_id))
);
-- Owner/snapshot cannot change after capture, including switching an existing mobile row.
-- Existing mobile credit/account rebinding semantics remain untouched.
CREATE FUNCTION payment_order_trusted_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(NEW.owner_kind,NEW.trusted_owner_account_id,NEW.trusted_caller)
     IS DISTINCT FROM ROW(OLD.owner_kind,OLD.trusted_owner_account_id,OLD.trusted_caller)
 OR (OLD.owner_kind='telegram_account' AND
     ROW(NEW.checkout_owner_account_id,NEW.quote,NEW.amount,NEW.currency,NEW.months,NEW.tariff_key,NEW.source_quote_id)
     IS DISTINCT FROM
     ROW(OLD.checkout_owner_account_id,OLD.quote,OLD.amount,OLD.currency,OLD.months,OLD.tariff_key,OLD.source_quote_id)) THEN
   RAISE EXCEPTION 'immutable payment order ownership/snapshot' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER payment_order_trusted_immutable BEFORE UPDATE ON payment_orders
 FOR EACH ROW EXECUTE FUNCTION payment_order_trusted_immutable();
-- Existing global idempotency_key and provider identity unique indexes stay unchanged.
