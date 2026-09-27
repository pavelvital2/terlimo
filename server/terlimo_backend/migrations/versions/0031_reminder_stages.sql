-- 0031_reminder_stages: turn the single pre-expiry reminder into four calendar stages
-- (T-3/T-2/T-1/day-of) keyed by the entitlement, its revision, the exact end timestamp and the
-- stage kind. Additive to 0030: existing rows migrate to the T-3 stage.

-- Stage end comes from the 0030 provenance announcement.show_until, NOT from the entitlement's
-- current ends_at which may already reflect a renewal. For an ACTIVE 0030 row show_until holds the
-- original deadline. For an already-SUPERSEDED historical row show_until may have been shortened by
-- the supersede/hide step, so the exact original end is not always recoverable: the backfilled
-- value is then an inert historical value only and is never used for active identity, reconcile or
-- send (that row's state is 'superseded').
ALTER TABLE entitlement_reminders ADD COLUMN stage_end_at timestamptz;
UPDATE entitlement_reminders AS r
SET stage_end_at = a.show_until
FROM announcements AS a
WHERE a.id = r.announcement_id;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM entitlement_reminders WHERE stage_end_at IS NULL) THEN
        RAISE EXCEPTION 'MIGRATION_UNSAFE_0031: reminder rows without recoverable provenance end';
    END IF;
END $$;
ALTER TABLE entitlement_reminders ALTER COLUMN stage_end_at SET NOT NULL;
ALTER TABLE entitlement_reminders DROP CONSTRAINT entitlement_reminders_kind_check;
UPDATE entitlement_reminders SET kind = 'pre_expiry_t3' WHERE kind = 'pre_expiry_3d';
ALTER TABLE entitlement_reminders
    ADD CONSTRAINT entitlement_reminders_kind_check
    CHECK (kind IN ('pre_expiry_t3', 'pre_expiry_t2', 'pre_expiry_t1', 'expiry_day'));
ALTER TABLE entitlement_reminders DROP CONSTRAINT entitlement_reminders_pkey;
ALTER TABLE entitlement_reminders
    ADD CONSTRAINT entitlement_reminders_pkey
    PRIMARY KEY (entitlement_id, entitlement_revision, stage_end_at, kind);
