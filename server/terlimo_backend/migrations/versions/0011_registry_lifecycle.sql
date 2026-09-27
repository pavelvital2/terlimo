-- 0011_registry_lifecycle: durable environment-level registry initialization marker. Additive;
-- 0001-0010 are not rewritten. The marker records that an authoritative registry lifecycle has
-- published this environment, so an empty registered set with no lifecycle tombstones can be a
-- known empty registry instead of an uninitialized/unknown environment. It is never derived
-- from subject revisions, error GETs or guessed state.

CREATE TABLE registry_environments (
    environment text PRIMARY KEY,
    initialized_at timestamptz NOT NULL DEFAULT now()
);

-- Backfill only from real registry rows already present in this database.
INSERT INTO registry_environments (environment, initialized_at)
SELECT environment, min(created_at)
FROM gateways
GROUP BY environment
ON CONFLICT (environment) DO NOTHING;
