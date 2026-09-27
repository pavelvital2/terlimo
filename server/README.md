# TERLIMO unified Backend (STEP 03.1 runtime)

Minimal, runnable TEST runtime for the single TERLIMO Backend product:

- one PostgreSQL database and one data model for installation/session/catalog/gateway-control;
- versioned, repeatable and reversible SQL migrations;
- HTTP API with `/health/live`, `/health/ready` (readiness checks the real database and migrations);
- mobile v1 installation/session endpoints (`/api/mobile/v1/auth/challenge`, `/installations`,
  `/auth/session`) implementing the accepted C01 PoP contract (WLBS-POP-1);
- durable outbox worker on PostgreSQL (`FOR UPDATE SKIP LOCKED`) with a per-claim token
  and bounded lease: completion/failure finalizations are fenced by the token, a live slow
  handler renews its lease, a lost owner can never overwrite a newer result, and unknown
  operation types are terminal failures. Delivery is at-least-once; the internal self_check
  effect is idempotent by its unique key, while future external RPCs (03.3) must use a stable
  idempotency key/generation and readback - no universal exactly-once is promised.

Scope boundary: STEP 03.1 only. Registration/session (03.2), gateway control (03.3),
Android integration (03.4) and the vertical check (03.5) are **not** implemented here, and
no placeholder business API pretends to succeed. Gateway is a technical component; there is
no second shop or second commercial database.

## What is reused vs new

Reused concepts/code from the pinned donor MiniShop `cb8caf41`
(worktree `/home/pavel/projects/worktrees/minishop-cb8caf41`):

- `schema_migrations` tracking table and apply/skip chain-runner semantics
  (`backend/db/migrator/engine.py`) - adapted to asyncpg and extended with per-version
  checksums and a reversible `down` step;
- aiohttp `Application`/`AppRunner`/`TCPSite` lifecycle and the `/health` endpoint idea
  (`backend/bot/app/web/web_server.py`) - reduced to the technical endpoints;
- redacted DSN logging (`backend/db/database_setup.py: redacted_database_url`) - reimplemented
  with `urllib.parse` instead of SQLAlchemy.

New in this repository: the unified model migration, asyncpg access layer, configuration,
readiness semantics, durable outbox worker with the self-check operation, the CLI tools,
tests and the smoke script.

## Requirements

- Python 3.12+
- PostgreSQL 16 (for tests/smoke the bundled `pgserver` binaries are used; no system service
  and no host port are required)

## Install

```bash
cd /home/pavel/projects/terlimo-backend
python3 -m venv .venv
.venv/bin/pip install -e '.[dev]'
```

## Configuration (no secrets in the repository)

Copy `.env.example` and set values in the environment; every process reads env vars:

| Variable | Purpose | TEST default |
|---|---|---|
| `DATABASE_URL` | required PostgreSQL DSN; runtime never falls back to another DB | - |
| `TERLIMO_ENV` | environment tag | `test` |
| `API_HOST` / `API_PORT` | API listener | `127.0.0.1` / `18081` |
| `DB_POOL_MIN` / `DB_POOL_MAX` | pool bounds | `1` / `5` |
| `DB_COMMAND_TIMEOUT_SECONDS` | PostgreSQL connect/command timeout | `15` |
| `DB_RECONNECT_COOLDOWN_SECONDS` | throttle for bounded readiness reconnects (same DSN) | `1.0` |
| `WORKER_POLL_INTERVAL_SECONDS` | idle poll interval | `1.0` |
| `WORKER_LOCK_TIMEOUT_SECONDS` | stale `processing` reclaim timeout | `30` |
| `WORKER_MAX_ATTEMPTS` | attempts before `dead` | `8` |
| `CHALLENGE_TTL_SECONDS` | single-use PoP challenge lifetime (provisional, S1-B02 pending) | `300` |
| `SESSION_TTL_SECONDS` | installation session lifetime (provisional) | `86400` |
| `PROOF_SKEW_SECONDS` | accepted `ts` window (provisional, S1-B02 pending) | `300` |
| `CHALLENGE_RATE_LIMIT_PER_MINUTE` | challenge issuances per installation fingerprint | `20` |
| `PUBLIC_CHALLENGE_LIMIT_PER_MINUTE` | atomic global public challenge cap (shared PostgreSQL) | `20` |
| `CHALLENGE_RETENTION_SECONDS` | expired/used challenges keep their lookup semantics for this long | `86400` |
| `RECEIPT_RESULT_TTL_SECONDS` | raw stored response (bearer) lifetime; never beyond session validity | `3600` |
| `IDEMPOTENCY_WINDOW_SECONDS` | receipt tombstone window after the raw result is purged | `604800` |
| `CLEANUP_INTERVAL_SECONDS` | maintenance sweep interval (API process) | `300` |
| `CLEANUP_BATCH_SIZE` | max rows per sweep step | `500` |
| `GATEWAY_ADMIN_MAIN_PASSWORD` | environment secret for the local gateway admin socket (never stored in the DB) | - |
| `GATEWAY_ADMIN_TIMEOUT_SECONDS` | admin socket RPC timeout | `10` |
| `GATEWAY_MAX_LEASE_SECONDS` | finite technical grant ceiling (provisional TEST; CONTROL_BOUND_PROFILE PENDING) | `900` |
| `GATEWAY_LOCAL_ADMIN_ENABLED` | explicit TEST opt-in for the local admin socket transport (default off; non-TEST always refused) | `false` |
| `GATEWAY_MANAGEMENT_CA_FILE` | CA that signs the gateway management server certificate | - |
| `GATEWAY_MANAGEMENT_CERT_FILE` | Backend mTLS client certificate | - |
| `GATEWAY_MANAGEMENT_KEY_FILE` | Backend mTLS client key | - |

