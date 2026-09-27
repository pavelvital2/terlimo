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
