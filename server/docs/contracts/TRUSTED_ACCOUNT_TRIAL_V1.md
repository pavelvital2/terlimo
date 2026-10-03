# Trusted Telegram account trial — SOURCE only

Base: f93c3300943e2a626bccb4bf697f55b4aaeae85e. No live enable/deploy in this slice.

POST `/api/mobile/v1/internal/telegram/trial`, existing constant-time `X-Telegram-Bot-Key`.
Body exactly `{ "operation": "activate" | "status", "telegram_id": <positive int64> }`.
Booleans, strings, zero/out-of-range IDs and extra keys are rejected. No client account_ref, installation, eligibility, days or code. Server resolves an existing verified TG account; no ensure/registration/binding/hour creation.

Successful response: `{request_id,status:"ok",trial:{state:"none"|"active"|"used",starts_at,ends_at,replay,entitlement_id,revision,provisioning_state}}`; a new activation also has `account_state:"ACTIVE_TRIAL"`. Dates are RFC3339; missing trial has null dates/identity/provisioning and replay false. Actual existing entitlement UUID/revision identifies the logical source; `provisioning_state:"external_pending"` makes no VPN/grant/delivery/reward claim. Status is a read, not an activation permission or membership proof.

Explicit activate only: existing trial replay returns the same UUID/revision/interval without membership issuance, including expired/used. Otherwise shared once-only/imported_trial_used/current commercial checks, ready history, required official-channel membership, and final locked owner/history/entitlement recheck. Base7 days plus existing eligible referral bonus3=10 total. No new idempotency table/K/TTL/business rules. Imported used without local trial cannot create one. No referral attachment changes.

Error envelope uses existing API format: malformed BAD_MESSAGE400, invalid backend key REGISTRATION_AUTH403, absent/nonverified owner REGISTRATION_REQUIRED403, used history TRIAL_ALREADY_USED409, active paid/imported SUBSCRIPTION_ACTIVE409, missing/pending history SERVICE_UNAVAILABLE503 retryable, membership false CHANNEL_MEMBERSHIP_REQUIRED403, unavailable TRIAL_CHECK_UNAVAILABLE503 retryable. Status follows the same eligibility error projection when no local trial exists; it performs no external membership request, initialization or SQL mutation.

New trusted path: preliminary unlocked read → external membership outside SQL transaction → transaction verified owner FOR SHARE, paid-account advisory, trial advisory → final owner/history/entitlement checks → same insertion engine. Paid writes and mobile activation serialize on paid-account. Mobile wrapper retains binding lock/recheck, registration within_hour gates and exact existing public trial wire. Its existing membership placement is unchanged; this slice fixes no unrelated mobile I/O behavior.

No new route delivery effects: no grant/outbox/adapter call or reward enqueue from INSERT alone. Existing mobile provisioning/proof path remains untouched. Caller integration, typed external provisioning, actual membership network, live UI and deployment are NOT_RUN/separate gates.

T1 lock compatibility: account-owned paid credit already locks owner FOR SHARE before paid-account. Trusted trial takes the same owner SHARE first and retains it through final plain owner recheck/insertion. SHARE permits the existing mobile binding lock and entitlement FK KEY SHARE; it blocks initializer/attribution NO KEY UPDATE while held, without requesting their account advisory. Those initializers do not acquire paid-account/trial locks, so waiting on them introduces no reverse paid-account edge. No membership HTTP runs under SQL locks.
