-- S5 quotes are immutable, installation-bound price snapshots. They do not create invoices.
CREATE TABLE s5_payment_quotes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id uuid NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    idempotency_key text NOT NULL,
    request_digest text NOT NULL,
    plan_id text NOT NULL,
    months integer NOT NULL CHECK (months IN (1, 3, 6)),
    duration_code text NOT NULL,
    method text NOT NULL CHECK (method IN ('sbp', 'crypto')),
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    currency text NOT NULL,
    tariff_key text NOT NULL,
    plans_revision text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    UNIQUE (installation_id, idempotency_key)
);
