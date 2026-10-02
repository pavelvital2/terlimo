# AUTH nested budget candidate

Source review accepted; product NOT_TESTED. AUTH-only node22/relay21/API20 seconds, caller cancellation retained, inner loopback inherits fixed monotonic deadline; unrelated request budgets unchanged. 10 Python tests and targeted Go tests passed in executor candidate.

Canonical gateway retains previously published optional frame diagnostics; deployed baseline d847a411 omits these hooks. The minimal AUTH diff is integrated without removing canonical diagnostics. Exact reviewed runtime source (SHA301ebbe0ff4032b598b8c6706e8f5007ba7ee026465d456252340415162ff75d) is archived as runtime-client_test_service.go.txt; executor builds that runtime-derived candidate. This publication is not a claim that built binary uses canonical gateway tree. Build receipt must record actual source and toolchain; integration tests cover canonical variant separately. Python source baselines match canonical exactly.

No runtime applied or phone test yet. Underlying AUTH latency cause UNKNOWN. Previous node bebec698 and Python backups retained for rollback. Client b47069c/APK d3758189 installed idle; user scenario still NOT_TESTED. Full feature matrix is inherited from client-catalog-stages-20261002/README.VERSION.md; only source budget regression is accepted here.

Root integrated gateway verification: go test -run TestAuthBudget|TestServiceUnixTerminal|TestServiceFrame ./... PASS (wg-turn-client0.044s, internal/wlwire0.006s); git diff --check PASS.

Build accepted: artifact a42fa90e0b7459b128e445e473078f31b6c180d8da00310d332182c9dd210f1d, Go1.26.8. Root matched received binary SHA, exact archived runtime module and both canonical Python modules. Full inventory attached. Runtime d847 baseline also omits canonical optional UDP preaccept diagnostics (udp_listener.go), reviewed separately; no change to that runtime file. Use this explicit source inventory for apply, not a claim of full canonical gateway-tree identity. TEST-apply plan source-comparison prerequisite is resolved by ROOT_BUILD_REVIEW.json. Apply not yet performed.
