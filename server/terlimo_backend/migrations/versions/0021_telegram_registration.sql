-- 0021_telegram_registration: S3-A one-time Telegram registration link bound to an installation.
-- Additive; no existing account/hour/VPN semantics are rewritten. Registration is not a trial and
-- creates no entitlement: it only records the confirmed installation -> Telegram identity link and
-- the server-computed trial eligibility at confirmation time.

CREATE TABLE registration_links (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_sha256    text NOT NULL UNIQUE,
    installation_id uuid NOT NULL REFERENCES installations (id) ON DELETE CASCADE,
    environment     text NOT NULL,
    status          text NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'confirmed', 'expired')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    confirmed_at    timestamptz,
    telegram_id     bigint,
    within_hour     boolean,
    trial_available boolean,
    trial_reason    text
);

-- At most one outstanding pending link per installation; a re-request supersedes the old one.
CREATE UNIQUE INDEX registration_links_pending_uniq
    ON registration_links (installation_id)
    WHERE status = 'pending';
