"""Targeted guards for the STEP03.5 orchestration package (no shared network, no real kills)."""

from __future__ import annotations

import importlib.util
import json
import os
from pathlib import Path
from unittest import mock

import pytest

ROOT = Path(__file__).resolve().parents[1]


def _load_run_module():
    spec = importlib.util.spec_from_file_location(
        "step035_run_isolated_guards", ROOT / "tools" / "step035" / "run_isolated.py"
    )
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


run_module = _load_run_module()


@pytest.fixture
def sandbox(tmp_path, monkeypatch):
    root = tmp_path / "runs"
    root.mkdir()
    monkeypatch.setattr(run_module, "WORK_ROOT", root)
    return root


def _marker_work(root: Path, name: str = "pkg") -> Path:
    work = root / name
    work.mkdir(mode=0o700)
    (work / "logs").mkdir(mode=0o700)
    run_module.write_marker(work, "run-test", "2026-01-01T00:00:00Z")
    return work


def test_foreign_and_unowned_paths_are_refused_without_side_effects(sandbox, tmp_path):
    foreign = tmp_path / "foreign"
    foreign.mkdir()
    (foreign / "data").write_text("keep")
    unowned = sandbox / "unowned"
    unowned.mkdir()
    (unowned / "data").write_text("keep")
    with mock.patch("subprocess.run") as run_spy, mock.patch("shutil.rmtree") as rmtree_spy:
        for candidate in (str(foreign), str(unowned), str(sandbox), str(ROOT)):
            with pytest.raises(RuntimeError):
                run_module.resolve_work(candidate, require_owned=True, for_destructive=True)
            with pytest.raises(RuntimeError):
                run_module.remove_owned_work(Path(candidate))
        assert run_spy.call_count == 0
        assert rmtree_spy.call_count == 0
    assert (foreign / "data").exists() and (unowned / "data").exists()


def test_symlinked_work_is_refused(sandbox):
    real = _marker_work(sandbox, "real")
    link = sandbox / "link"
    link.symlink_to(real)
    with pytest.raises(RuntimeError):
        run_module.resolve_work(str(link), require_owned=True, for_destructive=True)
    with mock.patch("subprocess.run") as run_spy, mock.patch("shutil.rmtree") as rmtree_spy:
        with pytest.raises(RuntimeError):
            run_module.remove_owned_work(link)
        assert run_spy.call_count == 0 and rmtree_spy.call_count == 0
    assert real.exists()


def test_marker_mismatch_is_refused(sandbox):
    work = _marker_work(sandbox, "moved")
    marker = json.loads((work / run_module.MARKER_NAME).read_text())
    marker["work"] = str(sandbox / "other")
    (work / run_module.MARKER_NAME).write_text(json.dumps(marker))
    with pytest.raises(RuntimeError):
        run_module.resolve_work(str(work), require_owned=True, for_destructive=True)


def test_owned_work_removal_uses_targeted_paths_only(sandbox):
    work = _marker_work(sandbox, "clean")
    gateway = work / "gateway"
    gateway.mkdir()
    (gateway / "creds").write_text("x")
    with mock.patch("subprocess.run") as run_spy:
        run_module.remove_owned_work(work)
    assert not work.exists()
    calls = [call.args[0] for call in run_spy.call_args_list]
    assert calls == [["sudo", "-n", "rm", "-rf", str(gateway)]]


