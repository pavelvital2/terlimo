"""RPC tracing preserves results/exceptions without copying remote data."""

from datetime import UTC, datetime
from uuid import uuid4

import pytest

from terlimo_backend.operation_timing import timed_rpc


async def test_rpc_trace_excludes_payload_result_and_exception_secrets(caplog):
    caplog.set_level("INFO", logger="terlimo_backend.operation_timing")
    secret = "never-log-this-credential"
    operation = {
        "id": uuid4(),
        "correlation_id": uuid4(),
        "attempts": 1,
        "created_at": datetime.now(UTC),
        "available_at": datetime.now(UTC),
        "payload": {"credential": secret},
        "idempotency_key": secret,
    }
    result = {"credential": secret}

    async def succeed():
        return result

    assert await timed_rpc(operation, "refresh", succeed()) is result
    failure = RuntimeError(secret)

    async def fail():
        raise failure

    with pytest.raises(RuntimeError) as caught:
        await timed_rpc(operation, "refresh", fail())
    assert caught.value is failure
    assert secret not in caplog.text
    messages = [record.message for record in caplog.records]
    assert [message.split()[1] for message in messages] == [
        "phase=rpc_begin",
        "phase=rpc_end",
        "phase=rpc_begin",
        "phase=rpc_end",
    ]
    assert "result=exception" in messages[-1]
