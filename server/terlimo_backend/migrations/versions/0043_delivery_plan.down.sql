DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM delivery_manifests) OR EXISTS(SELECT 1 FROM delivery_fulfillments) THEN
 RAISE EXCEPTION '0043 contains durable delivery evidence; downgrade refused'; END IF;
END $$;
DROP TABLE delivery_items,delivery_fulfillments,delivery_mapping_proofs,delivery_targets,delivery_manifests;
DROP FUNCTION delivery_item_guard(),delivery_fulfillment_guard(),delivery_immutable_row();
