# AUTH budget source review

Scope: POST /api/mobile/v1/auth/challenge and /auth/session only. Source-only; baseline/live files, service state, network, flags, grants, phone unchanged. Root source acceptance required before TEST installation. Due12:00MSK.

## Caps inventoried before patch

Node: Unix dial3s; IO15s after dial (encode+ReadSlice, not per chunk); reply write5s; request-frame idle/read10s; max8 sequential frames. Watcher has no deadline, aborts Unix on peer loss. Node logging can consume elapsed time and synchronous code cannot promise scheduler latency.
Relay: unknown frame read15s; forward ClientTimeout15s; wait upstream15s; drain/close each15s. mTLS outer admitted handler15s includes frame parse, context pool/resolve, replay. Loopback replay ClientTimeout15s across connection/body. aiohttp rounds deadlines >=5s upward unless disabled.
API: no aggregate auth handler timeout before patch. DB connect/command15s per operation, not request aggregate; pool acquisition bounded by outer cancellation. Transactions retain existing rollback/cancellation; no DB settings changed. No synchronous crypto timer: cancellable awaits bounded, synchronous CPU/scheduler stall cannot be preempted by asyncio.
Transport/session unrelated caps remain: DTLS8s client, classifier/idle; node idle10 and max8; challenge validity300s/session TTL86400/cache600 are lifetimes, not request budgets.

## Selected finite profile

Client remains20/25/10/10 whole65, device stage25 is one shared challenge+session budget; no host/native timers changed. Server caps apply per validated AUTH request, not separate25s allowances. Caller socket close still cancels outstanding relay via existing watcher and mTLS handler_cancellation.
Node AUTH22s absolute from validated request start, including Unix dial/encode/read and response delivery. Dial3s retained inside22; inherited ctx deadline can only shorten. AUTH IO cannot renew at chunks/retries; response5s remains clipped by whole22s timer. NonAUTH IO15-after-dial and write5 unchanged.
Relay AUTH21s absolute from Unix callback; unknown input read15 remains a preclassification frame cap, inside whole21 once classified. Forward HTTP total21, no aiohttp roundup; upstream wait+drain share remaining21. Close cleanup1s for AUTH only, within node22 guard. NonAUTH behavior/timers remain unchanged, including invalid frames forwarded for backend rejection.
Outer mTLS AUTH20s absolute from admitted-handler entry, rescheduled after strict frame classification from original start, not reset at parse/progress. Context lookup + replay share this20. Replay gets remaining20 via existing asyncio timeout.when(); its ClientTimeout uses remaining, no roundup.
Inner AUTH API uses min(20, inherited remaining deadline). Fixed loopback replayer adds X-WL-Auth-Deadline-Monotonic; same-host monotonic clock only, header not part of public service frame and rejected by existing allowed-header validation. Missing/invalid/nonfinite header cannot expand20; even direct callers can only shorten. Necessary because ordinary loopback API run_app has no handler_cancellation flag; closing replay otherwise would leave API handler running without aggregate deadline. Authentication/idempotency/DTO/routes unchanged. Timeout maps to existing SERVICE_UNAVAILABLE, cancellation rethrows.

No aggregate hidden15 remains after validated AUTH classification. Retained15 caps are explicit preclassification input-frame read and individual DB connect/command safeguards; they can reject a single slow body/query, but cannot cap total AUTH at15. No evidence that a DB query hit its15s command timeout in the original run. Increasing these would change unrelated protection and is not justified by available evidence.

## Verification

Go targeted TestAuthBudget + TestServiceUnixTerminal PASS. Logical late upstream16s is rejected by actual old ordinaryRelayWithDialForTest socket budget15, accepted by candidate challenge/session22;23s rejected, ME/GW unchanged, caller remaining12 respected, caller cancellation closes socket promptly. Virtual completion uses production SetDeadline, no actual16s sleeps/network. Existing terminal fixture updated only for intentional AUTH22 vs old15 expectation; response/validation/frame parity retained.
Python10 tests PASS: actual original relay module OLD cutoff vs NEW late16 scaled1:100;21 excess/body+upstream sharing; cancellation/slot/socket cleanup; nonAUTH defaults; direct challenge/session20 finite cancellation; outer context+replay share20; replay carries inherited absolute deadline and remaining total; internal header rejected in public frame; inherited deadline cannot expand; actual forward selector21 and no roundup. Scaling affects constants in fixture process only, not sources/runtime. No proof of performance SLA or underlying delay resolution.

## Build and rollback

Need new LinuxAMD64 node artifact from candidate client_test_service.go after source acceptance, with accepted node source d847a411 baseline plus exact review.diff. Tests compiled with available Go1.26.8; current accepted runtime is Go1.26.5/bebec698... . Build toolchain/artifact identity must be explicit, no silent replacement. Python modules source deploy require API and relay restart under a separate TEST apply scope; relay dependency can require node restart, so do not publish/install piecemeal and preserve protected-service baseline/backup.
Before approved apply: backup exact deployed auth_api.py/service_relay.py and node artifact/config/units; verify baseline hashes from source-hashes.json, API/relay/node dependency order and owned readiness, flagsOFF/full101/currentrights unchanged. Rollback exact backups + previously accepted node artifact bebec6980ed3a41242cd7ac0455839bf4dd2c2297c8875df00ae781f0b8bb1ff, restore original metadata and approved dependency order/readback. Current source-only rollback is simply discard isolated candidate: live originals are unchanged.

Root then coordinates corrected Android/native + server for one ordinary acceptance run. No new run/window/flag/READY now.
