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

## Devices source — 2026-09-27

Accepted backend `21878c78fde61217ea6fda5da06d944bc654a2b1`, tree `b9b97ec1661a1122bed002e29888e6bc7f038a7f`, from95e270a. Full patch SHA256 `97ed830c71f9dab3e507ba06f4676c0bf870f2e4aed829437b8beb9adae6534e`. Account device management, full-slot Telegram proof without data access, generation-fenced removal/rebinding, canonical devices DTO, metadata migration0033. Root reviewed successive corrections; executor reports21device tests and52related checks passed in final iteration. Gateway readback test uses wire-faithful fake, not live gateway acceptance. Five existing active-plan test failures remain reported on base; not a green full suite. Required additive canonical errors schema patch preserved in server/docs/contracts/devices-errors-schema.patch (external contracts repo separate). TEST rollout and Android integration pending.


2026-09-27: server service-channel source accepted 318ff77d30fda1a941f7f87e68d52616a0e94fdc / tree 5da4edb3b49138e50c05b3263aa420435c43848b, parent21878c7. Exact GET devices and DELETE devices/{uuid}; unchanged header forwarding. Patch SHA256021051f3db7f083f218d72befb23ab28614745282794fdd0324f58a6d499de66. Executor reports16focused relay tests passed; root reviewed diff. Live application and phone acceptance pending.

2026-09-27 Android source accepted9dc932e7ac7ffc5dd8ef5cd1a4100842d652fbd7/tree46335312d0ca28f900c84c4cc2b40af48b09fd95, base0ebb023. Devices list/delete canonical DTO, request correlation and stop sideeffect gate. FullpatchSHAa6b996657bcd776be975c77cac0d5b8fae4410693357269a543f0c68fbf74cce. FocusedKotlin11+4PASS/compile0; previous unchangedGo tests/build PASS. Root reviewed correction. Compatible server318ff77/schema0033. APK/phone devices acceptance pending; operation polling not implemented.

2026-09-27 source-only Doer correction accepted2c343d3c3ae7cb2f7398dbb2d52f87c67380cafe/tree3105e87d5c54bf85f80a3d6774869f45b3a249bd, patch1d88804a0a178be3b432591e85a16791c4246832a9f0452121d607e225eb1015. FullDoer client GET/DELETE tests PASS; phone devices NOT accepted: old GET local rejection unknown, new build blocked before ME at VK establish. Installed APKf35a85c7; prior known rollbackbb9d3ba8.


Gateway source checkpoint 2026-09-27 UTC: 2619c860809c4fa14c1e4c9c3277ea46e629545f, tree8176f6ad156385765bf6b7cfcfc3556a6872a5b9 (base59bf41c). Exact GET devices and DELETE devices UUID allowed in node validator; other routes unchanged. Focused Service tests passed per executor receipt; patch reviewed by project lead. Candidate binary e27efa76c196911e15700668395fe34f9e1eb1ba682f8b4ff3fa533acd9aeb99, Go1.26.5 (live predecessor Go1.26.4). TEST rollout assigned, not yet verified; phone devices acceptance remains open. Server318ff77 compatible.


Backend source checkpoint f8875757223d3610f428414cc4436afb0257f65b/tree433026ff0203b1610944c8df4f5b59d29c282891, base318ff77. Echo validated request_id on service parse-stage rejection; preserves validation/caps. Root reviewed patch49c34ecb; executor reports 17 focused tests passing and reproducer failing before fix. Devices BAD_RESPONSE traced to stale API process rejecting devices and zero error request_id. API-only rollout assigned, pending runtime receipt; phone acceptance still open. Gateway2619c86 already active.


Android devices GET accepted 2026-09-27 21:28Z: source9bb9ab3241a90bfe5d708d3a493cc98c439599f6/tree0fd91e574385304cb500d9ef2e432f8aa8d389df, APK6b261597fe0dc879fb14bedc4b5b31c97b521fbed8f8adf22bc80d3f980c167e. Ordinary SEND_NOW refresh produced current Android row; root correlated API GET /devices 200 at21:28:55.342UTC. Explicit disconnect clean, no force-stop. Compatible gateway2619c86 active and APIf887575 reloaded at21:22:26UTC, service probes matchingrequestid/readiness24of24. DELETE/revoke/fullslot replacement and cold-start branch remain outside this acceptance. Latest focused Kotlin/Go tests passed by receipt; earlier bdb1ef8 Kotlin claim was withdrawn and repaired in8d313f1. Source only, no overlay/APK copied.

## 2026-09-28 ordinary device deletion acceptance

Android 9bb9ab3 / APK 6b261597fe0dc879fb14bedc4b5b31c97b521fbed8f8adf22bc80d3f980c167e, gateway 2619c86, backend f887575: one explicit noncurrent TEST device DELETE returned successfully, list slots changed 2/2 to 1/2, current device preserved. Server confirmed explicit gateway revoke with generation/readback before grant expiry. Ordinary deletion accepted; UI still contains a stale summary counter and historical pending text, correction assigned. Full-slot replacement after fresh Telegram login remains unaccepted. No product source changes in this checkpoint.

## 2026-09-28 device UI reconciliation source

