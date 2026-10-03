-- Additive account-owned referral state. Existing identities remain history_pending.
ALTER TABLE accounts ADD COLUMN referral_code text UNIQUE;
ALTER TABLE accounts ADD COLUMN referral_attribution_receipt_id uuid;
ALTER TABLE accounts ADD COLUMN referred_by_account_id uuid REFERENCES accounts(id);
ALTER TABLE accounts ADD COLUMN referral_attributed_at timestamptz;
ALTER TABLE accounts ADD COLUMN referral_terms_version text;
ALTER TABLE accounts ADD CONSTRAINT referral_not_self CHECK (referred_by_account_id IS DISTINCT FROM id);
CREATE TABLE referral_benefits (
 account_id uuid PRIMARY KEY REFERENCES accounts(id),
 imported_trial_used boolean NOT NULL DEFAULT false, imported_first_main_paid boolean NOT NULL DEFAULT false,
 history_state text NOT NULL DEFAULT 'history_pending' CHECK(history_state IN ('ready','history_pending','ineligible')),
 first_paid_order_id uuid REFERENCES payment_orders(id),
 reserved_order_id uuid REFERENCES payment_orders(id),
 consumed_order_id uuid REFERENCES payment_orders(id),
 reservation_state text CHECK(reservation_state IN ('reserved','reconciling','consumed')),
 revision bigint NOT NULL DEFAULT 1
);
INSERT INTO referral_benefits(account_id) SELECT id FROM accounts;
CREATE TABLE referral_candidates (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), installation_id uuid NOT NULL REFERENCES installations(id),
 code text NOT NULL, state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','cleared','attached','rejected')),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX referral_candidate_pending ON referral_candidates(installation_id) WHERE state='pending';
CREATE TABLE referral_operations (
 installation_id uuid NOT NULL REFERENCES installations(id), operation text NOT NULL,
 idempotency_key text NOT NULL, digest text NOT NULL, result jsonb NOT NULL,
 PRIMARY KEY(installation_id,operation,idempotency_key)
);
ALTER TABLE registration_links ADD COLUMN referral_candidate_id uuid REFERENCES referral_candidates(id);
ALTER TABLE registration_links ADD COLUMN referral_idempotency_key text;
ALTER TABLE registration_links ADD COLUMN referral_attribution jsonb;
CREATE TABLE referral_rewards (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), invitee_account_id uuid NOT NULL REFERENCES accounts(id),
 inviter_account_id uuid NOT NULL REFERENCES accounts(id), event_kind text NOT NULL CHECK(event_kind IN ('trial','first_main_paid')),
 source_entitlement_id uuid REFERENCES entitlements(id), source_order_id uuid REFERENCES payment_orders(id),
 imported boolean NOT NULL DEFAULT false, legacy_source_id text UNIQUE, import_source_sha256 text,
 CHECK(source_entitlement_id IS NOT NULL OR (imported AND legacy_source_id IS NOT NULL AND import_source_sha256 ~ '^[0-9a-f]{64}$')),
 days integer NOT NULL CHECK(days>0), state text NOT NULL DEFAULT 'WAITING' CHECK(state IN ('WAITING','APPLYING','APPLIED')),
 grant_targets jsonb NOT NULL DEFAULT '{}', target_base_ends_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 target_entitlement_id uuid REFERENCES entitlements(id), target_revision bigint, target_ends_at timestamptz,
 UNIQUE(invitee_account_id,event_kind)
);
CREATE INDEX referral_rewards_sweep_queue ON referral_rewards(updated_at,id)
 WHERE state IN ('WAITING','APPLYING');
ALTER TABLE s5_payment_quotes ADD COLUMN pricing jsonb;
ALTER TABLE s5_payment_quotes ADD COLUMN referral_create_resolution_reason text;

-- Operator-only protected staging; no public API or automatic absence-based eligibility.
CREATE TABLE referral_history_staging (
 telegram_id bigint PRIMARY KEY CHECK(telegram_id>0), code text UNIQUE,
 referred_by_telegram_id bigint, proven_new boolean NOT NULL,
 trial_used boolean NOT NULL, first_main_paid boolean NOT NULL,
 source_sha256 text NOT NULL CHECK(source_sha256 ~ '^[0-9a-f]{64}$'),
 CHECK((NOT proven_new) OR (code IS NULL AND referred_by_telegram_id IS NULL AND NOT trial_used AND NOT first_main_paid))
);

CREATE TABLE referral_trial_targets (
 source_entitlement_id uuid NOT NULL REFERENCES entitlements(id),
 grant_id uuid NOT NULL REFERENCES grants(id), generation bigint NOT NULL,
 PRIMARY KEY(source_entitlement_id,grant_id)
);
