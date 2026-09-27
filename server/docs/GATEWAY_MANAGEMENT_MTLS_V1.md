# TERLIMO gateway management transport v1 (Backend ↔ management-handler)

Status: internal technical transport for `CONTROL_BOUND_PROFILE`-pending TEST; not a mobile
or C01 contract change. It adds a private authenticated TLS/mTLS endpoint in front of the
existing local WDTT `client-test` admin socket, per target architecture §5.2.

## 1. Topology

```
Backend (GatewayControlHandlers)
  └─ ManagementTlsClient ── mTLS (client cert = Backend identity) ──► management-handler
                                                                       │ fixed typed ops only
                                                                       ▼
                                                       local admin.sock ("client-test")
                                                       main_password stays local to the node
```

The handler never accepts the node `main_password` from the network and never forwards
arbitrary args, shell, SQL, URLs or filesystem paths. The local admin socket remains the only
gateway control surface and is not exposed publicly.

## 2. Versioned request/response schema v1

One JSON object per TLS connection.

Request:

```json
{
  "v": 1,
  "op": "provision|refresh|revoke|get|status",
  "node_id": "<expected gateway node id>",
  "credential": "<per-grant technical credential>",
  "fields": { "...": "op-specific, strict" }
}
```

Response:

```json
{"v": 1, "status": "ok", "readback": { "...": "existing client-test readback" }}
{"v": 1, "status": "error", "code": "AUTHZ_DENIED|NODE_ID_MISMATCH|BAD_MESSAGE|<gateway code>"}
```

Strictness: `node_id` is required for every request; `credential` is optional at the schema
level and required by the op (provision/refresh/revoke/get), not by `status`. Unknown
top-level keys, unknown `fields`, wrong types, oversize values and expired/out-of-bounds
`expires_at` are rejected before any local socket call. No field is
passed through as-is: the handler validates and rebuilds the fixed local command.

Allowed `fields` per op (exact set; all values validated):

| op | fields |
|---|---|
| `provision` | `grant_id`, `registration_id`, `public_key_spki`, `generation`, `lease_seq`, `operation_id`, `expires_at` |
| `refresh` | same as `provision` plus `expected_seq` |
| `revoke` | `grant_id`, `registration_id`, `public_key_spki`, `generation`, `lease_seq`, `operation_id`, `expires_at` |
| `get` | none |
| `status` | none (no credential required) |

Bounds: ids/operation id ≤ 128 chars; `public_key_spki` ≤ 512 base64url chars; generation and
lease_seq are decimal strings ≤ 19 digits, ≥ 1; `expires_at` is an integer within
`[now - 120, now + GATEWAY_MANAGEMENT_MAX_NOT_AFTER]` (default 86400 s).

## 3. Mapping to the existing local commands (1:1, no tunnel)

| network op | local `client-test` command |
|---|---|
| `provision` | `grant_provision` (revoked=false) |
| `refresh` | `refresh_lease` (with `expected_seq`) |
| `revoke` | `grant_revoke` (revoked=true) |
| `get` | `grant_get` (password=credential, grant.node_id=node_id) |
| `status` | `engine_status` |

The `node_id` sent by the Backend must equal the handler's configured node id; a mismatch is
`NODE_ID_MISMATCH` and the local socket is not touched. The local main password is used only
by the handler, from its env/credentials file.

## 4. Identity model (mTLS)

- Server identity: handler certificate/key (`GATEWAY_MANAGEMENT_TLS_CERT`,
  `GATEWAY_MANAGEMENT_TLS_KEY`); SAN/hostname checked by the Backend client
  (`server_name` from the admin-controlled gateway registry entry).
- Client identity: Backend client certificate (`GATEWAY_MANAGEMENT_CA_FILE`,
  `GATEWAY_MANAGEMENT_CERT_FILE`, `GATEWAY_MANAGEMENT_KEY_FILE`).
- The handler requires a client certificate (`CERT_REQUIRED`, trusted CA loaded from
  `GATEWAY_MANAGEMENT_CLIENT_CA`), then explicitly checks the peer certificate's CN/SAN against
  the allowlist `GATEWAY_MANAGEMENT_ALLOWED_BACKEND_IDENTITIES` — a valid certificate from the
  same CA is not sufficient.
- TLS verify is never disabled; no hostname/SAN/CA bypass; no plaintext or local fallback if
  TLS setup fails (the handler exits non-zero instead of starting).

## 5. Registry and deployment

- Gateway registry rows (admin-controlled in the Backend database) carry a technical
  `endpoints` object: `{"node_id": ..., "management": {"host": "127.0.0.1", "port": 56400,
  "server_name": "gw-node.test"}}`. Without a `management` entry the Backend uses the local
  admin socket (isolated TEST only); arbitrary URLs from mobile requests are never used.
- Handler env (private bind by default `127.0.0.1`): `GATEWAY_MANAGEMENT_BIND`,
  `GATEWAY_MANAGEMENT_PORT`, `GATEWAY_MANAGEMENT_TLS_CERT`, `GATEWAY_MANAGEMENT_TLS_KEY`,
  `GATEWAY_MANAGEMENT_CLIENT_CA`, `GATEWAY_MANAGEMENT_ALLOWED_BACKEND_IDENTITIES`,
  `GATEWAY_MANAGEMENT_NODE_ID`, `GATEWAY_MANAGEMENT_ADMIN_SOCKET`,
  `GATEWAY_MANAGEMENT_MAIN_PASSWORD`, `GATEWAY_MANAGEMENT_MAX_NOT_AFTER`.
- Certificates/keys are provisioned per deployment (TEST: temporary CA/server/client
  identities under a private directory); they are never committed and never printed in
  reports. Rotation is a restart with new credential files; no fallback listener is opened.

## 6. Compatibility and versioning

- `v` is mandatory; unknown versions are refused. Adding fields requires a new version or a
  documented additive field in a frozen schema revision; existing mobile/C01 contracts are
  untouched.
- The local admin wire stays the reviewed `client-test` surface; this layer is replaceable
  without changing Backend application services or the gateway data plane.
