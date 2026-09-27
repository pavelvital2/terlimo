# STEP03.5 isolated TEST package (server side, reviewable)

Status: prepared package for the owner-reviewed 03.5 run. It is **not** applied anywhere:
the shared Caddy config is untouched, no phone/AB/production resource is used, and the real
A/B grants are never touched. The fixture is synthetic TEST data, not a customer trial.

## What it runs (isolated namespace)

| Piece | Location | Notes |
|---|---|---|
| PostgreSQL | `<work>/pg` via `pgserver` | dedicated datadir/db `terlimo_035`; unix socket only |
| API | `127.0.0.1:18091` (subprocess `terlimo-api`) | `TERLIMO_ENV=test` |
| Worker | subprocess `terlimo-worker` | same DATABASE_URL, mTLS gateway client |
| Real WDTT node | throwaway mount+net namespace (`sudo -n unshare`), `-max-workers-per-access 36` | reuses exactly the accepted 03.3 harness (`tools/gateway_integration_03_3.py`) |
| management-handler | private mTLS on `127.0.0.1:56331` | reuses `tools/gateway_integration_mtls_03_3.py` PKI helpers; client-cert allowlist |
| PKI/secrets | `<work>/pki`, `<work>/gateway/creds`, generated main password | created 0600, never printed, removed with `<work>` |

Registry row: `gateway_key == endpoints.node_id == terlimo-035-node`, `target_workers=36`,
`transport` pin/ports from the generated node certificate, `management` mTLS route only.

## Fixture (synthetic, no customer import)

- Real PoP flow only: `POST /auth/challenge` → `POST /installations` (real P-256 PoP signed with
  the accepted contract module) → seed **synthetic** `accounts`/`account_bindings`/`entitlements`
  (`kind='paid'`, active, synthetic revision; never `trial`) → `POST /auth/session` with
  `requested_scopes=[session:read, access:sync]` (linked via the proven binding, no manual
  `UPDATE sessions`) → `GET /me` → `GET /gateways` → `POST /access/sync` → worker applies through
  the private mTLS handler → `GET /gateways` must show the confirmed generation/lease_seq pair,
  `target_workers=36`, and the node readback must confirm the grant.
- Terminal-dead recovery and fresh trusted-login provenance remain **open product gates**; this
  fixture does not implement or claim them.

## Commands

```bash
cd /home/pavel/projects/terlimo-backend
# local verification of the whole isolated loop (work root: .step035-runs/<name>):
.venv/bin/python tools/step035/run_isolated.py --keep
# the loopback self-test revokes only when explicitly asked:
.venv/bin/python tools/step035/run_isolated.py --keep --verify-revoke
# readiness only, no real gateway (still isolated PG/API/worker):
.venv/bin/python tools/step035/run_isolated.py --no-gateway
# then stop everything and remove only this package's owned work dir:
.venv/bin/python tools/step035/run_isolated.py --cleanup
```

`run_isolated.py` prints a small JSON report (no secrets) with each verified step.

## Public route (owner action, after package review)

Phone already carries `https://terlimo.193-5-251-217.sslip.io`. The minimal diff for the existing
vhost (add before the catch-all `reverse_proxy 127.0.0.1:18082`) is in
`tools/step035/caddy_mobile_api.diff`; it exposes **only** `/api/mobile/v1/*` to the isolated
backend on `127.0.0.1:18091`. `/health/*` and `/version` are deliberately not published: the
API route is `GET /health/ready` (private); the Caddy diff exposes only `/api/mobile/v1/*`.
Do not apply/reload until the owner reviews this package; rollback is removing the two lines and
reloading (no other vhost/handler is touched).

## Phone-ready mode (external TEST, owner-applied networking)

Full design/recon/rollback: `phone_network_plan.md`. Preflight (read-only) and the exact
apply/rollback are scripted in `preflight_phone.sh` / `phone_network.sh` (not applied).

Joint order (TEST scope only; no production/public reload in this task):

```bash
# 0. owner reviews this package; read-only check:
tools/step035/preflight_phone.sh
# 1. owner applies the minimal transactional network additions, then verifies egress:
tools/step035/phone_network.py apply
tools/step035/phone_network.py status
tools/step035/phone_network.py egress-check   # namespace DNS/TCP + MASQUERADE counters
# 2. persistent server (phone node in netns, registry external 193.5.251.217:57400):
.venv/bin/python tools/step035/run_isolated.py --phone --serve
# 3. owner applies the reviewed Caddy diff (caddy_mobile_api.diff); health stays PRIVATE on
#    the API's real route (there is no /api/mobile/v1/health):
curl -fsS http://127.0.0.1:18091/health/ready
# 4. Android registers itself through the existing seed URL (real PoP enrollment)
# 5. bind the synthetic fixture to the phone's OWN fingerprint (no key transfer, no session edit):
.venv/bin/python tools/step035/attach_android.py --fingerprint <hex64>
# 6. phone: fresh PoP session (session:read, access:sync) -> /me -> /gateways -> /access/sync -> connect
# 7. cleanup:
.venv/bin/python tools/step035/attach_android.py --fingerprint <hex64> --revoke
.venv/bin/python tools/step035/run_isolated.py --cleanup
tools/step035/phone_network.py rollback
```

