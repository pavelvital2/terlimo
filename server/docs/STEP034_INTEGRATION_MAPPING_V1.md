# STEP03.4 server-side integration mapping (v1, preparation)

Status: server-side preparation package. It does **not** implement new shared business routes
and does not change the accepted C01 wire. Android/native work is not done here; the exact
`account_access corr4` source/evidence is **not present on the server side** (see §8).

Basis: `terlimo-s1-contracts @ 336bd6dae9e9340874d6e128a360fe6cfe2f26e6` (C01) and the
accepted Backend `590481f0477f07627b0749f3187b0e93aff83297`. Machine-readable fixtures:
`fixtures/step034/`; conformance checks: `tests/test_step034_integration_contract.py`.

## 1. Endpoint / DTO mapping

| Endpoint | Auth | Proof scope / op | Session auth (C01) | Data access | Implemented server-side |
|---|---|---|---|---|---|
| `POST /auth/challenge` | public | N/A | - | none | yes (03.2) |
| `POST /installations` | public | `enrollment` / `installations.create` | - | none (never an entitlement) | yes (03.2) |
| `POST /auth/session` | public | `session` / `auth.session` | - | none | yes (03.2) |
| `GET /me` | session | N/A | read-only snapshot | none | **gap** |
| `GET /gateways` | session | N/A | verified catalog; requires an active entitlement | subscription_data | **gap** |
| `POST /access/sync` | session | `N/A_SESSION_AUTH` | `access:sync`, states `ACTIVE_TRIAL/ACTIVE_PAID`, ownership account/installation owns binding | subscription_data | **gap** |
| `POST /trial` | session | `N/A_SESSION_AUTH` | `session:write`, state `VERIFIED_NO_ENTITLEMENT` | subscription_data (after activation) | not in 03.4 |
| `POST /payments/{id}/checkout-session` | session | `N/A_SESSION_AUTH` | `payment:write`, bounded checkout classes | restricted_checkout | not in 03.4 |

DTO sources (exact, reused): `schemas/session.json` (Challenge/Proof/Session),
`schemas/installation.json` (Enrollment), `schemas/subscription.json` (`MeResponse`,
`EntitlementSnapshot`), `schemas/catalog.json` (`CatalogResponse`), `schemas/operation.json`
(`AccessSyncRequest/Response`, `OperationResponse`, `NodeProgress`), `schemas/onboarding.json`
(`OnboardingHour`, `GrantResolution`, `DataAccessClass`), `schemas/errors.json`.

Native `/me/adapters` projection rules come from `mapping/op_scope_field_mapping.json`
(`gateways[]`←`nodes[]`, `gateway_id`←`node_id`, `valid_until`←`catalog_expires_at`,
`device_ref`←`device_id`, `transport.dtls_spki_sha256` strict base64url, `access.not_after`
finite, revision/generation as decimal strings). The native adapter keeps
`selected_node_id` locally (not on the wire) and clears it only if the selected gateway leaves
the verified catalog.

## 2. Field ownership and revisions

| Field | Owner | Rule |
|---|---|---|
| `request_id` | server response | Echo of the signed `request_id` for proof routes; generated otherwise |
| `server_time` | server | RFC3339 UTC, `Z` |
| `revision` | server (subject snapshot / catalog / operation) | monotonic decimal string; for `/me` a persisted subject-wide counter (account+installation) changes whenever the displayed right/binding/hour/slots snapshot changes, never on an ordinary session refresh; native compares numerically, Kotlin equality only |
| `binding_revision` | server `/me` (owner decision 2026-09-21) | decimal string of the real binding generation, or `null` when the subject has no binding; never `0`, never the subject revision. It is the source for `AccessSyncRequest.binding_revision`; reading `/me` never creates a slot or right |
| `generation` | server (grant/session/binding fence) | decimal string; stale generations never revive access |
| `valid_until` | server catalog | finite catalog validity instant; last-good copy may be displayed but never treated as valid access |
| `access.not_after` | server grant | `<=` entitlement end and `<=` technical lease ceiling (`GATEWAY_MAX_LEASE_SECONDS`) |
| `account_access` / `grant_resolution.data_access` | server | resolved from account state, entitlement, onboarding hour, binding; management read alone grants no VPN |
| `onboarding` | server | one-time 3600 s, `started_by=server_confirmed_first_connection`, never extended on refresh/restart, never creates a trial; the hour unit is the **installation fingerprint** and an hour is visible only when explicitly linked to the session installation (legacy `installation_id NULL` rows stay undetermined and grant no data) |

