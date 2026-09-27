"""Lifecycle delta: maintenance sweep + dead-provision terminal failure + remediation surface."""
from __future__ import annotations

import asyncio
import json
import uuid

import asyncpg
import pytest
from test_step036_onboarding_hour_storage import (
    FakeBootstrapClient,
    FakeCipher,
    _connect,
    _revoke_op_count,
    _seed_gateway,
    _seed_installation,
)

from terlimo_backend.db import Database
from terlimo_backend.gateway_control import GatewayError
from terlimo_backend.maintenance import run_maintenance_loop, sweep_once
from terlimo_backend.onboarding_hour import (
    BOOTSTRAP_PROVISION_OPERATION,
    OnboardingHourHandlers,
    create_intent,
    revoke_backlog,
)


async def _intent(database_url: str, request_key: str = "life-key-1"):
    installation_id = await _seed_installation(database_url)
    await _seed_gateway(database_url, key=f"gw-{request_key}")
    connection = await _connect(database_url)
    try:
        created = await create_intent(
            connection,
            installation_id=installation_id,
            request_key=request_key,
            environment="test",
            cipher=FakeCipher(),
        )
    finally:
        await connection.close()
    return created


async def _mark_provision_dead(database_url: str, intent_id: str) -> None:
    connection = await _connect(database_url)
    try:
        await connection.execute(
            """
            UPDATE outbox_operations SET status = 'dead', attempts = max_attempts
            WHERE operation_type = $1 AND payload->>'intent_id' = $2
            """,
            BOOTSTRAP_PROVISION_OPERATION,
            intent_id,
        )
    finally:
        await connection.close()


async def _hour_count(database_url: str) -> int:
    connection = await _connect(database_url)
    try:
        return int(
            await connection.fetchval(
                "SELECT count(*) FROM entitlements WHERE kind = 'onboarding_hour'"
            )
        )
    finally:
        await connection.close()


async def test_sweep_expires_intent_without_client_request(migrated_url, settings_factory):
    created = await _intent(migrated_url)
    settings = settings_factory(migrated_url)
    connection = await _connect(migrated_url)
    try:
        await connection.execute(
            "UPDATE onboarding_intents SET expires_at = now() - interval '1 second' WHERE id = $1",
            created["id"],
        )
        first = await sweep_once(connection, settings)
        assert first["expired_onboarding"] == 1
        row = await connection.fetchrow(
            "SELECT state, secret_enc, secret_hash FROM onboarding_intents WHERE id = $1",
            created["id"],
        )
        assert row["state"] == "expired" and row["secret_enc"] is None and row["secret_hash"]
        assert await _revoke_op_count(migrated_url, created["credential_id"]) == 1
        assert await _hour_count(migrated_url) == 0
        second = await sweep_once(connection, settings)
        assert second["expired_onboarding"] == 0
        assert await _revoke_op_count(migrated_url, created["credential_id"]) == 1
    finally:
        await connection.close()


async def test_sweep_fails_dead_provision_and_never_starts_hour(migrated_url, settings_factory):
    created = await _intent(migrated_url, request_key="life-key-2")
    await _mark_provision_dead(migrated_url, created["id"])
    settings = settings_factory(migrated_url)
    connection = await _connect(migrated_url)
    try:
        first = await sweep_once(connection, settings)
        assert first["failed_onboarding"] == 1
        row = await connection.fetchrow(
            "SELECT state, failed_reason, secret_enc FROM onboarding_intents WHERE id = $1",
            created["id"],
        )
        assert row["state"] == "failed" and row["failed_reason"] == "provision_dead"
        assert row["secret_enc"] is None
        assert await _revoke_op_count(migrated_url, created["credential_id"]) == 1
        assert await _hour_count(migrated_url) == 0
        second = await sweep_once(connection, settings)
        assert second["failed_onboarding"] == 0
        assert await _revoke_op_count(migrated_url, created["credential_id"]) == 1
    finally:
        await connection.close()


