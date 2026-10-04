# Typed inviter delivery — SOURCE contract

Exact published input: 8aac3ff8d20ff67535c57e152acc8040cd1186a8; archive SHA256 86924b297a8361dde834d4c4caa25e1f6e1b450443fffff8723d09f931651710.

## State and eligibility

Existing referral_rewards remains the sole earned reward ledger; days/event/financial provenance are unchanged. New mapped WAITING attempts require verified owner, existing finite active entitlement and a nonempty complete current usable typed set from the actual latest source/current revision and membership. readiness rechecks direct application receipts and mobile current incarnation/generation/source/readback/lease. Claims, historical completion alone, partial or expired proof do not qualify. No qualifying set leaves WAITING without subscription/destination/grant creation. WAITING carrying historical target fields or grant_targets stays unresolved rather than replaying an extension.

sweep_rewards takes mapped owner SHARE before paid-account advisory, reward row and entitlement UPDATE fences. Admission/revoke takes owner UPDATE before binding; grant producer early owner discipline remains. The same transaction extends the existing entitlement once, increments revision, records base/target/revision, captures a distinct immutable reward source and changes WAITING to typed APPLYING. No network call occurs in this transaction. Base_end + existing days is exact; existing extra deadlines are unchanged.

Migration0047 adds source_reward_id and fulfillment_kind/typed_source_id to existing tables. Exclusive paid/trial/reward identity, unique references, exact typed period, immutable source and typed identity are enforced. Existing mobile APPLYING/APPLIED cannot be converted. Empty migration rollback is supported; durable typed rows require forward recovery. Runtime enabling/migration execution is outside this SOURCE delivery.

## Source and proof integration

capture_reward uses ordinary source counters and snapshot policy; reward commercial metadata saves original base/target, days, reward id and eligible plan id. source_plan/entitlement kind remain real. Original paid/trial snapshots, receipts and first-paid anchor are unchanged.

Existing schedule_latest, grant_source, current revision selection, dispatch counters, freeze/current_item, private wire validation, worker/direct client/FakeAdmin paths remain in use. grant_source selects actual latest source for the exact entitlement revision, without paid/trial filtering. _applicable additionally validates exact typed reward/source identity. No new transport or binding, second ledger or business days.

Mixed reconcile retains owner -> benefit -> paid-account discipline and adds reward row before source/token locks. Full applicable current membership proof creates immutable delivery_completion and changes the exact typed reward to APPLIED atomically. Reward branch neither enqueues another reward nor clears an unrelated payment. M1 duplicate outbox insertion remains INSERT DO NOTHING with exact payload validation.

APPLYING sweeps resume the saved source using a stable event cause; no period replay, source recreation or new uncertain RPC. Original physical dispatch/possibly-issued exactwire/unknown-ACK predecessor fences remain. A superseding actual financial/reward revision does not complete the older unresolved reward; its immutable target and APPLYING remain for subsequent policy. Historical APPLIED/completion persists after later joins; current readiness is separate. Completion valid for a direct-only set between a revoke and later rejoin is historical completion, not proof of that future rejoin.

Legacy unmapped mobile WAITING/APPLYING uses its original path. Mapped historical mobile APPLYING keeps stored target/grant evidence without +days or typed conversion. Imported APPLIED receipts remain untouched.

## Verification and limits

Only21 distinct affected cases on disposable PostgreSQL, accepted fake direct Unix transport and actual mobile handlers/FakeAdmin. Raw logs include initial fixture failures and corrected interleavings. No accepted22+5 mixed, worker/capacity/provider/history suites rerun. Concurrent sweeps and actual pg_blocking_pids admission/reward barrier cover changed fences. Migration empty roundtrip/durable rollback refusal and immutable typed identity tested.

Not implemented: full expiry/revoke lifecycle, tombstone reissue, pending older first-paid anchor reconciliation, artifact/caller/inline billing wiring, imported mixed initial activation, production cutover. Same-gateway revoked tombstone cannot silently revive; rejoin proof remains pending. Superseded unresolved reward retains APPLYING, with no invented cancellation/transfer policy. No live DB/runtime/provider/admin/phone/production, enabling, push, or publication occurred.
