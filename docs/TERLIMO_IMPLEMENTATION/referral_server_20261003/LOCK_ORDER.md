# Actual writer order and source evidence

| Writer | Locks in this implementation | Provider I/O |
| --- | --- | --- |
| Candidate | installation row → durable original operation receipt → immutable candidate insertion | None |
| Keyed/ordinary registration | existing bind-account advisory when known → installation → registration → candidate → account row → benefit | None; Telegram confirmation identity is supplied only by existing authenticated bot path |
| Protected history/reward staging | account → benefit / immutable reward receipt, sole operator under separately accepted cutover | None |
| All new MAIN orders (discounted or ordinary, including legacy create) | existing payment installation session lock → immutable owner or current legacy binding account KEY SHARE → referral-discount advisory → benefit row → quote/new order | Only after transaction commits order+reserve |
| Callback/reconcile matching paid | existing order row/update → immutable owner / current credit / prior credited account KEY SHARE, then their inviter FK accounts KEY SHARE → referral-discount advisory → benefit/first-paid anchor → existing paid-account advisory → entitlement/binding/grant | Lookup HTTP outside DB transaction; matching update and credit inside short transaction |
| Trial activation | paid-account advisory → binding row → existing trial advisory → trial entitlement | Existing membership check unchanged; no payment provider |
| Waiting reward | paid-account advisory → reward receipt → active entitlement → ordinary binding/grant rows | No new billing call; existing outbox delivers separately |
| Gateway readback publish | existing claim/gateway/grant fencing → exact trial target reward insert | Existing admin RPC precedes publication transaction |

The existing apply_paid_entitlement order→paid-account sequence is retained. All new MAIN provider writes pass account serialization, including a previously ordinary quote and legacy create without an immutable owner. Existing invoice replay remains earlier than this guard; quote-less legacy refusal is retryable and has no fabricated create_resolution. New-create reservation does not lock an existing order under benefit advisory; global K/source Q and unknown are checked before reserve and rechecked after an advisory wait. Original quote invalidation commits before emitted ApiError, so a later eligibility change cannot resurrect the same Q.

Account KEY SHARE before benefit prevents an implicit new-order FK lock from inverting registration/import account→benefit. Trial now uses the same paid-account serialization before binding, preventing binding→trial versus reward entitlement→binding inversion. Paid consume acquires explicit KEY SHARE locks before the benefit row, including invitation FK accounts used by later reward insertion. These locks replace the implicit late-FK acquisition identified by review; the payment path adds no binding row lock before paid-account and no installation lock. Existing binding owner is immutable in product writers; foreign active bindings are refused. Protected history/import retains account FOR UPDATE→benefit. Paid callback/reconcile/deferred versus valid history initialization is covered by an actual PostgreSQL blocking-pid barrier, rather than only scheduler concurrency. Protected multi-account legacy cutover remains a separately reviewed single operator procedure.

Actual checks: separate-connection two-installation reservation races, simultaneous foreign global K, existing global K/unknown replay tests, callback duplicate and late CONFIRMED, first needs_review anchor chronology, ordinary gateway outbox/Unix readback and readback mismatch/retry/revoke fencing. No long DB transaction spans provider create or get_status. Exact check names/results are recorded in CHECKS.md at package completion.

## Root correction evidence

R2 target generation advances only when ordinary ensure_grant proves the same active bonus trial/current owned grant; exact desired=target=applied readback remains mandatory. A paid or foreign/revoked grant cannot update its target. R3 uses updated_at/id as examination order and rotates every inspected pending receipt; a partial pending-queue index bounds selection. WAITING receipts and amounts persist. CORRECTION_CHECKS.md records precise affected checks. No external I/O moved into a transaction.
