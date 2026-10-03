# Trusted Telegram referral adapter — SOURCE slice

POST `/api/mobile/v1/internal/telegram/referral` is registered in the existing API.
Authentication: existing `X-Telegram-Bot-Key`, constant-time comparison, reject empty
configured key, missing or wrong header with403 REGISTRATION_AUTH. This is a trusted
server-to-server boundary, not Telegram-ID authentication for a public browser.
The bot verifies update sender; WebApp backend verifies its current Telegram session
before forming the request. Neither query ref, pasted ID, account_ref nor arbitrary
body caller may establish identity. Body keys are closed; TelegramID is positive
integer (not bool/string). The existing shared credential identifies one principal
`telegram_backend` for both bot and site. Client cannot select a different principal.

Read body `{operation:"read",telegram_id:INTEGER}`. No idempotency key needed.
Server resolves existing verified accounts.telegram_id, then uses the same account
projection/initializer as mobile. No account/installation/binding is created, no
VPN grant or active subscription required. Missing verified mapping:
409 REGISTRATION_REQUIRED; incomplete history:503 REFERRAL_HISTORY_PENDING retryable.
No generated local fallback code. Success `{request_id,referral:...}` is the existing
canonical account DTO/terms/links; see wire-fixtures.json.

Attach body `{operation:"attach",telegram_id:INTEGER,code:"ASCII1to32"}` plus
`Idempotency-Key` header (existing8–128 printable ASCII convention). Caller persists
K+exact body BEFORE send and retries that operation; library/server never generates
an automatic replacement K. Success is `{request_id,attribution:{receipt_id,
account_ref,idempotency_key,state,reason}}`, with state attached|rejected. Attached
has null reason; rejected reason invalid|self|already_attributed|ineligible. No fake
candidate_id or registration_id. Rejection is a200 terminal receipt; it did not
mutate inviter. Do not confuse it with a503 transient/history error.

0040 stores immutable receipt at (account_id,telegram_backend,attach,K) with body
digest. After trusted identity verification, replay lookup precedes mutable code,
history and eligibility; sameK/different valid body yields409 IDEMPOTENCY_CONFLICT.
Transient failure rolls back, does not create a terminal rejection. Exact reply's
attribution object is stable; request_id is per HTTP request. Invalid code syntax
or malformed body gives existing400 BAD_MESSAGE; no new attribution. Receipt table
has no TTL/expiry. Down refuses used receipts.

Shared `attach_account` implements the existing mobile rules; `attach_candidate`
retains its original candidate/link authorization and full mobile receipt fields.
Both mobile registration and internal adapter acquire existing bind-account advisory
before account mutation; internal path takes no installation/candidate locks. Account
row serializes ownership; account update/FK happens before receipt insert. Initializer
keeps existing account→benefit order, and no provider/external I/O occurs inside the
transaction. Existing paid/trial/reward writers and their locks are unchanged.

Mobile read still checks context active binding AND DB active installation ownership
before initialization; missing installation cannot select the trusted projection.
Common projection prefers the canonical attached receipt over an older rejected
registration receipt after a later trusted attach. This preserves shape and reports
the actual immutable owner instead of a stale channel rejection.

## Existing deployed callers for the next slice (not modified here)

Read-only source basis: root's local protected deployed bundle
`/home/pavel/.local/state/referral-deployed-source-root-20261003/source`, actualimage
0ab363… per root provenance; no new production reads. Addressed only:
- bot/handlers/user/referral.py: referral_command_handler, referral_action_handler,
  _generate_webapp_referral_link (currently local ensure_referral_code).
- bot/handlers/user/start.py and start_flow.py: verified bot sender/ref capture.
- bot/app/web/webapp/auth_referral.py: _ensure_user_from_telegram,
  _apply_referral_to_existing_user, _resolve_referrer_id; verified session precedes
  this adapter. Do not preserve the local has_active_subscription exception→inactive
  fallback as canonical eligibility.
- bot/app/web/webapp/referral_links.py: existing visible links projection.

Next bot/site patch must persist K/body in their existing trusted backend context,
call this adapter, and replace local code/attribution writes with its receipts only
for the transferred scope. Existing email-only identity needs verified Telegram mapping
before this endpoint can authorize it; no mapping guessed from email/browser ID.
Canonical history coverage activation remains an explicit protected operation.

This slice does NOT route inline bot/site purchases to app or alter trial/checkout.
Their current functionality must remain. Before whole-program single-writer cutover,
the next compatible checkout/trial adapter and protected legacy writer transfer must
cover legacy pending invoices, applied reward targets and catchup. Leaving old award
writers competing is not completion. No production/single-writer/runtime PASS claimed.

## Checks

Only new adapter/shared-seam PG/API tests; no accepted history13+11, payment, journal
or updater suites rerun. HTTP tests use aiohttp local test server and disposable PG;
mobile/trusted competition uses two PG connections and the existing bind-account
serialization. No real bot/site calls, provider, device or product service used.

## A1/A2 correction

Account ownership locks in trusted attach, shared initializer and shared attachment
use `FOR NO KEY UPDATE`: same-account writers still serialize, while the reciprocal
inviter FK can acquire `KEY SHARE`. Account→benefit and binding advisory order remain
unchanged. Initializer assigns the unique referral code only when missing, separately
from ordinary account attribution updates; listing that unique column even with its
old value would upgrade the lock. Ready accounts with a code, including pre-epoch
legacy staging, are never re-imported over later canonical attribution. Trusted new-K
attach initializes only pending/missing history/code; exact-K replay remains first.

Real PG owner-row barriers cover trusted/trusted, mobile/trusted and mobile/mobile
initializer-before-attach. Ready reciprocal accounts both succeed; concurrently
pending inviter history remains transient with no durable rejection or attribution.
No reciprocal-referral prohibition, automatic retry or TTL has been added. Historical
code assignment/import still requires the existing protected coverage/import scope;
this correction does not claim a concurrent production history cutover.
