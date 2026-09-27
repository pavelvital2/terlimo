# STEP03.5 TEST resume package (concrete, from the saved checkpoint)

Preparation/source package — **not applied** by producing it: no live start, no phone, no
migration run, no entitlement issued. This file supersedes the 03.5 description in `README.md`
where they differ. It is prepared on branch `step035-resume-package`, which already includes the
accepted backend `59d0acb` and the runtime stopfix `3c04b31`; the exact final merged tree hash is
recorded in the acceptance report and verified with `git rev-parse HEAD^{tree}`.

## 1. Pinned versions, paths, node binary (shared install is never replaced)

| What | Exact value |
|---|---|
| Backend source in this branch | `59d0acb7b0d10b26cb3dd84a7934eb658bad49cd` (R1 correction3, accepted) |
| Runtime stopfix (in this branch) | `3c04b31c77fe1ea6f1f6be5f603309bde31e5b31` (`deadline_stop.py` path fix) |
| Merge preview (runtime `3c04b31` + this branch) | clean; exact final tree in the report |
| Package node binary | **`<work>/bin/wdtt-server`** ← `/tmp/opencode/wdtt-server-vk` sha256 `34e1f12bc95c16466e9878eb1bf5c7785831ddfe12bdd200c79519ef7b11ee9d` (build `f38200a`; bundle `/tmp/opencode/terlimo-wdtt-vkhash-node.bundle`) |
| Shared `/usr/local/bin/wdtt-server` | **untouched**; run_isolated refuses to start from a missing/foreign binary and uses the resolved `--node-bin` for start *and* identity verification |
| `run_isolated.py` (this branch) | adds `--node-bin` / `STEP035_NODE_BIN`; adoption compares cmdline/`exe` against the resolved package path |
| Migrations | `0015_gateway_vk_hashes` up `2497b54b…` down `15264e98…`; `0016_gateway_snapshot_epoch` up `92c74602…` down `73317d16…` |
| Checkpoint | `/home/pavel/step035-checkpoint` (0700/0600), provenance `839880c`, `pkg.sql` sha256 `0c99c4e1…`, `state.json` `266b761a…` |
| Baseline Caddyfile | `/home/pavel/step035-checkpoint/Caddyfile.baseline` sha256 `866b945c0bb87dacceb33390799fab24e70de0a640370b70ca836689410ef1e9` |
| Package transport | node `terlimo-035-node`, ext `193.5.251.217`, dtls `57400`, wg `57401`, api `127.0.0.1:18091`, mgmt mTLS `127.0.0.1:56331` (`gw-035.test`), workers 36 |
| Work/db (kept) | `/home/pavel/projects/terlimo-backend/.step035-runs/pkg`, db `terlimo_035` |

Binary rollback (no work-tree removal): after the package is stopped, replace
`<work>/bin/wdtt-server` (keep the previous file as a `.pre-f38200a.bak` if needed) or run the
package again with a previous `--node-bin`; pg/profile/state/network/logs are preserved. The
shared binary and A/B are never involved.

## 2. Trusted VK provisioning (protected, offline, no secrets in argv)

Source `/etc/terlimo/client-test/backend/config.json` (0600). Two node sets; never mixed:

| Config node_id | peer_ip | hashes | Use |
|---|---|---|---|
| `terlimo-test-193-5-251-217` | `193.5.251.217` | 4 | **USED** (matches the package host) |
| `test2` | `23.26.193.88` | 4 | NOT used |

Selection must match BOTH `node_id` and `peer_ip`; the config ports `56000`/`56002` are the
cohosted A/B services and are NOT used (package keeps `57400`/`57401`).

The helper never passes secrets through argv: it performs a protected, atomic offline update of
the node's own `passwords.json` (`admin_profile.vk_hashes`, comma-joined normalized list) with
same-directory temp + `fsync` + `os.replace`, mode preserved (0600), every other field
semantically preserved (reserialized JSON, not a byte copy), and a 0600
`passwords.json.pre-vk-<utc>.bak` backup. Ownership is enforced by the
existing package marker (`run_isolated.resolve_work(require_owned=True)`); the node must be
stopped (guard scans `/proc` for the owned `-config-dir <work>/gateway`, independent of any
binary name); the package binary must exist under the owned work dir.

