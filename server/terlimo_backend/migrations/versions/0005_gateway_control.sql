-- 0005_gateway_control: technical gateway-control state on the existing grants row.
-- Additive and reversible. No account/Telegram/payment/trial fields reach the gateway;
-- only ids, generations, lease sequence, finite not_after and the device PoP public key.

ALTER TABLE grants
    ADD COLUMN gateway_credential text,
    ADD COLUMN lease_seq bigint NOT NULL DEFAULT 0,
    ADD COLUMN gateway_generation integer NOT NULL DEFAULT 0,
    ADD COLUMN applied_not_after timestamptz,
    ADD COLUMN applied_at timestamptz,
    ADD COLUMN last_readback jsonb;

CREATE INDEX grants_state_not_after_idx ON grants (state, not_after);
