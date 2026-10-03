# Protected history coverage (0039)

No migration/default/env activates coverage. One explicit active epoch per database;
its scope is TEST or production, never both. No new public API or account database.
Existing account/benefit lock order and reward worker remain in place.

Operator contract: fence all relevant legacy and canonical history/benefit writers
before preparing the complete membership manifest and retain exclusive ownership
through import and activation. The writer_fence and snapshot/final watermarks are
accepted evidence references, NOT code that stops live writers. Production cutover
is not performed by this module. Late legacy obligations must be reconciled by the
separate single-writer cutover before activating absence proofs.

Call `python -m terlimo_backend.referral_history --manifest PATH --sha256 SHA
--scope test --dsn-file PRIVATE_DSN_FILE` for a full transaction dry-run. Both input
files must be owner-only regular files; DSN is never in argv/output. Default rolls
back everything. Add `--commit` to stage; separately add `--commit --activate` to
activate a complete exact manifest. A staged-only import creates no runnable reward.
Activation/import conflicts roll back the entire batch. No CLI prints personal rows
or database exception values. Set PYTHONPATH=server when running from a checkout.

SHA is `sha256(json.dumps(document,sort_keys=True,separators=(',',':'),ensure_ascii=True).encode())`,
not the pretty-printed file hash. Import immutable envelope schema1:

- epoch_id canonical UUID, scope test|production, complete strict boolean;
- snapshot_watermark, final_watermark, writer_fence: bounded nonempty evidence references;
- members: the COMPLETE verified legacy membership, not merely referred/paying users;
- rewards: original reward receipts.

Each member has telegram_id (positive integer), code (exact ASCII spelling or null),
code_absent_verified (strict bool), referred_by_telegram_id (or null), trial_used,
first_main_paid and dispositions={trial,first_main_paid}. Each disposition is earned,
not_earned or unresolved. Earned requires used history, inviter and original receipt.
not_earned records the operator's reviewed source conclusion, not a fake zero reward.
Unresolved leaves history_pending. Preserve protected source justification with the
manifest digest/watermarks. Missing code without absence proof remains pending.

Import resolves ONLY existing verified accounts.telegram_id; it never fabricates
an installation or claims an unverified owner. Verified account mapping, including
inviter accounts, must be imported by the existing identity procedure first. Existing
staging from an older import conflicts and requires explicit reconciliation; this
module will not replace proof/code/referred_by or erase prior receipts.

Reward shape is source_id, invitee_telegram_id, inviter_telegram_id, event_kind
(trial|first_main_paid), days, state, evidence. Evidence has source_reference,
source_sha256 and target. WAITING requires target=null. APPLIED, PENDING_REMOTE,
REMOTE_APPLIED require target={reference,sha256,base_ends_at,target_ends_at}; reference
points to the protected exact target/grant/revision receipt, UTC-aware timestamps
must differ by exactly days. SHA fields are lowercase64hex. APPLIED is never replayed.
WAITING enters the existing once-only worker only at activation. Intermediate remote
states are retained in the immutable manifest, never inserted as runnable WAITING;
the affected member stays pending. Reconciliation of these states requires a later
explicit reviewed import evolution, not mutation/relabeling of the accepted epoch.

Initializer handles an active epoch member or a proven-absent verified Telegram
account. A new local INSERT alone is insufficient. It conservatively incorporates
existing canonical trial entitlements and succeeded MAIN payment ownership, ORs
existing used flags, and stamps epoch/proof. Once ready it never reclassifies new
post-cutover events as imported history. Legacy absent-code generation preserves
used flags and parent; random codes cannot steal staged legacy codes. Conflicting
preexisting canonical code/attribution for an absent identity stays pending.

Rollback retains epochs/staging/rewards and used flags. 0039 down refuses any epoch
or import evidence. No DB restore/down migration as an eligibility reset.

Production limitation: pinned deployed direct-writer/full membership/reward target
bundle + fence/catchup receipt remains necessary before any real activation. Tests
use synthetic verified identities in a disposable PG, not production PII. This slice
DOES NOT replace bot/site purchases with app navigation, change their adapters,
change mobile wire, or implement runtime writer fencing.