```bash
# dry-run (checks only; prints identity/count/set fingerprint, no values):
sudo .venv/bin/python tools/step035/provision_trusted_vk.py --work .step035-runs/pkg \
  --node-id terlimo-test-193-5-251-217 --peer-ip 193.5.251.217 --expected-count 4 --dry-run
# real provisioning (node stopped):
sudo .venv/bin/python tools/step035/provision_trusted_vk.py --work .step035-runs/pkg \
  --node-id terlimo-test-193-5-251-217 --peer-ip 193.5.251.217 --expected-count 4
# scoped clear (stop procedure, optional):
sudo .venv/bin/python tools/step035/provision_trusted_vk.py --work .step035-runs/pkg \
  --node-id terlimo-test-193-5-251-217 --peer-ip 193.5.251.217 --clear
```

The node loads this profile at start; `engine_status` reports only the normalized list (node-side
`clientTestVKHashes`), which the worker snapshots into `gateways.vk_hashes` and the catalog emits.

## 3. Migrations 0015/0016 and rollback (code-first, row-preserving)

- 0015 adds nullable `gateways.vk_hashes jsonb`; 0016 adds `gateways.snapshot_epoch bigint NOT
  NULL DEFAULT 0` (metadata-only on PG 11+). Both are applied automatically by run_isolated on
  start; nothing else runs.
- Preferred rollback: revert the code (checkout the previous runtime commit) and keep the two
  additive columns. Readiness compatibility is checked explicitly: start the API with the old
  code against the migrated DB and require `/health/ready == 200` (extra columns are ignored by
  the old queries); rows and package data are untouched (`--cleanup` keeps data).
- Do **not** force-null metadata to bypass the fail-closed downs, and do not run schema-down as a
  routine step. If the project ever requires exact schema equality, the documented downs
  (`ROLLBACK_UNSAFE_0016/0015`) run only when the snapshot fields are legitimately empty; they
  delete no rows.

## 4. Scoped TEST onboarding (no deletes, PoP identity preserved)

Old access is **expired** (final checkpoint): grant `d15d3f67-0492-4458-b3aa-4b760bcc3657`,
`state=applied`, `not_after=applied_not_after=2026-09-22T00:23:24+03`, `lease_seq=2`; entitlement
`8e6f14c9-125a-4620-a65b-861df33714e2`, `ends_at=2026-09-22T00:59:17+03`. It is not presented or
reused as active, and the synthetic fixture is **updated, never deleted**:

- Identity stays: account `48b65bad-…3ab7`, installation `7b43b610-…6356` (`test`/`android`,
  state `technical`, fingerprint unchanged), binding `a35e8558-…0e61` (active).
- Take a fresh checkpoint first (the dump is the backup), then run the narrow idempotent update
  (re-run does nothing once the window is in the future):
  `UPDATE entitlements SET ends_at = now() + interval '3 hours', status='active', starts_at =
  LEAST(starts_at, now()), revision = revision + 1
   WHERE id='8e6f14c9-125a-4620-a65b-861df33714e2' AND account_id='48b65bad-9ba7-4247-b767-fddd6e093ab7'
     AND ends_at < now() RETURNING id, ends_at;`
  The refresh trigger is the window/lease bound, not the revision: `ensure_grant` compares the
  remaining lease against `max_lease_seconds` and the entitlement expiry (`ends_at`/`not_after`)
  and issues a new grant when the bound allows it; moving `ends_at` therefore leads the next
  sync to compute a fresh window inside the baseline lease rules. `revision = revision + 1` is
  kept for schema-consistent auditing and is not claimed as the cause. No new
  account/binding/entitlement, no DELETE, no appdata/installation reset.
