-- Operator-only coverage. No epoch is inserted or activated by migration/defaults.
CREATE TABLE referral_history_epochs (
 id uuid PRIMARY KEY,
 scope text NOT NULL CHECK(scope IN ('test','production')),
 manifest_sha256 text NOT NULL UNIQUE CHECK(manifest_sha256 ~ '^[0-9a-f]{64}$'),
 complete boolean NOT NULL,
 manifest jsonb NOT NULL,
 snapshot_watermark text NOT NULL CHECK(length(snapshot_watermark) BETWEEN 1 AND 256),
 final_watermark text NOT NULL CHECK(length(final_watermark) BETWEEN 1 AND 256),
 writer_fence text NOT NULL CHECK(length(writer_fence) BETWEEN 1 AND 256),
 active boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(), activated_at timestamptz,
 CHECK(NOT active OR (complete AND activated_at IS NOT NULL))
);
-- A database has one explicit cutover scope, never simultaneous TEST/production coverage.
CREATE UNIQUE INDEX referral_history_one_active ON referral_history_epochs((true)) WHERE active;
ALTER TABLE referral_history_staging ADD COLUMN epoch_id uuid REFERENCES referral_history_epochs(id);
ALTER TABLE referral_history_staging ADD COLUMN code_absent_verified boolean NOT NULL DEFAULT false;
ALTER TABLE referral_history_staging ADD COLUMN reward_dispositions jsonb;
ALTER TABLE referral_benefits ADD COLUMN history_epoch_id uuid REFERENCES referral_history_epochs(id);
ALTER TABLE referral_benefits ADD COLUMN history_proof text CHECK(history_proof IN ('member','absent'));
ALTER TABLE referral_rewards ADD COLUMN import_evidence jsonb;