def test_starttime_mismatch_and_unknown_processes_are_not_signalled(sandbox):
    work = _marker_work(sandbox, "pids")
    record = {
        "pid": os.getpid(),
        "starttime": "0",  # cannot match a live /proc starttime
        "kind": "api",
    }
    (work / "logs" / "api.pid").write_text(json.dumps(record))
    gateway_record = {
        "pid": os.getpid(),
        "starttime": run_module.proc_starttime(os.getpid()),
        "kind": "gateway",
    }
    (work / "logs" / "gateway.pid").write_text(json.dumps(gateway_record))
    with (
        mock.patch("os.kill") as kill_spy,
        mock.patch("subprocess.run") as run_spy,
    ):
        result = run_module.stop_recorded_processes(work)
    assert kill_spy.call_count == 0
    assert run_spy.call_count == 0
    assert result["stopped"] == []
    by_kind = {item["kind"]: item for item in result["results"]}
    assert by_kind["api"]["action"] == "already_gone"
    assert by_kind["gateway"]["action"] == "skipped"  # live PID, foreign cmdline: not signalled
    assert by_kind["serve"]["action"] == "missing_record"
    assert by_kind["worker"]["action"] == "missing_record"
    assert result["verified_exit"] is False


def test_stop_requires_ownership_marker(sandbox):
    work = sandbox / "no-marker"
    work.mkdir()
    (work / "logs").mkdir()
    with mock.patch("os.kill") as kill_spy, pytest.raises(RuntimeError):
        run_module.stop_recorded_processes(work)
    assert kill_spy.call_count == 0


class _FakeConnection:
    def __init__(self, exists: bool) -> None:
        self.exists = exists
        self.queries: list[str] = []

    async def fetchval(self, query, *args):
        self.queries.append(query)
        return 1 if self.exists else None

    async def execute(self, query, *args):
        self.queries.append(query)

    async def close(self):
        return None


@pytest.mark.asyncio
async def test_ensure_database_never_drops_and_reuses(monkeypatch):
    existing = _FakeConnection(exists=True)

    async def connect_existing(*args, **kwargs):
        return existing

    monkeypatch.setattr(run_module.base.asyncpg, "connect", connect_existing)
    state = await run_module.ensure_database("postgresql://ignored", "terlimo_035")
    assert state == "reused"
    assert not any("DROP" in query.upper() for query in existing.queries)
    assert not any("CREATE DATABASE" in query.upper() for query in existing.queries)

    missing = _FakeConnection(exists=False)

    async def connect_missing(*args, **kwargs):
        return missing

    monkeypatch.setattr(run_module.base.asyncpg, "connect", connect_missing)
    state = await run_module.ensure_database("postgresql://ignored", "terlimo_035")
    assert state == "created"
    assert sum("CREATE DATABASE" in query.upper() for query in missing.queries) == 1
    assert not any("DROP" in query.upper() for query in missing.queries)


def test_package_sources_have_no_broad_kill_or_drop_and_no_public_health():
    source = (ROOT / "tools" / "step035" / "run_isolated.py").read_text(encoding="utf-8")
    assert '"pgrep"' not in source
    assert "pgrep -" not in source
    assert "DROP DATABASE" not in source
    caddy = (ROOT / "tools" / "step035" / "caddy_mobile_api.diff").read_text(encoding="utf-8")
    assert "/health" not in caddy
    readme = (ROOT / "tools" / "step035" / "README.md").read_text(encoding="utf-8")
    assert "127.0.0.1:18091/health/ready" in readme
    assert "sslip.io/api/mobile/v1/health" not in readme


def test_lifecycle_records_kinds_and_identity(sandbox):
    work = _marker_work(sandbox, "lifecycle")
    # serve/gateway/cmdline token mapping
    assert run_module._kind_token(work, "serve") == "run_isolated.py"
    assert run_module._kind_token(work, "api") == "terlimo-api"
    assert run_module._kind_token(work, "worker") == "terlimo-worker"
    assert str(work / "gateway") in run_module._kind_token(work, "gateway")
    # identity checks
    good = {
        "pid": os.getpid(),
        "starttime": run_module.proc_starttime(os.getpid()),
        "kind": "serve",
    }
    assert run_module.verified_alive(good)
    assert not run_module.verified_alive(dict(good, starttime="0"))
    with mock.patch("os.kill") as kill_spy, mock.patch("subprocess.run") as run_spy:
        result = run_module.stop_one(work, dict(good, starttime="0"))
    assert result["action"] == "already_gone"
    assert kill_spy.call_count == 0 and run_spy.call_count == 0


