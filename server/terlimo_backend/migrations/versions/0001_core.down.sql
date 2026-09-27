-- Revert 0001_core. Applied only by `terlimo-migrate down --version 0001_core`.

DROP TABLE IF EXISTS outbox_effect_log;
DROP TABLE IF EXISTS outbox_operations;
DROP TABLE IF EXISTS grants;
DROP TABLE IF EXISTS gateways;
DROP TABLE IF EXISTS entitlements;
DROP TABLE IF EXISTS account_bindings;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS installations;
DROP TABLE IF EXISTS accounts;
