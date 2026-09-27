-- 0026_entitlement_source_plan: immutable purchase/right plan snapshot on the entitlement.
-- The snapshot is written only at right issuance from the durable order/trial source; it is
-- never derived from current offers at read time, so later catalog edits cannot rename rights.
ALTER TABLE entitlements ADD COLUMN source_plan jsonb;
