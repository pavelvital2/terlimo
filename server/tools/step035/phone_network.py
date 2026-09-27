#!/usr/bin/env python3
"""STEP03.5 phone-ready network: transactional, ownership-ledgered apply/rollback (not applied).

Own resources only (dedicated chains + netns/veth named for this package). Never flushes or
deletes foreign chains/rules; a collision with unknown resources refuses without changes. The
POSTUP egress path is mandatory: client tunnel -> node helper (namespace) -> veth -> host ens3
with subnet-scoped MASQUERADE and conntrack return. WG port 57401 is intentionally not opened
until the actual Android wdtt source/protocol path proves a direct WG port is required.
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def _load_run_module():
    spec = importlib.util.spec_from_file_location(
        "step035_run_isolated_net", Path(__file__).resolve().parent / "run_isolated.py"
    )
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


run_module = _load_run_module()

EXTERNAL_IP = "193.5.251.217"
IFACE = "ens3"
NS = "terlimo-035"
VETH = "veth35h"
VPEER = "veth35n"
HOST_IP = "10.35.0.1/30"
NS_CIDR = "10.35.0.0/30"
NS_IP = "10.35.0.2/30"
DTLS_PORT = 57400
WG_PORT = 57401
LEDGER = "network.json"
CHAIN_NAT = "STEP035-NAT"
CHAIN_POST = "STEP035-POST"
CHAIN_FWD = "STEP035-FWD"
# WG source isolation: the managed data plane lives on wdttm0 inside the namespace, but the host
# legacy engine uses the same 10.67.67.0/24 subnet and steals the return path. A package-owned
# SNAT inside the namespace maps the managed WG source to the namespace veth address, so the
# host only ever sees 10.35.0.2 and the existing host MASQUERADE/return route works without the
# legacy 10.67 dependency. Only our own chain is touched; the host tables are never edited here.
WG_IFACE = "wdttm0"
WG_CIDR = "10.67.67.0/24"
WG_NS_IP = "10.67.67.1/24"
CHAIN_SNAT = "STEP035-SNAT"


class Runner:
    """Executes system commands; injectable for fault/stub tests.

    Owner runs the tool as their own user; privileged commands (iptables/ip) get `sudo -n`
    so the ledger and work dir stay owner-owned.
    """

    PRIVILEGED = ("iptables", "ip")

    def _command(self, args: list[str]) -> list[str]:
        if args and args[0] in self.PRIVILEGED:
            return ["sudo", "-n", *args]
        return args

    def run(self, args: list[str], *, check: bool = True) -> str:
        command = self._command(args)
        result = subprocess.run(command, capture_output=True, text=True, check=False)
        if check and result.returncode != 0:
            raise RuntimeError(
                f"command failed ({result.returncode}): {' '.join(command)}: "
                f"{result.stderr.strip()[:200]}"
            )
        return result.stdout

    def exists(self, args: list[str]) -> bool:
        result = subprocess.run(self._command(args), capture_output=True, text=True, check=False)
        return result.returncode == 0 and bool(result.stdout.strip())

    def try_run(self, args: list[str]) -> tuple[bool, str]:
        result = subprocess.run(self._command(args), capture_output=True, text=True, check=False)
        return result.returncode == 0, result.stderr.strip()[:160]

    def probe(self, args: list[str]) -> tuple[int, str]:
        result = subprocess.run(self._command(args), capture_output=True, text=True, check=False)
        return result.returncode, result.stderr.strip()


def ledger_path(work: Path) -> Path:
    return work / LEDGER


def load_ledger(work: Path) -> dict | None:
    path = ledger_path(work)
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except ValueError:
        return None


def save_ledger(work: Path, data: dict) -> None:
    path = ledger_path(work)
    path.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    path.chmod(0o600)


def owned_work(raw: str) -> Path:
    work, _marker = run_module.resolve_work(raw, require_owned=True, for_destructive=True)
    return work


def _ns_present(runner: Runner) -> bool:
    return NS in runner.run(["ip", "netns", "list"]).split()


def _veth_present(runner: Runner) -> bool:
    return runner.exists(["ip", "link", "show", VETH])


def _chain_present(runner: Runner, table: str, chain: str) -> bool:
    return runner.exists(["iptables", "-t", table, "-S", chain])


def _jump_present(runner: Runner, table: str, parent: str, chain: str) -> bool:
    listing = runner.run(["iptables", "-t", table, "-S", parent])
    needle = f"-j {chain}"
    return any(line.startswith("-A") and needle in line for line in listing.splitlines())


def _chain_rules(runner: Runner, table: str, chain: str) -> list[str]:
    return [
        line
        for line in runner.run(["iptables", "-t", table, "-S", chain]).splitlines()
        if line.startswith("-A")
    ]


def _rule_probe(runner: Runner, check_args: list[str]) -> bool:
    """Semantic rule presence via `iptables -C`: 0 present, 1 absent, other = operational error.

    `-S` output is canonicalized (/32, -m udp, ctstate order, option order), so exact string
    matching is fragile; `-C` answers the intended question directly. A missing chain (exit 1
    with "No chain") is an operational error for callers, never silently "absent".
    """
    code, error = runner.probe(check_args)
    if code == 0:
        return True
    if code == 1 and "No chain" not in error:
        return False
    raise run_module.guard_error(f"iptables -C failed ({code}): {error[:120]}")


def _rule_present(runner: Runner, table: str, chain: str, spec: str) -> bool:
    return _rule_probe(runner, ["iptables", "-t", table, "-C", chain, *spec.split()])


def _ns_addr(runner: Runner, iface: str) -> str | None:
    """Exact IPv4 CIDR of an interface inside the package namespace (or None)."""
    listing = runner.run(["ip", "netns", "exec", NS, "ip", "-o", "-4", "addr", "show", "dev", iface], check=False)
    for token in listing.split():
        if "/" in token and token[0].isdigit():
            return token
    return None


def _ns_chain_present(runner: Runner, chain: str) -> bool:
    return runner.exists(["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-S", chain])


def _ns_chain_rules(runner: Runner, chain: str) -> list[str]:
    listing = runner.run(["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-S", chain], check=False)
    return [line for line in listing.splitlines() if line.startswith("-A")]


def _ns_rule_present(runner: Runner, chain: str, spec: str) -> bool:
    return _rule_probe(runner, ["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-C", chain, *spec.split()])


def _ns_jump_present(runner: Runner, parent: str, chain: str) -> bool:
    listing = runner.run(["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-S", parent], check=False)
    needle = f"-j {chain}"
    return any(line.startswith("-A") and needle in line for line in listing.splitlines())


def _snat_spec() -> str:
    return f"-s {WG_CIDR} -o {VPEER} -j SNAT --to-source {NS_IP.split('/')[0]}"


def snat_state(runner: Runner) -> dict:
    state = {"chain": False, "jump": False, "rule": False, "rules": 0, "unexpected_rules": 0, "wg_cidr": None}
    if not _ns_present(runner):
        return state
    state["wg_cidr"] = _ns_addr(runner, WG_IFACE)
    if not _ns_chain_present(runner, CHAIN_SNAT):
        return state
    state["chain"] = True
    state["jump"] = _ns_jump_present(runner, "POSTROUTING", CHAIN_SNAT)
    state["rule"] = _ns_rule_present(runner, CHAIN_SNAT, _snat_spec())
    rules = _ns_chain_rules(runner, CHAIN_SNAT)
    state["rules"] = len(rules)
    state["unexpected_rules"] = max(0, len(rules) - (1 if state["rule"] else 0))
    return state


def _snat_guards(runner: Runner) -> None:
    """Fail closed unless this is the owned namespace with the expected managed data-plane."""
    if not _ns_present(runner):
        raise run_module.guard_error("namespace absent: apply the package network first")
    peer = _ns_addr(runner, VPEER)
    if peer != NS_IP:
        raise run_module.guard_error(f"namespace peer address mismatch ({peer!r} != {NS_IP!r})")
    wg = _ns_addr(runner, WG_IFACE)
    if wg != WG_NS_IP:
        raise run_module.guard_error(f"managed WG address mismatch ({wg!r} != {WG_NS_IP!r})")


def current_state(runner: Runner) -> dict:
    return {
        "namespace": _ns_present(runner),
        "veth": _veth_present(runner),
        "chains": {
            CHAIN_NAT: _chain_present(runner, "nat", CHAIN_NAT),
            CHAIN_POST: _chain_present(runner, "nat", CHAIN_POST),
            CHAIN_FWD: _chain_present(runner, "filter", CHAIN_FWD),
        },
        "jumps": {
            CHAIN_NAT: _jump_present(runner, "nat", "PREROUTING", CHAIN_NAT),
            CHAIN_POST: _jump_present(runner, "nat", "POSTROUTING", CHAIN_POST),
            CHAIN_FWD: _jump_present(runner, "filter", "FORWARD", CHAIN_FWD),
        },
        "snat": snat_state(runner),
    }


def _rule_specs() -> dict[str, list[str]]:
    return {
        CHAIN_NAT: [
            (
                f"-d {EXTERNAL_IP} -p udp --dport {DTLS_PORT} "
                f"-j DNAT --to-destination 10.35.0.2:{DTLS_PORT}"
            )
        ],
        CHAIN_POST: [f"-s {NS_CIDR} -o {IFACE} -j MASQUERADE"],
        CHAIN_FWD: [
            # 1) node/helper egress from the namespace (DNS, proxy, tunnel payloads)
            f"-i {VETH} -o {IFACE} -j ACCEPT",
            # 2) return path for that egress
            f"-i {IFACE} -o {VETH} -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
            # 3) inbound DTLS after DNAT (return covered by conntrack)
            f"-i {IFACE} -o {VETH} -p udp -d 10.35.0.2 --dport {DTLS_PORT} -j ACCEPT",
        ],
    }


def ensure_snat(runner: Runner, step, owned: bool) -> dict:
    """Idempotent package-owned SNAT inside the namespace (exact own chain only).

    A same-named chain without package ownership evidence is foreign: fail closed, never add
    rules to it and never flush/delete it. An owned chain must contain exactly the intended
    rule; any unexpected rule refuses the apply (no silent cleanup of foreign content).
    """
    _snat_guards(runner)
    spec = _snat_spec()
    changes = {"chain": False, "rule": False, "jump": False}
    if _ns_chain_present(runner, CHAIN_SNAT):
        if not owned:
            raise run_module.guard_error(
                f"{CHAIN_SNAT} exists without package ownership evidence: refusing (foreign chain)"
            )
        intended = _ns_rule_present(runner, CHAIN_SNAT, spec)
        rules = _ns_chain_rules(runner, CHAIN_SNAT)
        if len(rules) - (1 if intended else 0) > 0:
            raise run_module.guard_error(
                f"{CHAIN_SNAT} contains unexpected rules ({len(rules)}): refusing without cleanup"
            )
    else:
        step("ns-snat-chain", ["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-N", CHAIN_SNAT])
        changes["chain"] = True
    if not _ns_rule_present(runner, CHAIN_SNAT, spec):
        step("ns-snat-rule", ["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-A", CHAIN_SNAT, *spec.split()])
        changes["rule"] = True
    if not _ns_jump_present(runner, "POSTROUTING", CHAIN_SNAT):
        step("ns-snat-jump", ["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-I", "POSTROUTING", "1", "-j", CHAIN_SNAT])
        changes["jump"] = True
    return changes


def remove_snat(runner: Runner, owned: bool) -> list[str]:
    """Remove exactly the package-owned SNAT rule/jump; delete the chain only if owned+empty."""
    failures: list[str] = []
    if not _ns_present(runner) or not _ns_chain_present(runner, CHAIN_SNAT):
        return failures
    if not owned:
        return [f"{CHAIN_SNAT} has no package ownership evidence: left untouched"]
    spec = _snat_spec()
    if _ns_jump_present(runner, "POSTROUTING", CHAIN_SNAT):
        ok, error = runner.try_run(["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-D", "POSTROUTING", "-j", CHAIN_SNAT])
        if not ok:
            failures.append(f"snat-jump: {error}")
    if _ns_rule_present(runner, CHAIN_SNAT, spec):
        ok, error = runner.try_run(["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-D", CHAIN_SNAT, *spec.split()])
        if not ok:
            failures.append(f"snat-rule: {error}")
    remaining = _ns_chain_rules(runner, CHAIN_SNAT)
    if remaining:
        failures.append(f"snat-chain: {len(remaining)} unexpected rule(s) remain; chain kept")
        return failures
    ok, error = runner.try_run(["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-F", CHAIN_SNAT])
    if not ok:
        failures.append(f"snat-flush: {error}")
        return failures
    ok, error = runner.try_run(["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-X", CHAIN_SNAT])
    if not ok:
        failures.append(f"snat-delete: {error}")
    return failures


def apply(runner: Runner, work: Path) -> dict:
    ledger = load_ledger(work)
    state = current_state(runner)
    foreign = []
    if (state["namespace"] or state["veth"] or any(state["chains"].values())) and ledger is None:
        foreign.append("network resources exist without this package's ledger")
    if foreign:
        raise run_module.guard_error("; ".join(foreign))
    if ledger is not None and state["namespace"] and state["veth"] and all(state["chains"].values()) and all(
        state["jumps"].values()
    ):
        return {"result": "already_applied", "ledger": str(ledger_path(work))}

    created: list[dict] = []
    if ledger is not None and ledger.get("created"):
        created = list(ledger["created"])
    result: dict = {
        "version": 1,
        "namespace": NS,
        "veth": VETH,
        "peer": VPEER,
        "host_ip": HOST_IP,
        "ns_ip": NS_IP,
        "external_ip": EXTERNAL_IP,
        "dtls_port": DTLS_PORT,
        "wg_port_opened": False,
        "chains": {"nat": CHAIN_NAT, "post": CHAIN_POST, "filter": CHAIN_FWD},
        "created": created,
        "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }

    def step(kind: str, args: list[str]) -> None:
        runner.run(args)
        result["created"].append({"kind": kind, "args": args})
        save_ledger(work, result)

    try:
        if not _ns_present(runner):
            step("netns", ["ip", "netns", "add", NS])
        if not _veth_present(runner):
            step("veth", ["ip", "link", "add", VETH, "type", "veth", "peer", "name", VPEER])
            step("veth-ns", ["ip", "link", "set", VPEER, "netns", NS])
            step("veth-host-addr", ["ip", "addr", "add", HOST_IP, "dev", VETH])
            step("veth-host-up", ["ip", "link", "set", VETH, "up"])
            step("ns-lo-up", ["ip", "netns", "exec", NS, "ip", "link", "set", "lo", "up"])
            step("ns-peer-addr", ["ip", "netns", "exec", NS, "ip", "addr", "add", NS_IP, "dev", VPEER])
            step("ns-peer-up", ["ip", "netns", "exec", NS, "ip", "link", "set", VPEER, "up"])
            step("ns-default", ["ip", "netns", "exec", NS, "ip", "route", "add", "default", "via", "10.35.0.1"])
        specs = _rule_specs()
        for table, chain, jump_parent in (
            ("nat", CHAIN_NAT, "PREROUTING"),
            ("nat", CHAIN_POST, "POSTROUTING"),
            ("filter", CHAIN_FWD, "FORWARD"),
        ):
            if not _chain_present(runner, table, chain):
                step(f"chain-{chain}", ["iptables", "-t", table, "-N", chain])
            for spec in specs[chain]:
                if not _rule_present(runner, table, chain, spec):
                    step(f"rule-{chain}", ["iptables", "-t", table, "-A", chain, *spec.split()])
            if not _jump_present(runner, table, jump_parent, chain):
                step(f"jump-{chain}", ["iptables", "-t", table, "-I", jump_parent, "1", "-j", chain])
        ledger_owned = bool(
            ledger and (ledger.get("snat") or any(
                str(entry.get("kind", "")).startswith("ns-snat-") for entry in ledger.get("created", [])
            ))
        )
        if _ns_addr(runner, WG_IFACE) == WG_NS_IP:
            changes = ensure_snat(runner, step, owned=ledger_owned)
            result["snat"] = {"chain": CHAIN_SNAT, "source": WG_CIDR, "to": NS_IP.split("/")[0],
                              "iface": VPEER, "applied": changes}
        elif _ns_addr(runner, WG_IFACE) is None:
            result["snat"] = {"status": "deferred", "reason": "managed WG interface not present yet; run `snat apply` when the node is up"}
        else:
            raise run_module.guard_error(
                f"managed WG address mismatch ({_ns_addr(runner, WG_IFACE)!r} != {WG_NS_IP!r})"
            )
    except Exception as error:
        rollback_created(runner, result["created"])
        result["failed"] = str(error)
        result["created"] = []
        save_ledger(work, result)
        raise run_module.guard_error(
            f"apply failed at current step and rolled back this transaction only: {error}"
        ) from error
    result["result"] = "applied"
    save_ledger(work, result)
    return result


def rollback_created(runner: Runner, created: list[dict]) -> list[str]:
    """Reverse only the resources created by this transaction (exact commands)."""
    failures: list[str] = []
    for entry in reversed(created):
        kind = entry["kind"]
        args = list(entry["args"])
        try:
            if kind.startswith("rule-"):
                chain = args[args.index("-A") + 1]
                table = args[args.index("-t") + 1]
                spec = args[args.index(chain) + 1 :]
                runner.run(["iptables", "-t", table, "-D", chain, *spec], check=False)
            elif kind.startswith("jump-"):
                chain = args[-1]
                parent = args[args.index("-I") + 1]
                table = args[args.index("-t") + 1]
                runner.run(["iptables", "-t", table, "-D", parent, "-j", chain], check=False)
            elif kind.startswith("chain-"):
                chain = args[-1]
                table = args[args.index("-t") + 1]
                runner.run(["iptables", "-t", table, "-F", chain], check=False)
                runner.run(["iptables", "-t", table, "-X", chain], check=False)
            elif kind.startswith("ns-snat-"):
                if "rule" in kind:
                    chain = args[args.index("-A") + 1]
                    spec = args[args.index(chain) + 1 :]
                    ok, error = runner.try_run(["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-D", chain, *spec])
                    if not ok:
                        failures.append(f"{kind}: {error}")
                elif "jump" in kind:
                    parent = args[args.index("-I") + 1]
                    ok, error = runner.try_run(["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-D", parent, "-j", CHAIN_SNAT])
                    if not ok:
                        failures.append(f"{kind}: {error}")
                elif "chain" in kind:
                    if _ns_chain_rules(runner, CHAIN_SNAT):
                        failures.append("ns-snat-chain: unexpected rules remain; chain kept")
                    else:
                        ok, error = runner.try_run(["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-F", CHAIN_SNAT])
                        if ok:
                            ok, error = runner.try_run(["ip", "netns", "exec", NS, "iptables", "-t", "nat", "-X", CHAIN_SNAT])
                        if not ok:
                            failures.append(f"{kind}: {error}")
            elif kind == "netns":
                runner.run(["ip", "netns", "del", NS], check=False)
            elif kind == "veth":
                runner.run(["ip", "link", "del", VETH], check=False)
        except Exception as error:  # noqa: BLE001 - explicit rollback failure
            failures.append(f"{kind}: {error}")
    return failures


def _live_package_processes(work: Path) -> list[str]:
    live: list[str] = []
    for name in ("serve", "gateway"):
        record_path = run_module.pid_record_path(work, name)
        if not record_path.is_file():
            continue
        try:
            record = json.loads(record_path.read_text(encoding="utf-8"))
        except ValueError:
            continue
        if run_module.verified_alive(record):
            live.append(name)
    return live


def rollback(runner: Runner, work: Path) -> dict:
    live = _live_package_processes(work)
    if live:
        raise run_module.guard_error(
            f"{', '.join(live)} still alive: stop the package processes before network rollback"
        )
    unproven = [
        kind
        for kind in run_module.expected_kinds(work)
        if not run_module.pid_record_path(work, kind).is_file()
    ] if (work / "state.json").is_file() else []
    if unproven:
        raise run_module.guard_error(
            f"cannot prove stopped: missing ownership records {', '.join(unproven)}; "
            "run --adopt (live) or --stop (fresh) first"
        )
    ledger = load_ledger(work)
    if ledger is None:
        raise run_module.guard_error("no ownership ledger: refusing to roll back unknown resources")
    created = list(ledger.get("created", []))
    # Prefer the complete recorded set if the incremental ledger missed trailing entries.
    state = current_state(runner)
    if state["namespace"] and not any(entry["kind"] == "netns" for entry in created):
        created.append({"kind": "netns", "args": ["ip", "netns", "add", NS]})
    if state["veth"] and not any(entry["kind"] == "veth" for entry in created):
        created.append({"kind": "veth", "args": ["ip", "link", "add", VETH, "type", "veth", "peer", "name", VPEER]})
    failures = rollback_created(runner, created)
    if not failures:
        ledger_path(work).unlink(missing_ok=True)
    result = {"result": "rolled_back" if not failures else "rollback_failed", "failures": failures}
    if failures:
        save_ledger(work, {**ledger, "rollback_failures": failures})
    return result


def egress_check(runner: Runner) -> dict:
    """Exact post-apply check: namespace egress (DNS UDP + TCP) and MASQUERADE counters."""
    if not _ns_present(runner):
        raise run_module.guard_error("namespace absent: apply first")
    probe = (
        "import socket,sys;"
        "s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.settimeout(5);"
        "s.sendto(b'\\x12\\x34\\x01\\x00\\x00\\x01\\x00\\x00\\x00\\x00\\x00\\x00"
        "\\x06github\\x03com\\x00\\x00\\x01\\x00\\x01',('1.1.1.1',53));s.recvfrom(512);"
        "t=socket.create_connection(('1.1.1.1',443),timeout=5);t.close();print('egress_ok')"
    )
    output = runner.run(["ip", "netns", "exec", NS, sys.executable, "-c", probe])
    counters = runner.run(["iptables", "-t", "nat", "-L", CHAIN_POST, "-v", "-n"])
    masq_packets = 0
    for line in counters.splitlines():
        if "MASQUERADE" in line:
            masq_packets = int(line.split()[0])
    result = {
        "namespace_egress": "egress_ok" in output,
        "masquerade_packets": masq_packets,
        "wg_port_opened": False,
    }
    if not result["namespace_egress"] or masq_packets <= 0:
        raise run_module.guard_error(f"egress check failed: {result}")
    return result


def status(runner: Runner, work: Path) -> dict:
    ledger = load_ledger(work)
    owned = bool(
        (ledger or {}).get("snat") or any(
            str(entry.get("kind", "")).startswith("ns-snat-") for entry in (ledger or {}).get("created", [])
        )
    )
    return {
        "ledger_present": ledger is not None,
        "ledger_created": len((ledger or {}).get("created", [])),
        "state": current_state(runner),
        "ledger_snat": (ledger or {}).get("snat"),
        "snat_owned": owned,
        "wg_port_opened": False,
        "note": "WG 57401 is not opened; the managed WG source is mapped by the package SNAT chain",
    }


def snat_action(runner: Runner, work: Path, mode: str) -> dict:
    ledger = load_ledger(work)
    if ledger is None:
        raise run_module.guard_error("no ownership ledger: apply the package network first")
    owned = bool(
        ledger.get("snat") or any(
            str(entry.get("kind", "")).startswith("ns-snat-") for entry in ledger.get("created", [])
        )
    )
    if mode == "status":
        return {"snat": snat_state(runner), "owned": owned, "ledger_snat": ledger.get("snat")}
    if mode == "apply":
        def step(kind: str, args: list[str]) -> None:
            runner.run(args)
            ledger.setdefault("created", []).append({"kind": kind, "args": args})
            save_ledger(work, ledger)
        changes = ensure_snat(runner, step, owned=owned)
        ledger["snat"] = {"chain": CHAIN_SNAT, "source": WG_CIDR, "to": NS_IP.split("/")[0],
                          "iface": VPEER, "applied": changes,
                          "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
        save_ledger(work, ledger)
        return {"snat": snat_state(runner), "applied": changes}
    live = _live_package_processes(work)
    if live:
        raise run_module.guard_error(
            f"{', '.join(live)} still alive: stop the package processes before snat rollback"
        )
    failures = remove_snat(runner, owned=owned)
    if not failures:
        ledger.pop("snat", None)
        ledger["created"] = [
            entry for entry in ledger.get("created", []) if not entry["kind"].startswith("ns-snat-")
        ]
        save_ledger(work, ledger)
    return {"result": "rolled_back" if not failures else "rollback_failed", "failures": failures}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=["apply", "rollback", "status", "egress-check", "snat"])
    parser.add_argument("--work", default=str(run_module.WORK_ROOT / "pkg"))
    parser.add_argument("--mode", choices=["apply", "status", "rollback"], default="status")
    arguments = parser.parse_args()
    runner = Runner()
    try:
        work = owned_work(arguments.work)
        if arguments.action == "apply":
            payload = apply(runner, work)
        elif arguments.action == "rollback":
            payload = rollback(runner, work)
        elif arguments.action == "status":
            payload = status(runner, work)
        elif arguments.action == "snat":
            payload = snat_action(runner, work, arguments.mode)
        else:
            payload = egress_check(runner)
    except RuntimeError as error:
        print(json.dumps({"ok": False, "reason": str(error)}, indent=2))
        return 2
    print(json.dumps({"ok": True, **payload}, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
