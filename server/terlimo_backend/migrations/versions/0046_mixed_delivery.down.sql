DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM delivery_plans) OR EXISTS(SELECT 1 FROM delivery_completions)
 OR EXISTS(SELECT 1 FROM capacity_heads WHERE membership_revision>0)
 OR EXISTS(SELECT 1 FROM grants WHERE delivery_source_id IS NOT NULL) THEN
 RAISE EXCEPTION 'durable mixed delivery; forward recovery required'; END IF;
END $$;
DROP TABLE delivery_completions;
DROP TABLE delivery_mobile_proofs;
DROP TABLE delivery_mobile_items;
ALTER TABLE grants DROP CONSTRAINT grant_delivery_shape;
ALTER TABLE grants DROP COLUMN delivery_binding_generation;
ALTER TABLE grants DROP COLUMN delivery_source_id;
ALTER TABLE delivery_items DROP CONSTRAINT delivery_item_plan_order;
DROP INDEX delivery_plan_direct_target;
DROP INDEX delivery_legacy_target;
ALTER TABLE delivery_items DROP COLUMN delivery_order;
ALTER TABLE delivery_items DROP COLUMN plan_id;
ALTER TABLE delivery_items ADD UNIQUE(fulfillment_id,target_id);
DROP TABLE delivery_plans;
DROP TABLE capacity_heads;
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
