-- Explicit quote owner; historical mobile snapshots stay installation-owned.
ALTER TABLE s5_payment_quotes ALTER COLUMN installation_id DROP NOT NULL;
ALTER TABLE s5_payment_quotes ADD COLUMN owner_kind text NOT NULL DEFAULT 'installation';
ALTER TABLE s5_payment_quotes ADD COLUMN trusted_owner_account_id uuid REFERENCES accounts(id);
ALTER TABLE s5_payment_quotes ADD COLUMN trusted_caller text;
ALTER TABLE s5_payment_quotes ADD CONSTRAINT s5_quote_owner_shape CHECK (
 (owner_kind='installation' AND installation_id IS NOT NULL AND trusted_owner_account_id IS NULL AND trusted_caller IS NULL)
 OR (owner_kind='telegram_account' AND installation_id IS NULL AND trusted_owner_account_id IS NOT NULL
     AND trusted_caller IS NOT NULL AND trusted_caller='telegram_backend')
);
CREATE UNIQUE INDEX s5_account_quote_key ON s5_payment_quotes(trusted_owner_account_id,trusted_caller,idempotency_key)
 WHERE owner_kind='telegram_account';
