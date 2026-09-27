-- 0023_payment_orders down: drop payment event/order storage (no paid entitlement is touched;
-- entitlement writes are the normal entitlement path and are not rolled back here).
DROP INDEX IF EXISTS payment_events_order_idx;
DROP INDEX IF EXISTS payment_events_provider_event_uniq;
DROP TABLE IF EXISTS payment_events;
DROP INDEX IF EXISTS payment_orders_needs_grant_idx;
DROP INDEX IF EXISTS payment_orders_parked_idx;
DROP INDEX IF EXISTS payment_orders_installation_idx;
DROP INDEX IF EXISTS payment_orders_provider_payment_uniq;
DROP TABLE IF EXISTS payment_orders;
