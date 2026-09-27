-- Revert 0017. Fail-closed while onboarding data or installation-scoped grants exist; no rows
-- are deleted by this script.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM onboarding_intents)
        OR EXISTS (SELECT 1 FROM onboarding_evidence)
        OR EXISTS (SELECT 1 FROM grants WHERE installation_id IS NOT NULL)
    THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0017: onboarding hour data exists';
    END IF;
END $$;

DROP TABLE onboarding_evidence;
DROP TABLE onboarding_intents;
DROP INDEX entitlements_hour_installation_uniq;
CREATE INDEX entitlements_hour_installation_idx
    ON entitlements (installation_id) WHERE kind = 'onboarding_hour';
DROP INDEX grants_installation_gateway_uniq;
DROP INDEX grants_binding_gateway_uniq;
ALTER TABLE grants ADD CONSTRAINT grants_binding_id_gateway_id_key UNIQUE (binding_id, gateway_id);
ALTER TABLE grants DROP CONSTRAINT grants_subject_xor;
ALTER TABLE grants DROP COLUMN installation_id;
ALTER TABLE grants ALTER COLUMN binding_id SET NOT NULL;
