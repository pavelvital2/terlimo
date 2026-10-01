"""Operation phase timings: technical IDs only, never payloads or RPC data."""

import logging
import os
import time
from contextvars import ContextVar
from datetime import UTC, datetime

logger = logging.getLogger(__name__)
# ContextVar isolates overlapping tasks; each timed_rpc owns and resets its state.
_management_refresh_boundary = ContextVar("management_refresh_boundary", default=None)


def mark_management_refresh_drained():
    state = _management_refresh_boundary.get()
    if state is not None and state["drain_at"] is None:
        state["drain_at"] = time.monotonic()


def log_phase(operation, phase, started=None, *, result="ok", rpc="none", pool_ms=0.0, drain_at=None):
    now = datetime.now(UTC)
    finished = time.monotonic() if started is not None else None
    elapsed = (finished - started) * 1000 if started is not None else 0.0
    # Enqueue/eligibility are durable DB timestamps. Claim completion is app UTC;
    # its monotonic duration includes the claim transaction but excludes pool wait.
    enqueue = operation["created_at"] if phase == "claim" else None
    available = operation["available_at"] if phase == "claim" else None
    message = (
        "OPTIME phase=%s operation_id=%s correlation_id=%s attempt=%s utc=%s "
        "duration_ms=%.3f result=%s rpc=%s pool_ms=%.3f enqueue_at=%s available_at=%s"
    )
    values = (
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
    if drain_at is not None:
        message += " boundary=management_post_drain setup_send_ms=%.3f response_rest_ms=%.3f"
        values += ((drain_at - started) * 1000, (finished - drain_at) * 1000)
    logger.info(message, *values)


async def timed_rpc(operation, rpc, awaitable):
    started = time.monotonic()
    log_phase(operation, "rpc_begin", rpc=rpc)
    state = None
    if rpc == "refresh" and os.environ.get("TERLIMO_MANAGEMENT_RPC_BOUNDARY") == "1":
        state = {"drain_at": None}
    token = _management_refresh_boundary.set(state)
    try:
        try:
            result = await awaitable
        except BaseException:
            # Record only the fixed outcome, including cancellation; preserve the exception.
            log_phase(
                operation, "rpc_end", started, result="exception", rpc=rpc,
                drain_at=state["drain_at"] if state is not None else None,
            )
            raise
        log_phase(
            operation, "rpc_end", started, rpc=rpc,
            drain_at=state["drain_at"] if state is not None else None,
        )
        return result
    finally:
        _management_refresh_boundary.reset(token)
