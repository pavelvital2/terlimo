# Trusted Telegram account ensure — SOURCE

POST `/api/mobile/v1/internal/telegram/account`
Header `X-Telegram-Bot-Key`: existing backend credential, shared constant-time auth
with referral adapter. Empty configured/supplied key and wrong key denied before
body/DB access. Server-controlled trust principal remains `telegram_backend`.
Upstream caller MUST verify bot sender / WebApp Telegram identity; this endpoint
itself does not validate a Telegram update/signature. Never expose credential or
accept a browser-provided Telegram ID as proof.

Exact body `{ "operation": "ensure", "telegram_id": 701 }`.
Telegram ID is integer1..9223372036854775807; bool/string/fraction/overflow and
additional caller/account_ref/status fields rejected with400 BAD_MESSAGE.
Success `{ "request_id": "...", "account": { "account_ref": "UUID",
"status": "verified", "history_state": "ready" | "history_pending" } }`.
No Idempotency-Key needed: unique Telegram mapping is this operation's identity.
No referral read/attach or candidate state is implicitly changed by the caller.
Initializer may set canonical code/import flags/attribution solely from existing
protected history, as ordinary initialization already does.

Existing nonverified mapping:409 REGISTRATION_REQUIRED, retryablefalse. No status
promotion/update-on-conflict. Current accounts schema permits unlinked/verified;
it has no account-level revoked enum. Every nonverified value is denied; revoked
installation/binding state is never touched or used as permission to resurrect it.
History incomplete/missing: successful ensure with history_pending, no fabricated
new-user proof or legacy code. Complete active coverage uses existing initializer
and membership. Existing ready legacy code/used flags/attribution remain unchanged.

Lock discipline: existing mapping pre-read → bind-account advisory → NO KEY UPDATE
account recheck → ordinary initializer. This is FK-compatible with reciprocal
attribution. No installation lock or mobile SessionContext. New mapping inserts
with ON CONFLICT DO NOTHING; only its own private new UUID is advisory-locked after
insertion, matching mobile's new-account pattern. If another insert wins, return
retryable409 REVISION_CONFLICT and roll back before acquiring its advisory. Caller
may explicitly repeat exact ensure; no automatic retry loop. Concurrent mobile
confirm's existing retryable insertion-conflict and same-token replay remain intact.
No changes to telegram_binding.py or its account-before-installation lock ordering.

No timestamp churn for repeat ready ensure, no new schema/dependency/daemon.
No installation, binding, hour, entitlement, award, payment or financial creation.
Referral benefit/history initialization is expected, not an access/reward grant.

Tests: only new test_trusted_account.py using actual aiohttp route, isolated PG,
real create_registration_link/confirm_registration. Two insert races paused after
absent-mapping lookup/before INSERT: ensure wins/mobile retries and reverse; also
ensure vs ensure. Explicit original-token retry yields oneUUID/one real binding.
Auth/strict inputs, active/incomplete/missing coverage, legacy flags/attribution,
nonverified denial, stable repeat and absence of ensure access side effects covered.
No accepted adapter/payment/history suites rerun. Runtime/caller bot/site integration
NOT_RUN; this endpoint is the account-only SOURCE prerequisite, not full cutover.
