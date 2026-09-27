# STEP036 onboarding-hour delta v1 — revision 4 (final self-contained; proposal only)

Status: implementation-decision document. No DB/runtime/code applied. Base backend `c2d97bb`.
Owner intent: `OWNER_ONBOARDING_HOUR_AND_CHECKOUT.md` SHA256 `a8d5ff99…` (full VPN hour; start =
server-confirmed first connection; `not_after=start+3600`; restart/refresh never extend; not
trial/paid; subscriptions never shortened; control/checkout preserved; reinstall-unit limit).
Contract model: `terlimo-s1-contracts/handler/onboarding_hour.py`. This revision is
self-contained: full DDL, explicit checks, state/HTTP table, negative vectors; the new remote
seams (relay + backend mTLS evidence endpoint) are declared open, not implemented.

## 1. G6 — transport and identity (backend registry-enrichment of the existing envelope)

The existing node bootstrap envelope stays `{credential_id, connection_id, request_id, body}`
(node `client_test_transport.go:424-447`); v2 is **not** invented. New seams:

- **Gateway host, local hop:** node → technical relay over AF_UNIX with restricted peer (uid/gid
  of the node process). The relay is transport only (not a business service, no bot/minishop);
  it does not add or trust any field from the client body.
- **Relay → Backend, remote hop (mandatory mTLS):** the relay connects to the Backend mTLS
  evidence endpoint using an **individual node certificate** from the existing management CA
  discipline. Backend derives `gateway_key`/`environment` from the certificate identity and the
  registry row (`gateway_key == endpoints.node_id`, environment match). Node identity is never
  taken from JSON.
- **Backend enrichment:** with the unchanged 4-field envelope, Backend looks up
  `credential_id` in the durable DB, resolves `intent_id → installation_id/gateway_id/environment`,
  and crosschecks them against the certificate-derived gateway/environment **before any return**.
  A credential that is unknown or belongs to another gateway/installation/environment is rejected
  without touching intent state.

## 2. Strict callback order (G1) and start checks (G4)

Every evidence callback performs, in this order, with no return before (0)–(2):

0. mTLS certificate → registry row: `gateway_key`, `environment` (mismatch → 403
   `ONBOARDING_ENV_MISMATCH`); unknown/foreign certificate → 403.
1. `SELECT ... FROM installations WHERE id = <intent.installation_id> FOR UPDATE`.
2. Binding check: credential → intent → installation/gateway/environment must match the callback
   attributes exactly (else 403 `ONBOARDING_ENV_MISMATCH`/`CREDENTIAL_UNKNOWN`).
3. Then, inside the same transaction:
   - authenticated idempotent replay: if the unit is `started` and this credential matches the
     recorded evidence → **200 with the historical start** (`started_at`, `not_after`), no new
     rights, no re-issue — this holds even after hour expiry; the live right is read separately
     (catalog/sync re-evaluate);
   - unit started with a different credential → 409 `ONBOARDING_UNIT_STARTED`;
   - intent `expired` or `expires_at <= now()` → 410 `ONBOARDING_INTENT_EXPIRED`;
   - intent/credential `revoked` or installation `revoked` → 403 `ONBOARDING_INTENT_REVOKED` /
     `INSTALLATION_REVOKED`;
   - intent must be `ready`; gateway `registered` with descriptor valid and confirmed worker cap
     (else 503 `ONBOARDING_GATEWAY_NOT_READY` with `retry_after`);
   - only if all checks pass: insert evidence, insert the hour unit, mark intent `started`,
     wipe `secret_enc`.
   Rejected callbacks start nothing and write nothing. Revoke-before-evidence is enforced here:
   the revoked state is checked under the lock before any insert.

## 3. Full DDL (G3; proposal, additive)

