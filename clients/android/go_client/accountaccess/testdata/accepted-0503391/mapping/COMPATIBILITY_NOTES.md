# S1-C01 — compatibility notes (candidate)

These notes constrain the target contract so the existing access/profile parser and the
existing control path stay compatible. No runtime change and no shared-wire freeze here.

## 1. Bootstrap is a control seed; catalog is the gateway list

- The signed link `bootstrap` array is a **control seed** used to reach the control surface.
  It is not the gateway list. The gateway-list source is the **verified received catalog**
  (`GET /gateways`, target) — matching the existing `_catalog` projection
  (`service.py:264`) and WDTT `clientTestBootstrapServe` (`client_test_transport.go:403`).
- Consequence: do **not** put two entries in the link bootstrap to change a gateway list.
  A gateway list is never derived from the bootstrap seed; `bootstrap` stays a control seed.
- No client-side root-cause claim is made here. S1-B01 proved only the backend catalog
  projection, not the payload delivered to the device. Owner clarified that **both gateways
  exist and the second becomes visible after scrolling**; the search for a "missing" node is
  therefore cancelled. A/B **traffic/endpoint verification is a separate task**.
- The link `bootstrap_secret` and per-node `access.password` are different secrets
  (`provision.py:132-133`); the schema keeps them in different objects.

## 2. Legacy TEST commercial `expiry=0` vs target finite technical grants

- Legacy TEST commercial records may carry `expiry=0` / `perpetual_commercial=true`
  (`Subscription.end_date` with no expiry; `access_expiry_mode=none`).
- This **must not** produce an unbounded technical grant. Target `access.not_after` is
  always finite and requires renewal (V4-03 §7). The candidate encodes:
  `EntitlementSnapshot.perpetual_commercial` (commercial marker) is independent from
  `AccessDescriptor.not_after` (finite technical bound).
- `_grant_expiry`/`_wire_access_expiry` (`service.py:71,76`) currently return the unlimited
  sentinel; ADAPT to a finite lease governed by the (pending) CONTROL_BOUND_PROFILE.

## 3. Existing access/profile parser compatibility

- Keep the existing per-node access shape names that the parser already reads where
  possible: `grant_id`, `password`, `expires_at`/`not_after`, `vk_hashes`, `dtls_spki_sha256`.
  The target catalog relocates them under `access{}` and `transport{}`, which the new API
  fills automatically (V4-03 §10). The candidate does **not** remove the legacy fields; a
  versioned migration maps them.
- Pin check remains: `dtls_spki_sha256` verified in handshake before any service data
  (WDTT `client_test_transport.go:246,313`). Target encoding is **canonical unpadded
  base64url decoding to exactly 32 bytes** with strict re-encode equality (shared decision
  2026-09-19); hex64 is replaced addressively. `installation_id`/SHA-256 fingerprints stay
  lowercase hex.
- Catalog revisions are decimal strings (existing `service.py:350 str(sub.client_revision)`),
  not JSON numbers — preserving the parser.

## 4. Backend is not a data relay; bot is not separate billing authority

- The gateways do not receive account/Telegram/invoice/trial/tariff data (V4-03 §7). The
  catalog `access` carries only technical grant fields.
- The bot and API call the same application services (V4-03 §1); the donor MiniShop bot is
  not a second billing authority. Payment provider modules are migrated into the central
  backend boundary, not called as a permanent external MiniShop.

## 4a. Strict-consumer incompatibility (pending/empty admission delta)

- `MeResponse` in `schemas/subscription.json` has `additionalProperties: false` and now requires
  `binding_revision` (Revision decimal string or `null` when the subject has no binding). A strict
  consumer built against `1.5.0-candidate.1` will reject the new required field; this is an
  **explicit, acknowledged incompatibility**, not a silent additive change. `schema_version`
  stays `1.0` per owner decision (no wire-freeze change here).
- Android readiness is **not** claimed by this delta. The native adapter must be checked against
  the new field and its persistence/ordering/last-good behavior after the Laptop returns; the
  actual `account_access corr4` source is still unavailable server-side.
- `GET /gateways` may now answer `409 ACCESS_SYNC_PENDING` (retryable, `retry_after_ms`, with
  bounded admission tokens `details.catalog_revision` / `details.binding_revision`) or
  `503 SERVICE_UNAVAILABLE` for an actual application failure. `POST /access/sync` keeps its
  `200` + `access_application_state` operation state. A consumer that treats any non-200 catalog
  response as a fatal error will misbehave; keeping the last-good catalog and retrying is required.
- Indefinite commercial `/me`: `grant_resolution.effective_deadline` may be `null` ONLY for
  `data_access="subscription_data"` together with `entitlement.perpetual_commercial=true` and
  `entitlement.valid_until=null` (cross-object invariant enforced on `MeResponse`). Finite
  subscription_data and `onboarding_hour` keep a finite string deadline. The technical node grant
  lease (900 s), catalog validity and session validity stay finite and are unaffected by the
  nullable business deadline.

## 4b. Admission delta (candidate 1.5.0-candidate.3)

- `access.lease_seq` is now a **required** Generation decimal string (confirmed applied
  sequence). A strict consumer built against `1.5.0-candidate.2` will reject the new required
  field; this is an explicit, acknowledged incompatibility, not silently compatible.
  `schema_version` stays `1.0` per owner decision.
- Single node identity: `gateway_key == endpoints.node_id` is a registry invariant; an
  inconsistent row is rejected (never fallback-renamed). The advertised `target_workers` equals
  the readback-confirmed node `max_workers`; unknown/0/mismatch gets no verified admission.
- `wdtt-v17` remains the existing managed `installation-pop-v1` wire; no `auth_mode` field.
  `vk_hashes` stays optional/technical and is never a worker-cap or rights source.

## 5. Additive / versioned evolution

- `schema_version` is mandatory in every response. The signed payload top level is **strict**:
  an unknown top-level field is `BAD_MESSAGE`; additive data lives in signed `extensions`;
  a field named in `critical` that the server does not understand causes
  `UNKNOWN_CRITICAL_FIELD` (V4-03 §5, auth/POP_PAYLOAD_V1.md §4).
- Revisions and generations are string-encoded integers; comparisons are numeric, not
  lexicographic. Conflicting revisions → `REVISION_CONFLICT` (409).
- No production secrets, merchant credentials or arbitrary URLs appear in any payload.

## 6. Android review scope (no UI root-cause claim)

- Both gateways exist; the second is visible after scrolling. There is no missing-node defect
  to explain, and this contract does not assert any client-side UI root cause.
- Android review (PENDING) covers only contract consumption: which `schema_version`/catalog
  revision fields the client accepts and how it labels the seed vs the received catalog.
- Endpoint reachability and A/B traffic verification are a **separate** Android/routing task,
  not part of this contract candidate.

## 7. Pending profile values (not invented)

- `CONTROL_BOUND_PROFILE`: max lease, renewal margin, retry/backoff, clock skew, expiry
  overshoot, revoke delay — values are **PENDING (S1-B02)**; the schema marks them as
  profile-of-record rather than hard-coding numbers.
- Deployment host/origin, plan prices, and per-node worker caps are deployment/plan data,
  not contract constants.