async def test_late_handler_after_failed_does_not_resurrect_or_call_node(migrated_url, settings_factory):
    created = await _intent(migrated_url, request_key="life-key-3")
    await _mark_provision_dead(migrated_url, created["id"])
    settings = settings_factory(migrated_url)
    connection = await _connect(migrated_url)
    try:
        assert (await sweep_once(connection, settings))["failed_onboarding"] == 1
        client = FakeBootstrapClient()
        handlers = OnboardingHourHandlers(
            cipher=FakeCipher(), client_factory=lambda _key, _endpoints: client
        )
        operation = await connection.fetchrow(
            "SELECT * FROM outbox_operations WHERE operation_type = $1 AND payload->>'intent_id' = $2",
            BOOTSTRAP_PROVISION_OPERATION,
            created["id"],
        )
        await handlers.bootstrap_provision(connection, operation)
        assert client.provisions == []
        assert await connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", created["id"]
        ) == "failed"
        assert await _revoke_op_count(migrated_url, created["credential_id"]) == 1
    finally:
        await connection.close()


async def test_pending_dead_op_handler_success_then_sweep_keeps_ready(migrated_url, settings_factory):
    created = await _intent(migrated_url, request_key="life-key-4")
    await _mark_provision_dead(migrated_url, created["id"])
    settings = settings_factory(migrated_url)
    connection = await _connect(migrated_url)
    try:
        client = FakeBootstrapClient()
        handlers = OnboardingHourHandlers(
            cipher=FakeCipher(), client_factory=lambda _key, _endpoints: client
        )
        operation = await connection.fetchrow(
            "SELECT * FROM outbox_operations WHERE operation_type = $1 AND payload->>'intent_id' = $2",
            BOOTSTRAP_PROVISION_OPERATION,
            created["id"],
        )
        await handlers.bootstrap_provision(connection, operation)
        assert len(client.provisions) == 1
        assert await connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", created["id"]
        ) == "ready"
        result = await sweep_once(connection, settings)
        assert result["failed_onboarding"] == 0
        assert await connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", created["id"]
        ) == "ready"
        assert await _revoke_op_count(migrated_url, created["credential_id"]) == 0
    finally:
        await connection.close()


async def test_dead_revoke_is_observable_and_not_requeued(migrated_url, settings_factory):
    created = await _intent(migrated_url, request_key="life-key-5")
    await _mark_provision_dead(migrated_url, created["id"])
    settings = settings_factory(migrated_url)
    connection = await _connect(migrated_url)
    try:
        assert (await sweep_once(connection, settings))["failed_onboarding"] == 1
        await connection.execute(
            """
            UPDATE outbox_operations SET status = 'dead', attempts = max_attempts, last_error = 'fake'
            WHERE operation_type = 'gateway.bootstrap_revoke' AND payload->>'credential_id' = $1
            """,
            created["credential_id"],
        )
        backlog = await revoke_backlog(connection)
        assert len(backlog) == 1 and backlog[0]["credential_id"] == created["credential_id"]
        await sweep_once(connection, settings)
        assert await _revoke_op_count(migrated_url, created["credential_id"]) == 1
        status = await connection.fetchval(
            "SELECT status FROM outbox_operations WHERE operation_type = 'gateway.bootstrap_revoke'"
        )
        assert status == "dead"
        assert len(await revoke_backlog(connection)) == 1
    finally:
        await connection.close()


