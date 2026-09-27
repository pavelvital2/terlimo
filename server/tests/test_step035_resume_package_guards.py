"""Targeted synthetic guards for the STEP03.5 resume package (no DB, no live resources).

Guards cover: protected atomic profile update (field preservation, backup, mode, idempotency),
package-owned node binary linkage (shared install refused), trusted-config selection (no mixing),
and a static check that run_isolated only keeps the shared default in its constant.
"""

from __future__ import annotations

import importlib.util
import json
import os
import pathlib
import stat

import pytest

TOOLS = pathlib.Path(__file__).resolve().parents[1] / "tools" / "step035"


def _load(name: str):
    spec = importlib.util.spec_from_file_location(name, TOOLS / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


provision = _load("provision_trusted_vk")


def test_update_profile_file_preserves_fields_backup_mode_and_is_idempotent(tmp_path):
    profile = tmp_path / "passwords.json"
    original = {
        "main_password": "synth-main",
        "admin_profile": {"workers": 16, "protocol": "udp"},
        "passwords": {"a": {"x": 1}},
        "devices": [],
    }
    profile.write_text(json.dumps(original))
    os.chmod(profile, 0o600)

    result = provision.update_profile_file(profile, ["h1", "h2", "h3", "h4"])
    updated = json.loads(profile.read_text())
    assert updated["admin_profile"]["vk_hashes"] == "h1,h2,h3,h4"
    assert updated["admin_profile"]["workers"] == 16
    assert updated["passwords"] == original["passwords"]
    assert updated["main_password"] == original["main_password"]
    assert stat.S_IMODE(profile.stat().st_mode) == 0o600
    backup = tmp_path / result["backup"]
    assert backup.is_file() and json.loads(backup.read_text()) == original
    assert stat.S_IMODE(backup.stat().st_mode) == 0o600

    before = profile.read_text()
    provision.update_profile_file(profile, ["h1", "h2", "h3", "h4"])
    assert profile.read_text() == before

    provision.update_profile_file(profile, [])
    cleared = json.loads(profile.read_text())
    assert cleared["admin_profile"]["vk_hashes"] == ""
    assert cleared["admin_profile"]["workers"] == 16


def test_package_node_binary_must_live_inside_owned_work(tmp_path):
    work = tmp_path / "pkg"
    (work / "bin").mkdir(parents=True)
    binary = work / "bin" / "wdtt-server"
    binary.write_text("#!/bin/sh\n")
    os.chmod(binary, 0o755)
    assert provision._validate_node_bin(work, str(binary)) == binary.resolve()

    outside = tmp_path / "elsewhere" / "wdtt-server"
    outside.parent.mkdir()
    outside.write_text("#!/bin/sh\n")
    os.chmod(outside, 0o755)
    with pytest.raises(ValueError):
        provision._validate_node_bin(work, str(outside))
    with pytest.raises(ValueError):
        provision._validate_node_bin(work, "/usr/local/bin/wdtt-server")
    with pytest.raises(ValueError):
        provision._validate_node_bin(work, str(work / "bin" / "missing"))


def test_trusted_config_selection_never_mixes_node_sets(tmp_path):
    config = tmp_path / "config.json"
    config.write_text(
        json.dumps(
            {
                "nodes": [
                    {
                        "node": {
                            "node_id": "node-A",
                            "peer_ip": "193.5.251.217",
                            "vk_hashes": ["a1", "a2", "a3", "a4"],
                        }
                    },
                    {
                        "node": {
                            "node_id": "node-B",
                            "peer_ip": "23.26.193.88",
                            "vk_hashes": ["b1", "b2", "b3", "b4"],
                        }
                    },
                ]
            }
        )
    )
    node, extra = provision._select_node(config, "node-A", "193.5.251.217")
    assert [item["node_id"] for item in extra["available"]] == ["node-A", "node-B"]
    assert provision._validate_hashes(node, 4) == ["a1", "a2", "a3", "a4"]
    with pytest.raises(ValueError):
        provision._select_node(config, "node-A", "23.26.193.88")
    with pytest.raises(ValueError):
        provision._validate_hashes(node, 5)
    assert provision._normalize("a1, a2 ,,a3") == ["a1", "a2", "a3"]
    assert provision._node_running(tmp_path / "no-such-gateway") is False


def test_run_isolated_keeps_shared_path_only_as_default_constant():
    source = (TOOLS / "run_isolated.py").read_text()
    assert source.count('"/usr/local/bin/wdtt-server"') == 1
    assert "--node-bin" in source
    assert "startswith(SERVER_BIN)" in source
    assert '!= SERVER_BIN' in source
    assert "STEP035_NODE_BIN" in source


def _marker_work(run_module, root, work, uid=None):
    work.mkdir(parents=True, exist_ok=True)
    marker = {
        "version": run_module.MARKER_VERSION,
        "work": str(work),
        "uid": os.getuid() if uid is None else uid,
        "run_id": "guard",
        "created_at": "2026-01-01T00:00:00Z",
    }
    (work / run_module.MARKER_NAME).write_text(json.dumps(marker))
    return marker


def test_resolve_work_privileged_root_provisioning_and_negatives(tmp_path, monkeypatch):
    run_module = _load("run_isolated")
    root = tmp_path / "workroot"
    work = root / "pkg"
    monkeypatch.setattr(run_module, "WORK_ROOT", root)
    _marker_work(run_module, root, work)
    owner = work.stat().st_uid

    assert run_module.resolve_work(str(work), require_owned=True)[0] == work
    resolved, _ = run_module.resolve_work(
        str(work), require_owned=True, expected_uid=owner, caller_uid=0
    )
    assert resolved == work
    with pytest.raises(RuntimeError):
        run_module.resolve_work(
            str(work), require_owned=True, expected_uid=owner + 1, caller_uid=os.getuid()
        )
    with pytest.raises(RuntimeError):
        run_module.resolve_work(
            str(work), require_owned=True, for_destructive=True, expected_uid=owner, caller_uid=0
        )
    with pytest.raises(RuntimeError):
        run_module.resolve_work(str(work), require_owned=True, caller_uid=0)
    with pytest.raises(RuntimeError):
        run_module.resolve_work(
            str(work), require_owned=True, expected_uid=owner + 1, caller_uid=0
        )

    moved = root / "moved"
    marker = json.loads((work / run_module.MARKER_NAME).read_text())
    moved.mkdir()
    (moved / run_module.MARKER_NAME).write_text(json.dumps(marker))  # marker still points at work
    with pytest.raises(RuntimeError):
        run_module.resolve_work(str(moved), require_owned=True, expected_uid=owner, caller_uid=0)

    linkroot = root / "linkroot"
    real = linkroot / "real"
    _marker_work(run_module, root, real)
    link = root / "linked"
    link.symlink_to(real)
    with pytest.raises(RuntimeError):
        run_module.resolve_work(str(link), require_owned=True)


def test_runbook_and_generated_stop_order_are_pg_verified_then_network_then_caddy():
    text = (TOOLS / "RESUME-PACKAGE-TEST.md").read_text()
    order = [
        text.index("--checkpoint --work"),
        text.index("--stop --work"),
        text.rindex("--cleanup --work"),
        text.index("phone_network.py rollback"),
        text.rindex("caddy_mobile_api.py remove"),
    ]
    assert order == sorted(order), f"stop order regression: {order}"
    assert "never continue" in " ".join(text.split())
    assert "replace\n`<work>/bin/wdtt-server`" in text or "replace <work>/bin/wdtt-server" in text
    assert "never remove the work tree" in text

    source = (TOOLS / "run_isolated.py").read_text()
    generated = source[source.index("Stop order (fail-closed") : source.index("put(\"RUNBOOK-restore-stop.md\"")]
    assert "--checkpoint" in generated and "--cleanup" in generated
    assert generated.index("--cleanup") < generated.index("phone_network.py rollback")
    assert "data kept" in generated
