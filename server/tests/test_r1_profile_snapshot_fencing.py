"""R1 correction: coalesced per-gateway profile snapshot fencing (no stale writes).

Deterministic reproduction of the original R1 defect class (two different grants overwriting the
shared gateway snapshot) against the NEW coalesced operation: stale epoch/claim owners must
never write, a new event during processing (including a clear) must not be lost.
"""

from __future__ import annotations

import asyncio
import json

import asyncpg
import pytest
from fake_gateway_admin import FakeGatewayAdmin

from terlimo_backend import gateway_control as gc
from terlimo_backend.db import Database
from terlimo_backend.gateway_adapter import GatewayAdminClient
from terlimo_backend.gateway_control import (
    SYNC_PROFILE_OPERATION,
    GatewayControlHandlers,
    _enqueue,
    ensure_grant,
)
from terlimo_backend.pop import installation_fingerprint
from terlimo_backend.worker import OutboxWorker

SPKI = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEf2GWLvpp78Vox1cAEyfhgUKaywhAfUgWYaf5JldPvQZWP_mwVeNQNWK6hBTYmLpeci1h7TWFg-mkxysKZevnHA"


async def _connect(database_url: str) -> asyncpg.Connection:
    connection = await asyncpg.connect(database_url, timeout=10)
    await connection.set_type_codec(
        "jsonb", schema="pg_catalog", encoder=json.dumps, decoder=json.loads
    )
    return connection


