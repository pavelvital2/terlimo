# STEP036 explicit connect/start contract v1 — revision 3 (implemented field corrections)

Status: schema contract; base backend `6f1293b` on `d1e7638`. Revision 3 records the factual
fields of the implementation (source/test only): body carries `intent_id`/`request_key` and
no installation UUID (the signed payload keeps the accepted PoP installation fingerprint);
`onboarding_evidence.start_digest` is nullable with fail-closed legacy replay; existing
`onboarding_evidence.request_id` stores `proof.request_id` (no duplicate column);
`auth_challenges` gains `intent_id`/`superseded_at` and the outstanding index includes
superseded rows; the endpoint rejects an expired start challenge. Unchanged: fixed 3600 s,
intent TTL 600 s (max 900 s), scope `onboarding:start`, node envelope and architecture.

## 0. Fixed decisions

1. Public intent never starts the hour and never writes `onboarding_evidence`/`entitlements`.
2. Gateway is pinned by `select_gateway` before catalog; the endpoint in the intent response
   comes from the registry row, never from the client.
3. The hour starts only by a server-confirmed bootstrap connection through the assigned node,
   gated by the exact versioned start RPC inside the unchanged envelope `body`.
4. `background.*`/unknown bodies never reach `admit_evidence`, even when the intent is ready.
5. Pre-admission mobile branch is required (laptop map: no legacy bootstrap; `/me`
   `data_access:"none"`, `onboarding.state:"not_started"` currently dies before admission).

## 1. Public flow: challenge → intent → ready

| item | value |
| --- | --- |
| challenge route | `POST /api/mobile/v1/auth/challenge` (existing shape `{installation_fingerprint, purpose, environment}`) |
| intent purpose | `onboarding-start-intent` → scope `onboarding:start`, op `onboarding.intent`; bound to installation fingerprint only (intent does not exist yet); TTL `challenge_ttl_seconds`; rate-limited by the existing challenge limiter/retention |
| start purpose | `onboarding-start` → scope `onboarding:start`, op `onboarding.start`; issued only by the intent route when `state=ready`; bound to installation **and** `intent_id`; TTL `min(challenge_ttl_seconds, remaining intent TTL)` |
| start challenge outstanding rule | at most one unused start challenge per intent: a repeated ready poll returns the same `challenge_id`/`nonce_b64` while unused and unexpired; a new one is issued only when the previous is consumed or expired (race-safe, no unbounded accumulation) |
| intent route | `POST /api/mobile/v1/onboarding/intents`; bearer session of the same installation, scope `session:write`; body = existing signed PoP envelope |
| signed payload (intent) | `{env, scope:"onboarding:start", op:"onboarding.intent", installation_id, request_key, ts, nonce, request_id, extensions?}` |
| action fields | `PER_SCOPE_FIELDS["onboarding:start"] = ("request_key","intent_id")`; `REQUIRED_ACTION_FIELDS = ("request_key",)`; for `onboarding.start` `intent_id` is additionally required and cross-checked by the endpoint (step 2 matrix) |

Intent poll response (one shape per state; RFC3339 UTC `Z` for all times):

| state | JSON |
| --- | --- |
| pending | `{"status":"pending","state":"pending","intent_id","request_key","expires_at","retry_after","gateway":{"node_id","endpoint":{"peer_ip","dtls_port","dtls_spki_sha256"}}}` |
| ready | `{"status":"ready","state":"ready","intent_id","request_key","expires_at","gateway":{…},"credential_id","bootstrap":{"credential_id","secret"},"start_challenge":{"challenge_id","nonce_b64","expires_at"}}` |
| started | `{"status":"started","state":"started","intent_id","request_key","gateway":{…},"started_at","not_after"}` — status recovery only, never a secret |
| failed | `{"status":"failed","state":"failed","intent_id","request_key","reason"}` |
| expired/revoked | HTTP 410/403 with `{"status":"error","code":"ONBOARDING_INTENT_EXPIRED"|"ONBOARDING_INTENT_REVOKED","retryable":false}` |

`ready` does not carry `status:"pending"`; every state has its own `status` equal to `state`;
correlation fields (`intent_id`, `request_key`, `credential_id` when known, `gateway.node_id`)
are identical across polls. Retry of the intent operation after a lost response uses a fresh
challenge/proof (freshness fields are excluded from the business digest) and returns the same
intent state.

## 2. Bootstrap start RPC (inside the unchanged envelope `body`)

Envelope mapping (unchanged): `credential_id` = bootstrap credential; `connection_id` = node
session id (hash into evidence); `request_id` = node tunnel RPC id (not PoP); `body` = RPC.

```json
{"v":1,"op":"onboarding.start","intent_id":"<uuid>","request_key":"<1..128>",
 "proof":{"algorithm":"ES256","environment":"test","request_id":"<32hex>",
          "challenge_id":"…","nonce_b64":"…","payload_hash":"…",
          "signed_payload_b64":"…","signature_b64":"…"}}
```

Signed payload: `{env, scope:"onboarding:start", op:"onboarding.start", installation_id,
request_key, intent_id, ts, nonce, request_id}`.

Start RPC response (transport-level, distinct from the intent poll `status`):

```json
{"status":"ok","state":"started","intent_id":"…","credential_id":"…",
 "started_at":"…","not_after":"…","replay":false}
{"status":"error","code":"…","retryable":true,"retry_after":10}
```

## 3. Validation, two explicit branches, atomicity

Always verified (both branches, before any branch decision):

