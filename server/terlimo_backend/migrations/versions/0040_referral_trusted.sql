-- No installation or candidate fabrication for trusted bot/site operations.
CREATE TABLE referral_trusted_operations (
 account_id uuid NOT NULL REFERENCES accounts(id),
 caller text NOT NULL CHECK(caller='telegram_backend'),
 operation text NOT NULL CHECK(operation='attach'),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 8 AND 128),
 digest text NOT NULL CHECK(digest ~ '^[0-9a-f]{64}$'),
 result jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(account_id,caller,operation,idempotency_key)
);
