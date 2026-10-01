"""Operation phase timings: technical IDs only, never payloads or RPC data."""

import logging
import time
from datetime import UTC, datetime

logger = logging.getLogger(__name__)


def log_phase(operation, phase, started=None, *, result="ok", rpc="none", pool_ms=0.0):
    now = datetime.now(UTC)
    elapsed = (time.monotonic() - started) * 1000 if started is not None else 0.0
    # Enqueue/eligibility are durable DB timestamps. Claim completion is app UTC;
    # its monotonic duration includes the claim transaction but excludes pool wait.
    enqueue = operation["created_at"] if phase == "claim" else None
    available = operation["available_at"] if phase == "claim" else None
    logger.info(
        "OPTIME phase=%s operation_id=%s correlation_id=%s attempt=%s utc=%s "
        "duration_ms=%.3f result=%s rpc=%s pool_ms=%.3f enqueue_at=%s available_at=%s",
        phase,
        operation["id"],
        operation.get("correlation_id"),
        operation["attempts"],
        now.isoformat(),
        elapsed,
        result,
        rpc,
        pool_ms,
        enqueue.isoformat() if enqueue is not None else "none",
        available.isoformat() if available is not None else "none",
    )


async def timed_rpc(operation, rpc, awaitable):
    started = time.monotonic()
    log_phase(operation, "rpc_begin", rpc=rpc)
    try:
        result = await awaitable
    except BaseException:
        # Exception messages can carry remote data; record only the fixed outcome.
        log_phase(operation, "rpc_end", started, result="exception", rpc=rpc)
        raise
    log_phase(operation, "rpc_end", started, rpc=rpc)
    return result