## Run (isolated TEST PostgreSQL)

```bash
# terminal 1: isolated PostgreSQL with bundled binaries
.venv/bin/python tools/dev_postgres.py --pgdata ./.test-pg
# prints DATABASE_URL=postgresql://postgres:@/postgres?host=...

# terminal 2 (with DATABASE_URL from above)
.venv/bin/terlimo-migrate up
.venv/bin/terlimo-migrate status
.venv/bin/terlimo-api           # http://127.0.0.1:18081/health/live, /health/ready
.venv/bin/terlimo-worker        # durable outbox loop (SIGTERM stops gracefully)
```

Migrations are repeatable (`up` twice applies once), reversible
(`terlimo-migrate down --version <version>`, invoke versions in reverse order) and
drift-protected by checksums. Readiness recovers in-process: if PostgreSQL is down it
reports 503 `database_unavailable` and retries the same DSN without a restart. The reconnect
cooldown only throttles how often a probe attempts to create a pool; it is not an upper bound
on a readiness request. A probe that holds the pool lock is bounded by
`DB_COMMAND_TIMEOUT_SECONDS` (default 15 s connect timeout), while concurrent probes wait up to
the cooldown and otherwise report not ready.

Diagnostics:

```bash
.venv/bin/terlimo-outbox enqueue --type self_check --idempotency-key demo-1
.venv/bin/terlimo-outbox status
```

`self_check` is an internal durable-worker self-check (effect in `outbox_effect_log`), not a
business API; real gateway-control operations arrive in 03.3.

## Mobile auth (step 03.2)

Accepted contract input: `terlimo-s1-contracts @ 336bd6dae9e9340874d6e128a360fe6cfe2f26e6`
(`auth/POP_PAYLOAD_V1.md`, `auth/pop_canonical.py`, `vectors/auth_vectors.json`). The verifier is
a port in `terlimo_backend/pop.py` (provenance in the module docstring) and is cross-checked
against the contract module and its vectors by `tests/test_pop_contract.py`.

- `POST /api/mobile/v1/auth/challenge` - single-use challenge bound to
  `(installation_fingerprint, purpose, environment)`; returns `challenge_id`, `nonce_b64`,
  `expires_at`, `single_use`. Purposes `enrollment` and `session` are enabled; the remaining
  enum purposes arrive with their routes (03.3+).
- `POST /api/mobile/v1/installations` - `EnrollmentRequest` (key material + proof). Creates only
  a technical installation and an `enrollment`-scoped session; `entitlement_created` is always
  false. Repeated/concurrent enrollment with the same key+environment returns the same
  installation (fingerprint = SHA-256(SPKI DER)).
- `POST /api/mobile/v1/auth/session` - `SessionRequest` (proof only). The key is resolved from
  the stored installation; requested scopes are bounded by the session scope enum.

Idempotency follows the accepted model: the stable business digest excludes
`request_id`/`ts`/`nonce`/signature, so a lost-response retry with a fresh challenge/proof
returns the stored result with one effect; changed business data under the same key returns
`409 IDEMPOTENCY_CONFLICT`. The `Idempotency-Key` header is not required on these routes
(contract test classification); when present it must equal the signed key. The signed
`idempotency_key` is the storage key for `/auth/session`; enrollment may optionally carry a
signed one.