async def test_recurring_loop_runs_sweep_without_starving_cleanup(migrated_url, settings_factory):
    settings = settings_factory(migrated_url, cleanup_interval_seconds=0.05, cleanup_batch_size=2)
    connection = await _connect(migrated_url)
    try:
        # bounded batch handles onboarding and retention cleanup in the same call
        first = await _intent(migrated_url, request_key="life-key-6")
        second = await _intent(migrated_url, request_key="life-key-7")
        for created in (first, second):
            await connection.execute(
                "UPDATE onboarding_intents SET expires_at = now() - interval '1 second' WHERE id = $1",
                created["id"],
            )
        await connection.execute(
            """
            INSERT INTO operation_receipts
                (environment, installation_ref, account_ref, op, idempotency_key, business_digest,
                 result, result_expires_at, retain_until)
            VALUES ('test', 'inst', 'acct', 'access.sync', $1, 'digest', '{"ok": true}'::jsonb,
                    now() - interval '1 hour', now() + interval '1 day')
            """,
            "life-receipt-" + uuid.uuid4().hex,
        )
        result = await sweep_once(connection, settings)
        assert result["expired_onboarding"] == 2
        assert result["purged_receipt_results"] >= 1

        # the recurring loop itself drives the sweep (no client request involved)
        third = await _intent(migrated_url, request_key="life-key-8")
        await connection.execute(
            "UPDATE onboarding_intents SET expires_at = now() - interval '1 second' WHERE id = $1",
            third["id"],
        )
        database = Database(settings)
        await database.connect()
        stop_event = asyncio.Event()
        task = asyncio.create_task(run_maintenance_loop(database, settings, stop_event))
        try:
            deadline = asyncio.get_running_loop().time() + 5
            while asyncio.get_running_loop().time() < deadline:
                state = await connection.fetchval(
                    "SELECT state FROM onboarding_intents WHERE id = $1", third["id"]
                )
                if state == "expired":
                    break
                await asyncio.sleep(0.05)
            assert state == "expired"
        finally:
            stop_event.set()
            await asyncio.wait_for(task, timeout=2)
            await database.close()
    finally:
        await connection.close()


class BarrierClient:
    """Two-task barrier client: blocks inside provision and records the call order."""

    def __init__(self, started: asyncio.Event, release: asyncio.Event) -> None:
        self.started = started
        self.release = release
        self.order: list[str] = []
        self.provisions: list[dict] = []

    async def bootstrap_provision(self, *, credential_id, secret, expires_at, node_id):
        self.started.set()
        await self.release.wait()
        self.order.append("provision")
        self.provisions.append({"credential_id": credential_id})
        return {"ok": True}

    async def bootstrap_revoke(self, *, credential_id):
        self.order.append("revoke")


async def test_barrier_sweep_skips_busy_inflight_provision(migrated_url, settings_factory):
    """Bounded sweep never waits: a busy credential is skipped, the in-flight intent survives."""
    created = await _intent(migrated_url, request_key="bar-key-1")
    await _mark_provision_dead(migrated_url, created["id"])
    settings = settings_factory(migrated_url)
    started, release = asyncio.Event(), asyncio.Event()
    client = BarrierClient(started, release)
    handlers = OnboardingHourHandlers(
        cipher=FakeCipher(), client_factory=lambda _key, _endpoints: client
    )
    provision_connection = await _connect(migrated_url)
    sweep_connection = await _connect(migrated_url)
    try:
        operation = await provision_connection.fetchrow(
            "SELECT * FROM outbox_operations WHERE operation_type = $1 AND payload->>'intent_id' = $2",
            BOOTSTRAP_PROVISION_OPERATION,
            created["id"],
        )
        provision_task = asyncio.create_task(
            handlers.bootstrap_provision(provision_connection, operation)
        )
        await asyncio.wait_for(started.wait(), timeout=5)
        result = await asyncio.wait_for(sweep_once(sweep_connection, settings), timeout=5)
        assert result["failed_onboarding"] == 0, "busy candidate must not be failed"
        assert await sweep_connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", created["id"]
        ) == "pending"
        release.set()
        await asyncio.wait_for(provision_task, timeout=5)
        assert client.order == ["provision"]
        assert await provision_connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", created["id"]
        ) == "ready"
    finally:
        await provision_connection.close()
        await sweep_connection.close()


