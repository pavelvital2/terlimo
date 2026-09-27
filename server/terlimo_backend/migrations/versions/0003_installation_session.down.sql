-- Revert 0003_installation_session.

DROP TABLE IF EXISTS operation_receipts;
ALTER TABLE sessions DROP COLUMN IF EXISTS token_sha256;
ALTER TABLE installations
    DROP COLUMN IF EXISTS public_key_spki_b64,
    DROP COLUMN IF EXISTS state;
DROP INDEX IF EXISTS auth_challenges_expiry_idx;
DROP INDEX IF EXISTS auth_challenges_fingerprint_idx;
DROP TABLE IF EXISTS auth_challenges;
