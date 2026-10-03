-- Keep durable identities/receipts after use; application rollback need not drop schema.
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM accounts WHERE referral_code IS NOT NULL OR referred_by_account_id IS NOT NULL)
 OR EXISTS(SELECT 1 FROM referral_candidates) OR EXISTS(SELECT 1 FROM referral_operations)
 OR EXISTS(SELECT 1 FROM referral_history_staging)
 OR EXISTS(SELECT 1 FROM referral_trial_targets)
 OR EXISTS(SELECT 1 FROM s5_payment_quotes WHERE pricing IS NOT NULL OR referral_create_resolution_reason IS NOT NULL)
 OR EXISTS(SELECT 1 FROM referral_rewards) OR EXISTS(SELECT 1 FROM referral_benefits WHERE history_state<>'history_pending' OR first_paid_order_id IS NOT NULL OR reserved_order_id IS NOT NULL OR consumed_order_id IS NOT NULL)
 THEN RAISE EXCEPTION 'referral history in use; retain additive schema'; END IF;
END $$;
DROP TABLE referral_history_staging;
ALTER TABLE s5_payment_quotes DROP COLUMN referral_create_resolution_reason;
ALTER TABLE s5_payment_quotes DROP COLUMN pricing;
DROP TABLE referral_trial_targets;
DROP TABLE referral_rewards;
ALTER TABLE registration_links DROP COLUMN referral_attribution;
ALTER TABLE registration_links DROP COLUMN referral_idempotency_key;
ALTER TABLE registration_links DROP COLUMN referral_candidate_id;
DROP TABLE referral_operations;
DROP TABLE referral_candidates;
DROP TABLE referral_benefits;
ALTER TABLE accounts DROP CONSTRAINT referral_not_self;
ALTER TABLE accounts DROP COLUMN referral_terms_version;
ALTER TABLE accounts DROP COLUMN referral_attributed_at;
ALTER TABLE accounts DROP COLUMN referred_by_account_id;
ALTER TABLE accounts DROP COLUMN referral_code;
ALTER TABLE accounts DROP COLUMN referral_attribution_receipt_id;