async def _seed_gateway(database_url: str, socket_path: str, key: str = "gw-fence") -> str:
    connection = await _connect(database_url)
    try:
        gateway_id = await connection.fetchval(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, capabilities, registry_state)
            VALUES ($1, 'test', $2::jsonb, '["managed"]'::jsonb, 'registered')
            RETURNING id
            """,
            key,
            {
                "node_id": key,
                "admin_socket": socket_path,
                "target_workers": 36,
                "peer_ip": "127.0.0.1",
                "dtls_port": 56300,
                "wg_port": 56302,
                "dtls_spki_sha256": "u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7s",
            },
        )
        return str(gateway_id)
    finally:
        await connection.close()


async def _enqueue_profile(
    database_url: str, gateway_id: str, key: str, node_id: str, socket_path: str
) -> int:
    connection = await _connect(database_url)
    try:
        epoch = await connection.fetchval(
            "UPDATE gateways SET snapshot_epoch = snapshot_epoch + 1 WHERE id = $1 RETURNING snapshot_epoch",
            gateway_id,
        )
        endpoints = await connection.fetchval(
            "SELECT endpoints FROM gateways WHERE id = $1", gateway_id
        )
        await _enqueue(
            connection,
            operation_type=SYNC_PROFILE_OPERATION,
            idempotency_key=f"profile:{gateway_id}:{epoch}",
            payload={
                "node_id": node_id,
                "route": {"admin_socket": socket_path},
                "epoch": epoch,
                "endpoints": endpoints,
            },
            gateway_id=gateway_id,
            target_revision=int(epoch),
        )
        return int(epoch)
    finally:
        await connection.close()


async def _vk(database_url: str, gateway_id: str):
    connection = await _connect(database_url)
    try:
        return await connection.fetchval(
            "SELECT vk_hashes FROM gateways WHERE id = $1", gateway_id
        )
    finally:
        await connection.close()


async def _op(database_url: str, gateway_id: str, epoch: int):
    connection = await _connect(database_url)
    try:
        return await connection.fetchrow(
            "SELECT * FROM outbox_operations WHERE gateway_id = $1 AND target_revision = $2",
            gateway_id,
            epoch,
        )
    finally:
        await connection.close()


def _handlers(settings, factory):
    return GatewayControlHandlers(settings, client_factory=factory).as_handlers()


def _admin_factory(socket_path, main_password="fixture-main"):
    return lambda _key, endpoints: GatewayAdminClient(
        socket_path=str(endpoints.get("admin_socket", socket_path)),
        main_password=main_password,
        timeout_seconds=5,
    )


@pytest.fixture
async def fence_env(migrated_url, settings_factory):
    settings = settings_factory(migrated_url)
    database = Database(settings)
    await database.connect()
    try:
        yield migrated_url, settings, database
    finally:
        await database.close()


async def test_newer_epoch_wins_over_late_stale_readback(fence_env):
    database_url, settings, database = fence_env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-fence")
    await fake.start()
    try:
        gateway_id = await _seed_gateway(database_url, fake.socket_path)
        await _enqueue_profile(database_url, gateway_id, "gw-fence", "gw-fence", fake.socket_path)
        fake.vk_hashes = ["fresh-1", "fresh-2"]

        started, release = asyncio.Event(), asyncio.Event()

        class BlockingClient(GatewayAdminClient):
            async def engine_status(self):
                started.set()
                await release.wait()
                return {"node_id": "gw-fence", "vk_hashes": ["stale-old"]}

        stale_handlers = _handlers(
            settings,
            lambda _key, endpoints: BlockingClient(
                socket_path=str(endpoints["admin_socket"]),
                main_password="fixture-main",
                timeout_seconds=5,
            ),
        )
        fresh_handlers = _handlers(settings, _admin_factory(fake.socket_path))
        stale_worker = OutboxWorker(database, settings, handlers=stale_handlers, worker_id="stale")
        fresh_worker = OutboxWorker(database, settings, handlers=fresh_handlers, worker_id="fresh")

        task = asyncio.create_task(stale_worker.run_once())
        await asyncio.wait_for(started.wait(), timeout=10)
        # NEW event while the stale RPC is still blocked: epoch 2 + its own operation.
        await _enqueue_profile(database_url, gateway_id, "gw-fence", "gw-fence", fake.socket_path)
        assert await fresh_worker.run_once() is True
        release.set()
        assert await asyncio.wait_for(task, timeout=10) is True

        assert await _vk(database_url, gateway_id) == ["fresh-1", "fresh-2"]
        assert (await _op(database_url, gateway_id, 1))["status"] == "done"
        assert (await _op(database_url, gateway_id, 2))["status"] == "done"
    finally:
        await fake.stop()


async def test_reclaim_and_event_during_processing_with_clear(fence_env):
    database_url, settings, database = fence_env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-fence")
    await fake.start()
    try:
        gateway_id = await _seed_gateway(database_url, fake.socket_path)
        connection = await _connect(database_url)
        try:
            await connection.execute(
                "UPDATE gateways SET vk_hashes = '[\"keep\"]'::jsonb WHERE id = $1", gateway_id
            )
        finally:
            await connection.close()
        await _enqueue_profile(database_url, gateway_id, "gw-fence", "gw-fence", fake.socket_path)

        started, release = asyncio.Event(), asyncio.Event()

        class BlockingClient(GatewayAdminClient):
            async def engine_status(self):
                started.set()
                await release.wait()
                return {"node_id": "gw-fence", "vk_hashes": ["stale-old"]}

        stale_handlers = _handlers(
            settings,
            lambda _key, endpoints: BlockingClient(
                socket_path=str(endpoints["admin_socket"]),
                main_password="fixture-main",
                timeout_seconds=5,
            ),
        )
        stale_worker = OutboxWorker(database, settings, handlers=stale_handlers, worker_id="stale")
        task = asyncio.create_task(stale_worker.run_once())
        await asyncio.wait_for(started.wait(), timeout=10)

        # New event during processing: confirmed EMPTY snapshot must win (clear, not lost).
        fake.vk_hashes = []
        await _enqueue_profile(database_url, gateway_id, "gw-fence", "gw-fence", fake.socket_path)
        fresh_handlers = _handlers(settings, _admin_factory(fake.socket_path))
        fresh_worker = OutboxWorker(database, settings, handlers=fresh_handlers, worker_id="fresh")
        assert await fresh_worker.run_once() is True

        # Simulated claim reclaim while the stale owner is still in its RPC.
        connection = await _connect(database_url)
        try:
            await connection.execute(
                """
                UPDATE outbox_operations
                SET claim_token = gen_random_uuid(), locked_by = 'reclaimer'
                WHERE gateway_id = $1 AND target_revision = 1
                """,
                gateway_id,
            )
        finally:
            await connection.close()
        release.set()
        assert await asyncio.wait_for(task, timeout=10) is True

        assert await _vk(database_url, gateway_id) == []
        stale_op = await _op(database_url, gateway_id, 1)
        assert stale_op["status"] == "processing"  # stale owner lost its claim: no finalize
        assert stale_op["locked_by"] == "reclaimer"
        assert (await _op(database_url, gateway_id, 2))["status"] == "done"
    finally:
        await fake.stop()


async def test_same_epoch_enqueue_is_coalesced(fence_env):
    database_url, _, _ = fence_env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-coalesce")
    await fake.start()
    try:
        gateway_id = await _seed_gateway(database_url, fake.socket_path, key="gw-coalesce")
        connection = await _connect(database_url)
        try:
            epoch = await connection.fetchval(
                "UPDATE gateways SET snapshot_epoch = 5 WHERE id = $1 RETURNING snapshot_epoch",
                gateway_id,
            )
            for _ in range(3):
                await _enqueue(
                    connection,
                    operation_type=SYNC_PROFILE_OPERATION,
                    idempotency_key=f"profile:{gateway_id}:{epoch}",
                    payload={"node_id": "gw-coalesce", "route": {"admin_socket": fake.socket_path}},
                    gateway_id=gateway_id,
                    target_revision=int(epoch),
                )
            count = await connection.fetchval(
                "SELECT count(*) FROM outbox_operations WHERE gateway_id = $1 AND target_revision = 5",
                gateway_id,
            )
        finally:
            await connection.close()
        assert count == 1
    finally:
        await fake.stop()


async def test_malformed_absent_and_identity_change_keep_last_good(fence_env):
    database_url, settings, database = fence_env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-keep")
    await fake.start()
    try:
        gateway_id = await _seed_gateway(database_url, fake.socket_path, key="gw-keep")
        connection = await _connect(database_url)
        try:
            await connection.execute(
                "UPDATE gateways SET vk_hashes = '[\"keep\"]'::jsonb WHERE id = $1", gateway_id
            )
        finally:
            await connection.close()

        async def run_profile_once() -> None:
            handlers = _handlers(settings, _admin_factory(fake.socket_path))
            worker = OutboxWorker(database, settings, handlers=handlers, worker_id="keep")
            epoch = await _enqueue_profile(
                database_url, gateway_id, "gw-keep", "gw-keep", fake.socket_path
            )
            assert await worker.run_once() is True
            assert (await _op(database_url, gateway_id, epoch))["status"] == "done"

        fake.vk_hashes = None  # old node without the field
        await run_profile_once()
        assert await _vk(database_url, gateway_id) == ["keep"]

        fake.vk_hashes = [f"h{i}" for i in range(9)]  # malformed (>8)
        await run_profile_once()
        assert await _vk(database_url, gateway_id) == ["keep"]

        # Registry identity replaced: never retarget the snapshot.
        fake.vk_hashes = ["would-write"]
        connection = await _connect(database_url)
        try:
            await connection.execute(
                "UPDATE gateways SET gateway_key = 'gw-replaced' WHERE id = $1", gateway_id
            )
        finally:
            await connection.close()
        await run_profile_once()
        assert await _vk(database_url, gateway_id) == ["keep"]
    finally:
        await fake.stop()


async def _epoch(database_url: str, gateway_id: str) -> int:
    connection = await _connect(database_url)
    try:
        return int(
            await connection.fetchval(
                "SELECT snapshot_epoch FROM gateways WHERE id = $1", gateway_id
            )
        )
    finally:
        await connection.close()


async def _profile_count(database_url: str, gateway_id: str) -> int:
    connection = await _connect(database_url)
    try:
        return int(
            await connection.fetchval(
                "SELECT count(*) FROM outbox_operations WHERE gateway_id = $1 AND operation_type = $2",
                gateway_id,
                SYNC_PROFILE_OPERATION,
            )
        )
    finally:
        await connection.close()


async def _grant_row(database_url: str, binding_id: str) -> asyncpg.Record:
    connection = await _connect(database_url)
    try:
        return await connection.fetchrow(
            "SELECT state, lease_seq, applied_generation, desired_generation FROM grants WHERE binding_id = $1",
            binding_id,
        )
    finally:
        await connection.close()


async def _seed_grant(database_url: str, gateway_id: str, spki: str) -> str:
    connection = await _connect(database_url)
    try:
        account = await connection.fetchval(
            "INSERT INTO accounts (status) VALUES ('verified') RETURNING id"
        )
        installation = await connection.fetchval(
            """
            INSERT INTO installations
                (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test', 'android', $1, $2, 'technical') RETURNING id
            """,
            installation_fingerprint(spki),
            spki,
        )
        binding = await connection.fetchval(
            "INSERT INTO account_bindings (account_id, installation_id, status) VALUES ($1,$2,'active') RETURNING id",
            account,
            installation,
        )
        entitlement = await connection.fetchval(
            """
            INSERT INTO entitlements (account_id, kind, status, ends_at, device_limit)
            VALUES ($1,'imported','active', now()+interval '2 hours', 1) RETURNING id
            """,
            account,
        )
        await ensure_grant(
            connection,
            binding_id=binding,
            gateway_id=gateway_id,
            entitlement_id=entitlement,
            max_lease_seconds=7200,
        )
        return str(binding)
    finally:
        await connection.close()


async def _enqueue_profile_full(
    database_url: str,
    gateway_id: str,
    node_id: str,
    route: dict,
    endpoints: dict,
    epoch: int | None = None,
) -> int:
    connection = await _connect(database_url)
    try:
        if epoch is None:
            epoch = await connection.fetchval(
                "UPDATE gateways SET snapshot_epoch = snapshot_epoch + 1 WHERE id = $1 RETURNING snapshot_epoch",
                gateway_id,
            )
        await _enqueue(
            connection,
            operation_type=SYNC_PROFILE_OPERATION,
            idempotency_key=f"profile:{gateway_id}:{epoch}",
            payload={
                "node_id": node_id,
                "route": route,
                "epoch": int(epoch),
                "endpoints": endpoints,
            },
            gateway_id=gateway_id,
            target_revision=int(epoch),
        )
        return int(epoch)
    finally:
        await connection.close()


async def _set_last_good(database_url: str, gateway_id: str, last_good: list[str]) -> None:
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE gateways SET vk_hashes = $2::jsonb WHERE id = $1", gateway_id, last_good
        )
    finally:
        await connection.close()


async def _get_endpoints(database_url: str, gateway_id: str) -> dict:
    connection = await _connect(database_url)
    try:
        return await connection.fetchval(
            "SELECT endpoints FROM gateways WHERE id = $1", gateway_id
        )
    finally:
        await connection.close()


async def _set_endpoints(database_url: str, gateway_id: str, endpoints: dict) -> None:
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE gateways SET endpoints = $2::jsonb WHERE id = $1", gateway_id, endpoints
        )
    finally:
        await connection.close()


async def _make_pending_now(database_url: str, gateway_id: str) -> None:
    connection = await _connect(database_url)
    try:
        await connection.execute(
            "UPDATE outbox_operations SET available_at = now() WHERE gateway_id = $1 AND status = 'pending'",
            gateway_id,
        )
    finally:
        await connection.close()


async def test_enqueue_failure_rolls_back_publish_and_retry_reaches_snapshot(
    fence_env, monkeypatch
):
    """B1: an enqueue failure inside the publish step must not leave applied state that lets a
    retry skip the snapshot; the standard retry (idempotent remote apply) must reach it."""
    database_url, settings, database = fence_env
    fake = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-dirty", vk_hashes=["dirty-ok"])
    await fake.start()
    try:
        gateway_id = await _seed_gateway(database_url, fake.socket_path, key="gw-dirty")
        binding_id = await _seed_grant(database_url, gateway_id, SPKI)

        original = gc._enqueue
        state = {"failed": False}

        async def flaky_enqueue(*args, **kwargs):
            if not state["failed"] and kwargs.get("operation_type") == SYNC_PROFILE_OPERATION:
                state["failed"] = True
                raise RuntimeError("simulated crash between applied update and enqueue")
            return await original(*args, **kwargs)

        monkeypatch.setattr(gc, "_enqueue", flaky_enqueue)
        handlers = _handlers(settings, _admin_factory(fake.socket_path))
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="dirty")
        assert await worker.run_once() is True  # apply op fails at the dirty event enqueue
        monkeypatch.setattr(gc, "_enqueue", original)

        # Nothing was published that would let the retry skip the snapshot.
        grant = await _grant_row(database_url, binding_id)
        assert grant["state"] != "applied"
        assert int(grant["lease_seq"]) == 0
        assert await _epoch(database_url, gateway_id) == 0
        assert await _profile_count(database_url, gateway_id) == 0
        assert await _vk(database_url, gateway_id) is None

        # Retry the same apply op the normal way; no new external event is required.
        await _make_pending_now(database_url, gateway_id)
        assert await worker.run_once() is True
        assert await _epoch(database_url, gateway_id) == 1
        assert await _profile_count(database_url, gateway_id) == 1
        assert await worker.drain() == 1
        assert await _vk(database_url, gateway_id) == ["dirty-ok"]
        grant = await _grant_row(database_url, binding_id)
        assert grant["state"] == "applied"
        assert int(grant["lease_seq"]) == 1
    finally:
        await fake.stop()


async def test_registry_replacement_before_rpc_is_not_contacted(fence_env):
    """B2: a same-key registry replacement before processing must not receive the RPC and must
    not overwrite last-good; the frozen operation is never retargeted."""
    database_url, settings, database = fence_env
    fake_a = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-repl", vk_hashes=["from-A"])
    fake_b = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-repl", vk_hashes=["from-B"])
    await fake_a.start()
    await fake_b.start()
    try:
        gateway_id = await _seed_gateway(database_url, fake_a.socket_path, key="gw-repl")
        await _set_last_good(database_url, gateway_id, ["last-good"])
        endpoints_a = await _get_endpoints(database_url, gateway_id)
        epoch = await _enqueue_profile_full(
            database_url,
            gateway_id,
            "gw-repl",
            {"admin_socket": fake_a.socket_path},
            endpoints_a,
        )
        await _set_endpoints(
            database_url,
            gateway_id,
            {**endpoints_a, "admin_socket": fake_b.socket_path},
        )
        commands_a = len(fake_a.commands)
        commands_b = len(fake_b.commands)
        handlers = _handlers(settings, _admin_factory(fake_a.socket_path))
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="repl-before")
        assert await worker.run_once() is True
        assert len(fake_a.commands) == commands_a, "old endpoint was contacted after replacement"
        assert len(fake_b.commands) == commands_b, "stale operation was retargeted to new endpoint"
        assert await _vk(database_url, gateway_id) == ["last-good"]
        assert (await _op(database_url, gateway_id, epoch))["status"] == "done"
    finally:
        await fake_a.stop()
        await fake_b.stop()


async def test_registry_replacement_during_rpc_rejects_stale_readback(fence_env):
    """B2: a same-key replacement DURING the delayed RPC must make the old readback match no
    row in the final UPDATE (atomic registry recheck), keeping last-good."""
    database_url, settings, database = fence_env
    fake_a = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-mid", vk_hashes=["from-A"])
    fake_b = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-mid", vk_hashes=["from-B"])
    await fake_a.start()
    await fake_b.start()
    try:
        gateway_id = await _seed_gateway(database_url, fake_a.socket_path, key="gw-mid")
        await _set_last_good(database_url, gateway_id, ["last-good"])
        endpoints_a = await _get_endpoints(database_url, gateway_id)
        epoch = await _enqueue_profile_full(
            database_url,
            gateway_id,
            "gw-mid",
            {"admin_socket": fake_a.socket_path},
            endpoints_a,
        )

        started, release = asyncio.Event(), asyncio.Event()
        created: dict[str, dict] = {}

        class BlockingClient(GatewayAdminClient):
            async def engine_status(self):
                started.set()
                await release.wait()
                return {"node_id": "gw-mid", "vk_hashes": ["stale-from-A"]}

        def factory(_key, endpoints):
            created["route"] = dict(endpoints)
            return BlockingClient(
                socket_path=str(endpoints["admin_socket"]),
                main_password="fixture-main",
                timeout_seconds=5,
            )

        handlers = _handlers(settings, factory)
        worker = OutboxWorker(database, settings, handlers=handlers, worker_id="repl-mid")
        task = asyncio.create_task(worker.run_once())
        await asyncio.wait_for(started.wait(), timeout=10)
        # Replacement while the old RPC is in flight (same key, different endpoint).
        await _set_endpoints(
            database_url,
            gateway_id,
            {**endpoints_a, "admin_socket": fake_b.socket_path},
        )
        release.set()
        assert await asyncio.wait_for(task, timeout=10) is True

        assert created["route"] == {"admin_socket": fake_a.socket_path}, "op was retargeted"
        assert await _vk(database_url, gateway_id) == ["last-good"]
        assert (await _op(database_url, gateway_id, epoch))["status"] == "done"
        assert len(fake_b.commands) == 0
    finally:
        await fake_a.stop()
        await fake_b.stop()


async def test_missing_null_and_non_dict_endpoints_reject_before_rpc(fence_env):
    """Correction3: an operation without a usable frozen registry snapshot must abstain (no
    RPC, last-good and epoch untouched) even if the registry is replaced meanwhile."""
    database_url, settings, database = fence_env
    fake_a = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-shape", vk_hashes=["shape-a"])
    fake_b = FakeGatewayAdmin(main_password="fixture-main", node_id="gw-shape", vk_hashes=["shape-b"])
    await fake_a.start()
    await fake_b.start()
    omit = object()
    try:
        gateway_id = await _seed_gateway(database_url, fake_a.socket_path, key="gw-shape")
        await _set_last_good(database_url, gateway_id, ["last-good"])
        route = {"admin_socket": fake_a.socket_path}
        variants = [
            ("missing", omit),
            ("null", None),
            ("non_dict_string", "not-a-dict"),
            ("non_dict_list", ["not-a-dict"]),
        ]
        for label, endpoints in variants:
            connection = await _connect(database_url)
            try:
                epoch = int(
                    await connection.fetchval(
                        "UPDATE gateways SET snapshot_epoch = snapshot_epoch + 1 WHERE id = $1 RETURNING snapshot_epoch",
                        gateway_id,
                    )
                )
                payload = {"node_id": "gw-shape", "route": route, "epoch": epoch}
                if endpoints is not omit:
                    payload["endpoints"] = endpoints
                await _enqueue(
                    connection,
                    operation_type=SYNC_PROFILE_OPERATION,
                    idempotency_key=f"profile:{gateway_id}:{epoch}",
                    payload=payload,
                    gateway_id=gateway_id,
                    target_revision=epoch,
                )
            finally:
                await connection.close()
            # Replacement before processing: irrelevant, the payload may not fence it -> abstain.
            await _set_endpoints(
                database_url,
                gateway_id,
                {**await _get_endpoints(database_url, gateway_id), "admin_socket": fake_b.socket_path},
            )
            commands_a = len(fake_a.commands)
            commands_b = len(fake_b.commands)
            handlers = _handlers(settings, _admin_factory(fake_a.socket_path))
            worker = OutboxWorker(database, settings, handlers=handlers, worker_id=f"shape-{label}")
            assert await worker.run_once() is True
            assert len(fake_a.commands) == commands_a, f"{label}: old endpoint was contacted"
            assert len(fake_b.commands) == commands_b, f"{label}: new endpoint was contacted"
            assert await _vk(database_url, gateway_id) == ["last-good"], f"{label}: last-good clobbered"
            assert (await _op(database_url, gateway_id, epoch))["status"] == "done"
            assert await _epoch(database_url, gateway_id) == epoch, f"{label}: epoch damaged"
    finally:
        await fake_a.stop()
        await fake_b.stop()
