-- 0017_onboarding_hour_storage: durable one-time onboarding hour storage + installation-scoped
-- grants. Additive; 0001-0016 are not rewritten. The read-only duplicate preflight runs FIRST so
-- a conflicted database fails before any schema change and no existing right is deleted.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM entitlements
        WHERE kind = 'onboarding_hour' AND installation_id IS NOT NULL
        GROUP BY installation_id HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'MIGRATION_UNSAFE_0017: duplicate onboarding_hour units per installation';
    END IF;
END $$;

ALTER TABLE grants ALTER COLUMN binding_id DROP NOT NULL;
ALTER TABLE grants ADD COLUMN installation_id uuid REFERENCES installations (id) ON DELETE CASCADE;
ALTER TABLE grants ADD CONSTRAINT grants_subject_xor CHECK (
    (binding_id IS NOT NULL)::int + (installation_id IS NOT NULL)::int = 1
);
ALTER TABLE grants DROP CONSTRAINT grants_binding_id_gateway_id_key;
CREATE UNIQUE INDEX grants_binding_gateway_uniq
    ON grants (binding_id, gateway_id) WHERE binding_id IS NOT NULL;
CREATE UNIQUE INDEX grants_installation_gateway_uniq
    ON grants (installation_id, gateway_id) WHERE installation_id IS NOT NULL;

DROP INDEX entitlements_hour_installation_idx;
CREATE UNIQUE INDEX entitlements_hour_installation_uniq
    ON entitlements (installation_id)
    WHERE kind = 'onboarding_hour' AND installation_id IS NOT NULL;

CREATE TABLE onboarding_intents (
    id             uuid PRIMARY KEY,
    installation_id uuid NOT NULL REFERENCES installations (id) ON DELETE CASCADE,
    gateway_id     uuid NOT NULL REFERENCES gateways (id) ON DELETE RESTRICT,
    environment    text NOT NULL,
    unit_epoch     integer NOT NULL,
    credential_id  text NOT NULL UNIQUE,
    request_key    text NOT NULL,
    secret_hash    text NOT NULL,
    secret_enc     bytea,
    state          text NOT NULL CHECK (state IN
                       ('pending', 'ready', 'started', 'failed', 'expired', 'revoked')),
    expires_at     timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    started_at     timestamptz,
    hour_not_after timestamptz,
    failed_reason  text,
    revoked_at     timestamptz,
    UNIQUE (installation_id, unit_epoch),
    UNIQUE (installation_id, request_key),
    CHECK ((state IN ('pending', 'ready')) = (secret_enc IS NOT NULL))
);
CREATE UNIQUE INDEX onboarding_intents_active_uniq
    ON onboarding_intents (installation_id) WHERE state IN ('pending', 'ready');

CREATE TABLE onboarding_evidence (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    intent_id          uuid NOT NULL REFERENCES onboarding_intents (id) ON DELETE CASCADE,
    credential_id      text NOT NULL UNIQUE,
    installation_id    uuid NOT NULL REFERENCES installations (id) ON DELETE CASCADE,
    gateway_id         uuid NOT NULL REFERENCES gateways (id) ON DELETE RESTRICT,
    environment        text NOT NULL,
    connection_id_hash text NOT NULL,
    request_id         text NOT NULL,
    first_seen_at      timestamptz NOT NULL DEFAULT now(),
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (intent_id)
);
CREATE INDEX onboarding_evidence_installation_idx ON onboarding_evidence (installation_id);
CREATE INDEX onboarding_evidence_gateway_idx ON onboarding_evidence (gateway_id);