async def test_barrier_revoke_handler_serializes_behind_provision(migrated_url):
    created = await _intent(migrated_url, request_key="bar-key-2")
    started, release = asyncio.Event(), asyncio.Event()
    client = BarrierClient(started, release)
    handlers = OnboardingHourHandlers(
        cipher=FakeCipher(), client_factory=lambda _key, _endpoints: client
    )
    setup = await _connect(migrated_url)
    try:
        await setup.execute(
            """
            INSERT INTO outbox_operations
                (operation_type, payload, idempotency_key, target_revision, gateway_id, status,
                 attempts, max_attempts)
            VALUES ('gateway.bootstrap_revoke', $1::jsonb, 'manual-revoke-bar', 1, $2, 'pending', 0, 12)
            """,
            json.dumps({"intent_id": created["id"], "credential_id": created["credential_id"]}),
            uuid.UUID(created["gateway_id"]),
        )
        provision_operation = await setup.fetchrow(
            "SELECT * FROM outbox_operations WHERE operation_type = $1 AND payload->>'intent_id' = $2",
            BOOTSTRAP_PROVISION_OPERATION,
            created["id"],
        )
        revoke_operation = await setup.fetchrow(
            "SELECT * FROM outbox_operations WHERE idempotency_key = 'manual-revoke-bar'"
        )
    finally:
        await setup.close()
    provision_connection = await _connect(migrated_url)
    revoke_connection = await _connect(migrated_url)
    try:
        provision_task = asyncio.create_task(
            handlers.bootstrap_provision(provision_connection, provision_operation)
        )
        await asyncio.wait_for(started.wait(), timeout=5)
        revoke_task = asyncio.create_task(
            handlers.bootstrap_revoke(revoke_connection, revoke_operation)
        )
        await asyncio.sleep(0.3)
        assert client.order == [], "revoke must wait for the in-flight provision"
        release.set()
        await asyncio.wait_for(provision_task, timeout=5)
        await asyncio.wait_for(revoke_task, timeout=5)
        assert client.order == ["provision", "revoke"]
    finally:
        await provision_connection.close()
        await revoke_connection.close()


async def test_dead_revoke_stats_bounded_and_truncated(migrated_url, settings_factory):
    for index in (1, 2):
        created = await _intent(migrated_url, request_key=f"stats-key-{index}")
        await _mark_provision_dead(migrated_url, created["id"])
    settings = settings_factory(migrated_url)
    connection = await _connect(migrated_url)
    try:
        assert (await sweep_once(connection, settings, batch_size=10))["failed_onboarding"] == 2
        await connection.execute(
            "UPDATE outbox_operations SET status = 'dead', attempts = max_attempts WHERE operation_type = 'gateway.bootstrap_revoke'"
        )
        stats = await __import__("terlimo_backend.onboarding_hour", fromlist=["x"]).revoke_backlog_stats(
            connection, limit=1
        )
        assert stats == {"count": 1, "truncated": True, "limit": 1}
        bounded = await sweep_once(connection, settings, batch_size=1)
        assert bounded["dead_revokes"] == 1 and bounded["dead_revokes_truncated"] is True
        full = await sweep_once(connection, settings, batch_size=10)
        assert full["dead_revokes"] == 2 and full["dead_revokes_truncated"] is False
    finally:
        await connection.close()


async def _lock_held_by_other(database_url: str, credential_id: str) -> bool:
    connection = await _connect(database_url)
    try:
        return bool(
            await connection.fetchval(
                "SELECT pg_try_advisory_lock(hashtextextended($1, 0))",
                f"onboarding-credential:{credential_id}",
            )
        )
    finally:
        await connection.execute(
            "SELECT pg_advisory_unlock(hashtextextended($1, 0))",
            f"onboarding-credential:{credential_id}",
        )
        await connection.close()


