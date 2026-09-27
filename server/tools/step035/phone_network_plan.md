# STEP03.5 phone-ready network plan (TEST, not applied)

Status: prepared, **not applied**. No namespace/veth/firewall/route change was made; no public
reload. Owner review first. A/B node, personal grants, vhosts and production are untouched.

## 1. Read-only recon (2026-09-21, this host)

| Item | Observed |
|---|---|
| External address | `ens3 193.5.251.217/32`, default via `10.0.0.1 dev ens3 onlink` |
| A/B real WDTT node | pid 578560 `wdtt-server -listen=0.0.0.0:56000 -wg-port=56001 -config-dir=/etc/wdtt -max-workers-per-access=36 ...` |
| A/B UDP listeners | `*:56000` (DTLS), `127.0.0.1:56002` (local) |
| Legacy managed iface | `wdttm0 10.67.67.1/24`, NAT MASQUERADE rule `wl-test-managed-20260910` |
| Forwarding | `net.ipv4.ip_forward=1` (docker), ipv6 forwarding `0` |
| UFW | active, default INPUT/FORWARD DROP; allowed: 22/tcp, 80/tcp, 443/tcp, **56000/udp only** |
| Other rules | docker NAT for 18080/18082; no rule for 57400/57401 |
| Candidate ports | UDP `57400` (DTLS), `57401` (WG) — free, no TCP use |
| Existing namespaces | none (`ip netns list` empty); `veth35h` absent |

Consequences:
- The phone reaches the A/B node with **only the DTLS UDP port** allowed externally (WG port is
  not in UFW). The fixture node follows the same model: expose DTLS `57400/udp`; `57401` stays
  internal unless the Android data path proves it needs a direct WG port (then add the second
  rule, same pattern).
- A second **host-network** `wdtt-server` is not used: the binary carries its own
  iface/NAT helper logic (`WDTT_IFACE`, MASQUERADE, proxy chains) and A/B already owns `wdttm0`;
  running it on the host risks shared iface/route/NAT churn. The node therefore runs in a
  **named network namespace** with an explicit veth + DNAT, which is fully additive and
  individually reversible. This is the accepted 03.3 isolation model plus external routing.

## 2. Design (primary, transactional)

Names: namespace `terlimo-035`, host veth `veth35h 10.35.0.1/30`, ns veth `veth35n 10.35.0.2/30`,
node DTLS `0.0.0.0:57400`, node WG `57401` (NOT opened), admin socket
`<work>/gateway/admin.sock`, management mTLS handler `127.0.0.1:56331` (Backend side unchanged).
All host rules live in this package's dedicated chains (`STEP035-NAT`, `STEP035-POST`,
`STEP035-FWD`) jumped once from PREROUTING/POSTROUTING/FORWARD; no foreign chain is touched and
no UFW rule is added (the DNAT path is FORWARD, and our `-I FORWARD 1` jump precedes the UFW
chains; A/B UFW rules stay as they are).

Actual packet path (traced):

```
phone ──UDP/DTLS 193.5.251.217:57400──► ens3 ──PREROUTING STEP035-NAT DNAT──► 10.35.0.2:57400
                                                                                (netns node)
phone payload / node DNS ──node helper inside netns──► default via 10.35.0.1 ──► veth35h
   ──FORWARD STEP035-FWD (veth35h→ens3 ACCEPT)──► POSTROUTING STEP035-POST MASQUERADE
   (10.35.0.0/30 → ens3) ──► internet; returns match conntrack ESTABLISHED,RELATED
Backend worker ──mTLS 127.0.0.1:56331──► management-handler ──admin.sock──► node in netns
```

Rules created (exact, only these):

```
ip netns add terlimo-035; veth35h<->veth35n; 10.35.0.1/30, 10.35.0.2/30; ns default 10.35.0.1
iptables -t nat -N STEP035-NAT;  -I PREROUTING 1 -j STEP035-NAT
  -A STEP035-NAT -d 193.5.251.217 -p udp --dport 57400 -j DNAT --to-destination 10.35.0.2:57400
iptables -t nat -N STEP035-POST; -I POSTROUTING 1 -j STEP035-POST
  -A STEP035-POST -s 10.35.0.0/30 -o ens3 -j MASQUERADE
iptables -N STEP035-FWD;         -I FORWARD 1 -j STEP035-FWD
  -A STEP035-FWD -i veth35h -o ens3 -j ACCEPT                                   # egress (DNS/proxy/payload)
  -A STEP035-FWD -i ens3 -o veth35h -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  -A STEP035-FWD -i ens3 -o veth35h -p udp -d 10.35.0.2 --dport 57400 -j ACCEPT  # inbound DTLS
```

- Egress is mandatory and scoped: `veth35h→ens3` accept + subnet MASQUERADE + conntrack return.
- Never reused/borrowed: A/B `10.67.67.0/24`, its MASQUERADE (`wl-test-managed-*`), docker
  chains/rules, default routes, SSH.
- WG `57401` is intentionally **not** opened: current evidence (A/B allows only `56000/udp`
  externally; High's observation of DTLS-only) is an inference, and the actual Android wdtt
  source/protocol path (whether a direct WG port is needed) must be proven first; the tool
  records `wg_port_opened: false`.

Execution is transactional and ownership-ledgered:

- `tools/step035/phone_network.py apply|rollback|status|egress-check` (wrapper
  `phone_network.sh`), ledger `<work>/network.json` inside the 75352cf ownership-marked work
  dir.
- Collision with unknown namespace/veth/chains (no ledger) refuses with zero changes; an owned
  rerun is idempotent (`already_applied`, no duplicate rules); a partial failure rolls back
  **only** resources created by the current transaction and preserves prior owned state;
  rollback without a ledger refuses; rollback failure is explicit (`rollback_failed` +
  per-step failures). Fault-injection tests stub every command stage.
- `apply`/`rollback` were NOT executed here; shared network is untouched.

Static/read-only validation performed: `iptables-restore --test`-equivalent syntax checks are
covered by rule-assembly unit tests, guard/fault tests (8 passed), `bash -n`, and the read-only
`preflight_phone.sh` (ports free, A/B not overlapped, forwarding on, no foreign
netns/veth/rules).

## 4. Phone path after apply

1. Start the persistent server: `.venv/bin/python tools/step035/run_isolated.py --phone --serve`
   (starts netns node on 57400, registry `gateway_id == node_id == terlimo-035-node`,
   `dtls_port=57400`, `wg_port=57401`, `peer_ip=193.5.251.217`, `target_workers=36`, mTLS route).
2. Private readiness on the API's real route `http://127.0.0.1:18091/health/ready` (there is no
   `/api/mobile/v1/health`; `/health/*` is not published). Then the phone registers through the
   existing seed URL via the reviewed Caddy diff (`caddy_mobile_api.diff`).
3. `attach_android.py --fingerprint <phone installation fingerprint>` binds the synthetic
   TEST fixture (paid, not trial) to the **phone's own** installation and prints the next steps.
4. Phone performs its own PoP session (`session:read`, `access:sync`), `/me`, `/gateways`,
   `/access/sync`, then connects to `193.5.251.217:57400` with catalog transport/pin; readback
   must confirm the grant.
5. Cleanup: `attach_android.py --revoke --fingerprint ...` (desired revoke -> confirmed actual
   revoke) and `run_isolated.py --cleanup`; `phone_network.sh rollback`.

Fault tests (TEST scope, later): lost sync reply (same key/digest retry), register sync after
restart, worker restart mid-apply, unavailable node (bounded failure, no false applied) —
all documented in the README; no live production involvement.
