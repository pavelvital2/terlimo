# Recovery v1: integration + cleanup, 03.10.2026

Source-only follow-up `whitelist-20261003-recovery-client-join-cleanup-v1`.
Existing branch `laptop/recovery-code-client-20261003`, delta base
`aca327d9db798bbc9d77bd75c6451aa645efd8eb`, cumulative base
`32cac8dd571f8142c629d137503d00da53cc04a6`. Root is the sole publisher.
Exact resulting source/Android tree and patch hashes accompany the handoff.
This supersedes the four-field envelope and deferred ordinary update in the first checkpoint.

## Shared wire

`GET /api/mobile/v1/service-seed` decodes exactly five fields: request_id,
server_time, schema_version, status, recovery_code. schema_version is string
"1.0"; server_time uses existing RFC3339 UTC validation. Unknown, duplicate,
missing, alias, null and incorrectly typed metadata remain rejected.

The attached shared server vector is retained byte-identical at
`go_client/testdata/recovery-v1-server-vector.json`, SHA-256
`1d0d90591d33831753814136daa0ddad58bc557710565bbee7c296194e5a4d96`.
Its request_id actually contains 34 hex characters, despite the task description.
A negative assertion preserves this rejection. The positive HTTP Client decode →
Go Ed25519 verifier test corrects only request_id to the explicit synthetic 32-hex
value; signed code, public key, original payload and Seed are unchanged.
The deterministic server vector key is TEST-only, never a deployment key.
Root was notified to correct the canonical fixture metadata. No decoder relaxation.

## Ordinary update ownership

- The existing Runner calls one optional hook synchronously after successful
  verified/browse catalogue publication and attempt finish, before its idle wait.
  Publication must actually reach the bridge writer. Busy boundaries defer only
  to later natural successful cycles. One consumed attempt per service lifetime,
  including unavailable/invalid/cancelled outcomes; no retry worker/timer/client.
- Optional HTTP reuses an already open usable service connection. Cold, full or
  idle-expired transport does not dial just for this feature. It uses an existing
  cached bearer without Ensure/reauth. The session subject/token/generation are
  rechecked before commit. Missing packaged verifier disables this feature only.
- Existing 15-second request budget is shortened by the next normal cycle's
  deadline. Optional elapsed time is deducted from the existing idle wait; no
  catalogue/renewal timeout is expanded.
- Bridge control receipt preempts before enqueue. A short receipt guard plus
  pending foreground queues closes the receipt→enqueue registration gap.
  Runner foreground triggers and normal Doer requests also cancel optional work
  before they contend on Store/Channel. The optional lease lasts through persist ACK.
- Host foreground intent/command epochs, attempt/installation and the original
  namespace writer guard reject queued stale writes. Host deadline is checked
  inside that guard. Only service_seed_v1 changes; Store is not locked during GET.
- Signature/HTTP/writer failure retains old seed and leaves primary UI successful.
  Accepted endpoint/pin/hash changes are used at the next natural establishment.
  A live connection retains its own seed identity and is not evicted just by update.

### Exact transport limitation

Channel still serializes I/O. A foreground operation can cancel an in-flight
optional exchange immediately; the existing cancellation path closes that shared
connection and the foreground may need its normal bounded re-establishment.
This is preemption, not a guarantee of zero added foreground latency. Optional
work itself never establishes a connection. Eliminating even this effect would
require protocol-level multiplexing/draining or a separate connection, outside
this bounded implementation. No broader transport redesign was performed.

## UI cleanup

Removed old import/link/paste/QR/file/replace/saved-subscription controls and
ACTION_VIEW/ACTION_SEND import callbacks. Removed obsolete scanner activity and
camera declaration. Existing mobile resume and catalogue refresh, five tabs,
commerce, Telegram, account, devices and the single Settings Recovery editor remain.
Internal dormant legacy storage/core remains; no data deletion or migration.
Saved legacy links retain the explicit unsupported Recovery boundary.

## Targeted local evidence

All Go runs: GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off.

| Command/selection | Result |
| --- | --- |
| go test ./accountaccess -run '^TestRecoveryServiceSeed' -count=1 | PASS, 4 groups, shared five-field HTTP + signature join |
| go test ./servicechannel -run '^TestOptionalSeed' -count=1 | PASS, 6 groups: reuse-only, foreground preemption, lease, next-connection seed |
| go test . ./accountaccess -run '^TestIdleSeed' -count=1 | PASS, 2 native groups (7 outcome cases) + 6 Runner groups |
| OptionalSeedWriterTest | PASS, 3 JVM tests: current epoch, stale/foreign/cancelled writer, foreground classification |
| ImportSurfaceSourceTest | PASS, 5 JVM tests |
| UserStatusTextTest: 3 changed-copy methods | PASS, 3 JVM tests |
| UI source/XML checks | PASS, 6 bounded groups |
| Kotlin production + test compilation | PASS as part of the selected JVM run |
| git diff --check | PASS |

Gradle offline, max-workers2, heap1536m, excluding verifyNativeInput solely for
source/JVM verification. No assemble/native Android artifact, install, ADB, phone,
server/provider or deployment changes. Accepted manual Recovery/payment/ping/CAPTCHA
suites were not repeated. Runner/native targeted tests were repeated only after
closing receipt priority and next-cycle deadline gaps. The initial shared-fixture
failure is retained, as is a compile-only selector run (not counted as test evidence).

Logs and source receipts:
`/home/pavel/step036-receipts/recovery-client-join-cleanup-20261003`.
Actual TEST verifier/config and exact server package join, shared APK build and
phone acceptance remain with the coordinator's next authorized stage. Source PASS
is not device/product PASS.
