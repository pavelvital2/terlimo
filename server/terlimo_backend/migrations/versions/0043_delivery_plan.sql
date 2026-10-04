-- Staging never enables admission or dispatch. Proof writer belongs to a later slice.
CREATE TABLE delivery_manifests (
 id uuid PRIMARY KEY,
 account_id uuid NOT NULL REFERENCES accounts(id),
 digest text NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
 body jsonb NOT NULL CHECK (jsonb_typeof(body)='object'),
 state text NOT NULL CHECK (state IN ('staged','conflict')),
 conflicts jsonb NOT NULL CHECK (jsonb_typeof(conflicts)='array'),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(id,account_id)
);
CREATE TABLE delivery_targets (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 manifest_id uuid NOT NULL,
 account_id uuid NOT NULL,
 ordinal integer NOT NULL CHECK (ordinal>0),
 kind text NOT NULL CHECK(kind IN ('mobile','direct')),
 binding_id uuid REFERENCES account_bindings(id),
 deployment text,
 external_key text,
 evidence jsonb NOT NULL CHECK(jsonb_typeof(evidence)='object'),
 FOREIGN KEY(manifest_id,account_id) REFERENCES delivery_manifests(id,account_id),
 UNIQUE(manifest_id,ordinal), UNIQUE(id,manifest_id),
 CHECK ((kind='mobile' AND binding_id IS NOT NULL AND deployment IS NULL AND external_key IS NULL)
 OR (kind='direct' AND binding_id IS NULL AND deployment IS NOT NULL AND external_key IS NOT NULL))
);
CREATE UNIQUE INDEX delivery_target_mobile ON delivery_targets(manifest_id,binding_id) WHERE kind='mobile';
CREATE UNIQUE INDEX delivery_target_direct ON delivery_targets(manifest_id,deployment,external_key) WHERE kind='direct';
-- Intentionally no production writer/API in this slice. Later authenticated proof commit
-- must own these rows; a manifest, caller bool or successful readback is NOT this receipt.
CREATE TABLE delivery_mapping_proofs (
 manifest_id uuid PRIMARY KEY REFERENCES delivery_manifests(id),
 receipt_id uuid NOT NULL UNIQUE,
 manifest_digest text NOT NULL CHECK(manifest_digest ~ '^[0-9a-f]{64}$'),
 target_digest text NOT NULL CHECK(target_digest ~ '^[0-9a-f]{64}$'),
 evidence jsonb NOT NULL CHECK(jsonb_typeof(evidence)='object')
);
CREATE TABLE delivery_fulfillments (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 account_id uuid NOT NULL REFERENCES accounts(id),
 source_kind text NOT NULL CHECK(source_kind IN ('paid','trial')),
 source_order_id uuid UNIQUE REFERENCES payment_orders(id),
 entitlement_id uuid NOT NULL REFERENCES entitlements(id),
 source_revision bigint NOT NULL CHECK(source_revision>0),
 snapshot jsonb NOT NULL CHECK(jsonb_typeof(snapshot)='object'),
 snapshot_digest text NOT NULL CHECK(snapshot_digest ~ '^[0-9a-f]{64}$'),
 state text NOT NULL DEFAULT 'awaiting_mapping' CHECK(state IN ('awaiting_mapping','planned')),
 manifest_id uuid REFERENCES delivery_mapping_proofs(manifest_id),
 target_digest text,
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK((source_kind='paid' AND source_order_id IS NOT NULL) OR (source_kind='trial' AND source_order_id IS NULL)),
 CHECK((state='awaiting_mapping' AND manifest_id IS NULL AND target_digest IS NULL)
 OR (state='planned' AND manifest_id IS NOT NULL AND target_digest IS NOT NULL AND target_digest ~ '^[0-9a-f]{64}$'))
);
CREATE UNIQUE INDEX delivery_trial_source ON delivery_fulfillments(entitlement_id) WHERE source_kind='trial';
CREATE TABLE delivery_items (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 fulfillment_id uuid NOT NULL REFERENCES delivery_fulfillments(id),
 target_id uuid NOT NULL REFERENCES delivery_targets(id),
 operation_id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
 desired jsonb NOT NULL CHECK(jsonb_typeof(desired)='object'),
 preparation text NOT NULL DEFAULT 'unprepared' CHECK(preparation IN ('unprepared','prepared')),
 expected_revision bigint,
 request jsonb,
 request_digest text,
 UNIQUE(fulfillment_id,target_id),
 CHECK((preparation='unprepared' AND expected_revision IS NULL AND request IS NULL AND request_digest IS NULL)
 OR (preparation='prepared' AND expected_revision IS NOT NULL AND expected_revision>=0 AND request IS NOT NULL AND jsonb_typeof(request)='object'
     AND request_digest IS NOT NULL AND request_digest ~ '^[0-9a-f]{64}$'))
);
CREATE FUNCTION delivery_immutable_row() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'immutable delivery evidence' USING ERRCODE='23514'; END $$;
CREATE TRIGGER delivery_manifest_immutable BEFORE UPDATE OR DELETE ON delivery_manifests FOR EACH ROW EXECUTE FUNCTION delivery_immutable_row();
CREATE TRIGGER delivery_target_immutable BEFORE UPDATE OR DELETE ON delivery_targets FOR EACH ROW EXECUTE FUNCTION delivery_immutable_row();
CREATE TRIGGER delivery_proof_immutable BEFORE UPDATE OR DELETE ON delivery_mapping_proofs FOR EACH ROW EXECUTE FUNCTION delivery_immutable_row();
CREATE FUNCTION delivery_fulfillment_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'durable delivery source'; END IF;
 IF ROW(NEW.id,NEW.account_id,NEW.source_kind,NEW.source_order_id,NEW.entitlement_id,NEW.source_revision,NEW.snapshot,NEW.snapshot_digest,NEW.created_at)
 IS DISTINCT FROM ROW(OLD.id,OLD.account_id,OLD.source_kind,OLD.source_order_id,OLD.entitlement_id,OLD.source_revision,OLD.snapshot,OLD.snapshot_digest,OLD.created_at)
 OR (OLD.state='planned' AND ROW(NEW.state,NEW.manifest_id,NEW.target_digest) IS DISTINCT FROM ROW(OLD.state,OLD.manifest_id,OLD.target_digest)) THEN
 RAISE EXCEPTION 'immutable delivery source/plan' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER delivery_fulfillment_guard BEFORE UPDATE OR DELETE ON delivery_fulfillments FOR EACH ROW EXECUTE FUNCTION delivery_fulfillment_guard();
CREATE FUNCTION delivery_item_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'durable delivery item'; END IF;
 IF ROW(NEW.id,NEW.fulfillment_id,NEW.target_id,NEW.operation_id,NEW.desired) IS DISTINCT FROM ROW(OLD.id,OLD.fulfillment_id,OLD.target_id,OLD.operation_id,OLD.desired)
 OR (OLD.preparation='prepared' AND ROW(NEW.preparation,NEW.expected_revision,NEW.request,NEW.request_digest) IS DISTINCT FROM ROW(OLD.preparation,OLD.expected_revision,OLD.request,OLD.request_digest)) THEN
 RAISE EXCEPTION 'immutable delivery item/request' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER delivery_item_guard BEFORE UPDATE OR DELETE ON delivery_items FOR EACH ROW EXECUTE FUNCTION delivery_item_guard();
