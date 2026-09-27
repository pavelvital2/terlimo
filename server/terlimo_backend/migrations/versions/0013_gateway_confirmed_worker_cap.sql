-- 0013_gateway_confirmed_worker_cap: readback-confirmed node worker cap per gateway. Additive;
-- 0001-0012 are not rewritten. The catalog may advertise target_workers only when it equals
-- this confirmed readback value; unknown/0/mismatch means no verified admission. No guess and
-- no backfill (a value is only known after a confirmed apply).

ALTER TABLE gateways
    ADD COLUMN confirmed_max_workers integer;
