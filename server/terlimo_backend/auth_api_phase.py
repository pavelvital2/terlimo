"""Opt-in, fixed-field timing for the validated AUTH challenge response path.

Outer service frame RID and inner generated challenge RID are separate namespaces.
No correlation header is added. Prevalidation timestamps are discarded unless a
validated challenge request activates this probe.
"""

from __future__ import annotations

import logging
import os
import re
import time
from datetime import UTC, datetime

logger = logging.getLogger(__name__)
FLAG = "TERLIMO_AUTH_API_PHASE_PROBE"


class AuthApiPhase:
    def __init__(self, scope: str) -> None:
        self.scope = scope
        self.started = time.monotonic()
        self.request_id: str | None = None
        self.pending: list[tuple[str, str, float, str]] = []

    def mark(self, phase: str, outcome: str = "ok") -> None:
        record = (
            phase,
            datetime.now(UTC).isoformat(),
            (time.monotonic() - self.started) * 1000,
            outcome,
        )
        if self.request_id is None:
            self.pending.append(record)
        else:
            self._emit(record)

    def activate(self, request_id: str) -> None:
        # Validate again at the logging boundary; never log recovered/raw identifiers.
        if re.fullmatch(r"[0-9a-f]{32}", request_id) is None:
            return
        self.request_id = request_id
        for record in self.pending:
            self._emit(record)
        self.pending.clear()

    def _emit(self, record: tuple[str, str, float, str]) -> None:
        phase, utc, elapsed, outcome = record
        logger.info(
            "AUTHPHASE scope=%s request_id=%s phase=%s utc=%s elapsed_ms=%.3f outcome=%s",
            self.scope,
            self.request_id,
            phase,
            utc,
            elapsed,
            outcome,
        )


def new_auth_api_phase(scope: str) -> AuthApiPhase | None:
    return AuthApiPhase(scope) if os.environ.get(FLAG) == "1" else None