## 3. Error semantics / last-good

Policy (encoded in `fixtures/step034/error_semantics.json`): non-terminal errors keep the
**last correct** status/catalog for display and never invalidate a known right; an expired or
revoked right is never resurrected by a retry, cache or familiar session; `management-only`
grants no data. Errors use only the accepted `errors.json` codes with bounded payloads
(`401 SESSION_EXPIRED/SESSION_INVALID`, `403 DEVICE_REVOKED/SUBSCRIPTION_EXPIRED/
SUBSCRIPTION_MISSING`, `409 REVISION_CONFLICT/IDEMPOTENCY_CONFLICT`, `429 RATE_LIMITED`,
`200` + `access_application_state` for pending application).

## 3a. Session authorization and operation confirmation

- One authorizer for session routes: bearer hash, session expiry/revocation/generation,
  installation/environment binding, required scope (when the route pins one), account
  state/ownership; management-only grants no data; foreign resources are a neutral 404.
- A commercial right is effective only with `status=active`, `starts_at <= now < ends_at`
  (or the allowed indefinite end) and kind `trial/paid/imported`; a scheduled future right is
  not active, and an onboarding hour never hides or shortens an active subscription.
- `/me` snapshots are read under REPEATABLE READ with one effective timestamp per attempt and
  a bounded retry (3) on serialization conflicts; the subject revision and the body always come
  from one snapshot. Commercial entitlements without an account are rejected by the 0008
  invariant; only an onboarding hour may be accountless.
- `GET /operations/{id}` confirms this operation's own target: a done gateway apply/revoke is
  `applied` only when the payload action/generation match `target_revision` and the grant
  readback confirms the same generation and terminal state (apply -> applied, revoke ->
  revoked). Missing readback or a superseded target is `rejected`; it is never credited from
  newer generations.

## 3b. Pending vs authoritative empty (owner decision whitelist-20260921-step034-pending-empty-contract-v1)

- `GET /gateways` is whole-response:
  - nonterminal desired application (actual target + outbox state: pending/processing) → `409 ACCESS_SYNC_PENDING`, retryable, `retry_after_ms`, and bounded `details.catalog_revision` / `details.binding_revision` admission tokens of the current authorized subject; no CatalogResponse, last-good kept;
  - terminal application failure (failed/exhausted/missing readback) with an active right → `503 SERVICE_UNAVAILABLE` (`reason=access_application_failed`), no CatalogResponse, last-good kept; a NEW `POST /access/sync` in that state is also blocked with the same bounded failure and never resets attempts or duplicates effects (exact unresolved gap: operator/worker chain must resolve the dead attempt);
  - authoritative `200` empty/partial only when the absence is a true registry `disabled`/removed node or a desired `revoked` grant; a registered node without a desired application, an expired lease with an active right, an uninitialized/empty registry, a failed renewal or an unknown attempt are never an authoritative removal;
  - malformed registered descriptor stays a whole-response `503` (`registry_descriptors_incomplete`);
  - an empty registered set is `200` authoritative empty only when the environment is published: a registry tombstone (`registry_state='disabled'`) or the durable `registry_environments` marker (migration `0011`, backfilled only from real gateway rows and set by the authoritative registry lifecycle at publication) exists. A fresh/unknown environment (no rows, no tombstones, no marker) stays `503`. Subject revisions, error GETs and grant states never create this authority.
  - Supported removal in this Backend is the disabled/removed tombstone. Physical `DELETE` of a gateway that has grants is blocked by the `grants.gateway_id ON DELETE RESTRICT` foreign key, and there is no registry-admin/publish route, so **physical deletion through a runtime lifecycle is not supported here and is not claimed as passing**; the marker exists for the authoritative lifecycle when it is introduced.