Android source 3eca4d5263d881a511abc761d96307a2abcbaf0f, tree c8a9ef9a3092e84e92eea506c8448778a80c70fe, based on 9bb9ab3. Four-file correction a4fc4b6c6e3227976d60064048f723e09c098c39d1cf6e15c284cf0b6240a59d reviewed: matching account/attempt devices-list slot count overrides stale summary; pending deletion uses historical acknowledgement without claiming gateway completion. Focused test receipt: Gradle success, DevicesState14, ColdGate7, ColdSequence2, LifecycleSource2, SubscriptionDeviceText6 passed as reported. Build/install/UI verification assigned; installed accepted APK remains 6b261597 pending that receipt. Gateway2619c86/backendf887575 unchanged.

## 2026-09-28 installed UI checkpoint

Android3eca4d5 / APK920a4fcc03b1881dc3a95842916d90cc6c803289c249264d7e2c0a877b643925 installed in user0 with matching readback; prior APK6b261597 retained. SEND_NOW list refresh rendered both counters1/2 and preserved current device; explicit disconnect clean per receipt. Root inspected UI XML. Cold START_SERVICE refresh stopped before devices response; cause not yet established, diagnosis assigned. No new deletion executed; full-slot new-installation scenario remains open.

## 2026-09-28 final diagnostics source and cold acceptance

Android source91f78a7456f69bcb6cdf0f2f48c7f5e53aa672f9/tree260d573249486054abcaf425af868396fa86d54e accepted after review and focused Go build/vet/read-loop tests and Kotlin mirror31/31. Explicit cancel has a distinct first-cause token; optional exit output skips unsupported deadlines. Teardown releases in-flight device tokens after ownership and preserves prior errors.
Installed sourcec5a60de/APK654e5b20051e622e2a4b9ed71823fd90bc861b6e40ba99e9f0717aeb3da19638 separately passed one true cold devices GET at22:39:04UTC27Sep with correlated reply and automatic own-attempt stop. Original earlier intermittent stop cause remains unknown. Final diagnostic correction is not installed yet; no redundant phone retest. Gateway2619c86/backendf887575 unchanged. Full-slot new-installation replacement is next.

## 2026-09-28 existing-account Telegram entry

Android8b2f4ba4ed94ac1f2af38a6e5d1af14d1ab733e5/tree62d35ef12345c23e872e3f827014cf4249720619, parent91f78a7: explicit existing-account login without onboarding-hour activation, using existing link API. Source reviewed; focused registration visibility/gate tests reported1+3 PASS, devices15/mirror31 PASS. InstalledAPK6f2d765290bfcc0af4229ff36d1a3593c2cc55f9371d0d8dcf2110a45825c7cd. One user12 SEND_NOW tap produced a pending TEST Telegram link; no hour/trial/payment. Full-slot confirmation/management/rebinding still pending. Cold login branch has unit coverage, not phone acceptance; forced profile cleanup is not graceful lifecycle acceptance. TEST link/tokens excluded.

## Full-slot device replacement acceptance — 2026-09-27 23:27 UTC

Android 8b2f4ba4ed94ac1f2af38a6e5d1af14d1ab733e5, APK SHA-256 6f2d765290bfcc0af4229ff36d1a3593c2cc55f9371d0d8dcf2110a45825c7cd; TEST backend f887575 and gateway 2619c86. Unbound new installation managed a full two-slot account, removed only the disposable old device, bound the new installation, and retained the other device and original entitlement expiry. UI shows revoked historical rows and two active slots. New device connected, normal access sync succeeded, built-in WireGuard/DNS/HTTPS probe reported success, and ordinary Disconnect returned to OFF without force-stop. Root reviewed supplied list/connected/off XML and saved notification text. This accepts the bounded replacement flow with simulated TEST Telegram callbacks, not real Telegram-bot authentication. Prior forced cleanup is not reclassified as a normal lifecycle pass. No product source changed in this acceptance checkpoint.

## Account-wide usage source acceptance — 2026-09-27 23:44 UTC

Server source 5d391c6e77750a83931388fe9c3f963d1e42a51f, tree e993c4866dd6d52bbeab9c6158ec50c4389f9aa5, based on f887575 through 6e9817d and a81b66e. Totals use historical tick.account_id across devices/gateways; revocation retains history and rebind does not transfer it. Coverage is conservative for ambiguous ownership and retired devices without final-tail evidence. Canonical DTO, authentication and Moscow calendar windows remain unchanged. Executor reports eight focused tests passed (four account-scope and four DTO/schema), Ruff clean; root reviewed all three patches. These isolated database tests do not constitute live multi-node acceptance. Deployment and Android account-statistics acceptance remain pending. No migration or live data mutation in this source checkpoint.

## Account traffic Android acceptance — 2026-09-28 00:03 UTC

Android 0d1878fb1fdf30b9dc0eea56f1fdcf8fa218a0b5, tree 4c7d4dcbb178f16d1eb699360052b7f427a02eed, includes 0931040 over 8b2f4ba. Installed APK SHA-256 e4970cec0b955df9532595bd03b16b9d33c7481771fe501c705ca849473fe156. Subscription tab uses existing account buckets for today/7/30 days, subscriber-facing directions, totals, partial coverage and explicit server as_of date/time. Root reviewed source diffs and phone XML; displayed nonzero totals and as_of match reported server values. Executor reports 13 initial focused tests and four final renderer tests passed, normal Disconnect without force-stop. Backend 5d391c6 is live on TEST A since 2026-09-27 23:45:23 UTC, API PID647159; authenticated usage buckets matched historical-account SQL sums. Section20 accepted for implemented account aggregation and observed Android display; multiple device/gateway aggregation covered by isolated DB tests, not a live two-node traffic run. Conservative incomplete history remains explicit. No new payment, trial or entitlement changes.