| # | check |
| --- | --- |
| 0 | mTLS cert → registry row (`gateway_key`,`environment`,`registry_state`) |
| 1 | envelope exact 4 keys; `credential_id` → `onboarding_intents` durable binding; caller gateway == immutable `intent.gateway_id`; env match |
| 2 | body exact schema, `v==1`, `op=="onboarding.start"` (else 400 `EVIDENCE_UNKNOWN_OPERATION`, no `admit_evidence`) and the field matrix below |
| 3 | signature/PoP verification against `installations.public_key_spki_b64` (challenge single-use rules differ per branch, see below) |
| 4 | business digest = `pop.business_digest(signed_payload)`; identity is durable, freshness-independent |

Field matrix (all must hold; outer-body checks alone are insufficient — the **decoded signed
payload** is compared too):

| signed payload | body | durable row / transport |
| --- | --- | --- |
| `env` | `proof.environment` | `intent.environment` == endpoint environment |
| `scope` | — | must equal `onboarding:start` |
| `op` | `op` | must equal `onboarding.start` |
| `installation_id` (PoP fingerprint) | — | `installations.public_key_fingerprint` of `intent.installation_id` |
| `intent_id` | `intent_id` | `intent.id` |
| `request_key` | `request_key` | `intent.request_key` |
| `request_id` | `proof.request_id` | recorded as evidence `request_id` |
| `nonce` | `proof.nonce_b64` | challenge row `nonce_b64` |
| — | `proof.challenge_id` | challenge row (`intent_id` bound) |
| — | — | `envelope.credential_id == intent.credential_id` |

Branch A — new start (no `onboarding_evidence` row for the credential):

1. Freshness required for a NEW start: challenge `purpose=onboarding-start`, `op=onboarding.start`,
   `intent_id`/environment/nonce match, unused, not superseded, unexpired — all evaluated under
   the installation lock at the actual post-wait time (the endpoint never decides freshness
   from a pre-lock read); the signed `ts` skew is checked for a NEW start too. An unconfirmed
   request with an expired proof is **not accepted** (401 `CHALLENGE_EXPIRED` /
   `PROOF_INVALID`); the client obtains a new start challenge and re-signs (business digest
   unchanged). Historical replay is exempt from freshness and skew.
2. One atomic transaction: conditional challenge consumption (`UPDATE … SET used_at=now()
   WHERE used_at IS NULL`) + `admit_evidence` checks + evidence insert (with `start_digest`) +
   hour unit + intent `started` + secret wipe.
3. Any rejection rolls the whole transaction back, including the challenge consumption: no
   partial consume and no partial admission. Only a committed transaction writes.

Branch B — authenticated historical replay (`onboarding_evidence` row exists for the credential):

1. Cert/registry, envelope, schema, durable binding, signature and the field matrix are still
   verified; the digest must equal `evidence.start_digest`.
2. No challenge re-consumption, no freshness requirement: the recorded proof may be consumed
   or expired (`400 response was lost`, hour `not_after` may have passed).
3. Same digest → 200 historical (`replay:true`, no writes, no new hour). Different digest →
   409 `IDEMPOTENCY_CONFLICT` (no writes). Different credential → 409 `ONBOARDING_UNIT_STARTED`.

Durable digest storage (exact, migration 0019, additive, no backfill): `onboarding_evidence`
gains nullable `start_digest text` with a hex-64 CHECK; legacy rows keep NULL and replay for
them fails closed (`ONBOARDING_REPLAY_UNAVAILABLE`, no new hour). `proof.request_id` is stored
in the existing `onboarding_evidence.request_id` column (no duplicate column). The digest is
written in the same transaction as the evidence row; replay lookup is by the unique
`credential_id`. `auth_challenges` gains nullable `intent_id uuid REFERENCES
onboarding_intents(id) ON DELETE CASCADE` and `superseded_at timestamptz`, plus the CHECK
values `onboarding-start-intent`, `onboarding-start`; the partial unique index covers unused
outstanding rows (any expiry), and refresh atomically supersedes the previous unused row
before inserting a new one (bounded by the existing challenge retention cleanup).

## 4. Available pieces (unchanged, per review)

`installations.public_key_spki_b64`/`public_key_fingerprint`; `pop.py` (canonical JSON,
business projection/digest, `verify_proof`, `load_public_key`, `PREFIX=WLBS-POP-1`);
`auth_challenges` (`challenge_id`, `nonce_b64`, `installation_fingerprint`, `purpose`, `op`,
`environment`, `expires_at`, `used_at`); sessions/scopes (`session:write`,
`UNLINKED_INSTALLATION_SCOPES`); `onboarding_intents` fields; `onboarding_evidence`
fields plus the nullable `start_digest`; `pop.KNOWN_TOP_LEVEL` additionally covers
`request_key`/`intent_id`.

## 5. Client sequence and honest availability

1. `/me` pre-admission → explicit Connect (after VPN consent) → challenge/intent → poll ready.
2. Open bootstrap connection to `gateway.endpoint`, send the start RPC; retry the exact same
   signed request on timeout (Branch B is safe).
3. `started` from the **public intent poll is status recovery only**: it says the hour is
   recorded server-side. It does not promise that the old node-side bootstrap credential is
   still accepted — the node may have revoked it or its TTL may have expired. In that case the
   client must not retry the node hop; it continues with the normal session flow.
4. Then unchanged: `/me` (`data_access:"onboarding_hour"`), `auth/session` scope transition,
   catalog, `access:sync`, grant → VPN.
5. `background.*` traffic never carries `op:"onboarding.start"`.

## 6. Logging/secrets

`bootstrap.secret` only in the session-authenticated ready response over HTTPS; never logged,
never in the start body/response, never in evidence. Nonces, signatures, signed payloads and
digests are not logged. Existing redaction is reused.
