# Actual SOURCE checks, 2026-10-03

All database tests used the existing pgserver temporary PostgreSQL UNIX socket and separate synthetic databases. Provider calls went to local fake objects; gateway admin checks used the existing local Unix test fixture. Current TEST/production runtime, environment, rights, DB and phone were not changed.

## Accepted affected checks

The integrated run produced **42 PASS**, including all **32 new referral tests**:

- identity: 11, including permanent code/history staging, expired owncode, candidate durable rejection/replay, keyed token/expired intent/full receipt, self/foreign/nooverwrite and active-right attribution eligibility;
- billing: 8, including immutable pricing/credit, two installations one discount, simultaneous global K precedence, no_order quote tombstone, unknown reserve, canceled late CONFIRMED, needs_review consumption and first-paid chronology, prior addon not MAIN;
- rewards: 8, including account-once trial10, historical deny, exact trial readback, WAITING without resurrection, periods/duplicates, imported reward receipts, ordinary outbox/Unix readback and unchanged slots;
- migration: 2, unused down/up and refusal to discard a durable definitive operation receipt;
- authenticated HTTP/native closed wire/service allowlist: 3, including ordinary registration{} compatibility, full correlated receipt and malformed original-K request remaining retryable.

The other 10 PASS covered existing trial activation and gateway publication/fencing. An additional current regression run produced **7 PASS**: registration-before-payment scenarios and create-control core replay/unknown/callback/reconciliation. Thus 49 distinct passing tests were observed. This is not a claim that the complete historical test suite is green.

## Historical fixture failures retained transparently

The integrated run also had 8 failures in selected legacy test_s4_payments HTTP tests. Their session is created before direct fixture binding; the existing purchase-binding guard returns ACCESS_DENIED403 before provider I/O. A clean worktree at exact base ba3934f reproduced test_success_grants_paid_once_and_duplicate_events_do_not_repeat with the identical403. The guard was not weakened.

The additional regression run had 3 failures in test_payment_create_control::test_false_new_s5_no_records_or_provider_session and test_s5_payment_compat's two tests. These fixtures request a purchase quote from an unbound session. All three also fail at the exact clean base with the same purchase guard. Their failed receipts remain in the package; they are not reported as PASS. The remaining seven tests in that run passed.

An earlier legacy registration check also found the baseline's absent step036_registration_link_envelope.json fixture; no unrelated fixture was invented. Actual authenticated ordinary registration wire is covered by the new HTTP test instead.

## Addressed defects and limits

Addressed before final integrated PASS: ordinary registration replay leaked optional proof; malformed JSON needed retryable=true rather than a false definitive original-K claim; global K/source Q needed recheck after advisory wait; actual first MAIN paid needs its own immutable anchor even when needs_review; trial/reward lock order needed paid-account before binding. Nested JSON fixtures were decoded through the existing JSON codec. No repeated accepted recovery/updater/VPN suites were run.

Affected modules compile; git diff --check and scoped new-module Ruff pass. Exact raw logs accompany the package and are hashed in its manifest.

Runtime apply, client payment codec/user checks, bot/site single writer cutover and real provider behavior remain NOT_RUN. Automatic release of provider-unproved canceled reservations is an explicit technical remainder; there is no invented TTL/refund/finality.
