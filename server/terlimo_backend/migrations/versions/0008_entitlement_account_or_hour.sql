-- 0008_entitlement_account_or_hour: unrelaxed commercial invariant. Only an onboarding hour
-- may exist without an account; paid/trial/imported entitlements always belong to an account.
-- Additive; 0001-0007 are not rewritten. Existing invalid rows cause a safe migration refusal
-- (no destructive cleanup); legitimate legacy unlinked hours remain allowed.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM entitlements
        WHERE account_id IS NULL AND kind <> 'onboarding_hour'
    ) THEN
        RAISE EXCEPTION 'MIGRATION_UNSAFE_0008: accountless non-hour entitlements exist';
    END IF;
END $$;

ALTER TABLE entitlements
    ADD CONSTRAINT entitlements_account_or_hour_check
    CHECK (account_id IS NOT NULL OR kind = 'onboarding_hour');
