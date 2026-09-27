-- Revert 0022: drops only the uniqueness guard; trial rows are untouched.
DROP INDEX IF EXISTS entitlements_one_trial_per_account;
