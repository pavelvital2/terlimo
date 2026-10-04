# SOURCE / future compatible TEST build receipt

No TEST build/deploy/restart/remote query performed in this task. Local go test compiled test executables only. Product correction changes one runtime function and adjacent DELETE comment; backend/initializer/codec/API unchanged.

Authoritative saved passports (root management):
- recovery-server-test-prepared/node-source-inventory.safe.json, source=/home/pavel/step036-receipts/private/s5-recovery-server-test-prepare-20261003/node. Exactly39 inputs; basis ordinary d847 + service301ebbe0 + transport759346. The accepted prior overlay changed ONLY client_test_service.go to SHA7262e11cb94db06debfd45907818649e07a886739981125b9b23a53cb9c33ba2 (GET service-seed). It is not today's canonical file SHA26c1fbebf308aabc13270d2021e17aafd06230755d66859fa0b3b25a0dac6fac.
- recovery-test-artifact-routes-accepted/build-receipt.safe.json: canonical c4489467eb691769b802e825befc9bd87d95e8fd; source inventory SHA b59bcf1c52d084a92859cf9cf269ec5b9f7a3e6ee6f90a420956b7320c06153a. Artifact13916266B SHA d15d988848c5bb26a10fbe1ebcc72abe16ed70015a6fe4f618bd1da464d059ff.
- recovery-server-applied-accepted/REPORT.txt: compatible39-input overlay deployed at historical filename wdtt-server-s5-usage-59bf41c. Prior task's readonly /proc/1601838/exe hash independently matched d15d988. No claim of fullcanonical binary parity.

Minimal future authorized action:
1. Copy pinned node/ input tree to isolated build directory; compare all39 inputs with saved AFTER inventory. Verify current executable SHA at apply time. If donor missing/drifted, restore exact retained node/ bundle, not a whole current canonical gateway checkout. Named donor path/hash above is required; local current checkout is not a substitute.
2. Apply only reviewed semantic additions to donor servicePathAllowed: GET /referral; POST|DELETE /referral/candidate; optional adjacent comment. Preserve donor's other service-only forwarding branch, service-seed route, transport759346 and other38 inputs. Full canonical file replacement is forbidden by compatibility contract. Record fresh donor pre/post hash and exact minimal diff before build.
3. In isolated donor dir reuse documented recipe: GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 /home/pavel/.local/bin/go1.26 build -buildvcs=false -o ../wdtt-server-referral . (Go1.26.8). Compare module/dependency/settings passport; record artifact SHA. That toolchain path is on TEST build host, not assumed installed locally.
4. Separately authorized node-only apply: fresh executable/unit/launcher preimages and backup; node stop, atomic binary replacement preserving mode/owner at existing launcher target, node start; exact binary/readiness proof and rollback old binary if needed. Reuse established namespace/UDP/socket/service configuration. Do NOT restart API/relay or apply0042–47/lifecycle: backend service_relay already admits these routes and schema0041 suffices. Root schedules one ordinary phone GET only after compatible binary accepted. No phone/cutover PASS implied.

Tests actually run locally with go1.25.14:
- Before patch TestServiceReferralGate: expected failure on3 allowed pairs (SERVICE_PATH_DENIED),14 negatives pass; before.log retained.
- After patch TestServiceReferralGate + TestServiceReferralForward:17+3 distinct subcases PASS; real existing loopback DTLS/Unix fakebackend harness. Requests preserve method/path/auth/K/body; responses byte-equal fixture including account_ref/code. No external provider/product API.
- Separate `go run decoder_probe.go <gateway/testdata/referral_info.json>` in existing client module: existing DecodeReferralInfoStrict PASS, expected account/code. This is separate same-bytes decoder check, not one process running complete Android/native pipeline.
- git diff --check passed. Accepted unrelated suites not rerun.

Preparation issues disclosed: initial fixture write preceded mkdir and failed; corrected before forwarding run. First local commit lacked git identity, corrected with invocation-only -c user.name/email (no shared/global config edits). Neither caused product/test defect or runtime action.
