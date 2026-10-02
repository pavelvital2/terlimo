"""Bounded opt-in AUTH/relay phases; counters describe the shared native thread.

RID namespaces and relay task IDs remain separate. No payload or wire correlation.
Diagnostics are buffered per request and flushed once through the existing log sink.
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import threading
import time
from datetime import UTC, datetime

logger = logging.getLogger(__name__)
FLAG = "TERLIMO_AUTH_API_PHASE_PROBE"
MAX_RECORDS = 64


def _schedstat() -> tuple[int, int, int] | None:
    try:
        with open("/proc/thread-self/schedstat", encoding="ascii") as stream:
            raw = stream.read(129)
        if len(raw) > 128:
            return None
        values = tuple(int(v) for v in raw.split())
        if len(values) != 3 or any(v < 0 for v in values) or values[0] == 0:
            return None  # Disabled/zero counters cannot establish execution state.
        return values
    except Exception:
        return None


class AuthApiPhase:
    def __init__(self, scope: str, *, task_id: int | None = None,
                 sink: logging.Logger | None = None) -> None:
        self.scope = scope
        self.request_id: str | None = None
        self.task_id = task_id
        self.sink = sink if sink is not None else logger
        self.started = time.monotonic_ns()
        self.native_tid = threading.get_native_id()
        self.records: list[dict[str, object]] = []
        self.omitted = 0
        self.finished = False

    def mark(self, phase: str, outcome: str = "ok") -> None:
        try:
            if self.finished or re.fullmatch(r"[a-z_]{1,64}", phase) is None:
                return
            if outcome not in {"ok", "cancelled", "error", "rejected", "unavailable", "timeout"}:
                return
            if len(self.records) >= MAX_RECORDS - 1:
                self.omitted += 1
                return  # No counter/proc reads after the bounded record budget.
            wall = time.monotonic_ns()
            tid = threading.get_native_id()
            same_tid = tid == self.native_tid
            try:
                cpu = time.thread_time_ns() if same_tid else None
            except Exception:
                cpu = None
            sched = _schedstat() if same_tid else None
            self.records.append({
                "seq": len(self.records), "phase": phase, "outcome": outcome,
                "utc": datetime.now(UTC).isoformat(), "wall_ns": wall,
                "elapsed_ns": wall - self.started, "native_tid": tid,
                "thread_cpu_ns": cpu,
                "thread_cpu_state": "AVAILABLE" if cpu is not None else "UNKNOWN",
                "sched_runtime_ns": sched[0] if sched else None,
                "sched_runqueue_wait_ns": sched[1] if sched else None,
                "sched_slices": sched[2] if sched else None,
                "sched_state": "AVAILABLE" if sched else "UNKNOWN",
                "tid_state": "SAME" if same_tid else "UNKNOWN_CHANGED_TID",
            })
        except Exception:
            return  # Timing/log failures cannot alter the business operation.

    def activate(self, request_id: str) -> None:
        try:
            if re.fullmatch(r"[0-9a-f]{32}", request_id) is not None:
                if self.request_id is None or self.request_id == request_id:
                    self.request_id = request_id
        except Exception:
            return

    def finish(self) -> None:
        if self.finished:
            return
        self.finished = True
        try:
            if self.request_id is None and self.scope != "relay":
                return  # No validated RID: discard prevalidation records.
            if self.omitted:
                self.records.append({"seq": len(self.records), "phase": "truncated",
                                     "omitted_records": self.omitted})
            prefix = "RELAYPHASE" if self.scope == "relay" else "AUTHPHASE"
            self.sink.info("%s scope=%s request_id=%s task_id=%s records=%s",
                           prefix, self.scope, self.request_id or "UNKNOWN",
                           self.task_id if self.task_id is not None else "UNKNOWN",
                           json.dumps(self.records, separators=(",", ":")))
        except Exception:
            return


def new_auth_api_phase(scope: str) -> AuthApiPhase | None:
    if os.environ.get(FLAG) != "1":
        return None  # OFF: no clock, native TID, proc counter, allocation, or log work.
    try:
        task = asyncio.current_task()
        return AuthApiPhase(scope, task_id=id(task) if task else None)
    except Exception:
        return None
