#!/usr/bin/env bash
# STEP03.5 phone-ready preflight — READ-ONLY. Fails closed if the plan cannot be applied.
set -euo pipefail

EXTERNAL_IP="193.5.251.217"
IFACE="ens3"
NS="terlimo-035"
VETH="veth35h"
DTLS_PORT="57400"
WG_PORT="57401"
AB_PORTS="56000 56001 56002"

fail=0
note() { printf '%s\n' "$*"; }
bad() { printf 'BLOCKER: %s\n' "$*"; fail=1; }

# Host/interface
ip -brief addr show "$IFACE" | grep -q "$EXTERNAL_IP" || bad "$IFACE missing $EXTERNAL_IP"
ip route show default | grep -q "dev $IFACE" && note "default route via $IFACE (untouched)" || bad "default route not on $IFACE"

# Forwarding
[[ "$(sysctl -n net.ipv4.ip_forward)" == "1" ]] && note "ip_forward=1" || bad "ip_forward=0 (DNAT path needs it)"

# Namespace/veth conflicts
ip netns list 2>/dev/null | grep -qx "$NS" && bad "namespace $NS already exists"
ip link show "$VETH" >/dev/null 2>&1 && bad "$VETH already exists"

# Ports free (UDP + TCP) and no overlap with live A/B ports
for port in "$DTLS_PORT" "$WG_PORT"; do
  if ss -lnuH | awk '{print $4}' | grep -qE "[:.]$port$"; then bad "UDP $port already in use"; fi
  if ss -lntH | awk '{print $4}' | grep -qE "[:.]$port$"; then bad "TCP $port already in use"; fi
  note "port $port free"
done
for port in $AB_PORTS; do
  if ss -lnuH | awk '{print $4}' | grep -qE "[:.]$port$"; then
    note "A/B port $port live (must stay untouched)"
  fi
done

# Firewall state (no UFW mutation is planned: the DNAT/forward path uses our own chains)
if command -v ufw >/dev/null 2>&1; then
  ufw_status="$(sudo -n ufw status 2>/dev/null || ufw status 2>/dev/null || true)"
  printf '%s\n' "$ufw_status" | grep -q "^Status: active" && note "ufw active (left untouched)" || note "ufw not active/unreadable"
fi
sudo -n iptables -t nat -S 2>/dev/null | grep -q -- "--dport ${DTLS_PORT}" && bad "existing DNAT for ${DTLS_PORT}"
sudo -n iptables -S FORWARD 2>/dev/null | grep -q -- "veth35h" && bad "existing FORWARD rules for $VETH"
for chain in STEP035-NAT STEP035-POST STEP035-FWD; do
  sudo -n iptables -S "$chain" >/dev/null 2>&1 && bad "chain $chain already exists"
done
[[ -f "$(dirname "$0")/../../.step035-runs/pkg/network.json" ]] && note "existing package network ledger found (owned rerun path)"

if [[ "$fail" == "0" ]]; then
  note "PREFLIGHT OK: plan is additive (netns $NS, $VETH 10.35.0.1/30, DNAT $EXTERNAL_IP:$DTLS_PORT)"
else
  note "PREFLIGHT FAILED: nothing was changed"
fi
exit "$fail"
