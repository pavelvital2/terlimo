-- 0003_installation_session: PoP challenges, installation state, session token hashes and
-- the idempotency receipts for the mobile v1 installation/session routes (step 03.2).
-- Additive and reversible. Installation/session never create commercial rights.

CREATE TABLE auth_challenges (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    challenge_id text NOT NULL UNIQUE,
    nonce_b64 text NOT NULL,
    installation_fingerprint text NOT NULL,
    purpose text NOT NULL CHECK (purpose IN ('enrollment', 'session', 'binding', 'telegram-link', 'catalog', 'checkout')),
    op text NOT NULL,
    environment text NOT NULL CHECK (environment IN ('test', 'production')),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX auth_challenges_fingerprint_idx
    ON auth_challenges (installation_fingerprint, created_at DESC);
CREATE INDEX auth_challenges_expiry_idx ON auth_challenges (expires_at);

ALTER TABLE installations
    ADD COLUMN state text NOT NULL DEFAULT 'technical'
        CHECK (state IN ('technical', 'bound', 'revoked')),
    ADD COLUMN public_key_spki_b64 text;

ALTER TABLE sessions
    ADD COLUMN token_sha256 text UNIQUE;

CREATE TABLE operation_receipts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_ref text NOT NULL,
    account_ref text,
    op text NOT NULL,
    idempotency_key text NOT NULL,
    business_digest text NOT NULL,
    result jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (installation_ref, op, idempotency_key)
);
