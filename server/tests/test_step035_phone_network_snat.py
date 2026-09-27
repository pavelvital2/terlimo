"""Targeted checks for the netns-scoped WG SNAT (disposable namespace; host legacy untouched)."""
from __future__ import annotations

import importlib.util
import subprocess
from pathlib import Path

import pytest

TOOLS = Path(__file__).resolve().parents[1] / "tools" / "step035"


def _load():
    spec = importlib.util.spec_from_file_location("phone_network_snat", TOOLS / "phone_network.py")
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


def _sys(args, check=True):
    result = subprocess.run(["sudo", "-n", *args], capture_output=True, text=True, check=False)
    if check and result.returncode != 0:
        raise RuntimeError(f"{args}: {result.stderr}")
    return result.stdout


@pytest.fixture
def snat_env(monkeypatch):
    if subprocess.run(["sudo", "-n", "true"], capture_output=True, check=False).returncode != 0:
        pytest.skip("passwordless sudo not available")
    ns = "step035-snat-it"
    _sys(["ip", "netns", "del", ns], check=False)
    _sys(["ip", "link", "del", "snat35h"], check=False)
    _sys(["ip", "netns", "add", ns])
    _sys(["ip", "link", "add", "snat35h", "type", "veth", "peer", "name", "snat35n"])
    _sys(["ip", "link", "set", "snat35n", "netns", ns])
    _sys(["ip", "addr", "add", "10.99.35.1/30", "dev", "snat35h"])
    _sys(["ip", "link", "set", "snat35h", "up"])
    _sys(["ip", "netns", "exec", ns, "ip", "link", "set", "lo", "up"])
    _sys(["ip", "netns", "exec", ns, "ip", "addr", "add", "10.99.35.2/30", "dev", "snat35n"])
    _sys(["ip", "netns", "exec", ns, "ip", "link", "set", "snat35n", "up"])
    _sys(["ip", "netns", "exec", ns, "ip", "link", "add", "wdttm0", "type", "dummy"])
    _sys(["ip", "netns", "exec", ns, "ip", "addr", "add", "10.67.99.1/24", "dev", "wdttm0"])
    _sys(["ip", "netns", "exec", ns, "ip", "link", "set", "wdttm0", "up"])
    _sys(["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-N", "IT-FOREIGN"])
    _sys(["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-A", "IT-FOREIGN", "-d", "192.0.2.1/32", "-j", "RETURN"])
    _sys(["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-I", "POSTROUTING", "1", "-j", "IT-FOREIGN"])
    module = _load()
    monkeypatch.setattr(module, "NS", ns)
    monkeypatch.setattr(module, "VETH", "snat35h")
    monkeypatch.setattr(module, "VPEER", "snat35n")
    monkeypatch.setattr(module, "NS_IP", "10.99.35.2/30")
    monkeypatch.setattr(module, "WG_IFACE", "wdttm0")
    monkeypatch.setattr(module, "WG_CIDR", "10.67.99.0/24")
    monkeypatch.setattr(module, "WG_NS_IP", "10.67.99.1/24")
    try:
        yield module, ns
    finally:
        _sys(["ip", "netns", "del", ns], check=False)
        _sys(["ip", "link", "del", "snat35h"], check=False)


def _ns_rules(module, ns, chain):
    listing = _sys(["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-S", chain], check=False)
    return [line for line in listing.splitlines() if line.startswith("-A")]


def _step(runner, created):
    def step(kind, args):
        runner.run(args)
        created.append({"kind": kind, "args": args})
    return step


def test_apply_idempotent_rollback_and_foreign_preserved(snat_env):
    module, ns = snat_env
    runner = module.Runner()
    created: list[dict] = []
    host_before = _sys(["iptables", "-t", "nat", "-S"])

    changes = module.ensure_snat(runner, _step(runner, created), owned=False)
    assert changes == {"chain": True, "rule": True, "jump": True}
    state = module.snat_state(runner)
    assert state["chain"] and state["jump"] and state["rule"]
    assert state["unexpected_rules"] == 0

    repeat = module.ensure_snat(runner, _step(runner, created), owned=True)
    assert repeat == {"chain": False, "rule": False, "jump": False}
    assert len(created) == 3

    assert any("IT-FOREIGN" in line for line in _ns_rules(module, ns, "POSTROUTING"))
    assert _ns_rules(module, ns, "IT-FOREIGN")
    assert _sys(["iptables", "-t", "nat", "-S"]) == host_before

    failures = module.remove_snat(runner, owned=True)
    assert failures == []
    assert not module.snat_state(runner)["chain"]
    assert _ns_rules(module, ns, "IT-FOREIGN")
    assert _sys(["iptables", "-t", "nat", "-S"]) == host_before


