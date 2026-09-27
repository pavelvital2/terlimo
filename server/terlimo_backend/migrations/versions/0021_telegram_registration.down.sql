-- Revert 0021: the table carries only the registration link/eligibility snapshot; dropping it
-- deletes no entitlement, account, binding or hour right. Accounts/bindings created by a
-- confirmation remain (they are ordinary linked identity rows).
DROP TABLE IF EXISTS registration_links;
