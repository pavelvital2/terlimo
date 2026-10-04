-- New source order is captured in the original paid/trial transaction; no backfill.
CREATE TABLE delivery_source_counters (
 account_id uuid PRIMARY KEY REFERENCES accounts(id), sequence bigint NOT NULL CHECK(sequence>0)
);
ALTER TABLE delivery_fulfillments ADD COLUMN source_sequence bigint CHECK(source_sequence>0);
CREATE UNIQUE INDEX delivery_source_sequence ON delivery_fulfillments(account_id,source_sequence);
CREATE OR REPLACE FUNCTION delivery_fulfillment_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'durable delivery source'; END IF;
 IF ROW(NEW.id,NEW.account_id,NEW.source_kind,NEW.source_order_id,NEW.entitlement_id,NEW.source_revision,NEW.snapshot,NEW.snapshot_digest,NEW.created_at,NEW.source_sequence)
 IS DISTINCT FROM ROW(OLD.id,OLD.account_id,OLD.source_kind,OLD.source_order_id,OLD.entitlement_id,OLD.source_revision,OLD.snapshot,OLD.snapshot_digest,OLD.created_at,OLD.source_sequence)
 OR (OLD.state='planned' AND ROW(NEW.state,NEW.manifest_id,NEW.target_digest) IS DISTINCT FROM ROW(OLD.state,OLD.manifest_id,OLD.target_digest)) THEN
 RAISE EXCEPTION 'immutable delivery source/plan' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TABLE delivery_claim_schedules (
 manifest_id uuid PRIMARY KEY REFERENCES delivery_manifests(id),
 evidence jsonb NOT NULL CHECK(jsonb_typeof(evidence)='object'),
 evidence_digest text NOT NULL CHECK(evidence_digest ~ '^[0-9a-f]{64}$')
);
CREATE TRIGGER delivery_schedule_immutable BEFORE UPDATE OR DELETE ON delivery_claim_schedules FOR EACH ROW EXECUTE FUNCTION delivery_immutable_row();
CREATE TABLE delivery_physical_targets (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 deployment text NOT NULL, external_key text NOT NULL,
 account_id uuid NOT NULL REFERENCES accounts(id), grant_id uuid NOT NULL, fence_id uuid NOT NULL,
 claim_operation_id uuid NOT NULL UNIQUE, claim_wire text NOT NULL CHECK(octet_length(claim_wire)<16384),
 claim_digest text NOT NULL CHECK(claim_digest ~ '^[0-9a-f]{64}$'),
 initial_base jsonb NOT NULL CHECK(jsonb_typeof(initial_base)='object'),
 initial_revision bigint NOT NULL CHECK(initial_revision>=0),
 claim_outbox_id uuid REFERENCES outbox_operations(id),
 claim_started boolean NOT NULL DEFAULT false,
 claim_receipt jsonb CHECK(jsonb_typeof(claim_receipt)='object'),
 accepted_revision bigint NOT NULL CHECK(accepted_revision>=0),
 proven_revision bigint NOT NULL CHECK(proven_revision>=0 AND proven_revision<=accepted_revision),
 proven_snapshot jsonb NOT NULL CHECK(jsonb_typeof(proven_snapshot)='object'),
 source_frontier bigint NOT NULL DEFAULT 0 CHECK(source_frontier>=0),
 UNIQUE(deployment,external_key)
);
CREATE FUNCTION delivery_physical_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'durable physical target'; END IF;
 IF ROW(NEW.id,NEW.deployment,NEW.external_key,NEW.account_id,NEW.grant_id,NEW.fence_id,NEW.claim_operation_id,NEW.claim_wire,NEW.claim_digest,NEW.initial_base,NEW.initial_revision)
 IS DISTINCT FROM ROW(OLD.id,OLD.deployment,OLD.external_key,OLD.account_id,OLD.grant_id,OLD.fence_id,OLD.claim_operation_id,OLD.claim_wire,OLD.claim_digest,OLD.initial_base,OLD.initial_revision)
 OR NEW.accepted_revision<OLD.accepted_revision OR NEW.proven_revision<OLD.proven_revision OR NEW.source_frontier<OLD.source_frontier
 OR (OLD.claim_started AND NOT NEW.claim_started)
 OR (OLD.claim_outbox_id IS NOT NULL AND NEW.claim_outbox_id IS DISTINCT FROM OLD.claim_outbox_id)
 OR (OLD.claim_receipt IS NOT NULL AND NEW.claim_receipt IS DISTINCT FROM OLD.claim_receipt) THEN
 RAISE EXCEPTION 'immutable physical identity/proof or decreasing revision' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER delivery_physical_guard BEFORE UPDATE OR DELETE ON delivery_physical_targets FOR EACH ROW EXECUTE FUNCTION delivery_physical_guard();
