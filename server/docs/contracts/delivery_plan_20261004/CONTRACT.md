# Delivery registry / immutable planner — SOURCE boundary

Base `3bb1caceb16a91c467ecf6680fcd1dc4ffbcf6df`. This implements only the narrowed registry/source/planner slice. The prior owner-transfer contract is retained in the result package as `PRIOR_CONTRACT.md`; its proposed admission/worker/reward changes are **not** implemented here. Root narrowing takes precedence: mixed order is a conflict, never mobile preference or automatic direct eviction.

## Storage and real APIs

Migration `0043_delivery_plan` adds five small related tables in the existing database:

* `delivery_manifests`: immutable operator manifest UUID, account FK, canonical JSON digest/body, staged/conflict and reasons. Neither state enables access.
* `delivery_targets`: immutable versioned manifest target evidence, ordinal and typed mobile binding FK or direct deployment/key. Physical direct identity is `(deployment,external_key)`, **not** the row UUID; same-account later manifests can retain it. Stage serializes physical identities and rejects moving one to a different account. A later worker must queue by physical identity across manifest versions. No pairing inferred.
* `delivery_mapping_proofs`: reserved immutable receipt storage for a **future authenticated ownership-transfer commit**. No production writer, route, CLI option or `promote_owned` helper exists. Synthetic test INSERTs are explicitly labelled. A caller-supplied proof/bool cannot enter the planner API.
* `delivery_fulfillments`: unique paid order FK or trial entitlement FK, account/entitlement/revision and immutable source JSON/digest. States only awaiting_mapping/planned; neither means delivered. No fulfilled state in this slice.
* `delivery_items`: unique batch/target, stable operation UUID, immutable desired state/deadline/source digest. Initially unprepared and request/expected revision/digest NULL. Prepared closed shape reserved for later queuehead code; no preparation writer here. Frozen prepared request cannot change.

Down refuses any manifest or fulfillment data. FK source history, immutable triggers, exact replay guards prevent deletion/rewrite through ordinary APIs. This is private canonical storage, not an authorization mechanism against a privileged DB operator.

`delivery_plan.capture_paid(connection, order_id)` is called inside original `apply_paid_entitlement` after credited_product/applied_entitlement write, in both subscription and addon branches, only for telegram_account. It snapshots current exact revision, original order quote/amount/currency/months/receipt, source plan, starts/end, base/device limit, all extra slot IDs/deadlines and calculated absolute rank deadlines. Capture failure rolls back logical credit too. Old order replay keeps the original batch; no reconstruction from later entitlement mutation. Account needs_grant stays true; no grant/reward/outbox writes added.

`capture_trial(connection,row,moment)` is called only immediately after trusted `insert_trial` in its original transaction. Trial replay returns its existing entitlement and leaves the unique original source batch unchanged. Mobile-created/racing or pre-0043 trials are not backfilled from mutable state. Historical applied orders/trials lacking an original snapshot require separate reviewed cutover evidence; this slice does not invent it.

`capacity_deadlines(snapshot)` reproduces finite paid base seats plus live-at-capture extras sorted by expiry descending, each capped by main end. Slot IDs are evidence, not permanent device assignments. Trial/imported/nonfinite use existing stored device limit/default two. The result is frozen as `rank_deadlines` at source capture. This helper is **not wired into live mobile admission/ranking**.

`stage_manifest(connection, body, allowlist, dry_run=True)` validates the entire closed manifest before writes, checks verified account, exact current mobile binding ownership/bound_at/order and protected direct allowlist. It records conflict for mixed sets, incomplete/reordered mobile sets, non-contiguous direct device_index, overcapacity or no current capacity. It never truncates the set, reserves a seat, changes entitlement/grant or marks ownership proven. Duplicate/foreign identities, malformed fields and changed same-manifest body fail. Exact replay returns immutable prior result even if mutable capacity changed; a new manifest is an explicit new staging action, still not proof.

