"""STEP036 isolated backend worker -> real node admin socket provisioning.

This test wires the accepted local stand to a Go node process. The node starts
with no bootstrap credential; only the outbox worker may create one. Business
stages use onboarding service functions, never direct SQL state substitution.
"""

from __future__ import annotations

import asyncio
import base64
import importlib.util
import json
import os
import secrets
import shutil
import tempfile
import uuid
from dataclasses import replace
from pathlib import Path

from terlimo_backend.gateway_adapter import build_bootstrap_client_factory
from terlimo_backend.onboarding_hour import (
    OnboardingHourHandlers,
    build_secret_cipher,
    create_intent,
    intent_response,
)
from terlimo_backend.worker import OutboxWorker

NODE_DIR = Path(os.environ.get("WL_STEP036_NODE_DIR", "/tmp/opencode/step036-codex-fullchain-node"))
STAND_PATH = Path(__file__).resolve().parents[1] / "tools" / "step036" / "local_stand.py"


def _load_stand():
    spec = importlib.util.spec_from_file_location("step036_local_stand", STAND_PATH)
    module = importlib.util.module_from_spec(spec)
    assert spec is not None and spec.loader is not None
    spec.loader.exec_module(module)
    return module


async def _wait_file(path: Path, process: asyncio.subprocess.Process, timeout: float) -> None:
    end = asyncio.get_running_loop().time() + timeout
    while asyncio.get_running_loop().time() < end:
        if path.exists():
            return
        if process.returncode is not None:
            raise AssertionError(f"node stopped before ready: exit={process.returncode}")
        await asyncio.sleep(0.05)
    raise AssertionError("node readiness timed out")


async def _stop_node(process: asyncio.subprocess.Process, node_root: Path) -> None:
    (node_root / "stop").write_text("1")
    try:
        await asyncio.wait_for(process.communicate(), timeout=8)
    except TimeoutError:
        process.terminate()
        try:
            await asyncio.wait_for(process.communicate(), timeout=5)
        except TimeoutError:
            process.kill()
            await process.communicate()


async def test_real_worker_provisions_only_its_bootstrap_credential(migrated_url):
    stand_module = _load_stand()
    root = Path(tempfile.mkdtemp(prefix="step036-node-provision-", dir="/tmp"))
    root.chmod(0o700)
    node_root = root / "node"
    node_root.mkdir(mode=0o700)
    main_password = secrets.token_urlsafe(32)
    settings = replace(
        stand_module.build_stand_settings(migrated_url, root, service=True, relay=True),
        gateway_local_admin_enabled=True,
        gateway_admin_main_password=main_password,
    )
    stand = stand_module.LocalStand(settings, root)
    node: asyncio.subprocess.Process | None = None
    try:
        ready = await stand.start()
        assert ready["relay_socket"] != ready["service_socket"]
        assert Path(ready["relay_socket"]).is_socket()
        assert Path(ready["service_socket"]).is_socket()

        env = dict(os.environ)
        env.update(
            {
                "STEP036_NODE_DIR": str(node_root),
                "STEP036_NODE_MAIN_PASSWORD": main_password,
                "STEP036_EVIDENCE_SOCKET": ready["relay_socket"],
                "STEP036_SERVICE_SOCKET": ready["service_socket"],
                "STEP036_SERVICE_SEED": "step036-public-service-classifier",
                "PATH": "/tmp/go/bin:" + env.get("PATH", ""),
                "GOPATH": "/home/pavel/go",
                "GOPROXY": "off",
                "GOFLAGS": "-mod=mod",
                "GOTOOLCHAIN": "local",
            }
        )
        node = await asyncio.create_subprocess_exec(
            "go", "test", "-run", "^TestStep036FullchainNode$", "-count=1", ".",
            cwd=NODE_DIR,
            env=env,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.STDOUT,
        )
        await _wait_file(node_root / "node-ready.json", node, 45)
        node_ready = json.loads((node_root / "node-ready.json").read_text())
        assert node_ready["dtls_port"] > 0
        assert len(base64.urlsafe_b64decode(node_ready["dtls_spki_sha256"] + "==")) == 32
        assert Path(node_ready["admin_socket"]).is_socket()

        gateway_key = stand_module.GATEWAY_KEY
        endpoints = {
            "node_id": gateway_key,
            "peer_ip": "127.0.0.1",
            "dtls_port": node_ready["dtls_port"],
            "wg_port": 56001,
            "dtls_spki_sha256": node_ready["dtls_spki_sha256"],
            "target_workers": 36,
            "admin_socket": node_ready["admin_socket"],
        }
        async with stand._database.acquire() as connection:
            gateway_id = await connection.fetchval(
                """
                INSERT INTO gateways (gateway_key, environment, endpoints, capabilities,
                                      registry_state, confirmed_max_workers)
                VALUES ($1, 'test', $2::jsonb, '["managed"]'::jsonb, 'registered', 36)
                RETURNING id
                """,
                gateway_key,
                json.dumps(endpoints),
            )
            installation_id = await connection.fetchval(
                """
                INSERT INTO installations
                    (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
                VALUES ('test', 'android', $1, 'spki', 'technical') RETURNING id
                """,
                uuid.uuid4().hex,
            )
            cipher = build_secret_cipher(settings)
            request_key = "step036-node-provision-" + uuid.uuid4().hex
            created = await create_intent(
                connection,
                installation_id=installation_id,
                request_key=request_key,
                environment="test",
                gateway_id=gateway_id,
                cipher=cipher,
            )
            replay = await create_intent(
                connection,
                installation_id=installation_id,
                request_key=request_key,
                environment="test",
                gateway_id=gateway_id,
                cipher=cipher,
            )
            assert replay["id"] == created["id"]
            assert await connection.fetchval("SELECT count(*) FROM entitlements") == 0
            assert await connection.fetchval("SELECT count(*) FROM onboarding_evidence") == 0

        handlers = OnboardingHourHandlers(
            cipher=cipher,
            client_factory=build_bootstrap_client_factory(settings),
        ).as_handlers()
        worker = OutboxWorker(stand._database, settings, handlers=handlers, worker_id="step036-node")
        assert await worker.run_once() is True
        async with stand._database.acquire() as connection:
            ready_intent = await intent_response(
                connection,
                installation_id=installation_id,
                request_key=request_key,
                cipher=cipher,
            )
            assert ready_intent["state"] == "ready"
            assert ready_intent["id"] == created["id"]
            assert await connection.fetchval("SELECT count(*) FROM onboarding_intents") == 1
            assert await connection.fetchval("SELECT count(*) FROM entitlements") == 0
            assert await connection.fetchval("SELECT count(*) FROM onboarding_evidence") == 0
        saved = json.loads((node_root / "passwords.json").read_text())
        bootstrap = saved["client_bootstrap"]
        assert len(bootstrap) == 1
        node_credential = bootstrap[ready_intent["credential_id"]]
        assert node_credential["secret"] == ready_intent["secret"]
        assert node_credential["revoked"] is False
        assert node_credential["expires_at"] > 0
        assert (node_root / "passwords.json").stat().st_mode & 0o777 == 0o600

        # The worker's exact replay has no additional external effect or hour.
        assert await worker.run_once() is False
        assert len(bootstrap) == 1
        print("STEP036_NODE_PROVISION_OK intent=1 credential=1 entitlement=0 evidence=0 sockets=distinct")
    finally:
        if node is not None:
            await _stop_node(node, node_root)
        await stand.stop()
        shutil.rmtree(root)
