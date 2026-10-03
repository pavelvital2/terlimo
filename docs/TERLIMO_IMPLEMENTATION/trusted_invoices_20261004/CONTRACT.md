# Trusted account invoices — SOURCE only

Base `79256d8700f2b4499e29a73a1c868090c2cfc5c1`. Reuses accepted 0041/0042; no migration. No live activation or provider request was made. Account trial, Minishop caller checkout wiring, typed external delivery and owner transfer/cutover remain unfinished.

## Internal wire / caller obligations

Existing `POST /api/mobile/v1/internal/telegram/billing`, same constant-time `X-Telegram-Bot-Key`, server-controlled `telegram_backend`. Browser identity/account_ref/amount/discount/owner fields are rejected. Upstream bot/WebApp backend must verify Telegram identity before calling; this endpoint does not validate Telegram signatures itself.

- Create exact body: `{operation:"create",telegram_id:positive_int64,quote_id:canonical_UUID_string}`. Caller persists exact body + nonempty existing-format `Idempotency-Key` before first send. No generated key or automatic replacement Q/K after timeout.
- Status exact body: `{operation:"status",telegram_id:positive_int64,payment_id:canonical_UUID_string}`. No K needed; read-only, using existing persisted payment/reconcile model. It does not issue a provider request or create an invoice.
- Verified account mapping required, without installations/bindings/VPN/subscription requirement. Missing/nonverified → `REGISTRATION_REQUIRED` 409. No ensure or registration side effect. Foreign quote → neutral NOT_FOUND 404; foreign/unknown payment → PAYMENT_NOT_FOUND 404. Malformed/extra fields → BAD_MESSAGE 400.
- Plans/quote wire unchanged. `wire-fixtures.json` contains synthetic concrete examples generated through shared serializers, not production receipts.

Create/status reuse `_order_view` → `_payment_view` → `_with_product(contract2=True)`, preserving pricing, product, immutable credited_product, credit_state and referral_discount_state. Only internal addition is `provisioning_state`: `not_requested` until logical credit; `external_pending` after logical credit. This slice never returns delivered. Existing access_application_state remains its existing accounting/provisioning representation (`retryable_failure` for credited needs_grant), not proof of working VPN. Public mobile JSON is unchanged.

## Common create core

`TrustedPaymentOwner` is constructed by the trusted wrapper from verified mapping, never request JSON. `create_order(...,_trusted_owner=...)` authorizes original account Q/caller and creates explicit telegram_account order with NULL installation/bindings. Same existing Platega/provider call, order table, reservation, callbacks, reconcile and paid credit; no second billing engine.

1. Resolve original Q owner/caller, use frozen Q tariff/amount/currency/product/method; no current tariff repricing. Acquire bounded account preparation lock; recheck verified Telegram mapping and refresh Q terminal/expiry facts.
2. Global K replay validates explicit owner_kind/account/caller/Q before mutable gates. NULL installation is never a wildcard. Created invoice returns original URL/order; unknown/in_flight stays PAYMENT_PROVIDER_UNKNOWN, no second POST. Different owner/body or reused Q returns ORDER_CONFLICT.
3. New invoice transaction: account SHARE identity guard → existing account benefit barrier for every MAIN (ordinary or discounted) → recheck global K/source Q after wait → existing cross-channel unknown/reservation guards. Installation create uses unchanged binding/owner wrapper and common reserve_check. New account-owned unknown guard also covers addon, not just MAIN. History pending is retryable, never an undiscounted fallback or terminal no_order.
4. Original terminal Q reason is replayed; reserve/expiry no_order proof includes exact original Q/K and is committed **before** raising. Expired discount Q is validated using frozen gross/discount/payable proof. Unknown never becomes no_order. New-only gates/target/method/provider-create-enabled checks follow replay. Existing quote/product/period validation reused.
5. Commit order `in_flight` and matching referral reservation; release preparation lock before provider I/O. No trusted SQL/advisory transaction spans provider create. Fake-provider checks assert zero advisory locks, no active transaction and visible durable in_flight/reservation. Existing mobile core and public S5 wrapper retain their prior installation serialization (including concurrent same-K wait-for-result); it is not widened to account row locks.
6. Existing provider result finalization/callback/reconcile retained. Cancellation during provider wait leaves durable in_flight (honest unknown); error remains unknown. Caller must retain original intent. Status has no hidden provider-create retry. Failed/canceled finality and reservation release policy are unchanged.

New create never locks an existing order under account/benefit locks; callback remains order → account → benefit. Mapping is checked again before post-provider response authorization; a later revoked owner is denied access to the result while the immutable original invoice remains available for ordinary reconciliation. Public mobile callers cannot pass the private proof.

## Credit and remaining delivery

Callback/reconcile invokes accepted account credit: full period even when 200 RUB quote bills 100 RUB; exact paid replay; addon/renewal preserve selected target/slots. `needs_grant=true`, no fake binding, no grant/outbox or inviter reward from logical account credit. Status does not clear pending. Existing canceled invoice can still receive a matching CONFIRMED; this preserves existing late-confirmed accounting, without claiming provider finality.

Before activation: implement/review caller durable checkout intent using these fixtures, typed external target/application proof and subsequent exact reward trigger, account-only trial, legacy invoice/reward catchup/fence and owner transfer. Inline bot/site purchase flow must be preserved. Do not enable endpoint live as a complete checkout product from this SOURCE result.

## Evidence

`server/tests/test_trusted_invoices.py`: 12 distinct real temporary-PG/fake-provider/local-HTTP cases.

- initial.log: 4 PASS 2.54s — strict actual route/auth/identity/foreign, frozen create/replay/late callback, concurrent in_flight/unknown, durable expiry/reservation no_order.
- cross.log: 4 PASS / 4 deselected 2.71s — cross-channel ordinary-before/after discounted reservation (both directions); mobile real binding gate/replay/unchanged wire; addon/renewal/reconcile; history pending/global foreign K.
- barriers.log: 4 PASS / 7 deselected 2.70s — frozen replay repeated after adding currency-config coverage; provider cancellation; real PG global-K wait and identity revocation wait.
- owner-unknown.log: 4 PASS / 8 deselected 2.62s — new addon unknown guard plus its affected existing unknown/cross-channel cases. Repeats are tied to final guard change; not 16 distinct cases.
- finalize.log: 4 PASS / 8 deselected 2.54s after extracting the single provider finalizer while explicitly retaining legacy/mobile concurrent same-K lock behavior; covers mobile cross-channel, trusted unknown and frozen late-paid replay.
- `git diff --check` and Python import/compile checks PASS. No prior 0042/16-case suite, full payment/history/journal/updater suite rerun. No live merchant/production/phone/Minishop/TEST deployment.