async def test_error_rpc_releases_lock_and_connection_stays_usable(migrated_url):
    created = await _intent(migrated_url, request_key="lock-key-err")
    connection = await _connect(migrated_url)
    try:
        operation = await connection.fetchrow(
            "SELECT * FROM outbox_operations WHERE operation_type = $1 AND payload->>'intent_id' = $2",
            BOOTSTRAP_PROVISION_OPERATION,
            created["id"],
        )

        class FailingClient:
            async def bootstrap_provision(self, **_kwargs):
                raise GatewayError("GATEWAY_UNREACHABLE", "simulated")

            async def bootstrap_revoke(self, **_kwargs):
                raise GatewayError("GATEWAY_UNREACHABLE", "simulated")

        handlers = OnboardingHourHandlers(
            cipher=FakeCipher(), client_factory=lambda _key, _endpoints: FailingClient()
        )
        with pytest.raises(GatewayError):
            await handlers.bootstrap_provision(connection, operation)
        # the same pooled connection is still usable and holds no credential lock
        assert await connection.fetchval("SELECT 1") == 1
        assert await _lock_held_by_other(migrated_url, created["credential_id"]) is True
    finally:
        await connection.close()


async def test_cancel_during_busy_acquire_does_not_leak_lock(migrated_url):
    created = await _intent(migrated_url, request_key="lock-key-cancel")
    holder = await _connect(migrated_url)
    waiter = await _connect(migrated_url)
    try:
        assert await holder.fetchval(
            "SELECT pg_try_advisory_lock(hashtextextended($1, 0))",
            f"onboarding-credential:{created['credential_id']}",
        )
        operation = await waiter.fetchrow(
            "SELECT * FROM outbox_operations WHERE operation_type = $1 AND payload->>'intent_id' = $2",
            BOOTSTRAP_PROVISION_OPERATION,
            created["id"],
        )
        handlers = OnboardingHourHandlers(
            cipher=FakeCipher(), client_factory=lambda _key, _endpoints: FakeBootstrapClient()
        )
        task = asyncio.create_task(handlers.bootstrap_provision(waiter, operation))
        await asyncio.sleep(0.3)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        await holder.execute(
            "SELECT pg_advisory_unlock(hashtextextended($1, 0))",
            f"onboarding-credential:{created['credential_id']}",
        )
        assert await _lock_held_by_other(migrated_url, created["credential_id"]) is True
    finally:
        await holder.close()
        await waiter.close()


async def test_pool_reborrow_is_lockfree_after_cancel_during_rpc(migrated_url, settings_factory):
    created = await _intent(migrated_url, request_key="lock-key-pool")
    settings = settings_factory(migrated_url)
    database = Database(settings)
    await database.connect()
    started, release = asyncio.Event(), asyncio.Event()
    client = BarrierClient(started, release)
    handlers = OnboardingHourHandlers(
        cipher=FakeCipher(), client_factory=lambda _key, _endpoints: client
    )
    try:
        async with database.acquire() as operation_connection:
            operation = await operation_connection.fetchrow(
                "SELECT * FROM outbox_operations WHERE operation_type = $1 AND payload->>'intent_id' = $2",
                BOOTSTRAP_PROVISION_OPERATION,
                created["id"],
            )
        async with database.acquire() as pooled:
            task = asyncio.create_task(handlers.bootstrap_provision(pooled, operation))
            await asyncio.wait_for(started.wait(), timeout=5)
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task
            release.set()
        # fresh borrow from the same pool must not inherit a locked session
        async with database.acquire() as fresh:
            assert await fresh.fetchval("SELECT 1") == 1
        assert await _lock_held_by_other(migrated_url, created["credential_id"]) is True
        connection = await _connect(migrated_url)
        try:
            assert await connection.fetchval(
                "SELECT state FROM onboarding_intents WHERE id = $1", created["id"]
            ) == "pending"
        finally:
            await connection.close()
    finally:
        await database.close()


