# STEP036 local full-chain stand — backend side (TEST-only, no live)

Pieces: `tools/step036/local_stand.py` (managed lifecycle + ephemeral certs + readiness),
`tests/test_step036_local_stand.py` (executable smoke start/readiness/stop). Accepted
integration: commit `7548df7841b4fe93ab46158d4cc3dae4c338e717`, tree
`63bb81e51ed87616ee18f241ff8ec35c54cf1219` (merge `7d3433f`; git truth per
`git cat-file -t`).

## Start (foreground; loopback only)

    python tools/step036/local_stand.py \
        --database-url "<isolated TEST DSN, e.g. bundled pgserver>" \
        --root /tmp/step036-stand

Starts: public API on 127.0.0.1:<fixed ephemeral> (plain HTTP), EvidenceListener (mTLS;
evidence route, plus service route when `--service`), the local evidence Unix relay by
default (`--no-relay` to disable) and, with `--service`, the local service Unix relay wired
to the real public API port over the mTLS service endpoint. The start sequence registers
signal handlers first and tears down partially started pieces on failure or SIGTERM/SIGINT
(bounded 15 s stop), so no listener/socket/PG pool leaks and readiness never stays "ready"
after a stop.

Artifacts (0600): `<root>/readiness.json` (api_base, evidence host/port, server cert SPKI
pin, gateway_key, evidence relay socket, service relay socket) and the ephemeral
CA/server/node certificates under `<root>`. The Fernet onboarding key is generated in memory
and lives only in the process Settings; it is NOT written to disk, never printed, and the
stand never binds a non-loopback address, never uses SSH and never touches a live DB. On
stop the readiness file is rewritten as `{"status":"stopped"}` and sockets/certs stay under
`--root`.

## Client/node harness chain (expected, backend side ready)

1. Enroll + session over `api_base` (`/api/mobile/v1/installations`, `auth/session`) with the
   harness installation key.
2. `auth/challenge` purpose `onboarding-start-intent` → `POST /api/mobile/v1/onboarding/intents`
   (bearer session, scope `session:write`) → `pending`.
3. Real provisioning: the existing worker bootstrap callback provisions the credential to the
   TEST node (management mTLS from settings). The stand only hosts backend pieces; point the
   worker/gateway registry at the harness node.
4. Poll the same intent route → `ready` (bootstrap credential + start challenge + pinned
   gateway endpoint from the registry).
5. Client → node → `onboarding.start` RPC inside the unchanged envelope body → evidence
   admission: exactly 1 `onboarding_evidence` + 1 `onboarding_hour` entitlement,
   `not_after - started_at == 3600`, replay returns the same start without a second hour.
   Service/`background.*` bodies never create an hour.
6. Stop the stand (SIGTERM); readiness and certs stay under `--root`.

## Exact client interface still missing (honest boundary)

- The node/harness side of steps 3–5 (bootstrap credential provisioning target, node
  service/evidence relay wiring, DTLS bootstrap transport) comes with the Laptop client
  harness materials; this repository only exposes the exact start RPC schema in
  `docs/proposals/STEP036_EXPLICIT_CONNECT_CONTRACT_V1.md` (rev3) plus the live readiness pin.
- No fake business/API, no production transport/DB policy changes; provisioning callback is
  the existing worker path.

## Smoke (executed)

    PYTHONPATH=. pytest -q tests/test_step036_local_stand.py   # 2 passed, exit 0

Asserts: readiness fields + 0600 manifest, public `/health/live` 200, mTLS evidence endpoint
reachable (bounded 400 for a malformed internal body), relay socket exists and is dialable,
after stop the relay socket is removed and no evidence/entitlement rows exist.