Session secrets and retention: `session_id` is the opaque bearer. The `sessions` table stores
only its SHA-256; the idempotency receipt table keeps the exact stored response so a
lost-response retry can return the same bearer, but only within a bounded window:
`RECEIPT_RESULT_TTL_SECONDS` (and never beyond the session's own `expires_at`) after which the
raw result is purged; the non-secret digest tombstone stays for `IDEMPOTENCY_WINDOW_SECONDS`
and then the receipt row is removed (an explicit, documented policy: after that window the key
is no longer retained). The 7-day tombstone window applies to these installation/session
receipts only; it must NOT be reused later to expire payment, credit or other commercial
deduplication records. Physical purge happens on the next maintenance sweep while the API is
running; the on-read validity checks below are the actual guarantee and apply independently
of sweep timing.

Account access receipts (`access/sync`, migration `0010`) are the one exception and are
documented as this TEST slice's technical dedupe policy, not a commercial term: they keep only
the account/installation/operation identity, the business digest and the operation reference
(never a bearer or a raw auth result), are written with `result_expires_at = NULL` and
`retain_until = NULL`, and are therefore never purged or deleted by the retention sweep. They
remain until an explicit account lifecycle/archival policy removes them. The 7-day window is
never applied to them, and an expired session cannot be resurrected through a replay: every
retry is re-authorized against the live session/account/entitlement/binding state before the
receipt is even considered. The catalog revision a sync is admitted against is a persisted,
monotonic subject revision over the material registry and visible grant state
(`catalog_revisions`); it never changes because of time or repeated GETs. `CATALOG_VALIDITY_SECONDS`
(600 by default) is only a provisional TEST cache upper bound for a catalog response; every
response is additionally clipped to the applicable entitlement/grant deadlines, and it is never
a right, a lease or an access extension.

Before any stored result is returned, the server re-validates the live subject: installation
state/environment, the session row's expiry/revocation, its binding to the installation and
the stored generation. A revoked or expired bearer is never returned as a live success. A
legacy receipt without an environment (`environment IS NULL`) that is still inside its
`retain_until` window never resolves to a bearer and never permits a new effect: any request
with that key is refused with `409 IDEMPOTENCY_CONFLICT` /
`legacy_receipt_environment_unknown` (the environment is never guessed); once the explicitly
verified window has passed, the documented post-window policy applies. No session token,
signature or signed payload is logged.

Rate limits and cleanup: every challenge issuance is checked against a per-fingerprint limit
and an atomic global cap stored in the shared PostgreSQL (`public_endpoint_counters`), so it
holds across concurrent API processes without Redis. `RATE_LIMITED` (429) carries
`retry_after_ms` and the `Retry-After` header. The maintenance sweep in the API process
(bounded batches) purges expired raw results, removes receipts past their tombstone window,
deletes expired challenges after `CHALLENGE_RETENTION_SECONDS` and old rate counters.

Rollback procedure: the `0004` down script is fail-closed. If any environment-aware receipt
exists it raises `ROLLBACK_UNSAFE_0004` and the runner's transaction leaves receipts, indexes
and `schema_migrations` untouched; it never deletes idempotency data to satisfy the legacy
unique shape. Environment-aware receipts must be resolved/exported explicitly by the operator
before the schema rollback; with only legacy receipts the rollback succeeds and preserves
them. This changes only the rollback procedure, not the applied up-migration checksum.

Scope gate for 03.3: an enum-valid scope is not by itself an authorization. Step 03.2 issues
installation-only (`account_ref=null`) sessions and therefore grants only
`enrollment`, `session:read`, `session:write`, `management-only`; account/payment/data scopes
return `403 ACCESS_DENIED`. Future business routes must enforce account state, ownership and
revocation (per the accepted per-route `x-session-auth`) before any business action or stored
result.

## Gateway control (step 03.3)

Reused wire: the existing WDTT management surface (`admin.sock`, `args: ["client-test", <json>]`)
with the reviewed commands `grant_provision`, `refresh_lease`, `grant_revoke`, `grant_get`,
`engine_status` (`client_test_admin.go`). No new gateway API is invented. The gateway receives
only technical fields: opaque grant id, registration/fingerprint, node id, the installation
PoP public key, generation, lease sequence and a finite `not_after`; no account, Telegram,
payment, trial or tariff data.

Ownership: `ensure_grant` requires `entitlement.account_id = binding.account_id` (authoritative
join) and `installation.environment = gateway.environment`; mismatches return non-granting
outcomes (`ownership_mismatch`, `environment_mismatch`) and write **no** grant and no outbox
operation. Callers that pass a raw asyncpg connection must install the pool's JSON/JSONB codec
(the `Database` pool does this automatically) before calling the service with JSONB arguments.

Chain: `ensure_grant` / `revoke_binding_grants` write the desired state and a durable outbox
operation (`gateway.apply_grant` / `gateway.revoke_grant`) **atomically** in one transaction.
The worker claims the operation, re-checks the generation fence outside any DB transaction,
performs the RPC, reads the actual gateway state back (`grant_get`) and only then marks the
grant `applied` with the readback. RPCs never run inside a database transaction or row lock.

Remote transport (§5.2): when a gateway registry entry carries
`endpoints.management = {host, port, server_name}`, the Backend talks to a gateway-deployment
management-handler over private mTLS (`ManagementTlsClient`) instead of the local admin
socket. The handler requires a client certificate from the configured CA **and** an explicit
Backend identity allowlist, checks the expected node id, accepts only the fixed typed
operations `provision/refresh/revoke/get/status` with strict field/size/expiry validation, and
translates 1:1 into the existing local `client-test` admin command; the node main password
never leaves the gateway (env/credentials) and no arbitrary args/shell/SQL/URLs are forwarded.
There is no plaintext or local fallback when TLS fails. Schema and deployment instructions:
`docs/GATEWAY_MANAGEMENT_MTLS_V1.md`.

Fences and retries: an operation whose desired generation no longer matches or whose grant is
revoked is finalized as `superseded_by_newer_generation` without any RPC; the gateway's own
generation/lease_seq/revoke tombstone is the second fence and survives its restart. Apply
readback now checks the expected gateway generation and lease sequence (provision -> gen 1 /
lease 1; refresh -> the next lease of the same generation) in addition to grant id,
registration, node id, revoked flag, finite `expires_at` and `runtime_applied`; a mismatched
readback is retried, never marked applied. Delivery is
at-least-once with a stable operation identity (identical command bytes, gateway digest
idempotency) plus readback; `queued`/`accepted` is never treated as applied. Missing/expired
entitlements produce a finite grant (`not_after <= entitlement end` and `<= GATEWAY_MAX_LEASE_SECONDS`)
and never an infinite one.

Run:

```bash
DATABASE_URL=... GATEWAY_ADMIN_MAIN_PASSWORD=... .venv/bin/terlimo-worker
# real isolated gateway integration (mount+net namespace, synthetic grant, installed binary)
.venv/bin/python tools/gateway_integration_03_3.py 2>&1 | tee evidence/step033-gateway-integration.log
```

The integration tool uses the same admin wire against an isolated WDTT instance and evidence
covers apply+readback, lost response with identical-bytes retry (one effect), revoke, delayed
stale refresh rejection and fence persistence across a gateway restart.

## STEP03.4 server-side preparation

`docs/STEP034_INTEGRATION_MAPPING_V1.md` holds the compact versioned endpoint/DTO mapping for
the current challenge/enrollment/session routes and the future `/me`, `/gateways`,
`/access/sync`, field ownership/revision/`valid_until`/`account_access` rules, last-good error
semantics, the native single-owner retry/canonical rules and the onboarding-hour/control
boundary. `fixtures/step034/` contains machine-readable synthetic fixtures
(`MeResponse`, `CatalogResponse`, `AccessSyncResponse`, error policy) validated against the
exact accepted C01 JSON Schemas by `tests/test_step034_integration_contract.py`, together with
the server gaps that need an accepted limited delta before implementation. The exact
`account_access corr4` source is not available server-side (Laptop-only) and is not replaced
by a candidate.

The local admin socket transport now requires an explicit isolated-TEST opt-in
(`GATEWAY_LOCAL_ADMIN_ENABLED=true` plus `TERLIMO_ENV=test`); outside TEST it refuses
(`GATEWAY_LOCAL_ADMIN_FORBIDDEN`) instead of silently using the local socket.

## Account reads (step 03.4 server slice)

- `GET /api/mobile/v1/me` - real account/binding/entitlement snapshot as the exact accepted
  `MeResponse` (account state, entitlement, slots/limit, onboarding hour if one exists,
  `grant_resolution` data-access class, revisions, UTC deadlines). Reads never create a trial,
  hour or grant; unknown state is reported as `none`, never as a known right.
- `GET /api/mobile/v1/operations/{id}` - own operation status as the accepted
  `OperationResponse`: `queued/applying/applied/retryable_failure/rejected` from the real
  outbox row plus the grant readback, so `accepted` is never shown as `applied`. Ownership is
  the server-side `outbox_operations.account_id` link (migration 0006), never a client-supplied
  account id; foreign or unknown operations return a neutral `404 NOT_FOUND`.
- Shared session authorizer (`session_auth.py`): bearer hash lookup, session
  expiry/revocation/generation, installation/environment binding, required scope and account
  state/ownership; management-only grants no data. A commercial right is effective only with
  `status=active` and `starts_at <= now < ends_at` (or the allowed indefinite end); a
  scheduled future right is not active and an onboarding hour never hides an active
  subscription. Account-bound receipts will carry the account identity when their routes
  arrive; the 7-day session receipt policy does not apply to payment/credit deduplication.
- `/me` uses a persisted subject-snapshot revision (migration 0007): the revision increases
  when the displayed right/binding/hour/slots change and never on an ordinary session refresh;
  only revision/fingerprint metadata is written on read, never a right/trial/hour/grant. The
  whole snapshot+revision is read in one REPEATABLE READ transaction with one effective clock
  per attempt and a bounded retry (3) on serialization conflicts; exhaustion returns a
  retryable `503 SERVICE_UNAVAILABLE` and never a partial body. Migration 0008 enforces the
  commercial invariant (`account_id IS NOT NULL OR kind='onboarding_hour'`); `down 0007`/`down
  0008` are fail-closed/documented and never delete rows for a rollback.
- Onboarding hours are installation-bound (unit = installation fingerprint); an hour with
  `installation_id NULL` is treated as undetermined and grants no data, and legacy rows are
  neither deleted nor guessed. Operation status confirms only the operation's own target
  generation/action (apply -> applied, revoke -> revoked; superseded/missing readback ->
  rejected).

## Tests and smoke

```bash
.venv/bin/python -m pytest -q
.venv/bin/python tools/smoke_step031.py 2>&1 | tee evidence/step031-smoke.log
```

The smoke starts its own isolated PostgreSQL datadir (`/tmp/terlimo-backend-step031-pg`),
runs migrations through the CLIs, checks readiness with and without a database, enqueues
durable jobs, restarts the worker, simulates a crashed worker, checks the unknown-type
failure and verifies graceful SIGTERM.

## Known limitations

- The mobile surface covers installation enrollment/sessions plus `/me` and own operation
  status; `/trial`, Telegram, checkout, `/gateways`, `/access/sync`, binding and the onboarding
  hour start hook are later slices (a limited delta is required before new shared routes).
- `outbox_effect_log` and the `self_check` operation are internal diagnostics.
- The worker lease heartbeat uses a second pool connection; keep `DB_POOL_MAX >= 2` when a
  handler can run longer than `WORKER_LOCK_TIMEOUT_SECONDS`.
- `terlimo-migrate down` applies one version at a time; revert in reverse order (the runner
  does not enforce LIFO).
- Challenge TTL, session TTL and the `ts` skew are provisional profile values (S1-B02 leaves
  them pending); they are env-configurable.
- Only the `enrollment`/`session` proof routes exist so far; catalog/checkout/binding/
  telegram-link routes and any account/entitlement behavior are later steps. No commercial
  right is created here.
- Gateway control is a closed internal service/fixture chain: no mobile endpoint creates
  grants yet (03.4/03.5); the CONTROL_BOUND_PROFILE numbers remain provisional TEST values
  (B02 STOP preserved) and production data-access is not claimed.
- The real gateway integration requires the installed WDTT binary plus `sudo unshare`
  (isolated mount+network namespace); it is not part of `pytest`.
- The mTLS management handler is implemented and verified in isolated TEST; production node
  identity provisioning/rotation and the §5.2 deployment policy still require an operational
  step, so 03.3 is not declared fully accepted.
- Receipt retention is explicit: raw bearer until `RECEIPT_RESULT_TTL_SECONDS` / session
  expiry, digest tombstone until `IDEMPOTENCY_WINDOW_SECONDS`, then the key is no longer
  retained and a new request with the same key creates a new effect (clients must not reuse
  keys beyond the window).
- No real gateway control channel, no payments, no Telegram, no Android integration.
- The bundled `pgserver` route is for TEST only; production deployment configuration is a
  later step.