def test_cleanup_refuses_when_serve_or_gateway_alive(sandbox, monkeypatch):
    work = _marker_work(sandbox, "alive")
    (work / "state.json").write_text(json.dumps({"phone_ready": True}))
    real_start = run_module.proc_starttime(os.getpid())
    for kind in ("serve", "api", "worker", "gateway"):
        (work / "logs" / f"{kind}.pid").write_text(
            json.dumps({"pid": os.getpid(), "starttime": real_start, "kind": kind})
        )
    monkeypatch.setattr(run_module, "verified_alive", lambda record: True)
    with mock.patch("shutil.rmtree") as rmtree_spy, mock.patch("subprocess.run") as run_spy:
        result = run_module.cleanup(work)
        assert result["ok"] is False
        assert any(item["action"] == "still_alive" for item in result["unproven"])
        assert rmtree_spy.call_count == 0 and run_spy.call_count == 0


def test_network_rollback_refuses_while_package_alive(sandbox, monkeypatch):
    import importlib.util

    spec = importlib.util.spec_from_file_location(
        "step035_network_lifecycle", ROOT / "tools" / "step035" / "phone_network.py"
    )
    net = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(net)
    work = _marker_work(sandbox, "netlive")
    (work / "logs" / "gateway.pid").write_text(
        json.dumps({"pid": os.getpid(), "starttime": "x", "kind": "gateway"})
    )
    monkeypatch.setattr(net.run_module, "verified_alive", lambda record: True)
    with mock.patch("subprocess.run") as run_spy:
        with pytest.raises(RuntimeError):
            net.rollback(net.Runner(), work)
        assert run_spy.call_count == 0


def test_caddy_block_removal_is_context_checked_and_surgical():
    import importlib.util

    spec = importlib.util.spec_from_file_location(
        "step035_caddy", ROOT / "tools" / "step035" / "caddy_mobile_api.py"
    )
    caddy = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(caddy)
    foreign = "pay.terlimo.xyz {\n\thandle /x {\n\t\trespond 200\n\t}\n}\n"
    text = (
        "terlimo.193-5-251-217.sslip.io {\n\tbind 193.5.251.217\n\n"
        + caddy.BLOCK
        + "\n\t@telegram_webhook path /tg/webhook\n\treverse_proxy @telegram_webhook 127.0.0.1:18080\n}\n"
        + foreign
    )
    updated, removed = caddy.remove_block(text)
    assert removed is True
    assert "@mobile_api" not in updated
    assert "@telegram_webhook" in updated and "pay.terlimo.xyz" in updated
    same, removed_again = caddy.remove_block(updated)
    assert removed_again is False and same == updated
    with pytest.raises(RuntimeError):
        caddy.remove_block(caddy.BLOCK)


class _FakeProbe:
    def __init__(self, rows, ns_name=None, ns_pids=None, cwd=None, root_exe=None, env=None):
        self._rows = rows
        self._ns_name = ns_name
        self._ns_pids = ns_pids or []
        self._cwd = cwd
        self._root_exe = root_exe
        self._env = env or {}

    def processes(self):
        return self._rows

    def env_contains(self, pid, token):
        return self._env.get(pid, False)

    def cwd(self, pid):
        return self._cwd

    def ns_identify(self, pid):
        return self._ns_name

    def ns_pids(self, name):
        return self._ns_pids

    def root_exe(self, pid):
        return self._root_exe


def _live_rows(work: Path):
    return [
        {"pid": 500, "ppid": "1", "cmdline": "python tools/step035/run_isolated.py --phone --serve",
         "starttime": "111", "env_work": True, "exe": "/usr/bin/python3.12"},
        {"pid": 501, "ppid": "500", "cmdline": ".venv/bin/terlimo-api",
         "starttime": "112", "env_work": True, "exe": "/usr/bin/python3.12"},
        {"pid": 502, "ppid": "500", "cmdline": ".venv/bin/terlimo-worker",
         "starttime": "113", "env_work": True, "exe": "/usr/bin/python3.12"},
        {"pid": 503, "ppid": "500", "cmdline": f"/usr/local/bin/wdtt-server -config-dir {work / 'gateway'} -listen 0.0.0.0:57400",
         "starttime": "114", "env_work": None, "exe": None},
    ]


