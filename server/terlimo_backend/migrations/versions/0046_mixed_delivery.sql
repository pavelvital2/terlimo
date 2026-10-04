CREATE TABLE capacity_heads (
 account_id uuid PRIMARY KEY REFERENCES capacity_scopes(account_id),
 membership_revision bigint NOT NULL DEFAULT 0 CHECK(membership_revision>=0)
);
INSERT INTO capacity_heads(account_id) SELECT account_id FROM capacity_scopes;
CREATE TABLE delivery_plans (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 source_id uuid NOT NULL REFERENCES delivery_fulfillments(id),
 account_id uuid NOT NULL REFERENCES accounts(id),
 manifest_id uuid NOT NULL REFERENCES delivery_mapping_proofs(manifest_id),
 membership_revision bigint NOT NULL,
 members jsonb NOT NULL CHECK(jsonb_typeof(members)='array' AND jsonb_array_length(members)>0),
 destination_digest text NOT NULL CHECK(destination_digest ~ '^[0-9a-f]{64}$'),
 dispatch_sequence bigint NOT NULL CHECK(dispatch_sequence>0),
 UNIQUE(account_id,dispatch_sequence), UNIQUE(source_id,membership_revision,destination_digest)
);
CREATE TRIGGER delivery_plan_immutable BEFORE UPDATE OR DELETE ON delivery_plans FOR EACH ROW EXECUTE FUNCTION delivery_immutable_row();
ALTER TABLE delivery_items ADD COLUMN plan_id uuid REFERENCES delivery_plans(id);
ALTER TABLE delivery_items ADD COLUMN delivery_order bigint CHECK(delivery_order>0);
ALTER TABLE delivery_items ADD CONSTRAINT delivery_item_plan_order CHECK((plan_id IS NULL AND delivery_order IS NULL) OR (plan_id IS NOT NULL AND delivery_order IS NOT NULL));
ALTER TABLE delivery_items DROP CONSTRAINT delivery_items_fulfillment_id_target_id_key;
CREATE UNIQUE INDEX delivery_legacy_target ON delivery_items(fulfillment_id,target_id) WHERE plan_id IS NULL;
CREATE UNIQUE INDEX delivery_plan_direct_target ON delivery_items(plan_id,target_id) WHERE plan_id IS NOT NULL;
CREATE TABLE delivery_mobile_items (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 plan_id uuid NOT NULL REFERENCES delivery_plans(id),
 admission_id uuid NOT NULL REFERENCES capacity_admissions(id),
 binding_id uuid NOT NULL REFERENCES account_bindings(id),
 binding_generation integer NOT NULL CHECK(binding_generation>0),
 rank bigint NOT NULL CHECK(rank>0), deadline timestamptz,
 required_grants jsonb NOT NULL CHECK(jsonb_typeof(required_grants)='array'),
 UNIQUE(plan_id,binding_id)
);
CREATE TRIGGER delivery_mobile_immutable BEFORE UPDATE OR DELETE ON delivery_mobile_items FOR EACH ROW EXECUTE FUNCTION delivery_immutable_row();
CREATE TABLE delivery_mobile_proofs (
 item_id uuid PRIMARY KEY REFERENCES delivery_mobile_items(id),
 receipt jsonb NOT NULL CHECK(jsonb_typeof(receipt)='array' AND jsonb_array_length(receipt)>0)
);
CREATE TRIGGER delivery_mobile_proof_immutable BEFORE UPDATE OR DELETE ON delivery_mobile_proofs FOR EACH ROW EXECUTE FUNCTION delivery_immutable_row();
CREATE TABLE delivery_completions (
 source_id uuid PRIMARY KEY REFERENCES delivery_fulfillments(id),
 plan_id uuid NOT NULL REFERENCES delivery_plans(id),
 receipt jsonb NOT NULL CHECK(jsonb_typeof(receipt)='object'),
 completed_at timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER delivery_completion_immutable BEFORE UPDATE OR DELETE ON delivery_completions FOR EACH ROW EXECUTE FUNCTION delivery_immutable_row();
ALTER TABLE grants ADD COLUMN delivery_source_id uuid REFERENCES delivery_fulfillments(id);
ALTER TABLE grants ADD COLUMN delivery_binding_generation integer;
ALTER TABLE grants ADD CONSTRAINT grant_delivery_shape CHECK((delivery_source_id IS NULL AND delivery_binding_generation IS NULL) OR (binding_id IS NOT NULL AND hour_entitlement_id IS NULL AND delivery_source_id IS NOT NULL AND delivery_binding_generation>0));
CREATE OR REPLACE FUNCTION delivery_item_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'durable delivery item'; END IF;
 IF ROW(NEW.id,NEW.fulfillment_id,NEW.target_id,NEW.operation_id,NEW.desired,NEW.plan_id,NEW.delivery_order) IS DISTINCT FROM ROW(OLD.id,OLD.fulfillment_id,OLD.target_id,OLD.operation_id,OLD.desired,OLD.plan_id,OLD.delivery_order)
 OR (OLD.physical_id IS NOT NULL AND NEW.physical_id IS DISTINCT FROM OLD.physical_id)
 OR (OLD.outbox_id IS NOT NULL AND NEW.outbox_id IS DISTINCT FROM OLD.outbox_id)
 OR (OLD.preparation='prepared' AND ROW(NEW.preparation,NEW.expected_revision,NEW.request,NEW.request_digest,NEW.wire,NEW.delivery_revision) IS DISTINCT FROM ROW(OLD.preparation,OLD.expected_revision,OLD.request,OLD.request_digest,OLD.wire,OLD.delivery_revision))
 OR (OLD.dispatch_started AND NOT NEW.dispatch_started)
 OR (OLD.application_receipt IS NOT NULL AND NEW.application_receipt IS DISTINCT FROM OLD.application_receipt) THEN
 RAISE EXCEPTION 'immutable delivery item/request/proof' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
