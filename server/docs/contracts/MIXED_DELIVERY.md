# Activated account typed delivery — SOURCE candidate

Base: published 9486a6c7fdaa6ba5e382b7137f5e0c69036225a9; supplied exact server
archive SHA256 356bd8315a5ee6da63c09bbbea1fdf0ad6d8f358f574c6c7bd1ca0d390ef21a9.
No runtime enablement is included. Inject `MixedDeliveryHandlers(settings).as_handlers()`
into the existing worker alongside accepted external and gateway handlers for SOURCE tests.
Root owns publication and any later runtime decision.

## Data and identity

0046 is additive. `delivery_fulfillments` remains the immutable commercial identity:
original source sequence, revision, credited product, order/months, snapshot and rank
deadlines never change. Existing frozen legacy items remain untouched.
`capacity_heads.membership_revision` advances through the existing per-account sequence
counter on actual admission/release. Counter values also allocate plan dispatch order;
financial latest means maximum sequence of actual financial source rows, not counter head.

`delivery_plans` freezes a nonempty, ordered typed admission set, exact original manifest,
source, membership revision and destination digest. Each mobile admission includes real
binding UUID and incarnation generation. Direct identity uses existing physical claim.
Immutable plan/item/proof/completion triggers and partial uniqueness retain legacy rows.
Each new same-source membership/destination generation gets a fresh monotonic dispatch
sequence and separate items. No synthetic installation, identity or second billing source.
One mobile admission consumes one rank regardless of number of authorized gateways.

`delivery_mobile_items.required_grants` contains only grants already selected/issued by
ordinary authorized mobile paths. Planner never scans registry gateways to choose new
ones. Empty destination remains pending. Existing ensure_grant correlates actual grant
with exact latest captured entitlement/revision/source and binding generation, advancing
its ordinary desired generation when correlation changes. It does not alter mobile wire.
Mapped grant metadata is closed against hour grants. Unmapped metadata remains null.

## Scheduling and API

Private APIs: `queue(source_id,cause)`, `membership_changed(account_id)`,
`grant_source(row)`, `grant_event(grant_id,cause)`, `freeze(source,settings)`,
`mobile_receipt(source,item)`, `readiness(source_id)`; existing `fulfillment_proof`
delegates to readiness only for activated scopes. Existing protected planner call queues
reconciliation for activated scope instead of freezing an incomplete legacy direct set.

Capture, activation, actual mobile append/release, desired grant and authoritative grant
or direct proof enqueue stable existing outbox reconcile events. Grant desired event is
inserted before its gateway apply in the same transaction. Mapped gateway handler refuses
first RPC until exact source/binding/generation/grant required set is durably frozen.
A concurrent scheduling race raises `mixed_plan_not_frozen` locally and uses existing
bounded worker backoff; worker currently records exception class `RuntimeError`.
Incomplete reconcile jobs finish examination; later normal membership/destination/proof
events create catchup examination, without SQL resets or another scheduler.

Direct preparation uses delivery dispatch order against physical source frontier. Unsent
older work is superseded; possibly-issued old commands retain original exact wire. W1
parking, token/CAS, durable wakeup and unknown-ACK boundaries remain the existing machinery.
A newer plan never clears an unresolved issued predecessor. No shared worker/client/adapter
protocol edits.

## Actual SQL lock inventory

- Registration: existing bind-account advisory -> mapped account FOR UPDATE -> existing
  installation/link/binding rows -> append/head/counter/outbox. Unmapped owner lock unchanged.
- Authorized device delete: bind-account advisory -> mapped account FOR UPDATE -> session
  revalidation -> installation/link/binding -> revoke/release/head/outbox.
- Activation: bind-account advisory -> account FOR UPDATE -> existing owner/proof/capacity
  checks -> scope/admissions/head. First activation serializes with ordinary confirmations.
- Original paid credit: order FOR UPDATE -> existing early account SHARE/KEY SHARE ->
  benefit -> paid-account -> binding/entitlement -> original capture -> ordinary desired
  grant/outbox. No new late bind-account advisory under these locks.
- Ordinary mobile trial: early owner KEY SHARE added before existing paid-account and
  binding/trial locks; actual new trial insertion captures once only for mapped account.
- Reconcile: preliminary source read for keys -> original order FOR UPDATE if paid ->
  verified account FOR SHARE -> benefit advisory/row -> paid-account advisory -> source
  FOR UPDATE -> exact worker token row -> grants/plans/proofs/completion. Owner SHARE
  blocks mapped admission UPDATE for the whole full-set freeze/completion transaction.
- Direct prepare: existing early admission lock/owner UPDATE -> source -> physical -> item
  and token; SQL ends before Unix RPC. Existing direct proof transaction token-fenced.
- Gateway RPC uses the existing actual handler outside SQL. Only the short publication
  transaction adds exact mapped worker-token validation before grant generation CAS,
  trial proof and aggregate event; route/node fields commit with that same publication.

No external transport RPC is introduced inside freeze/completion SQL. The original mobile
trial channel-check implementation is retained; it is not a new transport or identity seam.

## Receipt and business completion

Direct proof requires immutable accepted application receipt, exact current proven
physical revision, usable observation and unexpired active proven snapshot. Mobile proof
requires live exact same-account binding incarnation, live shared rank, nonempty required
grants, matching source and desired/applied generation, lease expiry within original rank
deadline, actual authoritative runtime-applied/non-revoked readback, opaque grant identity,
registration fingerprint, node, gateway generation, lease sequence and exact expires_at.
Raw gateway generation and lease sequence are strings in the existing wire. Readback passwords,
credentials, artifacts and SPKI are not stored in new receipts.

