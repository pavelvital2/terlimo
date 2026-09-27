-- 0014_grant_proven_target: immutable proven target identity/route for applied grants. Additive;
-- 0001-0013 are not rewritten. The applied readback proves which node identity and which
-- management route actually confirmed the grant, so a later registry identity change cannot
-- silently retarget a revoke (and no secret is ever sent to a substituted endpoint). No
-- guessed backfill: legacy rows keep NULL and fail closed where a proven route is required.

ALTER TABLE grants
    ADD COLUMN target_node_id text,
    ADD COLUMN target_route jsonb;
