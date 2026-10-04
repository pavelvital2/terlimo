-- New typed attempts only; historical mobile/APPLIED evidence is not converted.
ALTER TABLE delivery_fulfillments ADD COLUMN source_reward_id uuid UNIQUE REFERENCES referral_rewards(id);
ALTER TABLE delivery_fulfillments DROP CONSTRAINT delivery_fulfillments_source_kind_check;
ALTER TABLE delivery_fulfillments DROP CONSTRAINT delivery_fulfillments_check;
ALTER TABLE delivery_fulfillments ADD CONSTRAINT delivery_source_kind CHECK(source_kind IN ('paid','trial','reward'));
ALTER TABLE delivery_fulfillments ADD CONSTRAINT delivery_source_identity CHECK(
 (source_kind='paid' AND source_order_id IS NOT NULL AND source_reward_id IS NULL)
 OR (source_kind='trial' AND source_order_id IS NULL AND source_reward_id IS NULL)
 OR (source_kind='reward' AND source_order_id IS NULL AND source_reward_id IS NOT NULL));
ALTER TABLE referral_rewards ADD COLUMN fulfillment_kind text NOT NULL DEFAULT 'mobile' CHECK(fulfillment_kind IN ('mobile','typed'));
ALTER TABLE referral_rewards ADD COLUMN typed_source_id uuid UNIQUE REFERENCES delivery_fulfillments(id);
ALTER TABLE referral_rewards ADD CONSTRAINT reward_typed_shape CHECK(
 (fulfillment_kind='mobile' AND typed_source_id IS NULL)
 OR (fulfillment_kind='typed' AND typed_source_id IS NOT NULL AND state IN ('APPLYING','APPLIED')
 AND target_entitlement_id IS NOT NULL AND target_revision IS NOT NULL AND target_revision>0
 AND target_base_ends_at IS NOT NULL AND target_ends_at IS NOT NULL AND target_ends_at=target_base_ends_at+make_interval(days=>days)));
CREATE OR REPLACE FUNCTION delivery_fulfillment_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'durable delivery source'; END IF;
 IF ROW(NEW.id,NEW.account_id,NEW.source_kind,NEW.source_order_id,NEW.entitlement_id,NEW.source_revision,NEW.snapshot,NEW.snapshot_digest,NEW.created_at,NEW.source_sequence,NEW.source_reward_id)
 IS DISTINCT FROM ROW(OLD.id,OLD.account_id,OLD.source_kind,OLD.source_order_id,OLD.entitlement_id,OLD.source_revision,OLD.snapshot,OLD.snapshot_digest,OLD.created_at,OLD.source_sequence,OLD.source_reward_id)
 OR (OLD.state='planned' AND ROW(NEW.state,NEW.manifest_id,NEW.target_digest) IS DISTINCT FROM ROW(OLD.state,OLD.manifest_id,OLD.target_digest)) THEN
 RAISE EXCEPTION 'immutable delivery source/plan' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE FUNCTION reward_typed_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND OLD.fulfillment_kind='mobile' AND NEW.fulfillment_kind='typed' AND OLD.state<>'WAITING' THEN
  RAISE EXCEPTION 'cannot convert historical applying/applied reward' USING ERRCODE='23514'; END IF;
 IF OLD.fulfillment_kind='typed' THEN
  IF TG_OP='DELETE' THEN RAISE EXCEPTION 'durable typed reward' USING ERRCODE='23514'; END IF;
  IF ROW(NEW.id,NEW.invitee_account_id,NEW.inviter_account_id,NEW.event_kind,NEW.source_entitlement_id,NEW.source_order_id,
    NEW.imported,NEW.legacy_source_id,NEW.import_source_sha256,NEW.days,NEW.grant_targets,
    NEW.fulfillment_kind,NEW.typed_source_id,NEW.target_entitlement_id,NEW.target_revision,NEW.target_base_ends_at,NEW.target_ends_at)
   IS DISTINCT FROM ROW(OLD.id,OLD.invitee_account_id,OLD.inviter_account_id,OLD.event_kind,OLD.source_entitlement_id,OLD.source_order_id,
    OLD.imported,OLD.legacy_source_id,OLD.import_source_sha256,OLD.days,OLD.grant_targets,
    OLD.fulfillment_kind,OLD.typed_source_id,OLD.target_entitlement_id,OLD.target_revision,OLD.target_base_ends_at,OLD.target_ends_at)
   OR (OLD.state='APPLIED' AND NEW.state<>'APPLIED') THEN
   RAISE EXCEPTION 'immutable typed reward identity/extension' USING ERRCODE='23514'; END IF;
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER reward_typed_guard BEFORE UPDATE OR DELETE ON referral_rewards FOR EACH ROW EXECUTE FUNCTION reward_typed_guard();