async def test_sweep_first_busy_then_next_free(migrated_url, settings_factory):
    first = await _intent(migrated_url, request_key="fair-key-1")
    second = await _intent(migrated_url, request_key="fair-key-2")
    settings = settings_factory(migrated_url)
    connection = await _connect(migrated_url)
    holder = await _connect(migrated_url)
    try:
        for created in (first, second):
            await connection.execute(
                "UPDATE onboarding_intents SET expires_at = now() - interval '1 hour' WHERE id = $1",
                created["id"],
            )
        assert await holder.fetchval(
            "SELECT pg_try_advisory_lock(hashtextextended($1, 0))",
            f"onboarding-credential:{first['credential_id']}",
        )
        result = await sweep_once(connection, settings, batch_size=1)
        assert result["expired_onboarding"] == 1
        first_state = await connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", first["id"]
        )
        second_state = await connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", second["id"]
        )
        assert first_state == "pending" and second_state == "expired"
        await holder.execute(
            "SELECT pg_advisory_unlock(hashtextextended($1, 0))",
            f"onboarding-credential:{first['credential_id']}",
        )
        assert (await sweep_once(connection, settings, batch_size=1))["expired_onboarding"] == 1
        assert await connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", first["id"]
        ) == "expired"
    finally:
        await connection.close()
        await holder.close()


async def _seed_expired_series(migrated_url: str, prefix: str, count: int) -> list[dict]:
    created: list[dict] = []
    connection = await _connect(migrated_url)
    try:
        for index in range(count):
            item = await _intent(migrated_url, request_key=f"{prefix}-{index}")
            await connection.execute(
                """
                UPDATE onboarding_intents
                SET expires_at = now() - interval '1 hour' + make_interval(secs => $2)
                WHERE id = $1
                """,
                item["id"],
                index * 10,
            )
            created.append(item)
    finally:
        await connection.close()
    return created


async def _hold_credential_locks(database_url: str, items: list[dict]) -> asyncpg.Connection:
    holder = await _connect(database_url)
    for item in items:
        await holder.execute(
            "SELECT pg_advisory_lock(hashtextextended($1, 0))",
            f"onboarding-credential:{item['credential_id']}",
        )
    return holder


async def _release_all(holder, items: list[dict]) -> None:
    for item in items:
        await holder.execute(
            "SELECT pg_advisory_unlock(hashtextextended($1, 0))",
            f"onboarding-credential:{item['credential_id']}",
        )


async def test_expire_busy_prefix_larger_than_window_progresses(migrated_url, settings_factory):
    """Busy prefix strictly above the actual window (10 > limit*8+1 with limit=1): no unlock
    between sweeps; first sweep processes nothing, the next sweep processes the free candidate;
    the durable cursor advances through the skipped busy prefix."""
    created = await _seed_expired_series(migrated_url, "busyprefix-exp", 11)
    settings = settings_factory(migrated_url)
    holder = await _hold_credential_locks(migrated_url, created[:10])
    connection = await _connect(migrated_url)
    free = created[10]
    try:
        first = await sweep_once(connection, settings, batch_size=1)
        assert first["expired_onboarding"] == 0, "window is fully busy, nothing may be processed"
        assert await connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", free["id"]
        ) == "pending"
        cursor_after_first = await connection.fetchrow(
            "SELECT last_id FROM onboarding_sweep_cursors WHERE sweep = 'expire_intents'"
        )
        assert cursor_after_first["last_id"] is not None

        second = await sweep_once(connection, settings, batch_size=1)
        assert second["expired_onboarding"] == 1
        assert await connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", free["id"]
        ) == "expired"
        cursor_after_second = await connection.fetchrow(
            "SELECT last_id FROM onboarding_sweep_cursors WHERE sweep = 'expire_intents'"
        )
        assert str(cursor_after_second["last_id"]) == free["id"], "cursor did not advance to the free candidate"
        busy_states = await connection.fetch(
            "SELECT state FROM onboarding_intents WHERE id = ANY($1::uuid[])",
            [item["id"] for item in created[:10]],
        )
        assert all(row["state"] == "pending" for row in busy_states), "busy candidates must stay untouched"

        await _release_all(holder, created[:10])
        for _ in range(14):
            states = [
                await connection.fetchval(
                    "SELECT state FROM onboarding_intents WHERE id = $1", item["id"]
                )
                for item in created
            ]
            if not any(state == "pending" for state in states):
                break
            await sweep_once(connection, settings, batch_size=1)
        assert await connection.fetchval(
            "SELECT count(*) FROM onboarding_intents WHERE state = 'expired'"
        ) == 11
    finally:
        await connection.close()
        await holder.close()


