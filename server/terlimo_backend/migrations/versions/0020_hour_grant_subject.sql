-- 0020_hour_grant_subject: exact onboarding_hour ownership for installation-scoped grants.
--
-- 0017 already made grants subject-scoped (binding_id nullable, installation_id, XOR ownership,
-- unique (installation_id, gateway_id)). What was still missing is a provable link from an
-- installation-scoped grant to the exact onboarding_hour entitlement it serves: without it a
-- revoke/delayed outbox op cannot name its owner and could not be fenced from a different hour
-- incarnation. This additive migration adds that owner only; it creates no new subsystem and no
-- runtime migration is applied here (source-only until accepted).

ALTER TABLE grants
    ADD COLUMN hour_entitlement_id uuid REFERENCES entitlements (id) ON DELETE CASCADE;

-- Every installation-scoped grant (binding_id IS NULL) must name its onboarding_hour owner, and
-- that owner must be the same installation and kind. binding-scoped grants keep it NULL.
ALTER TABLE grants ADD CONSTRAINT grants_hour_owner_required CHECK (
    (binding_id IS NOT NULL AND hour_entitlement_id IS NULL)
    OR (binding_id IS NULL AND hour_entitlement_id IS NOT NULL)
);

CREATE FUNCTION grants_hour_owner_check() RETURNS trigger AS $$
BEGIN
    IF NEW.hour_entitlement_id IS NULL THEN
        RETURN NEW;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM entitlements AS entitlement
        WHERE entitlement.id = NEW.hour_entitlement_id
          AND entitlement.installation_id = NEW.installation_id
          AND entitlement.kind = 'onboarding_hour'
    ) THEN
        RAISE EXCEPTION 'grant hour owner mismatch';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER grants_hour_owner_check
    BEFORE INSERT OR UPDATE OF hour_entitlement_id, installation_id, binding_id ON grants
    FOR EACH ROW EXECUTE FUNCTION grants_hour_owner_check();
