-- 0030_entitlement_reminders: durable provenance for entitlement pre-expiry reminders.
-- The reminder itself is a normal account-scoped announcement (0028); this table is the
-- idempotency key that makes repeated worker sweeps create exactly one reminder per
-- (entitlement, revision, kind). A revision bump (extension/renewal) supersedes the old
-- reminder and permits exactly one new reminder for the new revision.
CREATE TABLE entitlement_reminders (
    entitlement_id uuid NOT NULL REFERENCES entitlements (id) ON DELETE CASCADE,
    entitlement_revision integer NOT NULL,
    kind text NOT NULL CHECK (kind IN ('pre_expiry_3d')),
    announcement_id uuid NOT NULL REFERENCES announcements (id) ON DELETE CASCADE,
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'superseded')),
    created_at timestamptz NOT NULL DEFAULT now(),
    superseded_at timestamptz,
    PRIMARY KEY (entitlement_id, entitlement_revision, kind)
);
CREATE INDEX entitlement_reminders_active_idx ON entitlement_reminders (entitlement_id) WHERE state = 'active';