async def test_fail_exhausted_busy_prefix_larger_than_window_progresses(migrated_url, settings_factory):
    """Same strict busy-prefix scenario for the dead-provision sweep: first sweep 0, next sweep
    processes the free candidate, cursor advances, busy stay pending until released."""
    created = await _seed_expired_series(migrated_url, "busyprefix-fail", 11)
    future = await _connect(migrated_url)
    try:
        for index, item in enumerate(created):
            await future.execute(
                "UPDATE onboarding_intents SET expires_at = now() + interval '1 hour' + make_interval(secs => $2) WHERE id = $1",
                item["id"],
                index * 10,
            )
            await _mark_provision_dead(migrated_url, item["id"])
    finally:
        await future.close()
    settings = settings_factory(migrated_url)
    holder = await _hold_credential_locks(migrated_url, created[:10])
    connection = await _connect(migrated_url)
    free = created[10]
    try:
        first = await sweep_once(connection, settings, batch_size=1)
        assert first["failed_onboarding"] == 0, "window is fully busy, nothing may be processed"
        assert await connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", free["id"]
        ) == "pending"

        second = await sweep_once(connection, settings, batch_size=1)
        assert second["failed_onboarding"] == 1
        assert await connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", free["id"]
        ) == "failed"
        cursor = await connection.fetchrow(
            "SELECT last_id FROM onboarding_sweep_cursors WHERE sweep = 'fail_exhausted_intents'"
        )
        assert str(cursor["last_id"]) == free["id"], "cursor did not advance to the free candidate"

        await _release_all(holder, created[:10])
        for _ in range(14):
            pending = await connection.fetchval(
                "SELECT count(*) FROM onboarding_intents WHERE state = 'pending'"
            )
            if pending == 0:
                break
            await sweep_once(connection, settings, batch_size=1)
        assert await connection.fetchval(
            "SELECT count(*) FROM onboarding_intents WHERE state = 'failed'"
        ) == 11
    finally:
        await connection.close()
        await holder.close()


async def test_cursor_wraps_to_newer_eligible_rows(migrated_url, settings_factory):
    first = await _seed_expired_series(migrated_url, "wrap", 2)
    settings = settings_factory(migrated_url)
    connection = await _connect(migrated_url)
    try:
        assert (await sweep_once(connection, settings, batch_size=10))["expired_onboarding"] == 2
        late = await _intent(migrated_url, request_key="wrap-late")
        await connection.execute(
            "UPDATE onboarding_intents SET expires_at = now() - interval '5 hours' WHERE id = $1",
            late["id"],
        )
        assert (await sweep_once(connection, settings, batch_size=10))["expired_onboarding"] == 1
        assert await connection.fetchval(
            "SELECT state FROM onboarding_intents WHERE id = $1", late["id"]
        ) == "expired"
        assert await connection.fetchval(
            "SELECT count(*) FROM onboarding_intents WHERE state = 'pending'"
        ) == 0
        assert first  # series seeds are used for ordering only
    finally:
        await connection.close()


async def test_multi_instance_sweeps_are_safe(migrated_url, settings_factory):
    await _seed_expired_series(migrated_url, "multi", 4)
    settings = settings_factory(migrated_url)
    first = await _connect(migrated_url)
    second = await _connect(migrated_url)
    try:
        await asyncio.gather(
            sweep_once(first, settings, batch_size=4),
            sweep_once(second, settings, batch_size=4),
            return_exceptions=False,
        )
        await sweep_once(first, settings, batch_size=4)
        assert await first.fetchval(
            "SELECT count(*) FROM onboarding_intents WHERE state = 'expired'"
        ) == 4
    finally:
        await first.close()
        await second.close()