ALTER TABLE delivery_items
 ADD COLUMN physical_id uuid REFERENCES delivery_physical_targets(id),
 ADD COLUMN outbox_id uuid UNIQUE REFERENCES outbox_operations(id),
 ADD COLUMN wire text CHECK(octet_length(wire)<16384),
 ADD COLUMN delivery_revision bigint CHECK(delivery_revision>=0),
 ADD COLUMN dispatch_started boolean NOT NULL DEFAULT false,
 ADD COLUMN outcome text NOT NULL DEFAULT 'pending' CHECK(outcome IN ('pending','applied','conflict','blocked','superseded')),
 ADD COLUMN application_receipt jsonb CHECK(jsonb_typeof(application_receipt)='object'),
 ADD COLUMN observation jsonb CHECK(jsonb_typeof(observation)='object');
-- Replace the old reserved request-shape constraint; new prepared rows require exact wire.
DO $$ DECLARE n text; BEGIN
 FOR n IN SELECT conname FROM pg_constraint WHERE conrelid='delivery_items'::regclass AND contype='c'
 AND pg_get_constraintdef(oid) LIKE '%preparation%expected_revision%' LOOP
 EXECUTE format('ALTER TABLE delivery_items DROP CONSTRAINT %I',n);
 END LOOP;
END $$;
ALTER TABLE delivery_items ADD CONSTRAINT delivery_item_closed_preparation CHECK(
 (preparation='unprepared' AND expected_revision IS NULL AND request IS NULL AND request_digest IS NULL AND wire IS NULL AND delivery_revision IS NULL AND NOT dispatch_started)
 OR (preparation='prepared' AND physical_id IS NOT NULL AND expected_revision>=0 AND expected_revision IS NOT NULL
 AND delivery_revision IS NOT NULL AND delivery_revision>expected_revision AND request IS NOT NULL AND jsonb_typeof(request)='object'
 AND request_digest ~ '^[0-9a-f]{64}$' AND request_digest IS NOT NULL AND wire IS NOT NULL)
);
ALTER TABLE delivery_items ADD CONSTRAINT delivery_item_proof_shape CHECK(
 (application_receipt IS NULL OR (dispatch_started AND preparation='prepared')) AND
 (outcome<>'applied' OR application_receipt IS NOT NULL) AND
 (outcome<>'superseded' OR NOT dispatch_started)
);
CREATE OR REPLACE FUNCTION delivery_item_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'durable delivery item'; END IF;
 IF ROW(NEW.id,NEW.fulfillment_id,NEW.target_id,NEW.operation_id,NEW.desired) IS DISTINCT FROM ROW(OLD.id,OLD.fulfillment_id,OLD.target_id,OLD.operation_id,OLD.desired)
 OR (OLD.physical_id IS NOT NULL AND NEW.physical_id IS DISTINCT FROM OLD.physical_id)
 OR (OLD.outbox_id IS NOT NULL AND NEW.outbox_id IS DISTINCT FROM OLD.outbox_id)
 OR (OLD.preparation='prepared' AND ROW(NEW.preparation,NEW.expected_revision,NEW.request,NEW.request_digest,NEW.wire,NEW.delivery_revision) IS DISTINCT FROM ROW(OLD.preparation,OLD.expected_revision,OLD.request,OLD.request_digest,OLD.wire,OLD.delivery_revision))
 OR (OLD.dispatch_started AND NOT NEW.dispatch_started)
 OR (OLD.application_receipt IS NOT NULL AND NEW.application_receipt IS DISTINCT FROM OLD.application_receipt) THEN
 RAISE EXCEPTION 'immutable delivery item/request/proof' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
