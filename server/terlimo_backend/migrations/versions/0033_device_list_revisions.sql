-- 0033_device_list_revisions: stable revision/fingerprint metadata for the devices list snapshot.
-- Additive metadata table only. The product GET /me subject revision lives in subject_revisions and
-- must not be reused with a different snapshot: one devices snapshot keyed by the authenticated
-- installation keeps its own stable revision, so alternating /me and /devices reads cannot bump
-- each other. No binding state or TTL is stored here.
CREATE TABLE device_list_revisions (
    installation_id uuid PRIMARY KEY REFERENCES installations (id) ON DELETE CASCADE,
    account_id uuid REFERENCES accounts (id) ON DELETE CASCADE,
    revision bigint NOT NULL CHECK (revision > 0),
    fingerprint text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
