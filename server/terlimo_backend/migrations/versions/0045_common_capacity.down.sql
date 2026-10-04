DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM capacity_scopes) OR EXISTS(SELECT 1 FROM capacity_admissions) THEN
 RAISE EXCEPTION 'durable activated capacity; forward recovery required'; END IF;
END $$;
DROP VIEW capacity_mobile_ranks;
DROP VIEW capacity_live_admissions;
DROP TABLE capacity_admissions;
DROP TABLE capacity_scopes;
DROP FUNCTION capacity_owner_guard();
DROP FUNCTION capacity_guard();