`freeze_plan(connection, fulfillment_id, manifest_id)` accepts IDs only. It reads the stored future-proof receipt matching a staged same-account manifest and complete target digest; absent/mismatched proof fails with source still awaiting_mapping. It locks the source, freezes all target items once, returns same batch on concurrent replay, rejects another manifest after freeze. Empty sets and insufficient original source capacity fail. It uses original captured per-rank deadlines even after later renewal/addon. Direct deadlines floor to whole UTC seconds once (v3 protocol, never round access up); snapshot retains original precision. Late planning creates inactive desired state with the original deadline, never now+duration. Nonfinite direct deadlines remain NULL; later protocol preparation must reject unsupported active NULL expiry, not invent a lifetime date. No operation request is prepared or sent here.

An accepted staged set is evidence at staging time, **not live admission proof**. Future ownership/admission integration must establish a complete current set, serialize additions/retirement and handle later source/revoke revisions. It may not consume staging rows as enforcement activation. Mixed/overcapacity conflicts need accepted complete order/evidence before a new proof; no automatic preference.

## Protected operator use

`server/tools/delivery_stage.py --manifest FILE --allowlist FILE --dsn-fd FD [--stage]`.
Both local input files must be owner-only regular non-symlink files <=256KiB. Duplicate/nonfinite JSON rejected. DSN only inherited FD, never CLI text/output; failures omit exception/connection details. Default is dry-run; `--stage` writes staging only. No migration execution, network adapter, claim, remote read or activation option. Current task does not run this against any service.

Exact example is `operator-fixture.json` (TEST identities only). Allowlist is independent protected operator configuration with deployment/key -> exact account/grant. UUIDv5 grant identity must match the adapter namespace. Expected base is state, whole UTC-second expiry or absent/null, and nonnegative int64 revision. Evidence SHA refers to retained verified manifest/fence/cutover material; storing it does not assert that production cutover occurred. No artifact/password/Telegram ID is stored.

## Locks / atomicity

Paid hook adds only reads and source INSERT under existing order -> account SHARE -> benefit -> paid-account -> entitlement locks. It does not reacquire an account/advisory lock after entitlement; FK KEY SHARE is compatible. Trusted trial keeps existing account SHARE -> paid-account -> trial order and captures before commit.

Stage: account SHARE -> private account staging advisory -> sorted physical direct target advisories -> inserts. It does not lock an entitlement or acquire benefit/paid-account; its capacity view can be conservative while an uncommitted credit exists, and cannot activate access. Real barrier test holds original paid transaction: same-order replay waits, stage completes with no_current_capacity conflict, original source commits once.

Freeze: account SHARE -> fulfillment row lock -> immutable registry/proof reads -> items and planned update in one short transaction. No source writer acquires that fulfillment lock before account ownership. No path here performs external I/O. Production proof/request/receipt transitions and predecessor locking are not supplied yet.

## Checks and remaining integration

Only new targeted temporary PostgreSQL tests plus pure capacity checks. Tests use synthetic accounts/orders, fake membership and explicitly synthetic proof inserts, never WDTT/Platega or product DB. One affected mobile paid check verifies receipt/binding and prior reward behavior with zero new external batches. Exact run history/counts live in package CHECKS.md; no prior suites rerun.

Remaining: authenticated claim/proof writer; mapping cutover/admission locks and one shared mobile+direct capacity gate; mobile/gateway ranking and aggregate source fulfillment; queuehead-only expected revision/request freeze after confirmed predecessor; existing OutboxWorker dispatch/strict v3 client integration, durable ACK/proof/current-revision validation and unresolved issued blocking successor; expiry/revoke/new revisions; aggregate reward/needs_grant handling only after the complete required target set; inviter external reward branch (inactive stays WAITING); bot/site caller, artifact delivery and production import/fence/catchup. Nothing in this slice clears pending or claims VPN/external delivery PASS.
