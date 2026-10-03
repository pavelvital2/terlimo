DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM referral_trusted_operations)
 THEN RAISE EXCEPTION 'preserve trusted attribution receipts'; END IF;
END $$;
DROP TABLE referral_trusted_operations;
