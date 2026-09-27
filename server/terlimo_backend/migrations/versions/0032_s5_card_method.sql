-- 0032_s5_card_method: add the app's public "card" method to the immutable S5 quote snapshot.
-- Additive to 0024 (never rewritten). "card" is a public ALIAS of the Platega provider method
-- "international"; the concrete paymentMethod id is never hardcoded (it comes from settings and
-- may be overridden at runtime), only the public quote/order snapshot value is extended.
ALTER TABLE s5_payment_quotes DROP CONSTRAINT s5_payment_quotes_method_check;
ALTER TABLE s5_payment_quotes
    ADD CONSTRAINT s5_payment_quotes_method_check CHECK (method IN ('sbp', 'card', 'crypto'));