```sql
-- grants: installation-scoped alongside binding-scoped (XOR)
ALTER TABLE grants ALTER COLUMN binding_id DROP NOT NULL;
ALTER TABLE grants ADD COLUMN installation_id uuid REFERENCES installations(id) ON DELETE CASCADE;
ALTER TABLE grants ADD CONSTRAINT grants_subject_xor CHECK (
    (binding_id IS NOT NULL)::int + (installation_id IS NOT NULL)::int = 1);
ALTER TABLE grants DROP CONSTRAINT grants_binding_id_gateway_id_key;
CREATE UNIQUE INDEX grants_binding_gateway_uniq ON grants (binding_id, gateway_id) WHERE binding_id IS NOT NULL;
CREATE UNIQUE INDEX grants_installation_gateway_uniq ON grants (installation_id, gateway_id) WHERE installation_id IS NOT NULL;

-- one-time hour unit: read-only preflight first; fail/report without deleting rights
--   SELECT installation_id, count(*) FROM entitlements
--    WHERE kind='onboarding_hour' AND installation_id IS NOT NULL
--    GROUP BY installation_id HAVING count(*) > 1;
-- clean preflight required before:
DROP INDEX entitlements_hour_installation_idx;
CREATE UNIQUE INDEX entitlements_hour_installation_uniq ON entitlements (installation_id)
    WHERE kind = 'onboarding_hour' AND installation_id IS NOT NULL;

CREATE TABLE onboarding_intents (
    id             uuid PRIMARY KEY,
    installation_id uuid NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    gateway_id     uuid NOT NULL REFERENCES gateways(id) ON DELETE RESTRICT,  -- immutable
    environment    text NOT NULL,
    unit_epoch     integer NOT NULL,
    credential_id  text NOT NULL UNIQUE,
    secret_hash    text NOT NULL,
    secret_enc     bytea,
    state          text NOT NULL CHECK (state IN ('pending','ready','started','failed','expired','revoked')),
    expires_at     timestamptz NOT NULL,                 -- intent TTL, <= now()+900s at creation
    created_at     timestamptz NOT NULL DEFAULT now(),
    started_at     timestamptz,
    failed_reason  text,
    revoked_at     timestamptz,
    UNIQUE (installation_id, unit_epoch),
    CHECK (state IN ('pending','ready') OR secret_enc IS NULL),
    CHECK (state NOT IN ('pending','ready') OR secret_enc IS NOT NULL)
);
CREATE UNIQUE INDEX onboarding_intents_active_uniq ON onboarding_intents (installation_id)
    WHERE state IN ('pending','ready');
CREATE INDEX onboarding_intents_credential_lookup ON onboarding_intents (credential_id);

CREATE TABLE onboarding_evidence (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    intent_id         uuid NOT NULL REFERENCES onboarding_intents(id) ON DELETE CASCADE,
    credential_id     text NOT NULL UNIQUE,
    installation_id   uuid NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    gateway_id        uuid NOT NULL REFERENCES gateways(id) ON DELETE RESTRICT,
    environment       text NOT NULL,
    connection_id_hash text NOT NULL,
    request_id        text NOT NULL,
    first_seen_at     timestamptz NOT NULL DEFAULT now(),
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (intent_id)
);
CREATE INDEX onboarding_evidence_installation_idx ON onboarding_evidence (installation_id);
CREATE INDEX onboarding_evidence_gateway_idx ON onboarding_evidence (gateway_id);
```

## 4. Intent lifecycle, provisioning, idempotency (G2, G5)

- Creation (single DB transaction): eligible backend-selected gateway; `unit_epoch =
  max(unit_epoch)+1` for the installation under the installation lock; deterministic
  `credential_id = uuid5(NS, "hour:{installation_id}:{unit_epoch}")`; `secret_hash` +
  provisional `secret_enc`; state `pending`; `expires_at = now()+intent_ttl (<=900s)`;
  **outbox op `gateway.bootstrap_provision` enqueued in the same transaction** with payload
  `{intent_id, credential_id}` (never the secret) and idempotency key
  `bootstrap:{credential_id}`. The same idempotency key never creates a new unit; a repeated
  request returns the current record.
- `gateway_id` and `environment` are **immutable from durable creation**; there is no client
  retarget and no `pending` retarget path. A different gateway requires an explicit allowed
  action **before hour start** (operator/subject action), which revokes the old intent through
  the standard reconcile path and allocates a new epoch; after start the unit is consumed.
