# B3 expired discounted quote selection proof

**Current validation: complete, 20 distinct targeted HTTP cases PASS.** The STOP section below is retained historical evidence and is superseded by the final check.

Base c8ea0793e340dc57744cc95a906155d10783c955. SOURCE correction only in s5_payments._quote_selection_proven. Product gross MAIN+extras is compared to pricing.base, while frozen pricing.payable is compared to quote.amount. Pricing must have exactly the published fields, true integer amounts (not bool/string/float), fixed10000minor MAIN discount, MAIN base>10000, nonnegative extras, matching RUB currency, referral_first_main kind and accepted terms. Product plan/month/duration checks remain. Historical no-pricing proof retains its prior gross arithmetic. No expiry wire, auth/owner/globalK/sourceQ/unknown fence changed.

## Actual targeted checks

19 new actual HTTP cases passed using temporary PostgreSQL and local fake provider:

- expired discounted MAIN, exact existing expired_quote_no_order and repeated original Q/K;
- same proof for stored MAIN200+extras50=base250, discount100, payable150; the extras case is an explicitly synthetic frozen selection arithmetic fixture, not a claim that existing paid renewal became first-payment eligible;
- created immutable invoice replay after quote expiry, same payment/pricing and no second provider call; source-Q conflict remains earlier than expiry;
- twelve malformed pricing/product cases: base/discount/payable/currency/kind/terms, boolean/float/string amounts, MAIN base<=10000 even with extras, addon and unknown field;
- foreign owner refusal;
- additional malformed JSON/empty pricing/product-number loop;
- legacy no-pricing10/20RUB proof unchanged.

First run16 PASS/1 FAILED; extra run3 PASS/1 FAILED. Unknown test then had a third failed run; do not count it as PASS.

## STOP and exact remaining check

1. Initial unknown fixture raised bare TimeoutError. Payment core persists unknown then reraises that non-ApiError; aiohttp emitted504. The fixture expected503. This was fixture contract misuse, not a B3 source correction requirement.
2. Attempted fixture edit used absent python executable, so the second execution retained that same error. Evidence retained rather than hidden.
3. Corrected provider fixture uses ProviderUnknown and exact initial503. Initial and same-K expired replay503 passed. New K on an already occupied source Q correctly returned409 ORDER_CONFLICT: existing source-Q priority precedes installation unknown. The test incorrectly expected503 for both branches.

Final fixture assertion is corrected to same-K503 PAYMENT_PROVIDER_UNKNOWN and new-K409 ORDER_CONFLICT/quote_already_used, with both denying false expiry/create_resolution and requiring one provider call. It is NOT_RERUN: three unsuccessful runs reached the root STOP rule. No fourth execution, accepted suite repeat or product guard change was used. Root decision is needed to authorize only this remaining test. The observed third response and existing source trace support unchanged priority, but are not represented as a complete passing test.

Affected compile and git diff --check pass. Raw logs and hashes are retained in the package. No real provider/liveDB/TESTruntime/production/phone actions. B1/B2 review checkout and prior candidate remain untouched. Root is sole publisher; canceled finality and bot/site trusted history bridge remain release integration requirements.

## Final check and owner-rule correction

Under whitelist-20261003-referral-server-b3-last-check-v1, only test_unknown_invoice_outranks_discount_expiry_proof was run on exact07eaf20cb331c5afe24033c9bdd32f9a8e5ba4c3: **1 PASS in4.87s**. The previous19 passing cases were not repeated. Initial controlled503, same-K503 PAYMENT_PROVIDER_UNKNOWN, new-K409 ORDER_CONFLICT/quote_already_used, no false expiry/create_resolution and exactly one fake provider call all passed. Product source was unchanged.

The earlier unconditional three-attempt STOP interpretation was incorrect. Root relayed Pavel's22.09 clarification: stop after three only while the cause remains unclear and the team does not know the next step; a known cause and justified fix continue inside authorized scope without another approval. This supersedes the historical pending-check/GO requirement above; the earlier failed receipts remain unchanged.

Evidence: expiry-http-last-check.txt is hashed in the final result manifest. This follow-up changes documentation only and supplies a tiny delta from07eaf20; no cumulative rebuild or accepted suite/runtime check. Runtime, production, phone and real provider remain untouched. Root retains publication authority and separate release integration gates.
