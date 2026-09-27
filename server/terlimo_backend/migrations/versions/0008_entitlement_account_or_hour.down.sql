-- Revert 0008_entitlement_account_or_hour.
--
-- Downgrade boundary (explicit): dropping the constraint removes the accountless
-- paid/trial/imported protection while keeping all rows untouched. It is safe to drop only
-- when the operator accepts that boundary; no rows are deleted or rewritten here.

ALTER TABLE entitlements
    DROP CONSTRAINT IF EXISTS entitlements_account_or_hour_check;
