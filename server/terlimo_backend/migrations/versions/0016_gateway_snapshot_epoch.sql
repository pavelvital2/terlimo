-- 0016_gateway_snapshot_epoch: monotonic per-gateway epoch for coalesced profile snapshot
-- operations. Additive; 0001-0015 are not rewritten. The epoch fences stale claim owners:
-- only an operation whose target epoch equals the current gateway epoch may write.

ALTER TABLE gateways
    ADD COLUMN snapshot_epoch bigint NOT NULL DEFAULT 0;
