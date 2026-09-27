# STEP03.5 preserved-identity restore plan — correction3 / executable candidate (TEST; NOT executed)

Preparation only. No runtime start, no restore, no DB write, no phone. Executed guards are the
committed operator artifacts `tools/step035/recovery/*` (bounded, not a restore platform).

Candidate: branch `step035-restore-final` (isolated `/tmp/opencode/restore-final`) over accepted
lifecycle `99b299d4…`/tree `0ee48ce5…` and wire-generation fix `5c59d3a…`; pin the exact tree
after this commit and execute only on it. Dump `ebb45ef7…` stays immutable.

## 0. Verified facts (not repeated)

- Dump: identity rows 1/1/1/1/1/1, sessions 7, outbox 9 (all `done`), migrations 16; grant
  desired=applied=3, gateway_generation=1, lease_seq=3, state applied, credential present,
  `not_after` expired; entitlement ends `2026-09-22T13:38:22Z`.
- Real-flow synthetic (correction1): without baseline reset apply fails (`NOT_FOUND`); with only
  `lease_seq=0` the standard flow provisions the same grant (`desired=applied=4`, wire=1).
- Correction2 probes: disposable-PG target/transaction/backup checks below.

## 1. Canonical order (one pipeline; no step may be skipped or reordered)

| # | Step | Expected state after |
|---|---|---|
| 0 | Pre-checks (§2) | dump hash ok; A/B alive; entitlement still in window (else BLOCKED, no extension) |
| 1 | Marker-first + package binary (§3) | `resolve_work(require_owned=True)` passes on the exact canonical work |
| 2 | Scoped network apply/status/egress-check (§8 commands) | package netns/veth/chains owned+ledgered; namespace exists for `--phone` |
| 3 | Initial start `--keep` → `--stop` | API/worker/node stopped; **PG alive**; node profile/pki/creds materialized |
| 4 | `01_target_guard.sh` → `00_manifest.py` → dump import (§4) | exact package DB restored from dump (rows identical) |
| 5 | Offline VK provisioning (§5) | node profile has the trusted 4-hash set; node stopped |
| 6 | Atomic reconciliation `02_reconcile_lease.py` (§6) | in ONE tx: manifest guard + lease reset + `ensure_grant(900)` = `enqueued`; same grant/credential/binding; desired=4, applied=3, wire=1, lease=0, pending; exactly one pending APPLY (target 4); real catalog = pending 409 (NOT 503) |
| 7 | Start `--serve --keep` (worker/node) + `03 --stage post-worker` (§6/§7) | standard apply+profile: desired=applied=4, wire=1, lease=1, node gen1/seq1 same grant; catalog served with wire generation 1; `not_after <= entitlement` and `<= 900 s` |
| 8 | Stop quiescent → full protected backup (§7) → restart | new `step035-checkpoint-recovery-*` with pg_dump + keys/profile/pki/wg + manifest |
| 9 | `03_identity_check.py --stage reconcile/post-worker` + counts | identity/credential untampered, counts and outbox transitions exactly as above |
| 10 | Caddy additive block + validate + restart canonical | `/help` 200; mobile 401; A/B 56000/56002 intact; DTLS 57400 listener+DNAT; WG 57401 not published |
| 11 | Handoff phone (operator) | truthful renewal: short lease may expire after backup — normal pending via standard renewal, never hidden |

**No operator access/sync and no new session before step 8 completes.** Pre-phone expectations:
step 6 is pending (`lease_seq=0`, one pending APPLY); step 7 (post-worker, still pre-phone) binds
the node and confirms `desired=applied=4`, wire=1, lease>=1. Handoff only renews; after renewal
`lease_seq` may grow, so no fixed seq value is asserted there.

## 2. Pre-checks (read-only)

