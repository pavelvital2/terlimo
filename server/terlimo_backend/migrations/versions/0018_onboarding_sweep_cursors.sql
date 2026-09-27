-- 0018_onboarding_sweep_cursors: durable keyset cursors for the onboarding-hour maintenance
-- sweeps. Additive; cursors are pure scheduling anchors (no rights/rows are deleted), so a down
-- migration only removes the anchor table and cannot lose any entitlement/right.

CREATE TABLE onboarding_sweep_cursors (
    sweep           text PRIMARY KEY,
    last_expires_at timestamptz,
    last_id         uuid,
    updated_at      timestamptz NOT NULL DEFAULT now()
);
