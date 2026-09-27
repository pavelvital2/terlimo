-- 0001_core: unified TERLIMO backend model (accounts, installations, sessions,
-- bindings, entitlements, gateways, grants) and the durable outbox.
-- The schema is additive/reversible: see 0001_core.down.sql.

CREATE TABLE accounts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    status text NOT NULL DEFAULT 'unlinked' CHECK (status IN ('unlinked', 'verified')),
    telegram_id bigint UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE installations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    environment text NOT NULL,
    platform text,
    name text,
    public_key_fingerprint text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz,
    UNIQUE (public_key_fingerprint, environment)
);

CREATE TABLE sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid REFERENCES accounts (id) ON DELETE SET NULL,
    installation_id uuid NOT NULL REFERENCES installations (id) ON DELETE CASCADE,
    scopes text[] NOT NULL DEFAULT '{}',
    generation integer NOT NULL DEFAULT 1,
    issued_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);

CREATE TABLE account_bindings (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    installation_id uuid NOT NULL REFERENCES installations (id) ON DELETE CASCADE,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    generation integer NOT NULL DEFAULT 1,
    bound_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    UNIQUE (account_id, installation_id)
);

CREATE TABLE entitlements (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('onboarding_hour', 'trial', 'paid', 'imported')),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('pending', 'active', 'expired', 'revoked')),
    starts_at timestamptz NOT NULL DEFAULT now(),
    ends_at timestamptz,
    device_limit integer,
    revision integer NOT NULL DEFAULT 1,
    source_invoice_id text,
    source_legacy_id text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE gateways (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    gateway_key text NOT NULL UNIQUE,
    environment text NOT NULL,
    display_name text,
    endpoints jsonb NOT NULL DEFAULT '{}'::jsonb,
    capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
    registry_state text NOT NULL DEFAULT 'registered' CHECK (registry_state IN ('registered', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE grants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    binding_id uuid NOT NULL REFERENCES account_bindings (id) ON DELETE CASCADE,
    gateway_id uuid NOT NULL REFERENCES gateways (id) ON DELETE RESTRICT,
    opaque_id uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    desired_generation integer NOT NULL DEFAULT 1,
    applied_generation integer,
    not_after timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'applying', 'applied', 'revoked', 'failed')),
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (binding_id, gateway_id)
);

CREATE TABLE outbox_operations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    operation_type text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key text NOT NULL UNIQUE,
    target_revision integer,
    gateway_id uuid REFERENCES gateways (id) ON DELETE SET NULL,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'done', 'failed', 'dead')),
    attempts integer NOT NULL DEFAULT 0,
    max_attempts integer NOT NULL DEFAULT 8,
    available_at timestamptz NOT NULL DEFAULT now(),
    locked_by text,
    locked_at timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX outbox_operations_claim_idx ON outbox_operations (status, available_at, id);

-- Internal effect log for the durable worker self-check. It is not a public API
-- and will be superseded by real gateway-control effects in step 03.3.
CREATE TABLE outbox_effect_log (
    idempotency_key text PRIMARY KEY,
    operation_type text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