- Worker provisioning: before the external node call the worker re-reads the intent under the
  installation lock and provisions exactly the stored `gateway_id` snapshot; the readback may
  mark the intent `ready` only while it is still `pending` and the outbox op is owned. A late
  claimed worker/readback after terminal state is fenced and cannot revive the intent. Failure
  ends in terminal `failed` with `failed_reason`, never a silent retry loop.
- State/HTTP contract (G5):

| state | HTTP | body | secret |
|---|---|---|---|
| `pending` (async provision) | 200 | `{intent:{id,state,expires_at}, retry_after}` | no |
| `ready` | 200 | intent + `bootstrap` block | yes, idempotent re-delivery |
| `started` | 200 | `{intent:{state,started_at,not_after}}` (authenticated replay returns the historical start, even after expiry) | no (wiped) |
| `failed` | 200 | `{intent:{state,failed_reason}}`; new unit only via explicit allowed action before start | no |
| `expired` | 410 `ONBOARDING_INTENT_EXPIRED` | — | no |
| `revoked` | 403 `ONBOARDING_INTENT_REVOKED` | — | no |
| unit started, other credential | 409 `ONBOARDING_UNIT_STARTED` | — | no |
| cert/env mismatch | 403 `ONBOARDING_ENV_MISMATCH` | — | no |
| gateway not ready/cap unconfirmed | 503 `ONBOARDING_GATEWAY_NOT_READY` + `retry_after` | — | no |
| lost reply on evidence | 200 with recorded start (replay by `credential_id`) | — | no |

- TTL unchanged: `intent_ttl <= 900s`, independent of the hour; `not_after = started_at+3600`
  computed only at start; wipe of `secret_enc` at start/terminal leaves only `secret_hash`.

## 5. Data transition, revoke semantics, paid overlap

- New session with `access:sync` only with an active hour (installation subject) or paid/trial;
  live re-evaluation on every request; old tokens never extend the right; data cutoff
  `not_after <= min(hour.not_after, now+900 s)`; expiry → data 403, control/checkout remain.
- Revoke separation: bootstrap credential revoke after a successful start does **not** revoke
  the hour/data (it only blocks further bootstrap use); installation revoke revokes the
  installation-scoped grant and cuts data on the next live check; binding revoke stays as today.
- Paid overlap: an active paid/trial binding grant is authoritative for the same
  installation/gateway; the installation grant is not served simultaneously, the gateway worker
  cap remains the single shared `target_workers` budget (no doubling), and hour expiry never
  interrupts paid data.

## 6. Negative/state vectors (compact)

| Vector | Expected |
|---|---|
| Foreign/unknown certificate; JSON node_id spoof | reject before any return; spoof ignored |
| Cert gateway vs credential's intent gateway mismatch | 403 ENV_MISMATCH, no writes |
| Credential unknown/expired/revoked | 403/410, no writes |
| Intent revoked before evidence arrives | no start (under-lock check) |
| Unit started, same credential replay (even after expiry) | 200 historical start, no new rights |
| Unit started, different credential | 409 UNIT_STARTED |
| Duplicate connection_id / duplicate evidence | idempotent, one evidence, one hour |
| Concurrent evidence | installation lock → one hour, one evidence |
| Gateway-change attempt after durable creation | forbidden (immutable); explicit action only before start |
| Claimed worker readback after terminal intent | fenced, no revival |
| `secret_enc` at start/terminal | wiped; hash retained; no copy in outbox/logs |
| Duplicate existing hour rows at migration | read-only preflight fails/reports; no deletion |
| Paid/trial active while hour runs | paid authoritative; cap not doubled; hour expiry harmless |

## 7. Open seams (declared, not implemented)

1. Gateway technical relay and the Backend mTLS evidence endpoint (cert→registry mapping) — new
   minimal components; the TEST unix socket and the raw 4-field envelope over unix are not a
   production proof.
2. Production bootstrap admin operations remain an upstream gap
   (`BOOTSTRAP_CONTRACT_V1_20260910` §227).
3. Client `009d0a5` source is not locally available; the intent action/decoder must be validated
   by root/LaptopHigh.
4. Installation-scoped grants and all DDL above are proposals; nothing applied, hour for D1 is
   not activated.
