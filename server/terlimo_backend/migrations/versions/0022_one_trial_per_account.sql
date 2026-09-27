-- 0022_one_trial_per_account: at most one trial entitlement per account. This is the durable
-- race guard behind "the same Telegram identity gets at most one original 7-day interval"; the
-- activation code also re-checks trial history and channel membership at issuance time.
CREATE UNIQUE INDEX entitlements_one_trial_per_account
    ON entitlements (account_id)
    WHERE kind = 'trial';