def test_adoption_requires_full_proof_and_writes_nothing_on_failure(sandbox):
    work = _marker_work(sandbox, "adopt")
    (work / "state.json").write_text(json.dumps({"phone_ready": True}))
    probe = _FakeProbe(
        _live_rows(work),
        ns_name="terlimo-035",
        ns_pids=[503],
        cwd=str(ROOT),
        root_exe="/usr/local/bin/wdtt-server",
        env={501: True, 502: False},
    )
    with pytest.raises(RuntimeError):
        run_module.adopt_live_package(work, probe=probe)
    assert not (work / "logs" / "serve.pid").exists()

    probe = _FakeProbe(
        _live_rows(work),
        ns_name="other-ns",
        ns_pids=[503],
        cwd=str(ROOT),
        root_exe="/usr/local/bin/wdtt-server",
        env={501: True, 502: True},
    )
    with pytest.raises(RuntimeError):
        run_module.adopt_live_package(work, probe=probe)
    assert not (work / "logs" / "serve.pid").exists()

    probe = _FakeProbe(
        _live_rows(work),
        ns_name="terlimo-035",
        ns_pids=[503],
        cwd=str(ROOT),
        root_exe="/usr/local/bin/wdtt-server",
        env={501: True, 502: True},
    )
    result = run_module.adopt_live_package(work, probe=probe)
    assert result["adopted"] == {"serve": 500, "api": 501, "worker": 502, "gateway": 503}
    for kind in ("serve", "api", "worker", "gateway"):
        assert (work / "logs" / f"{kind}.pid").is_file()


def test_stop_is_fail_closed_on_missing_records(sandbox):
    work = _marker_work(sandbox, "missing")
    (work / "state.json").write_text(json.dumps({"phone_ready": True}))
    # api/worker records point to a dead/foreign process -> already_gone (verified), but the
    # gateway record is missing: verification must fail.
    for kind in ("api", "worker"):
        (work / "logs" / f"{kind}.pid").write_text(
            json.dumps({"pid": os.getpid(), "starttime": "999999999", "kind": kind})
        )
    result = run_module.stop_recorded_processes(work)
    assert result["verified_exit"] is False
    assert any(item["action"] == "missing_record" and item["kind"] == "gateway" for item in result["failed"])


def test_cleanup_blocks_deletion_and_pg_on_unverified_stop(sandbox, monkeypatch):
    work = _marker_work(sandbox, "unverified")
    (work / "state.json").write_text(json.dumps({"phone_ready": True}))
    # only api/worker records exist -> gateway record is missing; nothing may be stopped/deleted
    for kind in ("api", "worker"):
        (work / "logs" / f"{kind}.pid").write_text(
            json.dumps({"pid": os.getpid(), "starttime": "999999999", "kind": kind})
        )
    pg_spy = mock.Mock()
    monkeypatch.setattr(run_module, "stop_package_postgres", pg_spy)
    with mock.patch("shutil.rmtree") as rmtree_spy, mock.patch("os.kill") as kill_spy:
        result = run_module.cleanup(work)
        assert result["ok"] is False
        assert any(item["kind"] == "serve" for item in result["unproven"])
        assert pg_spy.call_count == 0 and rmtree_spy.call_count == 0 and kill_spy.call_count == 0
        # even with an explicit purge request nothing is deleted while unverified
        result = run_module.cleanup(work, purge_work=True)
    assert result["ok"] is False and rmtree_spy.call_count == 0


