DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM delivery_fulfillments WHERE source_kind='reward' OR source_reward_id IS NOT NULL)
 OR EXISTS(SELECT 1 FROM referral_rewards WHERE fulfillment_kind='typed') THEN
 RAISE EXCEPTION 'durable typed reward; forward recovery required'; END IF;
END $$;
DROP TRIGGER reward_typed_guard ON referral_rewards;
DROP FUNCTION reward_typed_guard();
ALTER TABLE referral_rewards DROP CONSTRAINT reward_typed_shape;
ALTER TABLE referral_rewards DROP COLUMN typed_source_id;
ALTER TABLE referral_rewards DROP COLUMN fulfillment_kind;
ALTER TABLE delivery_fulfillments DROP CONSTRAINT delivery_source_identity;
ALTER TABLE delivery_fulfillments DROP CONSTRAINT delivery_source_kind;
ALTER TABLE delivery_fulfillments DROP COLUMN source_reward_id;
ALTER TABLE delivery_fulfillments ADD CONSTRAINT delivery_fulfillments_source_kind_check CHECK(source_kind IN ('paid','trial'));
ALTER TABLE delivery_fulfillments ADD CONSTRAINT delivery_fulfillments_check CHECK((source_kind='paid' AND source_order_id IS NOT NULL) OR (source_kind='trial' AND source_order_id IS NULL));
CREATE OR REPLACE FUNCTION delivery_fulfillment_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'durable delivery source'; END IF;
 IF ROW(NEW.id,NEW.account_id,NEW.source_kind,NEW.source_order_id,NEW.entitlement_id,NEW.source_revision,NEW.snapshot,NEW.snapshot_digest,NEW.created_at,NEW.source_sequence)
 IS DISTINCT FROM ROW(OLD.id,OLD.account_id,OLD.source_kind,OLD.source_order_id,OLD.entitlement_id,OLD.source_revision,OLD.snapshot,OLD.snapshot_digest,OLD.created_at,OLD.source_sequence)
 OR (OLD.state='planned' AND ROW(NEW.state,NEW.manifest_id,NEW.target_digest) IS DISTINCT FROM ROW(OLD.state,OLD.manifest_id,OLD.target_digest)) THEN
 RAISE EXCEPTION 'immutable delivery source/plan' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