- Profile ownership stays as in the package: `passwords.json` is 0600 root-owned (the node runs
  as root inside the throwaway netns and reads it); the helper writes it via sudo while the
  expected uid is taken from the verified work-dir `stat`, not from the marker alone.
- `attach_android.py` is only for a genuinely fresh installation (phone reinstalled), never for
  re-onboarding the existing one.
- Fixture TTL and lease are separate: entitlement window `+3 h`; the access lease stays within
  the agreed baseline `gateway_max_lease_seconds = 900` (`terlimo_backend/config.py:118/164`, no
  package env override; the previous run showed the 900 s refresh cadence, e.g. last refresh
  00:08 → `not_after` 00:23). Timers are not extended for the run.

## 5. Control/bootstrap: HTTP integration vs transport/node adapter; live VK proof not performed

The HTTP flow (PoP challenge/enrollment → session → `/me` → catalog → `/access/sync` → worker
apply → catalog state/lease equality + node readback) is **integration only**: it exercises the
API/worker/registry and the HTTP admission path and does **not** prove VK bootstrap/control.
Direct HTTPS success remains insufficient.

The VK control/bootstrap path lives at the transport/node-adapter level, separately from the
HTTP API. The API has no request-side `vk_hashes` (contracts probe: request schemas are closed;
`catalog.AccessDescriptor.vk_hashes` is optional output-only), but that fact alone does not mean
the architecture has no VK path:
- node (build `f38200a`): owner links are built as `wdtt://host:dtls:wg:mainPassword:vkHashes`
  (`bot.go`), the profile hash list is normalized (`normalizeVKHashesInput`) and reported
  output-only via `engine_status` (`clientTestVKHashes`);
- host adapter boundary (TEST): `/etc/terlimo/client-test/backend/config.json` declares
  `/run/terlimo-client-test/bootstrap.sock` and per-node adapter sockets
  (`/run/terlimo-wdtt-adapter/adapter.sock`; test2 uses `test2_ssh_stdio`); the installed adapter
  carries `shared-vk-hash` and node admin-socket access (`/usr/local/libexec/terlimo-wdtt-adapter.py`).
- transport data plane: the isolated node's DTLS/WG endpoints (`193.5.251.217:57400/57401`) are
  the address/port side of the same bootstrap; they are package-scoped and A/B (`56000/56002`)
  is untouched.

Source inspection proves the path exists; it is **not** a live verification. The package-scoped
live transport bootstrap check (seed/hash/address/ports plus the emergency own-link input) is
owner doc `14_OWNER_VK_BOOTSTRAP_AND_RECOVERY.md` scope, is not implemented or verified here,
and no HTTP response is accepted as a substitute. A new emergency recovery UI is out of scope;
live proofs must come from the owner-approved transport procedure, never claimed from source.

## 6. Exact start / check / stop (TEST only; A/B and production protected)

