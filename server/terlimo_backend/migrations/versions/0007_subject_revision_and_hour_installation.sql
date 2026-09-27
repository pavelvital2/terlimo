-- 0007_subject_revision_and_hour_installation: stable monotonic revision snapshot per
-- subject (installation) and the explicit onboarding-hour -> installation link.
-- Additive and reversible. Existing ambiguous hours stay untouched (installation_id NULL is
-- "undetermined", never attributed to an installation by guessing).

ALTER TABLE entitlements
    ADD COLUMN installation_id uuid REFERENCES installations (id) ON DELETE SET NULL;

-- Legacy onboarding hours of an unlinked installation may have no account; keeping the
-- undetermined row is required, guessing an account is not.
ALTER TABLE entitlements
    ALTER COLUMN account_id DROP NOT NULL;

CREATE INDEX entitlements_hour_installation_idx
    ON entitlements (installation_id)
    WHERE kind = 'onboarding_hour';

CREATE TABLE subject_revisions (
    installation_id uuid PRIMARY KEY REFERENCES installations (id) ON DELETE CASCADE,
    account_id uuid REFERENCES accounts (id) ON DELETE SET NULL,
    revision bigint NOT NULL DEFAULT 0,
    fingerprint text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