def test_cleanup_keeps_data_by_default(sandbox, monkeypatch):
    work = _marker_work(sandbox, "keep")
    (work / "state.json").write_text(json.dumps({"phone_ready": False}))
    for kind in ("serve", "api", "worker"):
        (work / "logs" / f"{kind}.pid").write_text(
            json.dumps({"pid": os.getpid(), "starttime": "999999999", "kind": kind})
        )
    monkeypatch.setattr(
        run_module, "stop_package_postgres", lambda work, timeout=30.0: {"ok": True, "verified": True}
    )
    with mock.patch("shutil.rmtree") as rmtree_spy:
        result = run_module.cleanup(work)
    assert result["ok"] is True and result["removed"] is False and result["data_kept"] is True
    assert rmtree_spy.call_count == 0
    work2 = _marker_work(sandbox, "purge")
    (work2 / "state.json").write_text(json.dumps({"phone_ready": False}))
    for kind in ("serve", "api", "worker"):
        (work2 / "logs" / f"{kind}.pid").write_text(
            json.dumps({"pid": os.getpid(), "starttime": "999999999", "kind": kind})
        )
    result = run_module.cleanup(work2, purge_work=True)
    assert result["ok"] is True and result["removed"] is True


def test_network_rollback_fails_closed_without_records(sandbox, monkeypatch):
    import importlib.util

    spec = importlib.util.spec_from_file_location(
        "step035_network_c2", ROOT / "tools" / "step035" / "phone_network.py"
    )
    net = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(net)
    work = _marker_work(sandbox, "unproven")
    (work / "state.json").write_text(json.dumps({"phone_ready": True}))
    with mock.patch("subprocess.run") as run_spy, pytest.raises(RuntimeError):
        net.rollback(net.Runner(), work)
    assert run_spy.call_count == 0


def test_cleanup_rejects_invalid_identity_records(sandbox, monkeypatch):
    work = _marker_work(sandbox, "identity")
    (work / "state.json").write_text(json.dumps({"phone_ready": False}))
    # valid record (dead pid identity via starttime 0 is invalid by definition) -> unproven
    (work / "logs" / "serve.pid").write_text(json.dumps({"pid": os.getpid(), "kind": "serve"}))
    for kind in ("api", "worker"):
        (work / "logs" / f"{kind}.pid").write_text(
            json.dumps({"pid": os.getpid(), "starttime": "0", "kind": kind})
        )
    pg_spy = mock.Mock()
    monkeypatch.setattr(run_module, "stop_package_postgres", pg_spy)
    with mock.patch("shutil.rmtree") as rmtree_spy, mock.patch("os.kill") as kill_spy:
        result = run_module.cleanup(work)
    assert result["ok"] is False
    actions = {item["kind"]: item["action"] for item in result["unproven"]}
    assert actions["serve"] == "identity_missing"
    assert actions["api"] == "identity_missing"
    assert actions["worker"] == "identity_missing"
    assert pg_spy.call_count == 0 and rmtree_spy.call_count == 0 and kill_spy.call_count == 0


def test_pg_stop_verifies_by_exact_pgdata_even_without_pidfile(sandbox, monkeypatch):
    work = _marker_work(sandbox, "pgverify")
    pgdata = work / "pg"
    pgdata.mkdir()
    monkeypatch.setattr(run_module, "_processes_with_token", lambda token: [4242])
    result = run_module.stop_package_postgres(work)
    assert result["ok"] is False and result["verified"] is False
    monkeypatch.setattr(run_module, "_processes_with_token", lambda token: [])
    result = run_module.stop_package_postgres(work)
    assert result["ok"] is True and result["verified"] is True and result["stopped"] is False


def test_record_identity_requires_real_starttime(sandbox):
    assert not run_module.record_identity_valid({"pid": 1})
    assert not run_module.record_identity_valid({"pid": 1, "starttime": "0"})
    assert not run_module.record_identity_valid({"pid": 0, "starttime": "123"})
    assert run_module.record_identity_valid({"pid": 1, "starttime": "123"})