```bash
cd /home/pavel/projects/terlimo-backend
# 0. preflight (read-only): A/B ports, baseline edge hash, checkpoint, no package processes
ss -lunp | grep -E '56000|56002'; sha256sum /etc/terlimo-minishop-edge/Caddyfile; \
  sha256sum /home/pavel/step035-checkpoint/Caddyfile.baseline

# 1. merge the accepted package (manager): git merge --no-ff step035-resume-package
#    verify the exact final tree from the acceptance report: git rev-parse HEAD^{tree}

# 2. place the package-owned node binary and provision trusted VK while stopped (§2):
install -D -m 0755 /tmp/opencode/wdtt-server-vk .step035-runs/pkg/bin/wdtt-server
sha256sum .step035-runs/pkg/bin/wdtt-server            # 34e1f12b…
sudo .venv/bin/python tools/step035/provision_trusted_vk.py --work .step035-runs/pkg \
  --node-id terlimo-test-193-5-251-217 --peer-ip 193.5.251.217 --expected-count 4

# 3. start (package binary; no shared binary, no timeout overrides):
.venv/bin/python tools/step035/run_isolated.py --phone --serve --work .step035-runs/pkg \
  --node-bin .step035-runs/pkg/bin/wdtt-server
curl -fsS http://127.0.0.1:18091/health/ready

# 4. owner-applied TEST networking (args verified: apply/rollback/status/egress-check + --work)
tools/step035/phone_network.py apply --work .step035-runs/pkg
tools/step035/phone_network.py status --work .step035-runs/pkg
tools/step035/phone_network.py egress-check --work .step035-runs/pkg

# 5. Caddy: owner applies exactly tools/step035/caddy_mobile_api.diff, then validate + restart
#    (the edge unit's reload is broken by design: admin off), then verify the diff:
sudo /usr/local/libexec/terlimo-caddy validate --config /etc/terlimo-minishop-edge/Caddyfile
sudo systemctl restart terlimo-minishop-edge
sudo diff -u /home/pavel/step035-checkpoint/Caddyfile.baseline /etc/terlimo-minishop-edge/Caddyfile
#    expected: ONLY the reviewed 4-line @mobile_api block; other routes byte-identical; /help 200

# 6. onboarding (§4) + integration checks (§5); then the stop order (fail-closed: if any step
#    is not ok, STOP and report - never continue as if the stop succeeded):
tools/step035/attach_android.py --fingerprint <hex64> --revoke   # lifecycle revoke, rows kept
.venv/bin/python tools/step035/run_isolated.py --checkpoint --work .step035-runs/pkg   # PG live
.venv/bin/python tools/step035/run_isolated.py --stop --work .step035-runs/pkg         # processes only
.venv/bin/python tools/step035/run_isolated.py --cleanup --work .step035-runs/pkg      # no purge:
#    scoped pg_ctl stop with verified exit; pg/profile/state/network/logs kept (data_kept)
# 7. verify absence of owned processes/PG/ports BEFORE network/Caddy cleanup:
pgrep -af 'terlimo-api|terlimo-worker|wdtt-server.*config-dir .step035-runs/pkg' || echo 'no owned processes'
test -f .step035-runs/pkg/pg/postmaster.pid && echo 'PG still running' || echo 'PG stopped'
tools/step035/phone_network.py status --work .step035-runs/pkg   # only our ledgered chains/marks remain
# 8. network rollback, then Caddy removal:
tools/step035/phone_network.py rollback --work .step035-runs/pkg
tools/step035/caddy_mobile_api.py remove                        # helper supports remove/status only
sha256sum /etc/terlimo-minishop-edge/Caddyfile                  # equals baseline 866b945c…
# binary rollback (if needed): after the package is stopped, replace .step035-runs/pkg/bin/wdtt-server
# or re-run with a previous --node-bin; never remove the work tree and never touch the shared binary
```

Caddy apply/remove expectations: after apply the file hash is necessarily different; the check is
the exact diff above plus unchanged other routes and `/help` 200. After `remove` the file must be
byte-identical to the baseline (`866b945c…`) and `/help` 200 with the mobile path back to 404.
Package networking is ledgered to its own veth/netns/DNAT `57400`/`57401`; A/B `56000`/`56002` are
never touched (the stop report shows them live after rollback with `/help` 200); no production
vhost other than the additive block is edited, and only the edge unit itself is restarted.

## 7. Secret-absence control

- No hash or password value ever appears in argv, stdout, evidence, relay or git; the helper
  prints only identity/count/set fingerprint/backup name; argv-secret strings are absent from the
  source (guarded by tests).
- The trusted config and node profile stay 0600; backups are 0600; the checkpoint containing
  gateway credentials stays 0700/0600 and is never committed or relayed. Profile rewrites are
  semantic (reserialized JSON preserving every other field and the mode), not byte-preserving.

## 8. Non-goals

No live apply, phone run, migration run or entitlement issuance in this task; no A/B or
production change; no owner-addition implementation (docs 14/15); no new recovery function, API
or architectural component; no timeout extensions; Android stage `4224332` is a later step.