- The fixture is a closed synthetic TEST link for one installation fingerprint; it is **not**
  customer trusted-login provenance (open product gate) and sessions are only ever issued by the
  real PoP flow.
- Work paths live under the canonical root `<repo>/.step035-runs/<name>` with a written
  ownership marker; foreign/unmarked/symlinked paths are refused, and cleanup only signals
  identity-verified recorded PIDs (starttime + cmdline) and removes only the owned tree.
- `--serve` preserves its own PostgreSQL database/identity across restarts (no drop/recreate)
  and never revokes anything; state (DSNs, ports, node id) is written to `<work>/state.json`
  (0600). Fixture revocation happens only through an explicit cleanup command
  (`attach_android.py --revoke`; the loopback self-test revoke requires `--verify-revoke`).
  Secrets/PKI stay 0600 under `<work>`; the relay report contains only refs/redacted data.

Fault tests (later, TEST scope only): lost `/access/sync` reply (same key/digest retry returns
the same operation and no second effect), register sync after gateway restart, worker restart
mid-apply, unavailable node (bounded failure, never a false `applied`). They are not run here.

## Owner-independent actions for 03.5

1. Review this package + the Caddy diff; apply the diff and reload the existing Caddy service.
2. Provide the Android test window with the existing seed URL (not replaceable by a loopback URL
   for phone E2E).
3. Decide the real gateway reachability for the phone data plane: the fixture node is loopback
   only (`127.0.0.1`), so a phone cannot reach it. A phone E2E needs a real TEST node address and
   the corresponding DTLS/WG ports reachable from the phone. No firewall change is made here; the
   exact ports (`dtls_port`, `wg_port`) and address must be approved first.
4. Close the remaining product gates (terminal-dead recovery, fresh trusted-login provenance)
   before claiming them in 03.5 acceptance.

## Safe stop order (correction2; supersedes the old rollback→cleanup→Caddy-restore order)

The deadline is unconditional: start the stop no later than 03:30 MSK and finish before 04:00 MSK.
The executor is engineer_deepseek_max; no additional owner permission is needed for the mandatory
deadline stop. Final Android check must finish before cleanup starts.

1. Refresh the final checkpoint (the last run is the final snapshot):
   `.venv/bin/python tools/step035/run_isolated.py --checkpoint --work .step035-runs/pkg`
   — owner-only 0700/0600: fresh `pg_dump`, marker/ledger/state/logs, current Caddy + iptables,
   `RUNBOOK-restore-stop.md`. Never treat an older copy as final.
2. If the live package predates the serve/gateway PID records, adopt it with proof:
   `.venv/bin/python tools/step035/run_isolated.py --adopt --work .step035-runs/pkg`
   — proves serve (cmdline+cwd), api/worker (ppid=serve + env work path), gateway (config-dir +
   `ip netns identify` singleton + executable + starttime) and only then writes the four records.
   Any missing proof refuses and writes nothing (no bare old PIDs, no broad kill).
3. `.venv/bin/python tools/step035/run_isolated.py --stop --work .step035-runs/pkg`
   — identity-verified (starttime + cmdline) graceful SIGTERM of serve first, then api/worker/gateway,
   bounded wait and SIGKILL escalation only for the same verified own PIDs; never touches PG/network/work.
4. `.venv/bin/python tools/step035/phone_network.py rollback --work .step035-runs/pkg`
   — refuses while serve/gateway are verified alive or when any ownership record is missing while the
   package state exists; removes only ledgered own chains/veth/netns.
5. `.venv/bin/python tools/step035/caddy_mobile_api.py remove`
   — context-checked removal of exactly our block (backup, validate, restart because the unit's
   reload is broken by design with `admin off`), then checks `/help` and the mobile path.
6. `.venv/bin/python tools/step035/run_isolated.py --cleanup --work .step035-runs/pkg`
   — fail-closed: signals nothing, requires every expected record to be verified gone, stops only this
   work dir's postmaster via scoped `pg_ctl` with proven exit, and KEEPS the work data by default
   (`data_kept`). `--purge-owned-work` removes the owned tree only after all of the above verifies.

`ip netns del` is not assumed to kill processes: the node PID is recorded/adopted and stopped
explicitly. No broad pgrep/kill, no foreign flush, no deletion under a live or unverified package.

## Cleanup guarantees

- `--cleanup` removes only the chosen `<work>` directory (default `.test-035`) and stops only the
  processes started from it (API/worker PID files, namespaced node matched by its config dir,
  management-handler, package PG).
- No old TEST data purge, no global process kill, no change to other worktrees, A/B grants,
  personal data or production.
