-- 0029_usage_pipeline: account usage accounting (per-grant user-data counters on one layer).
-- Cursors dedupe node samples by (gateway, boot, grant identity); ticks carry non-negative
-- deltas only; coverage records the first trusted sample per installation and gaps.

CREATE TABLE usage_cursors (
    gateway_id      uuid NOT NULL REFERENCES gateways (id) ON DELETE CASCADE,
    registration_id text NOT NULL,
    boot_id         text NOT NULL,
    boot_started_at bigint NOT NULL CHECK (boot_started_at >= 0),
    rx_bytes        bigint NOT NULL CHECK (rx_bytes >= 0),
    tx_bytes        bigint NOT NULL CHECK (tx_bytes >= 0),
    sequence        bigint NOT NULL DEFAULT 0,
    closed          boolean NOT NULL DEFAULT false,
    installation_id uuid REFERENCES installations (id) ON DELETE SET NULL,
    account_id      uuid,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (gateway_id, registration_id)
);

CREATE TABLE usage_ticks (
    id              bigserial PRIMARY KEY,
    gateway_id      uuid NOT NULL REFERENCES gateways (id) ON DELETE CASCADE,
    boot_id         text NOT NULL,
    registration_id text NOT NULL,
    installation_id uuid REFERENCES installations (id) ON DELETE SET NULL,
    account_id      uuid,
    observed_at     timestamptz NOT NULL,
    rx_delta        bigint NOT NULL CHECK (rx_delta >= 0),
    tx_delta        bigint NOT NULL CHECK (tx_delta >= 0),
    UNIQUE (gateway_id, registration_id, observed_at)
);
CREATE INDEX usage_ticks_installation_idx ON usage_ticks (installation_id, observed_at);

CREATE TABLE usage_coverage (
    installation_id uuid PRIMARY KEY REFERENCES installations (id) ON DELETE CASCADE,
    segment_start   timestamptz,
    last_gap_at     timestamptz,
    last_trusted_at timestamptz,
    updated_at      timestamptz NOT NULL DEFAULT now()
);