```bash
cd /home/pavel/projects/terlimo-backend
sha256sum /home/pavel/step035-checkpoint/pkg.sql     # ebb45ef7… ; 0700/0600
pgrep -af 'run_isolated.py --phone --serve' || echo 'package stopped'
ss -lunp | grep -E '56000|56002'                     # A/B alive
```

## 3. Marker-first materialize (run_loop refuses unmarked non-empty work)

```bash
WORK=/home/pavel/projects/terlimo-backend/.step035-runs/pkg
test ! -e "$WORK" || { echo 'work exists; refuse'; exit 1; }
install -d -m 700 "$WORK"
install -m 600 /home/pavel/step035-checkpoint/.step035-owner.json "$WORK/.step035-owner.json"
.venv/bin/python - "$WORK" <<'PY'
import os, sys; sys.path.insert(0, "tools/step035"); import run_isolated as r
work, marker = r.resolve_work(sys.argv[1], require_owned=True)
assert str(work) == sys.argv[1] and marker["work"] == str(work)
assert marker["uid"] == work.stat().st_uid == os.getuid()
print("MARKER_OK", work)
PY
install -D -m 755 /tmp/opencode/wdtt-server-vk "$WORK/bin/wdtt-server"     # 34e1f12b…
# PREREQUISITE: scoped network apply/status/egress-check (§8 step-2 commands) must run before the
# FIRST `--phone --serve` — the package namespace terlimo-035 does not exist yet.
.venv/bin/python tools/step035/run_isolated.py --phone --serve --work "$WORK" \
  --node-bin "$WORK/bin/wdtt-server" --keep
.venv/bin/python tools/step035/run_isolated.py --stop --work "$WORK"       # PG stays
```

## 4. Target guard + manifest + import (committed artifacts; no DSN in argv)

```bash
WORK=/home/pavel/projects/terlimo-backend/.step035-runs/pkg
PSQL=$(.venv/bin/python -c 'import sys;sys.path.insert(0,"tools/step035");import run_isolated as r;print(r.pg_bin("psql"))')
PGCTL=$(.venv/bin/python -c 'import sys;sys.path.insert(0,"tools/step035");import run_isolated as r;print(r.pg_bin("pg_ctl"))')
LOG=/home/pavel/step035-checkpoint-recovery-psql.log; umask 077; : > "$LOG"
G=tools/step035/recovery
WORK="$WORK" PSQL="$PSQL" PGCTL="$PGCTL" LOG="$LOG" bash "$G/01_target_guard.sh"   # TARGET_OK or abort
.venv/bin/python "$G/00_manifest.py" /home/pavel/step035-checkpoint/pkg.sql \
  /home/pavel/step035-checkpoint-recovery-manifest.json
export PGHOST="$WORK/pg" PGUSER=postgres PGDATABASE=postgres
"$PSQL" -v ON_ERROR_STOP=1 -c 'DROP DATABASE terlimo_035' >>"$LOG" 2>&1
"$PSQL" -v ON_ERROR_STOP=1 -c 'CREATE DATABASE terlimo_035' >>"$LOG" 2>&1
PGDATABASE=terlimo_035 "$PSQL" -v ON_ERROR_STOP=1 -f /home/pavel/step035-checkpoint/pkg.sql >>"$LOG" 2>&1
```

`01_target_guard.sh` reads fresh `state.json`, exports PGHOST/PGPORT/PGUSER/PGDATABASE (no secret
argv), and machine-asserts `current_setting('data_directory')` **canonical == WORK/pg**, every
`unix_socket_directories` entry **canonical == WORK/pg**, `listen_addresses=''`, `pg_ctl status`
OK, postmaster cmdline `-D WORK/pg`, owner == caller; any mismatch aborts before DROP.

## 5. Trusted VK offline (node stopped)

```bash
sudo .venv/bin/python tools/step035/provision_trusted_vk.py --work "$WORK" \
  --node-id terlimo-test-193-5-251-217 --peer-ip 193.5.251.217 --expected-count 4 --dry-run
sudo .venv/bin/python tools/step035/provision_trusted_vk.py --work "$WORK" \
  --node-id terlimo-test-193-5-251-217 --peer-ip 193.5.251.217 --expected-count 4
```

