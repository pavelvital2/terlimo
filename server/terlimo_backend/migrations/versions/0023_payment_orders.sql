-- 0023_payment_orders: server-authoritative purchase orders for the unified TERLIMO backend.
-- An order is created for a proven installation from a server-side quote; provider payment
-- identity is unique and the success transition is claimed exactly once. Paid access is only
-- ever granted through this backend's entitlement/grant path (no second source of paid access).
CREATE TABLE payment_orders (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id uuid NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    account_id uuid REFERENCES accounts(id) ON DELETE SET NULL,
    binding_id uuid REFERENCES account_bindings(id) ON DELETE SET NULL,
    provider text NOT NULL DEFAULT 'platega',
    provider_variant text,
    provider_payment_id text,
    provider_payment_url text,
    provider_qr text,
    idempotency_key text NOT NULL UNIQUE,
    quote jsonb NOT NULL,
    amount integer NOT NULL CHECK (amount > 0),
    currency text NOT NULL,
    months integer NOT NULL CHECK (months IN (1, 3, 6)),
    tariff_key text NOT NULL,
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'succeeded', 'failed', 'canceled', 'expired')),
    applied_entitlement_id uuid REFERENCES entitlements(id) ON DELETE SET NULL,
    -- Provider-create is serialized and fail-closed: in_flight/unknown mean "do not create a
    -- second provider invoice"; unknown requires server-side reconciliation/identity lookup.
    provider_create_state text NOT NULL DEFAULT 'not_created'
        CHECK (provider_create_state IN ('not_created', 'in_flight', 'created', 'unknown')),
    provider_created_at timestamptz,
    -- Durable recovery marker: paid right exists but the gateway grant/outbox enqueue has not
    -- succeeded yet; reconciliation retries it without any client access.sync.
    needs_grant boolean NOT NULL DEFAULT false,
    paid_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX payment_orders_provider_payment_uniq
    ON payment_orders (provider, provider_payment_id)
    WHERE provider_payment_id IS NOT NULL;
CREATE INDEX payment_orders_installation_idx
    ON payment_orders (installation_id, created_at DESC);
CREATE INDEX payment_orders_parked_idx
    ON payment_orders (installation_id)
    WHERE status = 'succeeded' AND applied_entitlement_id IS NULL;
CREATE INDEX payment_orders_needs_grant_idx
    ON payment_orders (installation_id)
    WHERE needs_grant;

CREATE TABLE payment_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id uuid NOT NULL REFERENCES payment_orders(id) ON DELETE CASCADE,
    provider text NOT NULL,
    provider_event_id text,
    kind text NOT NULL,
    amount integer,
    currency text,
    raw_status text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX payment_events_provider_event_uniq
    ON payment_events (provider, provider_event_id)
    WHERE provider_event_id IS NOT NULL;
CREATE INDEX payment_events_order_idx ON payment_events (order_id, created_at);
