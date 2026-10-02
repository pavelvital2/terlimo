# Exact TEST apply and rollback plan (not executed)

Authorization so far: whitelist-20261002-auth-nested-budget-build-v1 BUILD/PACKAGE ONLY. Source acceptance: whitelist-20261002-auth-nested-budget-source-fix-v1 completed, source tests accepted. No runtime permission inferred from this package. Before apply require root canonical publication + exact source/artifact comparison and explicit TEST apply scope. No donor push. No phone/window/READY/new diagnostics.

## Package identities

Artifact: /home/pavel/step036-receipts/private/s5-auth-nested-budget-build-20261002/wdtt-server-auth-budget-go1.26.8
SHA256: a42fa90e0b7459b128e445e473078f31b6c180d8da00310d332182c9dd210f1d; 13905729 bytes; ELF64 AMD64 LE, Go1.26.8, CGO0, GOAMD64v1.
Reviewed node module301ebbe0ff4032b598b8c6706e8f5007ba7ee026465d456252340415162ff75d; all other build Go sources/go.mod/go.sum exact d847a411 accepted ordinary baseline. Full build input inventory attached. Tests already accepted, no rerun.
Python authd16bd8f4678200ab8068baa5e7fc3a6a219b970cce59da7811bc6aef0c76eef3 / relay8c4667ba2eac7c8d34526578a0890945b95e11301278bdcba97dc08c708b69de; files included verbatim.

## Publish comparison and just-before-apply baseline

Root provides full canonical commit. Read canonical files from that immutable commit; compare the three reviewed module byte SHA and the whole node build input inventory. Canonical repository relocation is allowed, but any additional changed build source or dependency invalidates this artifact comparison; report mismatch without applying/rebuilding unapproved changes. Do not treat the candidate as a canonical commit or replay source report.
Immediately before approved apply compare all targets, unit/launcher/config/full-passport hashes, diagnostic flags and protected service PIDs/starttimes against package-baseline.safe.json (refresh legitimately changed baseline with root scope, never silently bypass drift). Confirm existing TEST phone/VPN idle from original client terminal or root current state; preserve active connections, no new minimum-remaining/OBSERVER_READY gate. No synthetic AUTH or created bearer/grant.
Current read-only baseline: eight services active; API-ownedTCP18092/18093, node-ownedUDP56002/57500, named netns terlimo-036; node running SHA bebec6980ed3a41242cd7ac0455839bf4dd2c2297c8875df00ae781f0b8bb1ff. Catalog7bec remains installed. API/relay/node diagnostics absent/OFF.

## Backup and staging (only after apply authorization)

Create a task-local private backup directory mode0700. Copy exact three target files + stat uid/gid/mode and their hashes into backup before stopping anything. Verify backup hashes against baseline. Keep backup of referenced unit/launcher/config files locally private with original metadata; their bytes will not change, never attach private environment/config contents. Record only SHA in safe receipt. No DB migration/write/grant/session or account/key identity changes.
Stage each package file to a unique temporary file beside its target; use target's existing uid/gid/mode, rehash staged bytes. No target replacement before complete backup/staging validation. Exact targets:
- auth_api.py: /home/pavel/terlimo-test-a-live/terlimo_backend/auth_api.py uid=1000 gid=1000 mode=0o644; baselineSHA=383ffff577a9fce802b43e7d7593237e924bd2634aeeb22e6531470d940f3ca2
- service_relay.py: /home/pavel/terlimo-test-a-live/terlimo_backend/service_relay.py uid=1000 gid=1000 mode=0o644; baselineSHA=6f6ca3081b0c2dbf1de97b0434516ad6fa109fe29b595a33d4c94f96fd41f83d
- node: /home/pavel/step036-device-stage/pkg/bin/wdtt-server-s5-usage-59bf41c uid=0 gid=0 mode=0o755; baselineSHA=bebec6980ed3a41242cd7ac0455839bf4dd2c2297c8875df00ae781f0b8bb1ff

## Dependency order and atomic install

Inspected systemd: API Requires pg, no RequiredBy; relay Wants/After API; node Requires/After relay+net and After mgmt. Stopping relay can stop dependent node, so do not rely on automatic recovery or restart relay alone.
1. `sudo -n systemctl stop terlimo-test-a-node.service`; confirm inactive/MainPID0.
2. `sudo -n systemctl stop terlimo-test-a-relay.service`; confirm inactive/MainPID0.
3. `sudo -n systemctl stop terlimo-test-a-api.service`; confirm inactive/MainPID0.
4. Atomically replace both Python modules and node binary from already verified sibling staging files (`os.replace` / same-filesystem rename), preserving recorded ownership/mode. Config, units, launchers, mobile_catalog and full passport untouched. Verify three target hashes.
5. `sudo -n systemctl start terlimo-test-a-api.service`; wait for active plus **this PID's fd-owned LISTEN TCP18092 and18093**. No HTTP functional probe/bearer/DB write.
6. `sudo -n systemctl start terlimo-test-a-relay.service`; verify active, /run/terlimo-test-a/service-relay.sock listening inode owned by **this relay PID**, uid1000/gid1000/mode0600. Socket path existence alone is insufficient.
7. `sudo -n systemctl start terlimo-test-a-node.service`; verify active, actual /proc/PID/exe SHA exact newartifact, same named netns, **this PID's fd-owned UDP56002/57500**, service socket binding present. No synthetic/client request.
Each readiness wait is bounded by the next root approved apply duration and uses ordinary process/socket facts; do not install new timers/flags/diagnostic windows. Report concrete missed readiness and rollback immediately instead of starting a phone run.

## Final direct checks

New API/relay/node PIDs/exe/source hashes as expected; protected PG/worker/evidence/mgmt/net PIDs+starttimes and all unit/launcher/config/full-passport/catalog hashes unchanged. API/relay/node stable process environment SHA unchanged excluding only INVOCATION_ID/SYSTEMD_EXEC_PID/JOURNAL_STREAM; flags absent/OFF, no tracer. No network or client access/DB changes; installer issues no SQL/migration or API auth requests, so existing rights preserved. Owned sockets/readiness verified as above. Read only newly appended ordinary startup logs for panic/fatal/config failure, emit safe categories without identities/credentials. Save result APPLIED_READY_FLAGS_OFF_NOT_PRODUCT_PASS. Then root decides the one combined corrected-client acceptance run.

## Rollback upon partial install or readiness failure

Do not launch phone or manufacture a session. Stop node → relay → API (same order), restore exact backups of both Python modules and old node bebec6980ed3a41242cd7ac0455839bf4dd2c2297c8875df00ae781f0b8bb1ff with original metadata via sibling atomic rename. Config/unit/launcher files should never have changed; verify their hashes rather than rewrite them. Start API → wait ownedTCP → start relay → wait ownedUnix → start node → wait ownedUDP/netns; verify baseline artifact, source/catalog, flags and protected service identities. Save ROLLED_BACK_READY_FLAGS_OFF or concrete failure if readiness unavailable; preserve all logs/materials. No transport/PG/net/evidence/mgmt/worker restart. Do not retry failed candidate without a specific new root decision.

## Current state

This plan is ready but not executed. Build count1, tests reused, no runtime source/binary replacement, no service/network/config/DB/phone change. Current source/profile corrects finite nested budgets; original underlying AUTH delay still UNKNOWN, no SLA/productPASS claim.