class SlowUnlockConnection:
    """Proxy that makes the advisory unlock slow, to exercise cancel-during-cleanup."""

    def __init__(self, connection, *, unlock_delay: float) -> None:
        self._connection = connection
        self._unlock_delay = unlock_delay
        self.poisoned = False

    def __getattr__(self, name):
        return getattr(self._connection, name)

    async def fetchval(self, query, *args):
        if "pg_advisory_unlock" in query:
            await asyncio.sleep(self._unlock_delay)
        return await self._connection.fetchval(query, *args)

    def terminate(self):
        self.poisoned = True
        terminate = getattr(self._connection, "terminate", None)
        if callable(terminate):
            terminate()


async def test_repeated_cancel_during_cleanup_poisons_or_releases(migrated_url, monkeypatch):
    from terlimo_backend import onboarding_hour as module

    created = await _intent(migrated_url, request_key="cancel-cleanup")
    operation_connection = await _connect(migrated_url)
    real = await _connect(migrated_url)
    slow = SlowUnlockConnection(real, unlock_delay=0.5)
    monkeypatch.setattr(module, "CREDENTIAL_LOCK_CLEANUP_SECONDS", 0.1)
    started, release = asyncio.Event(), asyncio.Event()
    client = BarrierClient(started, release)
    handlers = OnboardingHourHandlers(
        cipher=FakeCipher(), client_factory=lambda _key, _endpoints: client
    )
    try:
        operation = await operation_connection.fetchrow(
            "SELECT * FROM outbox_operations WHERE operation_type = $1 AND payload->>'intent_id' = $2",
            BOOTSTRAP_PROVISION_OPERATION,
            created["id"],
        )
        task = asyncio.create_task(handlers.bootstrap_provision(slow, operation))
        await asyncio.wait_for(started.wait(), timeout=5)
        task.cancel()
        await asyncio.sleep(0.2)
        task.cancel()  # repeated cancel while cleanup is awaiting the slow unlock
        with pytest.raises(asyncio.CancelledError):
            await task
        await asyncio.sleep(0.7)
        assert await _lock_held_by_other(migrated_url, created["credential_id"]) is True
    finally:
        await operation_connection.close()
        await real.close()


async def test_cancel_during_acquire_leaves_no_orphan_task_exception(migrated_url):
    created = await _intent(migrated_url, request_key="orphan-acq")
    holder = await _connect(migrated_url)
    waiter = await _connect(migrated_url)
    recorded: list[dict] = []
    loop = asyncio.get_running_loop()
    previous = loop.get_exception_handler()
    loop.set_exception_handler(lambda _loop, context: recorded.append(context))
    try:
        await holder.execute(
            "SELECT pg_advisory_lock(hashtextextended($1, 0))",
            f"onboarding-credential:{created['credential_id']}",
        )
        operation = await waiter.fetchrow(
            "SELECT * FROM outbox_operations WHERE operation_type = $1 AND payload->>'intent_id' = $2",
            BOOTSTRAP_PROVISION_OPERATION,
            created["id"],
        )
        handlers = OnboardingHourHandlers(
            cipher=FakeCipher(), client_factory=lambda _key, _endpoints: FakeBootstrapClient()
        )
        task = asyncio.create_task(handlers.bootstrap_provision(waiter, operation))
        await asyncio.sleep(0.3)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        await asyncio.sleep(0.2)
        assert recorded == [], f"orphan task exception surfaced: {recorded}"
        await holder.execute(
            "SELECT pg_advisory_unlock(hashtextextended($1, 0))",
            f"onboarding-credential:{created['credential_id']}",
        )
        assert await _lock_held_by_other(migrated_url, created["credential_id"]) is True
    finally:
        loop.set_exception_handler(previous)
        await holder.close()
        await waiter.close()