`check_batch` additionally checks latest actual financial source/revision, verified owner,
current full typed membership/head and latest dispatch generation. No empty/partial set
can complete. `delivery_completions` is immutable and unique by source; accepted historical
completion remains after a join/leave or renewal while current readiness may become pending.
In the same transaction, full proof validates succeeded original order/account/applied
entitlement/credited revision, marks needs_grant=false and invokes existing unique reward
with ORIGINAL order/months/entitlement. Trial uses original source bonus marker and existing
history/eligibility checks in enqueue_reward. Mapped mobile enqueue and individual trial
proof no longer bypass aggregate. First-paid anchor and discount consumption timing unchanged.

## Exact retained constraints and scope

- Existing UNIQUE(binding_id,gateway_id) and permanent revoked tombstone make ensure_grant
  return `revoked` for the same gateway after binding reactivation. This candidate does not
  revive or overwrite it. The old binding incarnation proof is rejected, direct rank stays
  stable, current readiness remains pending until an ordinary authorized valid destination
  exists. Any grant incarnation/reissue policy requires separate model decision.
- A pending older source superseded by a new commercial revision cannot newly complete:
  current matching source fence rejects it. Issued original direct proof is still recorded
  and releases successors. First-paid anchor remains original; this candidate never transfers
  that reward to a later renewal. Historical already completed receipt/reward remains intact.
- No reconstruction of pre-capture or unsequenced legacy sources from mutated entitlements.
- Existing inviter WAITING/APPLYING mobile fulfillment and imported reward receipts remain;
  grants with no matching source are allowed ordinary issuance but cannot prove aggregate.
  No inactive inviter revival, new subscription or external inviter fulfillment is added.
- External inviter APPLYING, expiry/revoke lifecycle batches, artifact/caller projection,
  imported mixed INITIAL activation/cutover and production release remain separate tasks.
- SOURCE only: disposable PG/SQLite and accepted fake transports; no live DB, provider,
  invoice/payment, phone, production, service installation/enable/restart, push or other-executor
  messages. No accepted broad suites repeated.

0046 down is allowed only with no plan/completion, no advanced membership and no correlated
grant; otherwise refuses durable state and requires forward recovery. Local source rollback
is reconstruction baseline; deployed rollback is not performed or authorized in this slice.

## M1/M2 bounded correction after independent review

M1: mixed queue now has its own `INSERT ... ON CONFLICT DO NOTHING` and separate
ordinary SELECT of the committed row, verifying exact operation type and immutable
payload before returning original ID. It never UPDATEs that duplicate event. Shared
external `_enqueue` and unrelated enqueue semantics are unchanged. Reconcile retains
its exact `_token` FOR UPDATE before freeze/ensure and completion. Producer may hold
binding/grant while reading the committed duplicate; token holder no longer creates
the opposite UPDATE wait edge. No new ID, reset, payload rewrite or token removal.

M2: `grant_owner_lock(account_id)` acquires KEY SHARE only for activated scope.
`ensure_grant` first reads binding owner without row lock, takes that owner fence,
then executes original authoritative binding FOR UPDATE JOIN entitlement query and
rejects any different current owner. Existing caller transaction retains fence through
grant/outbox FK insertion. Unmapped owners acquire no added row lock. A reentrant
KEY SHARE by an already owner-fenced caller adds no new lock acquisition edge.

Actual callers inspected and corrected:

| Entry | Owner before subordinate locks |
|---|---|
| Ordinary ensure, cap_existing_extra_grants maintenance | Within existing transaction, owner pre-read -> mapped owner KEY SHARE -> authoritative binding/current-owner check -> grant -> outbox FK |
| Mobile access-sync | Read-only authenticate_session -> mapped owner KEY SHARE -> receipt/catalog revision/grant-recovery mutations -> ensure reentrant owner fence -> binding/grant/outbox |
| Paid original credit | Existing consume/paid_account_locks already takes owner SHARE/KEY SHARE before benefit/paid-account/entitlement; ensure reacquires held fence |
| Paid needs_grant retry | Ordinary _enqueue_paid_grant only reads order/binding/installation/select_gateway before ensure's early owner fence; no prior row/paid lock |
| Inviter WAITING/APPLYING mobile reward sweep | Added mapped owner KEY SHARE BEFORE paid-account advisory/reward/entitlement rows; both ensure calls reacquire held fence |
| Mixed reconcile | Existing owner SHARE before benefit/paid-account/source/token/ensure; reentrant KEY SHARE, no late bind advisory |
| Authorized mapped revoke/confirmation | Existing bind-account -> owner UPDATE -> binding/grant/admission, now excluded by earlier grant producer owner KEY SHARE |

No RPC introduced in these SQL paths. No source schema, proof, business reward,
W1, shared worker/client/adapter, tombstone or pending superseded-source policy change.

Correction tests are separate from accepted 22 mixed cases. M1 uses actual original
committed desired event, actual CLAIM_SQL and worker._process; ordinary unchanged
ensure pauses after actual B/G row locks, pg_blocking_pids proves worker waiting on B,
and FOR UPDATE NOWAIT proves worker holds event token. Duplicate enqueue then completes
and worker finalizes original event done, one immutable key/payload and normal plan.
M2 pauses actual maintenance shortening ensure after B/G lock, proves authorized revoke
blocks on maintenance's earlier owner fence, verifies compatible owner KEY SHARE and
binding NOWAIT conflict, then refresh and revoke complete serially with generation+2,
revoked binding/incarnation and released mobile admission. Owner-change tests alter both
binding and entitlement owners after actual pre-read; old owner cannot mint a grant even
when later JOIN would otherwise match. Unmapped unchanged ensure finishes under another
connection's owner UPDATE, proving no added unmapped owner row lock.
