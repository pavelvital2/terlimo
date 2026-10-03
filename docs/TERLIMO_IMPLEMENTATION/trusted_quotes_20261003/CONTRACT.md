# Trusted account plans/quote — SOURCE prerequisite

POST `/api/mobile/v1/internal/telegram/billing`, existing backend-only
X-Telegram-Bot-Key, same require_trusted_backend and telegram_backend principal.
Upstream bot/WebApp verifies Telegram identity; body identity is not browser auth.
Existing verified accounts.telegram_id mapping required; missing/nonverified409
REGISTRATION_REQUIRED. No implicit ensure, installation, binding or history import.

Closed requests:
- `{operation:"plans",telegram_id:801}` without K.
- `{operation:"quote",telegram_id:801,plan_id:"terlimo-30d",duration_code:"days:30",method:"sbp"}`
  plus caller-persisted Idempotency-Key. Optional renew_extra_slot_ids:string[] follows
  existing contract2 selection checks (UUID, unique, actual owner slots).
Positive int64 only; no bool/amount/discount/account_ref/binding/caller. Only these
operations supported; no create/status/trial/delivery endpoint.

Responses reuse existing payment envelope (request_id/server_time/schema_version/
status), plans + plans_revision, or quote_id/amount/duration_code/device_limit/
method/expires_at/product and optional pricing. Product hides owner_account_id and
extra_price_basis as existing public_product does; persisted product retains them.
Fixed trusted contract is existing contract2. Plans show configured base offer and
account product selection; authoritative referral reduction is frozen at quote,
not advertised as a second price table. Buyer settings, MAIN1/3/6, addon/prorata,
renew selections, full30day/calendar duration and referral pricing all shared.

Shared extraction in s5_payments.py: quote_body_digest, account_quote_snapshot,
account_plan_products. Mobile auth/binding/replay/contract-version gating stays in
its wrapper; serializer and digest remain identical. No mutation to old snapshots.

0041 extends existing quotes with owner_kind (installation default),
trusted_owner_account_id FK(accounts), trusted_caller. There was no account column
on quotes; existing product.owner_account_id remains the commercial owner snapshot,
not an indexed authorization key. New explicit columns identify replay scope.
CHECK: installation iff nonnull installation and null trusted fields; telegram_account
iff null installation + nonnull account/caller='telegram_backend'. No NULL wildcard.
Verified status checked/rechecked by authenticated API under row lock; FK guarantees
account existence, does not claim SQL FK enforces mutable account status. Existing
installation/K uniqueness retained; new partial unique(account,caller,K). Down raises
if ANY account quote exists; never discards durable receipts. Orders schema untouched.

Lock/replay: derive verified UUID → existing bind-account advisory → account
NO KEY UPDATE recheck (FK-compatible, no installation locks) → exactowner/K lookup.
Samebody returns original snapshot before checking current history/settings, including
original expired timestamp; it neither renews Q nor promises future create acceptance.
Differentbody409 IDEMPOTENCY_CONFLICT. Digest is unchanged mobile selection-body JSON
sortkeys/separators policy; trusted operation/TG are validated and represented by the
owner scope. Different verified account may use same K only for its own distinctQ.

New quote/plans require ready history or503 REFERRAL_HISTORY_PENDING retryable;
no implicit undiscounted fallback on incomplete history. Historical paid/no inviter
legitimately use ordinary price through shared eligibility; eligible too-small MAIN
raises existing REFERRAL_PRICE_UNSUPPORTED. Quote calculation does not reserve,
consume, createorders, callprovider or awardaccess. No provider/network insideSQLtx.
No new TTL: existing15minute lifetime, addon capped by targetend. Concurrent sameK
serialized by account advisory and backed by partial unique index. Future create
must implement account ownership + globalorderK/sourceQ/unknown/reservation/credit;
current mobile create cannot consume these NULLinstallation quotes.

## Checks / limits

New targeted file:10PASS4.32s +3additional selection/calendar cases PASS2.08s,
13 distinct cases, disposablePG/localHTTP. Zero-install account API200→100,
full1/3/6 duration, noeligible/importedpaid/history/lowprice errors, auth/closedshape,
replay after settings/readiness change, foreignowner/concurrentK, invalid DBshapes,
down refusal, addon/selectedrenewal/noreservation. Mobile contract1/2 same quote
fields/replay/selection and real DB binding gate before replay checked. Session
context supplied by test stub; no claim of running full mobile auth suite.

Initial attempt to import old test_s5_payment_compat fixture skipped because accepted
external pop_canonical contract module/TERLIMO_CONTRACT_DIR is absent. No tests ran
in that attempt; no dependencies fetched. New tests avoid that fixture and directly
register production routes with localDB. Accepted payment/history/updater suites
not rerun. Productruntime/provider/phone/Minishopcaller NOT_RUN; root publishes.
