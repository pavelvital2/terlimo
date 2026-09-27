-- 0015_gateway_vk_hashes: nullable node-level vk hash snapshot per gateway. Additive; 0001-0014
-- are not rewritten. Absence (NULL) means "not confirmed yet / old node without the field";
-- a confirmed empty snapshot is stored as an empty JSON array (clears old hashes).

ALTER TABLE gateways
    ADD COLUMN vk_hashes jsonb;
