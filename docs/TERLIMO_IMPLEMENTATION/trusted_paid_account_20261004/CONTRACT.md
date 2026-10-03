# Account-owned paid credit: SOURCE prerequisite

Base `2c44d36680ca4ca0c8d718688cbb8d8a28a97d11`. No HTTP route or public DTO added. No account invoice creator, provider call, delivery adapter, trial, Minishop change or cutover.

## Stored proof / caller boundary

Migration 0042 extends `payment_orders` with `owner_kind` (default `installation`), `trusted_owner_account_id`, `trusted_caller`. Account shape requires `telegram_account`, NULL installation/bindings, `telegram_backend`, non-NULL trusted account equal to immutable `checkout_owner_account_id`; credited `account_id` is either NULL or that same account. Existing global K, source-Q and provider identity uniqueness remain intact. Trigger freezes owner fields and account-order commercial snapshot. Down refuses **any** account order; it never deletes receipts. Historical rows/snapshots remain installation-owned unchanged.

`apply_paid_entitlement(connection, settings, order_id=...)` consumes a **persisted succeeded order**, not an HTTP-supplied account_ref. Test SQL is a synthetic trusted-create stand-in, not a production creation API. Future trusted create must authenticate `X-Telegram-Bot-Key`, resolve verified Telegram identity and authorize original account-owned Q before it may insert this shape. A foreign key is not identity authorization. Existing mobile create cannot select account ownership or borrow an installation; no new input field is exposed.

For account credit, source Q must have matching explicit owner/caller. Frozen product, pricing, method, amount/currency/months/tariff, plan identity/duration and duration snapshot must match original Q. It does not reprice or reject a previously paid invoice because its quote has since expired. Missing/foreign proof becomes `credit_review_reason` (`owner_changed`, `owner_not_verified`, `snapshot_conflict`), not owner_unbound. Account identity must still be verified at first credit. Exact applied replay returns original entitlement before mutable eligibility/target checks and does not revise the receipt.

## Shared application / locks

`payment_products.validate_account_credit_target` contains the shared owner/product/paid target/period/selected-slot checks. `validate_credit_target` remains the mobile binding wrapper. Existing subscription calendar period, addon, slot renewal, source plan, revision and credited_product implementation is shared unchanged. New account credit does not fabricate bindings, installations or grants.

Existing order-row → account/FK → benefit → paid-account advisory → entitlement sequence is retained. `paid_account_locks` takes SHARE (instead of KEY SHARE) on account-kind owner to stabilize verified status **before** benefit acquisition, including callback/reconcile consume-before-credit. Mobile locks unchanged. Inviter FK locks remain KEY SHARE. There is no external I/O within credit transaction and no additional advisory acquired after installation lock.

Referral consume still records original first MAIN and consumes the matching reserved discount; full service period is independent of discounted payable amount. This is accounting of succeeded payment, not delivery evidence.

## Pending delivery is explicit, not success

After account-only logical credit, `applied_entitlement_id` / `credited_product` exist, `binding_id` remains NULL, and `needs_grant=true`. This existing flag accurately means provisioning unresolved; `owner_kind` disambiguates external from installation grant work. `_enqueue_paid_grant` immediately returns false for account orders. Mobile reconciliation excludes already-applied external-pending rows so they do not monopolize its bounded batch. Unapplied succeeded orders retain ordinary reconciliation behavior.

No inviter reward is enqueued for account-only logical credit, including callback/replay/addon/renewal. Existing mobile enqueue behavior is unchanged. No typed external receipt exists yet, so no code in this slice clears this pending flag or claims delivery. A future external-delivery implementation must persist exact target, immutable source order/entitlement/revision and actual application proof atomically before resolving pending and enqueuing the original first-MAIN reward. Do not use NULL binding or mere logical credit as proof. Account-only create/status must present provisioning honestly; they are not implemented here.

## Actual validation

`server/tests/test_paid_account_core.py`: 16 distinct cases, real disposable PostgreSQL; synthetic account order built from actual trusted quote operation, local callback only. No live provider/runtime.

- 12 PASS, 5.31s: discounted 200→100 full 30 days; concurrent same-order apply and replay unchanged; consume/reservation/receipt; zero installation/binding/grant/outbox/reward; foreign product/account-Q, nonverified owner, amount/duration mismatch; addon/renewal target+slot; mobile binding/receipt/reward and revoked-binding rejection; immutable/closed shapes, global K/provider uniqueness and down refusal; pre-0042 row exact preservation/up+down; real PG lock barrier against status mutation/second order; pending external excluded from mobile reconcile.
- 4 PASS, 12 deselected, 2.50s: full 3/6 calendar months; addon quoted before renewal fails target_period_changed without slot; callback claim→consume→credit and duplicate replay with external pending/no reward.
- First run retained: 8 PASS / 1 FAIL due test fixture `until:current` rather than required exact `until:<timestamp>`. Corrected fixture; no relaxation of product validation.
- `git diff --check` PASS. No accepted unrelated suites rerun. Runtime/provider/phone/external delivery/create/status/trial NOT_RUN / NOT_IMPLEMENTED.
