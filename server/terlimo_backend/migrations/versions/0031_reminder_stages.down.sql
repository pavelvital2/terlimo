-- Reverting 0031 must not leave visible/queued orphans: hide the announcements of the removed
-- stages and cancel their still-queued (pending/processing) outbox intents. Already-delivered
-- (done/dead) intents are historical and are left untouched.
WITH removed AS (
    DELETE FROM entitlement_reminders WHERE kind NOT IN ('pre_expiry_t3')
    RETURNING announcement_id, entitlement_id, entitlement_revision, kind
), hidden AS (
    UPDATE announcements AS a
    SET show_until = now()
    FROM removed
    WHERE a.id = removed.announcement_id
      AND (a.show_until IS NULL OR a.show_until > now())
    RETURNING 1
), cancelled AS (
    DELETE FROM outbox_operations AS o
    USING removed
    WHERE o.idempotency_key = 'notify-reminder:' || removed.entitlement_id
                              || ':' || removed.entitlement_revision || ':' || removed.kind
      AND o.status IN ('pending', 'processing')
    RETURNING 1
)
SELECT count(*) FROM removed;
UPDATE entitlement_reminders SET kind = 'pre_expiry_3d' WHERE kind = 'pre_expiry_t3';
ALTER TABLE entitlement_reminders DROP CONSTRAINT entitlement_reminders_pkey;
ALTER TABLE entitlement_reminders
    ADD CONSTRAINT entitlement_reminders_pkey PRIMARY KEY (entitlement_id, entitlement_revision, kind);
ALTER TABLE entitlement_reminders DROP CONSTRAINT entitlement_reminders_kind_check;
ALTER TABLE entitlement_reminders
    ADD CONSTRAINT entitlement_reminders_kind_check CHECK (kind IN ('pre_expiry_3d'));
ALTER TABLE entitlement_reminders DROP COLUMN stage_end_at;
