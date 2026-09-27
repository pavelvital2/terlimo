-- 0019 down: remove the explicit connect/start storage. Rows using the new purposes must not
-- exist when rolling back (the restored CHECK would reject them); this down path is best-effort
-- for isolated TEST databases only.
DROP INDEX IF EXISTS auth_challenges_start_outstanding_uniq;
DELETE FROM auth_challenges WHERE purpose IN ('onboarding-start-intent', 'onboarding-start');
ALTER TABLE auth_challenges DROP CONSTRAINT auth_challenges_purpose_check;
ALTER TABLE auth_challenges ADD CONSTRAINT auth_challenges_purpose_check CHECK (purpose IN (
    'enrollment', 'session', 'binding', 'telegram-link', 'catalog', 'checkout'
));
ALTER TABLE auth_challenges DROP COLUMN superseded_at;
ALTER TABLE auth_challenges DROP COLUMN intent_id;
ALTER TABLE onboarding_evidence DROP CONSTRAINT onboarding_evidence_start_digest_check;
ALTER TABLE onboarding_evidence DROP COLUMN start_digest;
