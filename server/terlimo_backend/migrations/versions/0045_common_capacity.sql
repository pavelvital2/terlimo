-- Explicit protected activation; no backfill or automatic reservations.
CREATE TABLE capacity_scopes (
 account_id uuid PRIMARY KEY REFERENCES accounts(id),
 manifest_id uuid NOT NULL UNIQUE REFERENCES delivery_manifests(id),
 manifest_digest text NOT NULL CHECK(manifest_digest ~ '^[0-9a-f]{64}$'),
 activated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE capacity_admissions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 account_id uuid NOT NULL REFERENCES capacity_scopes(account_id),
 admission_order bigint NOT NULL CHECK(admission_order>0),
 kind text NOT NULL CHECK(kind IN ('direct','mobile')),
 physical_id uuid REFERENCES delivery_physical_targets(id),
 binding_id uuid REFERENCES account_bindings(id),
 admitted_at timestamptz NOT NULL DEFAULT now(), released_at timestamptz,
 UNIQUE(account_id,admission_order),
 CHECK((kind='direct' AND physical_id IS NOT NULL AND binding_id IS NULL AND released_at IS NULL)
 OR (kind='mobile' AND binding_id IS NOT NULL AND physical_id IS NULL))
);
CREATE UNIQUE INDEX capacity_direct_identity ON capacity_admissions(physical_id) WHERE kind='direct';
CREATE UNIQUE INDEX capacity_mobile_current ON capacity_admissions(binding_id) WHERE kind='mobile' AND released_at IS NULL;
CREATE FUNCTION capacity_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_TABLE_NAME='capacity_scopes' OR TG_OP='DELETE' THEN RAISE EXCEPTION 'immutable capacity scope/admission'; END IF;
 IF ROW(NEW.id,NEW.account_id,NEW.admission_order,NEW.kind,NEW.physical_id,NEW.binding_id,NEW.admitted_at)
 IS DISTINCT FROM ROW(OLD.id,OLD.account_id,OLD.admission_order,OLD.kind,OLD.physical_id,OLD.binding_id,OLD.admitted_at)
 OR (OLD.released_at IS NOT NULL AND NEW.released_at IS DISTINCT FROM OLD.released_at) THEN
 RAISE EXCEPTION 'immutable capacity admission'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER capacity_scope_guard BEFORE UPDATE OR DELETE ON capacity_scopes FOR EACH ROW EXECUTE FUNCTION capacity_guard();
CREATE TRIGGER capacity_admission_guard BEFORE UPDATE OR DELETE ON capacity_admissions FOR EACH ROW EXECUTE FUNCTION capacity_guard();
CREATE FUNCTION capacity_owner_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.kind='direct' AND NOT EXISTS(SELECT 1 FROM delivery_physical_targets p WHERE p.id=NEW.physical_id AND p.account_id=NEW.account_id AND p.claim_receipt IS NOT NULL)
 OR NEW.kind='mobile' AND NOT EXISTS(SELECT 1 FROM account_bindings b WHERE b.id=NEW.binding_id AND b.account_id=NEW.account_id AND b.status='active') THEN
 RAISE EXCEPTION 'capacity identity/owner mismatch'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER capacity_admission_owner BEFORE INSERT ON capacity_admissions FOR EACH ROW EXECUTE FUNCTION capacity_owner_guard();
-- One live rank for both types; revoked mobile incarnations do not occupy a seat.
CREATE VIEW capacity_live_admissions AS
 SELECT a.*,row_number() OVER(PARTITION BY a.account_id ORDER BY a.admission_order) AS rank
 FROM capacity_admissions a LEFT JOIN account_bindings b ON b.id=a.binding_id
 WHERE a.released_at IS NULL AND (a.kind='direct' OR b.status='active');
CREATE VIEW capacity_mobile_ranks AS
 SELECT id,account_id,row_number() OVER(PARTITION BY account_id ORDER BY bound_at,id) AS rank
 FROM account_bindings b WHERE status='active' AND NOT EXISTS(SELECT 1 FROM capacity_scopes s WHERE s.account_id=b.account_id)
 UNION ALL SELECT binding_id AS id,account_id,rank FROM capacity_live_admissions WHERE kind='mobile';
