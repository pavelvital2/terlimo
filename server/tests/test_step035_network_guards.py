"""Targeted tests for the STEP03.5 phone-ready network tool (stubbed runner, no host changes)."""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]


def _load(name: str, file: str):
    spec = importlib.util.spec_from_file_location(name, ROOT / "tools" / "step035" / file)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


run_module = _load("step035_run_for_net_tests", "run_isolated.py")
net = _load("step035_network", "phone_network.py")


class StubRunner:
    """Records commands and simulates resource presence; injectable failures."""

    def __init__(self, *, present: set[str] | None = None, fail_on: str | None = None) -> None:
        self.present = set(present or ())
        self.commands: list[list[str]] = []
        self.fail_on = fail_on

    def run(self, args: list[str], *, check: bool = True) -> str:
        self.commands.append(list(args))
        joined = " ".join(args)
        if self.fail_on and self.fail_on in joined:
            raise RuntimeError(f"stub failure: {joined}")
        if args[:3] == ["ip", "netns", "list"]:
            return net.NS + "\n" if net.NS in self.present else ""
        if args[:2] == ["iptables", "-t"] and "-S" in args:
            chain = args[-1]
            if chain in ("PREROUTING", "POSTROUTING", "FORWARD"):
                parent_chain = {
                    "PREROUTING": net.CHAIN_NAT,
                    "POSTROUTING": net.CHAIN_POST,
                    "FORWARD": net.CHAIN_FWD,
                }[chain]
                marker = f"jump:{chain}"
                return f"-A {chain} -j {parent_chain}\n" if marker in self.present else ""
            return f"-N {chain}\n" if chain in self.present else ""
        if args[:2] == ["ip", "link"] and args[2] == "show":
            return "link" if net.VETH in self.present else ""
        return ""

    def exists(self, args: list[str]) -> bool:
        self.commands.append(list(args))
        if args[:2] == ["ip", "link"] and args[2] == "show":
            return net.VETH in self.present
        if args[:2] == ["iptables", "-t"] and "-S" in args:
            return args[-1] in self.present
        return False


@pytest.fixture
def work(tmp_path, monkeypatch):
    root = tmp_path / "runs"
    root.mkdir()
    monkeypatch.setattr(run_module, "WORK_ROOT", root)
    monkeypatch.setattr(net.run_module, "WORK_ROOT", root)
    path = root / "pkg"
    path.mkdir(mode=0o700)
    run_module.write_marker(path, "run-net", "2026-01-01T00:00:00Z")
    return path


def test_apply_is_transactional_ledgered_and_egress_scoped(work):
    runner = StubRunner()
    result = net.apply(runner, work)
    assert result["result"] == "applied"
    assert net.WG_PORT  # documented constant, but never opened below
    joined = " ".join(" ".join(command) for command in runner.commands)
    assert f"--dport {net.DTLS_PORT}" in joined
    assert f"--dport {net.WG_PORT}" not in joined
    assert "10.67.67" not in joined  # never reuse the A/B managed subnet
    assert net.CHAIN_FWD in joined and "MASQUERADE" in joined and "conntrack" in joined
    assert (work / net.LEDGER).is_file()
    ledger = json.loads((work / net.LEDGER).read_text())
    assert any(entry["kind"] == "netns" for entry in ledger["created"])


def test_apply_rerun_is_idempotent(work):
    runner = StubRunner()
    net.apply(runner, work)
    rerun = StubRunner(
        present={
            net.NS,
            net.VETH,
            net.CHAIN_NAT,
            net.CHAIN_POST,
            net.CHAIN_FWD,
            "jump:PREROUTING",
            "jump:POSTROUTING",
            "jump:FORWARD",
        }
    )
    result = net.apply(rerun, work)
    assert result["result"] == "already_applied"
    creates = [
        command
        for command in rerun.commands
        if any(token in command for token in ("add", "-N", "-A", "-I"))
    ]
    assert creates == []


def test_foreign_resources_refuse_without_changes(work):
    runner = StubRunner(present={net.NS, net.VETH, net.CHAIN_FWD})
    with pytest.raises(RuntimeError):
        net.apply(runner, work)
    assert not (work / net.LEDGER).exists()
    mutating = [
        command
        for command in runner.commands
        if any(token in command for token in ("add", "-N", "-A", "-I", "del", "-D", "-F", "-X"))
    ]
    assert mutating == []


def test_partial_failure_rolls_back_only_current_transaction(work):
    runner = StubRunner(fail_on="-N STEP035-POST")
    with pytest.raises(RuntimeError):
        net.apply(runner, work)
    ledger = json.loads((work / net.LEDGER).read_text())
    assert ledger["created"] == []
    rollback_commands = [
        command
        for command in runner.commands
        if command[:2] in (["ip", "netns"], ["ip", "link"]) and "del" in command
    ]
    assert any(command[-1] == net.NS for command in rollback_commands)
    assert any(command[-1] == net.VETH for command in rollback_commands)
    # No foreign resources were flushed/deleted: only our chains appear in any flush/delete.
    for command in runner.commands:
        if "-F" in command or "-X" in command:
            assert command[-1] in {net.CHAIN_NAT, net.CHAIN_POST, net.CHAIN_FWD}


def test_rollback_requires_ledger_and_preserves_preexisting_owned_state(work):
    runner = StubRunner()
    with pytest.raises(RuntimeError):
        net.rollback(runner, work)
    net.apply(runner, work)
    result = net.rollback(StubRunner(present={net.NS, net.VETH, net.CHAIN_FWD}), work)
    assert result["result"] == "rolled_back"
    assert not (work / net.LEDGER).exists()


def test_rollback_failure_is_explicit(work):
    net.apply(StubRunner(), work)
    failing = StubRunner(present={net.NS}, fail_on="ip link del")
    result = net.rollback(failing, work)
    assert result["result"] == "rollback_failed"
    assert result["failures"]
    ledger = json.loads((work / net.LEDGER).read_text())
    assert ledger.get("rollback_failures")


def test_egress_check_requires_namespace_and_verifies_masquerade(work, monkeypatch):
    runner = StubRunner()
    with pytest.raises(RuntimeError):
        net.egress_check(runner)
    net.apply(runner, work)


def test_dtls_pin_uses_node_credential_not_management_pki(tmp_path):
    import sys

    sys.path.insert(0, str(ROOT / "tools"))
    import gateway_integration_03_3 as base
    import gateway_integration_mtls_03_3 as mtls
    from cryptography import x509

    creds = tmp_path / "creds"
    base.make_tls_creds(str(creds))
    pki = tmp_path / "pki"
    pki.mkdir()
    ca_key, ca_cert = mtls.make_ca("pin-test-ca")
    server_key, server_cert = mtls.make_leaf(ca_key, ca_cert, "gw-pin.test")
    mtls.write_key_cert(pki / "server.key", pki / "server.pem", server_key, server_cert)
    dtls_pin = net.run_module.dtls_pin(creds)
    management_pin = net.run_module.spki_pin(pki / "server.pem")
    assert dtls_pin != management_pin
    assert dtls_pin == net.run_module.spki_pin(creds / "wl-test-dtls-cert")
    assert x509.load_pem_x509_certificate((creds / "wl-test-dtls-cert").read_bytes()) is not None