def test_repeat_does_not_duplicate_with_canonical_s_difference(snat_env):
    """D1: -S canonicalizes (/32, ctstate order); presence must be decided by `iptables -C`."""
    module, ns = snat_env
    runner = module.Runner()
    chain = "HOSTLIKE"
    _sys(["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-N", chain])
    spec = "-i snat35n -o snat35n -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN"
    _sys(["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-A", chain, *spec.split()])
    canonical = _ns_rules(module, ns, chain)[0]
    assert "RELATED,ESTABLISHED" in canonical  # -S reordered versus our spec
    assert canonical != f"-A {chain} {spec}"

    def check():
        return module._rule_probe(
            runner, ["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-C", chain, *spec.split()]
        )

    assert check() is True
    for _ in range(2):  # add-if-absent loop must stay idempotent
        if not check():
            runner.run(["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-A", chain, *spec.split()])
    assert len(_ns_rules(module, ns, chain)) == 1


def test_wrong_or_missing_rule_is_not_a_valid_status(snat_env):
    """D2: chain+jump+count are never enough; the exact intended rule decides."""
    module, ns = snat_env
    runner = module.Runner()
    module.ensure_snat(runner, _step(runner, []), owned=False)
    runner.run(["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-D", module.CHAIN_SNAT, *module._snat_spec().split()])
    runner.run(["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-A", module.CHAIN_SNAT,
                "-s", "10.67.99.0/24", "-o", "snat35n", "-j", "SNAT", "--to-source", "10.99.35.9"])
    state = module.snat_state(runner)
    assert state["chain"] and state["jump"] and state["rules"] == 1
    assert state["rule"] is False and state["unexpected_rules"] == 1
    with pytest.raises(RuntimeError, match="unexpected rules"):
        module.ensure_snat(runner, _step(runner, []), owned=True)


def test_foreign_same_named_chain_is_untouched(snat_env):
    """D3: same-named chain without ownership evidence is foreign for apply and rollback."""
    module, ns = snat_env
    runner = module.Runner()
    _sys(["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-N", module.CHAIN_SNAT])
    _sys(["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-A", module.CHAIN_SNAT, "-d", "198.51.100.1/32", "-j", "RETURN"])
    with pytest.raises(RuntimeError, match="foreign chain"):
        module.ensure_snat(runner, _step(runner, []), owned=False)
    failures = module.remove_snat(runner, owned=False)
    assert failures and "left untouched" in failures[0]
    assert len(_ns_rules(module, ns, module.CHAIN_SNAT)) == 1  # foreign rule survived


def test_partial_failure_reapply_and_foreign_content_in_own_chain(snat_env):
    module, ns = snat_env
    runner = module.Runner()
    module.ensure_snat(runner, _step(runner, []), owned=False)
    runner.run(["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-D", "POSTROUTING", "-j", module.CHAIN_SNAT])
    changes = module.ensure_snat(runner, _step(runner, []), owned=True)
    assert changes == {"chain": False, "rule": False, "jump": True}
    assert len(_ns_rules(module, ns, module.CHAIN_SNAT)) == 1
    # unexpected content inside an owned chain: rollback removes only ours, never flushes
    runner.run(["ip", "netns", "exec", ns, "iptables", "-t", "nat", "-A", module.CHAIN_SNAT, "-d", "203.0.113.1/32", "-j", "RETURN"])
    failures = module.remove_snat(runner, owned=True)
    assert any("unexpected rule(s) remain; chain kept" in item for item in failures)
    remaining = _ns_rules(module, ns, module.CHAIN_SNAT)
    assert len(remaining) == 1 and "203.0.113.1" in remaining[0]


def test_snat_fails_closed_on_unexpected_managed_address(snat_env):
    module, ns = snat_env
    runner = module.Runner()
    _sys(["ip", "netns", "exec", ns, "ip", "addr", "del", "10.67.99.1/24", "dev", "wdttm0"])
    _sys(["ip", "netns", "exec", ns, "ip", "addr", "add", "10.67.42.1/24", "dev", "wdttm0"])
    with pytest.raises(RuntimeError, match="managed WG address mismatch"):
        module.ensure_snat(runner, _step(runner, []), owned=False)
    assert not module.snat_state(runner)["chain"]
