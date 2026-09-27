-- 0010_subject_catalog_revision: persisted monotonic subject-scoped catalog revision, and the
-- durable account-access receipt policy for this TEST slice. Additive; 0001-0009 are not
-- rewritten (up 0009 stays as accepted).
--
-- The catalog revision fingerprints only material, non-ephemeral inputs (registry descriptors,
-- visible subject grants/generations/revocations, entitlement and binding identity). Issued-at
-- timestamps never enter the fingerprint, so repeated GETs do not churn the revision.

CREATE TABLE catalog_revisions (
    installation_id uuid PRIMARY KEY REFERENCES installations (id) ON DELETE CASCADE,
    account_id uuid REFERENCES accounts (id) ON DELETE SET NULL,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision >= 1),
    fingerprint text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Access/sync receipts are durable technical dedupe identity in this TEST slice: they carry no
-- bearer and no raw auth result, are exempt from the bounded retention sweeps, and are removed
-- only by an explicit account lifecycle/archival policy (not by this migration and not by
-- maintenance). This is dedupe policy, not a payment/credit term or an access extension.
UPDATE operation_receipts
SET result_expires_at = NULL,
    retain_until = NULL,
    updated_at = now()
WHERE op = 'access.sync'
  AND environment IS NOT NULL
  AND account_ref IS NOT NULL;