## 6. Atomic reconciliation (one transaction; no intermediate lease=0 commit)

```bash
G=tools/step035/recovery
.venv/bin/python "$G/02_reconcile_lease.py" "$WORK" /home/pavel/step035-checkpoint-recovery-manifest.json
.venv/bin/python "$G/03_identity_check.py" "$WORK" /home/pavel/step035-checkpoint-recovery-manifest.json --stage reconcile
```

Single transaction, before API/worker start: (a) the exact-manifest guard (grant/opaque/binding/
gateway, state `applied`, applied=desired=3, wire=1, old `lease_seq=3`, `not_after` as UTC
datetime == manifest and in the past, binding status/generation exact, installation id/state
exact, entitlement id/account/status/ends_at exact and > now, join cardinality exactly 1, no
pending APPLY), (b) full-key lease reset with exactly `UPDATE 1`, (c) the existing business
function `ensure_grant(max_lease_seconds=900)` in the SAME transaction → must return `enqueued`,
same grant/credential/binding, desired=4/applied=3/wire=1/lease=0, state pending, exactly one
matching pending APPLY (target 4). Any mismatch/expired/revoked → whole-transaction ROLLBACK
(including the reset); expired prints `BLOCKED_EXPIRED`; a repeat run refuses on the prestate
guard without a second mutation. Product contracts, applied/readback and terms are untouched.
Credential identity is preserved **by construction** (no code path updates `gateway_credential`
and no new credential is generated); the manifest intentionally omits the secret value, so
equality is not asserted by value comparison.

Start the worker/node and wait (bounded, read-only) before the post-worker assert:

```bash
.venv/bin/python tools/step035/run_isolated.py --phone --serve --work "$WORK" --node-bin "$WORK/bin/wdtt-server" --keep
.venv/bin/python "$G/04_wait_post_worker.py" "$WORK" /home/pavel/step035-checkpoint-recovery-manifest.json --timeout 120
.venv/bin/python "$G/03_identity_check.py" "$WORK" /home/pavel/step035-checkpoint-recovery-manifest.json --stage post-worker
```

`05_wait_readiness.sh` is a bounded HTTP readiness gate (curl `--connect-timeout`/`--max-time` per
request, default 60 attempts x 2 s) that exits non-zero with `READINESS_TIMEOUT` on exhaustion, so
the caller cannot silently continue. Readiness is an API HTTP check, never a single DB read.

`04_wait_post_worker.py` is SELECT-only (no writes, no new apply, no reconcile): it waits for
desired=applied=4, wire=1, lease>=1, state applied, persisted readback and zero pending/processing
outbox operations; terminal exits are `2` (failed/dead/revoked), `3` (expired entitlement, **unconditional**, never
masked by an applied state, never extended) and `4` (bounded timeout/stall; the whole poll
including connect and each snapshot is bounded by the same deadline), all without mutation. A
short grant lease expiring while the entitlement is alive is a different, truthful-pending case. These are not product timeouts, only a
bounded operator wait (default 120 s).

## 7. Quiescent full backup (mandatory before handoff)

