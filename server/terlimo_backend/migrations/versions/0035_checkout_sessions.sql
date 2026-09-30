-- 0035_checkout_sessions: durable bounded checkout receipt, at most one per paid-order.
-- It binds the immutable checkout owner (account+installation), the idempotency key, the
-- policy version and the bounded origins/redirects/expiry issued for that order. It is a
-- policy record only: no invoice, no provider call, no entitlement/credit side effect.
CREATE TABLE checkout_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id uuid NOT NULL UNIQUE REFERENCES payment_orders(id) ON DELETE CASCADE,
    account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    installation_id uuid NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    idempotency_key text NOT NULL,
    policy_version text NOT NULL,
    allowed_origins jsonb NOT NULL,
    allowed_redirects jsonb NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX checkout_sessions_account_idx
    ON checkout_sessions (account_id, created_at DESC);
