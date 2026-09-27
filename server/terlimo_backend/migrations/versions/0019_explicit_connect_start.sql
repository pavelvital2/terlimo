-- 0019_explicit_connect_start: additive contract storage for the explicit connect/start flow.
-- No backfill and no rewrite of legacy rows: onboarding_evidence.start_digest stays NULL for
-- rows admitted before this flow; historical replay for such rows fails closed at runtime
-- (ONBOARDING_REPLAY_UNAVAILABLE) and no digest is invented for them.

ALTER TABLE onboarding_evidence ADD COLUMN start_digest text;
ALTER TABLE onboarding_evidence ADD CONSTRAINT onboarding_evidence_start_digest_check
    CHECK (start_digest IS NULL OR start_digest ~ '^[0-9a-f]{64}$');

ALTER TABLE auth_challenges ADD COLUMN intent_id uuid REFERENCES onboarding_intents (id) ON DELETE CASCADE;
ALTER TABLE auth_challenges ADD COLUMN superseded_at timestamptz;

ALTER TABLE auth_challenges DROP CONSTRAINT auth_challenges_purpose_check;
ALTER TABLE auth_challenges ADD CONSTRAINT auth_challenges_purpose_check CHECK (purpose IN (
    'enrollment', 'session', 'binding', 'telegram-link', 'catalog', 'checkout',
    'onboarding-start-intent', 'onboarding-start'
));

CREATE UNIQUE INDEX auth_challenges_start_outstanding_uniq
    ON auth_challenges (installation_fingerprint, intent_id)
    WHERE purpose = 'onboarding-start' AND used_at IS NULL AND superseded_at IS NULL;
