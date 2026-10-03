DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM referral_history_epochs) OR EXISTS(SELECT 1 FROM referral_rewards WHERE import_evidence IS NOT NULL)
 THEN RAISE EXCEPTION 'referral history receipts must be retained'; END IF;
END $$;
ALTER TABLE referral_rewards DROP COLUMN import_evidence;
ALTER TABLE referral_benefits DROP COLUMN history_proof, DROP COLUMN history_epoch_id;
ALTER TABLE referral_history_staging DROP COLUMN reward_dispositions, DROP COLUMN code_absent_verified, DROP COLUMN epoch_id;
DROP TABLE referral_history_epochs;
