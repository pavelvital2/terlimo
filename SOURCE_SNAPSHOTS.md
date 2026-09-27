# Snapshot provenance — 2026-09-27

| Component | Original source commit | Original full tree | Local export |
| --- | --- | --- | --- |
| gateway | `59bf41c0dea9c486fec5540d0b6ef72dc686ce48` | `5f64a6dafee86ead036c39511e0c2793fda23223` | Filtered source tar SHA-256 `60207be3b546f365fab65b74587360ea6818caffd7e0d3e8ef058bd68b8df93d` |
| server | `c526b242d17b059690472d3e079dd1b7f73153d0` | `ef33979387743b892fec63f0c408455d01465362` | Filtered source tar SHA-256 `ebb71cd850389c20aaa06a6b2b033d410b805955c7db2946af6fcfdb25b9f7ee` |
| clients/android | `3eaea7121a917940ba1fa2183989023d4742f767` | `2aac64db5208c6ebbf56463286eae05822acc2c5` | Selected `app/`, `testapp/`, `go_client/`, Gradle, build tools, docs fixtures from Git archive; private `test-mobile.json` omitted. |

Gateway and server sources were exported from TEST A's persistent Git object databases. Android sources came from the accepted Laptop worktree commit rather than its dirty local TEST overlay. This repository has no live secrets, DB, profile, logs or compiled application artifacts by design. A local source backup is not proof of TEST B parity, production readiness or payment acceptance.

Follow-up source classification: `gateway/udp_listener_test.go` matches the pinned commit byte-for-byte (SHA-256 `be93b515b2040f3a0562aaa7a1e7b493180da16cd4d31aa1facea0eeee6215f0`); its flagged literal is confined to a loopback/in-memory DTLS test. `server/tools/gateway_integration_03_3.py` is a sanitized copy (SHA-256 `ec176e4bdd0bf0220139fd64c8563cd56c9b3a0abcbfe042162fe12c9a042a65`), with only the fixed password at original line 50 replaced by a placeholder. The original unredacted file is not part of this backup.

## Android checkout source acceptance — 2026-09-27

Integrated source: `46cde3aadd360a65791fbc781fd9f2e0b0900f3c`, tree `83cdaa766f72c4372716b8fbd6d8c2c56d3dc7e9`, based on `3eaea7121a917940ba1fa2183989023d4742f767`. Full patch SHA-256 `2d3641d743b615c9cdcfed1688c7602efca0a86b7885f82d4944b3affccff79c`. Existing subscription-binding tests from `a762198` retained.

Reviewed explicit browser-open policy, single-flight purchase correlation, truthful write/terminal handling, and capture identity release. Executor evidence: 48 focused tests passed before final two-file identity correction; final PaymentCreateCorrelationTest 14/14 passed. These are successive scoped runs, not a full-suite result. Source accepted; APK installation and phone checkout acceptance remain pending. TEST backend compatible source: `554151b`. Real payment provider remains disabled; no real checkout/payment success claimed.

## Expired-hour paid registration UI — 2026-09-27

Accepted Android source `c4e8a1532518368ccdce0895628bdc1d5042d0c3`, tree `3549a74cf60389539378d08671b5706510270cdf`, based on build `d06dac1`. Patch SHA-256 `8c777a33b61f6c5f25e964184e397a484cfd2eabb8ed379901388774f87e79c3`. Offers Telegram registration for paid orders awaiting binding after hour expiry; hides additional quote/payment controls in that state, preserves normal purchase visibility and registration error messages. Executor reports 30 focused tests passed; root reviewed all eight changed files. Source acceptance only; updated APK/phone state validation pending. Backend parked-payment binding is already implemented in `554151b`; real payment-provider E2E remains unverified.

## Catalog country length compatibility — 2026-09-27

Android source `1961d089b58f7997f6fed85702adead28e87f462`, tree `91223e4d926d0a0a7db1a610cc681b62df8bbaac`, patch SHA-256 `1752b8c029dddf6afdae78af381942750d4430fd495310ab0497cf03cfc0da84`. Credential parser and retained-cache decoder now accept country-code lengths up to eight; existing cache uppercase validation remains. Root reviewed all four changed files; executor reports NodeSelectionTest/CatalogCacheTest 32/32 passed. No new phone or large-catalog test. APK deployment pending.

