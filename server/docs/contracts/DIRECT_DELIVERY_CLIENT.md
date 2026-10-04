# Canonical direct-delivery client — SOURCE only

## Scope and ownership

Authority: accepted `server/tools/wdtt_direct_adapter.py` and `WDTT_DIRECT_V3.md`, published base `3bb1caceb16a91c467ecf6680fcd1dc4ffbcf6df`. This module transports v3 only. It does not modify adapter/journal, business cores, database schema, worker or Minishop. No runtime apply, socket daemon, public route, new credentials, identity/claim invention, UUID generation, retries, v1 fallback or alternate network transport.

Canonical account backend exclusively chooses target, operation ID, fence, revision and fixed absolute expiry and MUST durably persist operation/body before claim/apply. `Request(body: bytes, base: Snapshot | None)` is frozen, preserves supplied JSON bytes exactly, validates closed v3 types and identity, computes the adapter's actual SHA256 of `json.dumps(parsed, sort_keys=True, separators=(',', ':')).encode()` (default ensure_ascii=True). Wire is exactly body + one newline. For inactive apply with null expiry caller supplies the persisted BASE snapshot so desired expiry can be correlated; absent BASE supports the adapter's explicit absent-inactive conflict. Claim/apply accept the same typed Request with strict operation dispatch. Read Request carries only target fields; `read(request, expected=original_claim_or_apply)` validates latest returned operation against persisted caller expectation. If latest operation changed, caller must select its durable expected intent; client never invents one.

Caller must save wire body privately in its canonical persistence layer; this slice provides no persistence. Local API creates no request fields or new intents. Closed fields/UUID/grant identity, integer-not-bool/int64, UTC whole-second expiry, duplicate/nonfinite/unknown/malformed fields are rejected without coercion. Frozen Target/Snapshot/Request/Receipt/Response are typed value forms. Responses must match expected external key, deterministic grant, fence, operation ID, canonical digest, desired and receipt revision/applied snapshot. Later accepted/current revisions may legitimately differ from historical receipt and are exposed separately, never replaced by that receipt's revision.

## Transport and secrets

Default endpoint is existing protected `/run/terlimo-wdtt-adapter/adapter.sock`. Absolute Unix Path only; no host/URL/TCP fallback. Existing protected endpoint permissions and adapter SO_PEERCRED establish caller authentication; no new credentials here. The optional opener supports isolated tests and must not be overridden with an alternative transport in deployed wiring.

One total 5s asyncio deadline covers open/write/drain/read. Request and total response are bounded16KiB including newline. Repeated bounded reads consume fragmentation through EOF; exactly one newline frame, no trailing bytes. Existing adapter closes each response connection; withholding EOF also expires the total deadline. Writer is closed on completion/error/cancellation; no retries. EOF without a complete frame, malformed/oversized reply, timeout or cancellation after send cannot establish that no remote mutation occurred. `DeliveryError.unresolved` remains conservative; cancellation propagates `asyncio.CancelledError`, and callers must retain original intent as unresolved. Explicit closed version1/3 generic error envelope becomes `direct_peer_error`, never v3 success. No error echoes untrusted payload/secret. Request body and response artifact are excluded from repr; receipt contains metadata only.

`response.artifact_for_projection()` is the sole explicit secret accessor for later caller projection. Never log, include in exception/evidence or persist its result. This client does not persist or emit artifacts. Metadata evidence and receipts contain no artifact. Tests use synthetic admin/isolated in-memory transport only.

## Historical fulfillment versus current usable access

`historical_fulfilled(intent)` means a validated APPLY receipt for that immutable original operation and body; CLAIM receipt is adoption only. Historical fulfillment can coexist with pending/conflict or newer disabled/changed/expired access. It is not evidence of current delivery, mobile runtime_applied, working VPN or reward success.

`current_usable(intent, now=<caller epoch>)` requires that validated APPLY receipt, applied outcome with no error code, exact current and accepted revision equal to this intent's revision, exact desired/current active state/expiry, future expiry at supplied clock and a nonempty protected artifact. Missing artifact, natural expiry/inactive, mismatched current/revision, pending/conflict and claim all fail this predicate. Caller still owns projection and end-to-end policy; no node/mobile/VPN/reward claim follows from this direct admin predicate.

Read exposes actual pending/conflict plus historical receipt, rather than hiding unresolved delivery. Existing `admin_completion_unknown` and `absent_inactive_unsupported` protocol limits remain. Foreign/malformed correlation is a wire error, never a fallback grant.

## Checks and remaining integration

Focused tests use actual DirectAdapter.execute, real temporary SQLite and accepted fake admin over in-memory fragmented transport. No accepted adapter/payment/trial/history/updater tests are executed. They cover exact claim/apply/replay/read, unknown completion/pending/conflict, historical receipt with newer disable/natural expiry/pending, absent-inactive, exact target/fence/operation/digest/revision/desired/receipt correlation, numeric types/overflow/duplicates/nonfinite/malformed bytes, secret-safe representations/exceptions, fragmented and partial-EOF/oversized/trailing response, timeout and cancellation after a synthetic mutation.

Remaining work: canonical durable intent/receipt persistence and hook integration, business delivery scheduling/projection, authoritative target/fence mapping and legacy writer quiescence/cutover, runtime configuration/installation/enablement and authorized isolated/live acceptance. These are not implemented or authorized by this SOURCE task; root publishes accepted source separately.