```bash
CK2=/home/pavel/step035-checkpoint-recovery-$(date -u +%Y%m%dT%H%M%SZ); install -d -m 700 "$CK2"; umask 077
.venv/bin/python tools/step035/run_isolated.py --stop --work "$WORK"          # PG stays
.venv/bin/python tools/step035/run_isolated.py --checkpoint --work "$WORK" --checkpoint-dir "$CK2"
cp -a "$WORK/pki" "$CK2/pki"
cp -a "$WORK/gateway/creds" "$CK2/gateway-creds"
cp -a "$WORK/gateway/wg-keys.dat" "$CK2/"
cp -a "$WORK/gateway/passwords.json" "$CK2/"
cp -a "$WORK/gateway/managed-test" "$CK2/"
chmod -R go-rwx "$CK2"
( cd "$CK2" && find . -type f -printf '%M %s %p\n' | sort > MANIFEST.txt && sha256sum $(find . -type f ! -name MANIFEST.txt | sort) >> MANIFEST.txt )
# then restart, wait for readiness (bounded HTTP gate; reads only; no new apply) and re-check:
.venv/bin/python tools/step035/run_isolated.py --phone --serve --work "$WORK" --node-bin "$WORK/bin/wdtt-server" --keep
G=tools/step035/recovery
"$G/05_wait_readiness.sh" || { echo 'readiness not proven; aborting stop-forward path'; exit 1; }
.venv/bin/python tools/step035/recovery/03_identity_check.py "$WORK" /home/pavel/step035-checkpoint-recovery-manifest.json --stage post-worker
```

Source-confirmed copied paths: `pki/{ca,server,backend}.{key,pem}`, `gateway/creds/{wl-test-dtls-cert,key}`,
`gateway/wg-keys.dat` (server.go:1412-1419), `gateway/passwords.json`, `gateway/managed-test/`.
`passwords.json.previous` is excluded (no source confirmation). `ebb45ef7` is never touched.

## 8. Network (early, §1 step 2) + Caddy readiness (late, §1 step 10)

Network apply runs immediately after the marker/binary step — the `--phone` node needs the
namespace; the ledger is written into the owned work dir (`resolve_work(require_owned=True)`
verified in `phone_network.py`). Caddy exposure stays AFTER readiness/backup.

```bash
# step 2 (namespace before the first --phone start):
tools/step035/phone_network.py apply --work "$WORK"
tools/step035/phone_network.py status --work "$WORK"
tools/step035/phone_network.py egress-check --work "$WORK"
# Caddy: insert the exact reviewed additive block (identical text to caddy_mobile_api.py BLOCK),
sudo /usr/local/libexec/terlimo-caddy validate --config /etc/terlimo-minishop-edge/Caddyfile
sudo systemctl restart terlimo-minishop-edge
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18091/health/ready      # 200
curl -s -o /dev/null -w '%{http_code}\n' https://terlimo.193-5-251-217.sslip.io/help          # 200
curl -s -o /dev/null -w '%{http_code}\n' https://terlimo.193-5-251-217.sslip.io/api/mobile/v1/gateways  # 401
sudo ip netns exec terlimo-035 ss -lunp | grep 57400          # DTLS listener inside netns
sudo iptables-save | grep 57400                                # DNAT to 10.35.0.2:57400
ss -lunp | grep -E '56000|56002'                               # A/B preserved
```

WG `57401` is **not** published by this package (network status reports `wg_port_opened=false`);
no claim of a public WG port.

## 9. Replay/trust statement (proven only)

Old operations cannot replay under the restored node because the trust material is new
(management CA and DTLS key/pin are regenerated; the old node private keys are gone), the old
leases have expired, and the restored outbox operations are all `done` (nothing queued). The
protocol is unchanged; no claim is made about an "empty node" by itself.

## 10. Handoff and limits

- Phone is NOT in the executor scope: handoff after step 10. The post-worker confirmation already
  happened pre-phone (step 7); the handoff only renews via standard `access/sync`, so `lease_seq`
  may grow beyond the first value and the 900 s/expiry bounds stay truthful.
- No extension of entitlement `13:38:22Z`, fixture TTL or the 900 s lease bound; expired state is
  never advertised as active; revoked rows are never resurrected.
- Reconciliation is part of the server restore (before backup); no phone/operator session is used
  for it. A short lease may expire after the backup: truthful pending plus standard renewal is
  the expected behaviour — eternal `served` is not required and expiry is never hidden.
- Lifecycle fix `99b299d` is merged in the candidate; `--keep` is used so PG stays available for
  import/backup, not as a substitute for the fix.
