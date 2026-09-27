-- 0012_session_binding_fence: durable binding identity + generation on account-bound sessions.
-- Additive; 0001-0011 are not rewritten.
--
-- The account link alone is not sufficient: a same-account delete+insert (new binding id) or a
-- generation bump on the same binding must never resurrect an old bearer. Existing linked rows
-- have no provable binding identity and therefore fail closed at authorization (no destructive
-- backfill is guessed here).

ALTER TABLE sessions
    ADD COLUMN binding_id uuid REFERENCES account_bindings (id) ON DELETE SET NULL,
    ADD COLUMN binding_generation integer;
