-- 0028_announcements: durable one-way account announcements and per-account read markers.
-- No delivery/reminder state here: Android notification attempts are never evidence of delivery.
CREATE TABLE announcements (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    environment text NOT NULL,
    scope_subject_ref uuid REFERENCES accounts(id) ON DELETE CASCADE,
    kind text NOT NULL DEFAULT 'general',
    text text NOT NULL CHECK (length(text) >= 1 AND length(text) <= 2000),
    action jsonb,
    show_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX announcements_environment_idx ON announcements (environment, created_at DESC);
CREATE TABLE announcement_reads (
    announcement_id uuid NOT NULL REFERENCES announcements(id) ON DELETE CASCADE,
    account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    read_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (announcement_id, account_id)
);
