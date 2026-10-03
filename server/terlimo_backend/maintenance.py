"""Bounded retention sweeps and the periodic maintenance loop.

The raw stored response (which contains a session bearer) is removed after
`RECEIPT_RESULT_TTL_SECONDS` and never later than the session's own expiry. The receipt
tombstone (digest, no result) stays for `IDEMPOTENCY_WINDOW_SECONDS`, then is removed; the
result-policy is documented in the README. Expired challenges stay for
`CHALLENGE_RETENTION_SECONDS` so CHALLENGE_EXPIRED/REPLAY_DETECTED semantics are preserved
inside that window, then are deleted. Account access receipts (`access/sync`, migration 0010)
are durable dedupe identity for this TEST slice: they carry no bearer/raw auth result, are
stored with NULL retention bounds and are never touched by these sweeps (removal is an explicit
account lifecycle/archival decision, not a timeout). All sweeps are bounded batches and run in
the API process on `CLEANUP_INTERVAL_SECONDS`.
"""

from __future__ import annotations

import asyncio
import logging

import asyncpg

from .config import Settings
from .db import Database
from .reminders import sweep_entitlement_reminders
from .usage_pipeline import collect_gateway_usage

logger = logging.getLogger(__name__)

COUNTER_SCOPE_RETENTION_SECONDS = 600


async def sweep_once(
    connection: asyncpg.Connection, settings: Settings, *, batch_size: int | None = None
) -> dict[str, int]:
    batch = batch_size or settings.cleanup_batch_size
    purged_results = await connection.fetchval(
        """
        WITH target AS (
            SELECT id FROM operation_receipts
            WHERE result IS NOT NULL
              AND result_expires_at IS NOT NULL
              AND result_expires_at <= now()
            ORDER BY id
            LIMIT $1
        ), updated AS (
            UPDATE operation_receipts AS receipt
            SET result = NULL, updated_at = now()
            FROM target
            WHERE receipt.id = target.id
            RETURNING 1
        )
        SELECT count(*) FROM updated
        """,
        batch,
    )
    deleted_receipts = await connection.fetchval(
        """
        WITH target AS (
            SELECT id FROM operation_receipts
            WHERE retain_until IS NOT NULL AND retain_until <= now()
            ORDER BY id
            LIMIT $1
        ), deleted AS (
            DELETE FROM operation_receipts AS receipt
            USING target
            WHERE receipt.id = target.id
            RETURNING 1
        )
        SELECT count(*) FROM deleted
        """,
        batch,
    )
    deleted_challenges = await connection.fetchval(
        """
        WITH target AS (
            SELECT id FROM auth_challenges
            WHERE expires_at <= now() - make_interval(secs => $2)
            ORDER BY id
            LIMIT $1
        ), deleted AS (
            DELETE FROM auth_challenges AS challenge
            USING target
            WHERE challenge.id = target.id
            RETURNING 1
        )
        SELECT count(*) FROM deleted
        """,
        batch,
        float(settings.challenge_retention_seconds),
    )
    deleted_counters = await connection.fetchval(
        """
        WITH target AS (
            SELECT scope, window_start FROM public_endpoint_counters
            WHERE window_start < now() - make_interval(secs => $1)
            LIMIT $2
        ), deleted AS (
            DELETE FROM public_endpoint_counters AS counter
            USING target
            WHERE counter.scope = target.scope AND counter.window_start = target.window_start
            RETURNING 1
        )
        SELECT count(*) FROM deleted
        """,
        float(COUNTER_SCOPE_RETENTION_SECONDS),
        batch,
    )
    # Onboarding-hour lifecycle: bounded batches on the same recurring sweep. Kept after the
    # retention deletes above so the normal cleanup is never starved by hour work.
    from .onboarding_hour import (
        expire_hour_units,
        expire_intents,
        fail_exhausted_intents,
        revoke_backlog_stats,
    )

    expired_onboarding = await expire_intents(connection, limit=batch)
    expired_hours = await expire_hour_units(connection, limit=batch)
    failed_onboarding = await fail_exhausted_intents(connection, limit=batch)
    dead_revokes = await revoke_backlog_stats(connection, limit=batch)
    from .payment_products import refresh_expired_limits, cap_existing_extra_grants
    await refresh_expired_limits(connection,limit=batch)
    await cap_existing_extra_grants(connection,max_lease_seconds=settings.gateway_max_lease_seconds,limit=batch)
    # Bounded S4 payment reconciliation via the provider's own status call (no-op when the
    # provider is not configured). A provider/session failure must never break the sweep.
    from .payments import build_provider, reconcile_payments

    payment_provider = build_provider(settings)
    reconciled = {"checked": 0, "applied": 0, "status_changed": 0}
    if payment_provider is not None:
        try:
            reconciled = await reconcile_payments(connection, settings, payment_provider, limit=batch)
        except Exception:
            logger.exception("payment reconciliation failed")
        finally:
            close = getattr(payment_provider, "close", None)
            if close is not None:
                await close()
    from .referral_rewards import sweep_trial_rewards, sweep_rewards
    trial_rewards = await sweep_trial_rewards(connection, limit=batch)
    applied_rewards = await sweep_rewards(connection, settings, limit=batch)
    return {
        "referral_trial_rewards": trial_rewards,
        "referral_applied_rewards": applied_rewards,
        "purged_receipt_results": purged_results or 0,
        "deleted_receipts": deleted_receipts or 0,
        "deleted_challenges": deleted_challenges or 0,
        "deleted_rate_counters": deleted_counters or 0,
        "expired_onboarding": expired_onboarding,
        "expired_hours": expired_hours,
        "failed_onboarding": failed_onboarding,
        # Bounded, secret-free signal only; ``count`` is capped by the batch and never a full
        # count (``truncated`` flags that more dead revokes may exist).
        "dead_revokes": dead_revokes["count"],
        "dead_revokes_truncated": dead_revokes["truncated"],
        "reconciled_payments": reconciled["applied"] + reconciled["status_changed"],
    }


async def run_maintenance_loop(
    database: Database, settings: Settings, stop_event: asyncio.Event
) -> None:
    while not stop_event.is_set():
        try:
            if await database.ensure_ready():
                async with database.acquire() as connection:
                    result = await sweep_once(connection, settings)
                    if settings.reminders_enabled:
                        result.update(await sweep_entitlement_reminders(connection, settings))
                if settings.gateway_local_admin_enabled or settings.gateway_management_cert_file:
                    try:
                        async with database.acquire() as connection:
                            await collect_gateway_usage(connection, settings)
                    except Exception:
                        logger.exception("usage collection failed")
                if any(result.values()):
                    logger.info("maintenance sweep: %s", result)
        except asyncio.CancelledError:
            raise
        except Exception:
            logger.exception("maintenance sweep failed")
        try:
            await asyncio.wait_for(stop_event.wait(), timeout=settings.cleanup_interval_seconds)
        except TimeoutError:
            continue
