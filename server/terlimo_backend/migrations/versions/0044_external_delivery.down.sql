DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM delivery_physical_targets) OR EXISTS(SELECT 1 FROM delivery_source_counters)
 OR EXISTS(SELECT 1 FROM delivery_claim_schedules) OR EXISTS(SELECT 1 FROM delivery_items WHERE physical_id IS NOT NULL OR wire IS NOT NULL) THEN
 RAISE EXCEPTION 'durable external delivery data; reviewed forward recovery required'; END IF;
END $$;
CREATE OR REPLACE FUNCTION delivery_item_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'durable delivery item'; END IF;
 IF ROW(NEW.id,NEW.fulfillment_id,NEW.target_id,NEW.operation_id,NEW.desired) IS DISTINCT FROM ROW(OLD.id,OLD.fulfillment_id,OLD.target_id,OLD.operation_id,OLD.desired)
 OR (OLD.preparation='prepared' AND ROW(NEW.preparation,NEW.expected_revision,NEW.request,NEW.request_digest) IS DISTINCT FROM ROW(OLD.preparation,OLD.expected_revision,OLD.request,OLD.request_digest)) THEN
 RAISE EXCEPTION 'immutable delivery item/request' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION delivery_fulfillment_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'durable delivery source'; END IF;
 IF ROW(NEW.id,NEW.account_id,NEW.source_kind,NEW.source_order_id,NEW.entitlement_id,NEW.source_revision,NEW.snapshot,NEW.snapshot_digest,NEW.created_at)
 IS DISTINCT FROM ROW(OLD.id,OLD.account_id,OLD.source_kind,OLD.source_order_id,OLD.entitlement_id,OLD.source_revision,OLD.snapshot,OLD.snapshot_digest,OLD.created_at)
 OR (OLD.state='planned' AND ROW(NEW.state,NEW.manifest_id,NEW.target_digest) IS DISTINCT FROM ROW(OLD.state,OLD.manifest_id,OLD.target_digest)) THEN
 RAISE EXCEPTION 'immutable delivery source/plan' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
ALTER TABLE delivery_items DROP CONSTRAINT delivery_item_closed_preparation, DROP CONSTRAINT delivery_item_proof_shape;
ALTER TABLE delivery_items DROP COLUMN physical_id,DROP COLUMN outbox_id,DROP COLUMN wire,DROP COLUMN delivery_revision,DROP COLUMN dispatch_started,DROP COLUMN outcome,DROP COLUMN application_receipt,DROP COLUMN observation;
ALTER TABLE delivery_items ADD CHECK((preparation='unprepared' AND expected_revision IS NULL AND request IS NULL AND request_digest IS NULL)
 OR (preparation='prepared' AND expected_revision IS NOT NULL AND expected_revision>=0 AND request IS NOT NULL AND jsonb_typeof(request)='object' AND request_digest IS NOT NULL AND request_digest ~ '^[0-9a-f]{64}$'));
DROP TABLE delivery_physical_targets;
DROP FUNCTION delivery_physical_guard();
DROP TABLE delivery_claim_schedules;
ALTER TABLE delivery_fulfillments DROP COLUMN source_sequence;
DROP TABLE delivery_source_counters;