## Notification traffic at zero — 2026-09-27

Android source `19961c5a244e8fe34f4511bb69e074a68e8b219f`, tree `b5ce44885f9ffce4e8d02ef0882fb06b5db945cb`; patch SHA-256 `27b7d95a6db9d81c53405d1cb873ce627086db7ee09db281793d01c44b42b4b8`. Connected notification now includes incoming/outgoing totals even at zero. Root reviewed three-file diff; executor reports NotificationTextTest 2 and OnboardingHourSourceTest 6 passed. Workers/stream telemetry remains unresolved; this is partial source acceptance, not full notification acceptance. New APK pending. Previous country source1961d08 installed APK `4d18b0e6055e4486b907cab9a07438be9d5d5361ec13cd82e4da2572190beeff`, built/installed artifact hashes independently matched.

## Channel counts in quality and notification — 2026-09-27

Accepted source `ea7110be997f32dd5affe5df0e9019d562f00673`, tree `94dc539b7b60bedef590d79eeb7e0d4271acef57`, based on19961c5. Patch SHA256 `3af5847931dfc7670d667cf775734f57f1f8c64caa83f0c998ab1d5d71ace12b`. Local native bridge carries registered active channels versus configured maximum, fenced by attempt/runtime/lifecycle. Shared display preserves existing wake-ready priority; ordinary lifecycle revision0 is accepted. Sender uses runtime cancellation and1s timeout. Root reviewed original and correction diffs and inspected Kotlin XML7/7 PASS; executor reports focused Go lifecycle/dispatcher tests passed. Device/build validation pending. No server/network protocol change.

## Server-defined channel denominator — 2026-09-27

Accepted Android source `af5f7ebf58fe1ada4066601fe4246fd86d98e47a`, tree `35ac4a63dc3951b77ccc398a9ac474cdfc221dbf`. Incremental patch SHA256 `c227b624d1322c7c86edb21b63e7d35d5b2c22549b086cc034fa8b60af2b5b92`. Shared quality/notification projection uses server-configured target rather than variable registered-slot total during wake. Missing target is shown without a denominator; no hardcoded36. Existing supported parser cap36 unchanged. Root reviewed four-file diff; executor reports ChannelsStatus8/WakeRecoveryProjection4 tests passed. Native unchanged; APK update pending.

## Card payment method parity — 2026-09-27

Accepted backend source `95e270a30902267319e8c7223380e4d99b52cc26`, tree `6020057c706d17bc21257c2dfde761e914452372`, base554151b. Full patch SHA256 `0f30750fd92b9b84c3e96838c84dbc71f445cc24c38eafc7cefd1e575447475b`. Public card aliases provider international; numeric provider method remains configurable. Production read-only source/config confirms current override11. Migration0032 extends quote methods; rollback refuses existing card quotes without deleting them. Root reviewed initial and correction patches; executor reports64 affected tests passed. TEST rollout pending, provider disabled; no real invoice/payment acceptance.

## Routing editor — 2026-09-27

Accepted Android source `0ebb0230fc03eada48ab4b60f56cb932e95b588a`, tree `dece6bcfb22946e909961618c9ebae57696f5e2a`, based on af5f7eb. Integrated reviewed patches SHA256 `620a3be4d48b7cc91f61ff8b1c461235327e1b7de1b1bc3ad408c0f6ddaa2af0` and `c3c860798001ac515fe6808cb34267679cdae8ed418e4b93c2bffdab75c7a14e`. Search filters display without clearing selection, system apps hidden by default, scenario mode labels and one dynamic instruction follow saved/current mode. Existing whitelist additions and routing policy retained. Executor reports initial 26 focused passes and correction RoutingSystemFilterTest 12/12; root reviewed both patches. APK/UI acceptance pending. No native/server changes.