- The admission tokens are metadata only: they admit a NEW sync and never authorize data; the client's last-good catalog is kept and rights/lease/`valid_until` are never extended by an error.
- `POST /access/sync` keeps `200` + `access_application_state` (operation state). Same persisted key/body is never a new attempt.
- `fixtures/step034/error_semantics.json` distinguishes `catalog_pending_get` (GET 409), `catalog_application_failed` (GET 503) and `access_sync_pending` (POST 200 operation state).

## 3c. Linked session issuance (owner decision 2026-09-21, bounded 03.4 fix)

- `POST /auth/session` resolves the installation->account link **server-side under a row lock**
  (`account_bindings` FOR UPDATE): exactly one active binding whose account is trusted
  (`accounts.status='verified'` and a linked Telegram identity `telegram_id IS NOT NULL`).
  No link, a revoked link, two active bindings (ambiguous) or an unproven account never
  elevate scopes; the client payload never carries an account id.
- With a proven link the session may additionally request `access:sync` (linked set =
  unlinked set + `access:sync`). Future payment/admin scopes stay unavailable, and a valid
  session is never a VPN right: `/me`, `/gateways` and `/access/sync` still enforce the live
  entitlement/expiry/revocation state on every request.
- Unlinked enrollment/session behaviour is unchanged (`account_ref:null`, unlinked scopes).
  No account/binding/entitlement is created by session issuance and Telegram login is never
  simulated.
- Fresh authorization precedes any stored receipt: a replay is re-authorized against the live
  link and session row; a revoked/replaced/deleted link is never returned with the old
  subject's bearer (`SESSION_INVALID` / `binding_changed`), and an unauthorized replay is
  refused before the receipt is consulted.
- Missing invariant to confirm (not invented here): the trusted-login evidence used is
  `accounts.status='verified'` + `accounts.telegram_id IS NOT NULL`; there is no dedicated
  `trusted_login_at`/policy field. If the owner wants a different invariant, it is a policy
  decision, not a code change in this slice.
- Binding fence (correction1, migration `0012`): an account-bound session stores the resolved
  `binding_id` + `binding_generation`; business bearer authorization resolves that exact binding
  identity and refuses a session whose binding row was deleted/replaced or whose generation was
  bumped (`SESSION_INVALID`), so a same-account replacement never inherits old rights. Stored
  receipts are re-authorized against the live locked link; a changed identity/generation is
  `binding_changed`. Existing linked rows without a provable fence fail closed (no guessed
  backfill); unlinked sessions are unaffected.
- Fresh trusted-login provenance after a revoke (Telegram re-verification/rebind) is **not**
  implemented and this gate is **not** declared closed: `status='verified'` + `telegram_id`
  alone do not prove a new trusted login. Rebind/link routes remain a future architectural
  boundary.

## 3d. Admission mapping to the real gateway wire (owner decision 2026-09-21)

- `access.lease_seq` (required, Generation decimal string) is the **confirmed applied** lease
  sequence read back from the gateway for the published `generation`; the catalog emits exactly
  one confirmed generation/lease_seq pair and never a desired/pending pair. Source:
  `grants.lease_seq`, set only after a successful readback (`gateway_control.apply_grant`).
- Single node identity: `gateway_key == endpoints.node_id` is a registry invariant. Catalog and
  sync reject an inconsistent registered row explicitly (`503 registry_node_identity_mismatch`);
  there is no fallback rename, and the worker refuses to call the gateway
  (`node_identity_mismatch`) instead of guessing the node id.
- `target_workers` is advertised only when it equals the readback-confirmed node `max_workers`
  (`gateways.confirmed_max_workers`, migration 0013); unknown/0/mismatch gets no verified
  admission (pending then bounded application failure), and the cap is never derived from
  `vk_hashes`.
