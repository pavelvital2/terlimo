# Root review corrections R2/R3/B1/B2

Continues exact accepted fixture commit dc8262877ebc20507442e9777a02a67386b26a05, retaining the original ba94ed1 server candidate and tests-only fixture history in cumulative SOURCE. No runtime/environment/live DB/rights/production/phone/provider actions.

## Implemented changes

- R2: ordinary ensure_grant updates the same active bonus-trial target monotonically to its owned, current nonrevoked grant's desired generation. Confirmation still requires exact target=desired=applied and valid readback. Commercial paid/imported entitlement prevents recording paid generation as trial proof. Foreign account/binding, revoked grant/binding/installation and stale readback do not award.
- R3: bounded sweep orders pending receipts by updated_at/id and rotates examination time for every inspected pending reward, including inactive WAITING. Partial pending-queue index is added to the unapplied SOURCE migration0038. No WAITING expiry, amount loss, new subscription, slots, daemon or cursor table.
- B1: every new MAIN invoice participates in account serialization, including ordinary quotes and legacy create without immutable owner. Existing invoice replay and global K/source Q/unknown retain priority. Account-wide pending/unknown queries include legacy NULL-owner invoices through the existing current binding, without rewriting their snapshots. Reserved discounted O1 blocks a competing MAIN; consumed O1 permits an ordinary new purchase. Existing referral_discount_reserved/no_order wire is reused for durably invalidated exact Q. Quote-less legacy rejection is retryable409 with no fabricated create_resolution.
- B2: consume obtains account KEY SHARE for immutable owner, prior credited owner and current binding credit account, then their inviter FK accounts before the first benefit write. Callback/reconcile/deferred use the same preparation. No new binding lock before paid-account (which would invert trial/reward order); product writers preserve binding account identity. Protected operator repoint/import remains a separate release procedure.

## Exact affected evidence: 13 distinct PASS

Rewards: 5 affected cases (3 new +2 directly affected refusals):

- test_same_trial_superseded_lease_refresh_keeps_exact_reward_target: real ordinary outbox/Unix readback, superseded generation1, successful same-trial generation2/lease1, later generation3/lease2, one7-day reward;
- test_trial_targets_refuse_foreign_and_revoked_proof: foreign account and revoked binding/installation with otherwise valid generation1 proof, then revoked generation2 refuses target update; no backward generation fixture;
- test_bounded_reward_sweep_rotates_inactive_waiting_without_losing_days: batch2, two old inactive WAITING7 and one later active; processed on sweep2, old states/days preserved, no inactive entitlements. EXPLAIN with local seqscan disabled verifies queue index;
- test_trial_requires_current_readback_not_intent_or_old_lease: only monotonic1→2 transition, stale readback deny then current readback once;
- test_later_paid_apply_does_not_prove_pending_trial: attempted paid-generation correlation leaves prior trial target and no reward.

Actual runs: 5 PASS8.78s after fixing one integer/bigint query parameter inference error; affected indexed fairness1 PASS2.66s; strengthened paid refusal1 PASS2.55s; final monotonic foreign/revoked fixture1 PASS5.27s. These are overlapping affected runs, not nine distinct tests.

Billing: 8 affected cases (6 new parametrized cases +2 directly affected precedence regressions):

- test_ordinary_main_cannot_bypass_discount_reservation[False/True]: second installation, ordinary quote issued after/before reserved O1; no second provider POST, exactQ tombstone, legacy quote-less create denial without false proof, O1 consumed then ordinary purchase allowed;
- test_paid_account_fk_precedes_benefit_with_history_barrier[callback/reconcile/deferred]: two real PG connections, history holds account FOR UPDATE; pg_blocking_pids proves paid waits before benefit. Valid staged initialize finishes then paid credits one entitlement/first-paid anchor/consume/reward, without forced rollback;
- test_legacy_ordinary_invoice_fences_later_discounted_create: preexisting NULL-owner ordinary invoice retains200; discounted create from second installation makes zero second provider calls;
- test_simultaneous_global_key_outranks_reservation_after_lock_wait;
- test_legacy_unknown_globalkey_sourcequote_outrank_noorder.

Actual final runs: ordinary2+precedence2 PASS6.24s; legacy-first1 PASS15.93s after correcting only fixture order_id key; paid barriers3 PASS18.05s. Initial5-case run also passed before legacy refinements. No full accepted32/49/11 fixture/runtime suite repeated.

Affected compile, scoped Ruff and git diff --check PASS. Raw check logs retained and hashed in package manifest; initial failed receipts are not represented as PASS.

## Unchanged release boundaries

Trusted new-user/legacy history bridge, bot/site single authoritative writer, compatible client payment codec and canceled-provider finality are release integration requirements. Current corrections do not authorize production import/cutover or automatic canceled release. Root reviews/publishes exact cumulative SOURCE separately from runtime application.