- Proven revoke target (correction1): a successful apply stores the readback-proven
  `target_node_id` and the exact transport route used (`target_route`, migration 0014). An
  actual revoke follows that proven identity/route, never a freshly recomputed registry
  identity. There is deliberately **no** current-registry fallback by node name: a missing or
  malformed persisted `target_route` means the original route cannot be reconstructed, so no
  RPC is sent (`proven_target_unavailable`), the desired revoke is kept and the actual revoke is
  never claimed. The jsonb value is decoded from dict (pool codec) or string (raw connection)
  and any other shape fails closed.
  A desired DB revoke without a confirmed readback is never reported as a removal: the catalog
  keeps last-good with a bounded pending/failure, the public operation state stays
  non-applied, and a stale apply can never resurrect the grant. No secret is sent to a
  substituted endpoint, and the fail-closed new-apply guard is unchanged.
- `transport.protocol=wdtt-v17` uses the existing `installation-pop-v1` wire; no `auth_mode`
  field is added. `vk_hashes` stays optional/technical and is not a managed worker-cap or rights
  source. The mobile client never calls the gateway admin surface.

## 4. Native coordination (single owner)

- The native layer is the **sole** owner of retry/canonical state and revisions; the Kotlin
  layer only displays and stores fields.
- A same-subject bearer refresh (`account_ref`/`installation_ref` unchanged) must **not**
  reset the verified catalog baseline or `selected_node_id`; only a subject change, an explicit
  Disconnect or removal of the selected gateway from the verified catalog resets it.
- `queued`/`accepted` is never displayed as applied; the native polls the operation until a
  terminal state (per `operation.json`).

## 5. Onboarding hour and control/checkout boundary

Enrollment (03.2) creates no hour, no trial and no entitlement. The one-time hour starts at
the first **server-confirmed connection** (future confirmed-connect hook, 03.5), not at APK
download; `duration_seconds=3600`, `one_time=true`, `extends_on_refresh/restart=false`,
`creates_trial=false`. Control/challenge/link and the restricted checkout are available
without an active subscription and are **not** data; `management-only` sessions grant no data.
Account-state/ownership/revocation checks are required before every business action **and**
before returning any stored result.

## 6. Fixtures

- `endpoint_mapping.json` - route/auth/scope/session mapping mirror (compared against the
  accepted mapping file by the conformance test).
- `me_vectors.json` - `MeResponse` fixtures: `success_active_trial`, `success_paid_imported`,
  `no_entitlement`, `management_only_no_slot`, `expired_keeps_last_good`.
- `catalog_and_operations.json` - `CatalogResponse` fixture plus `AccessSyncResponse` cases
  `applied`, `pending`, `retryable_failure`.
- `error_semantics.json` - policy flags and error cases with accepted codes.

All fixtures are synthetic and must not be exposed as a user bypass or as real subscriptions.

## 7. Server gaps (limited delta required before implementation)

1. Account/entitlement read+activation application services (`/me`, `/trial`) - new service
   layer over the existing accounts/entitlements/bindings tables.
2. Catalog service: catalog revision/`valid_until`/`issued_at` and per-gateway technical
   descriptors; no "best server" selection.
3. Session-bearer authorization middleware enforcing `x-session-auth`
   (`required_scope` + `account_states` + `ownership` + revocation/expiry) **before** any
   business action and before stored results; the 03.2 enum gate is not sufficient.
4. Operation application state mapping (`queued/applying/applied/retryable_failure/rejected`)
   over the existing outbox + grants readback.
5. Account identity in receipts before any account-bound receipt route is implemented
   (session/enrollment receipts stay as accepted; the 7-day policy is not extended to
   payment/credit or revoke fences).
6. Confirmed-connect hook that starts the onboarding hour (03.5 boundary).

These are shared business routes/contract additions: implement only after a limited delta is
accepted by the manager; no stubs are being presented as readiness.

## 8. Blocked only by Laptop/offline

- `account_access corr4` exact source/evidence is not on the server side (searched
  `terlimo-access`, `terlimo-s1-b02/b03/j01`, contracts, relay inbox: only the 03_STEP text
  mentions it). It must not be substituted by another candidate.
- The Android/native adapter mapping, last-good display behavior and same-subject refresh
  baseline protection need the native source; no Android code/runtime work is done here.
