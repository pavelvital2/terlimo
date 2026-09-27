"""Wire-faithful fake of the WDTT admin `client-test` surface for focused tests.

This is NOT the real gateway: it reimplements the reviewed semantics of
`client_test_admin.go` (command digest over exact bytes, operation-id idempotency,
generation/lease_seq rules, revoke tombstone, readback shape) so the Backend adapter and
worker can be tested without a live WDTT instance. Real applied-grant evidence is produced
separately with the actual gateway binary in an isolated namespace.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import tempfile
from pathlib import Path
from typing import Any


def _compact(value: Any) -> str:
    return json.dumps(value, separators=(",", ":"), ensure_ascii=False, sort_keys=False)


class FakeGatewayAdmin:
    def __init__(
        self,
        main_password: str = "fixture-main",
        node_id: str = "fake-node",
        max_workers: int = 36,
        vk_hashes: list[str] | None = None,
        fail_engine_status: bool = False,
    ) -> None:
        self.main_password = main_password
        self.node_id = node_id
        self.max_workers = max_workers
        # None simulates an old node without the field; [] is a confirmed empty snapshot.
        self.vk_hashes = vk_hashes
        self.fail_engine_status = fail_engine_status
        self.grants: dict[str, dict[str, Any]] = {}
        self.commands: list[str] = []
        self.handler_invocations = 0
        self.drop_next_response = False
        self._server: asyncio.AbstractServer | None = None
        self._dir = Path(tempfile.mkdtemp(prefix="fake-gateway-"))
        self._dir.chmod(0o700)
        self.socket_path = str(self._dir / "admin.sock")

    async def start(self) -> None:
        self._server = await asyncio.start_unix_server(self._handle, path=self.socket_path)

    async def stop(self) -> None:
        if self._server is not None:
            self._server.close()
            self._server = None
            await asyncio.sleep(0)

    def close_socket(self) -> None:
        if os.path.exists(self.socket_path):
            os.unlink(self.socket_path)

    async def _handle(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        self.handler_invocations += 1
        request = None
        buffer = b""
        try:
            while True:
                chunk = await reader.read(65536)
                if not chunk:
                    break
                buffer += chunk
                try:
                    request = json.loads(buffer.decode("utf-8"))
                    break
                except (ValueError, UnicodeError):
                    continue
        except (ConnectionResetError, asyncio.IncompleteReadError):
            request = None
        if request is None:
            writer.close()
            return
        if self.drop_next_response:
            self.drop_next_response = False
            writer.close()
            return
        if request.get("main_password") != self.main_password:
            response: dict[str, Any] = {"ok": False, "message": "main password mismatch"}
        elif request.get("args", [None])[0] != "client-test" or len(request["args"]) != 2:
            response = {"ok": False, "code": "BAD_MESSAGE"}
        else:
            self.commands.append(request["args"][1])
            response = self._dispatch(request["args"][1])
        writer.write(_compact(response).encode("utf-8"))
        await writer.drain()
        writer.close()

    def _dispatch(self, raw: str) -> dict[str, Any]:
        try:
            command = json.loads(raw)
        except ValueError:
            return {"ok": False, "code": "BAD_MESSAGE"}
        operation = command.get("operation")
        if operation == "engine_status":
            if self.fail_engine_status:
                return {"ok": False, "code": "MANAGED_ENGINE_UNAVAILABLE"}
            payload: dict[str, Any] = {"node_id": self.node_id, "managed_ready": True}
            if self.vk_hashes is not None:
                payload["vk_hashes"] = list(self.vk_hashes)
            return {"ok": True, "client_test": payload}
        if operation == "grant_get":
            return self._readback(operation, command)
        if command.get("password") in (None, "", "fixture-main"):
            return {"ok": False, "code": "BAD_MESSAGE"}
        if command.get("grant", {}).get("node_id") != self.node_id:
            return {"ok": False, "code": "NODE_ID_MISMATCH"}
        if operation == "grant_provision":
            return self._provision(raw, command)
        if operation == "refresh_lease":
            return self._refresh(raw, command)
        if operation == "grant_revoke":
            return self._revoke(raw, command)
        return {"ok": False, "code": "BAD_OPERATION"}

    def _readback(self, operation: str, command: dict[str, Any]) -> dict[str, Any]:
        password = command.get("password", "")
        entry = self.grants.get(password)
        if entry is None:
            return {"ok": False, "code": "NOT_FOUND"}
        return {"ok": True, "client_test": self._readback_body(password, entry)}

    def _readback_body(self, password: str, entry: dict[str, Any]) -> dict[str, Any]:
        return {
            "grant_id": entry["grant_id"],
            "registration_id": entry["registration_id"],
            "node_id": entry["node_id"],
            "key_fingerprint": hashlib.sha256(entry["public_key_spki"].encode()).hexdigest(),
            "generation": str(entry["generation"]),
            "lease_seq": str(entry["lease_seq"]),
            "expires_at": entry["expires_at"],
            "revoked": entry["revoked"],
            "runtime_applied": (not entry["revoked"]) and entry["expires_at"] > 0,
            "max_workers": self.max_workers,
            "active_workers": 0,
        }

    def _idempotent(self, raw: str, command: dict[str, Any], entry: dict[str, Any] | None):
        if entry is None:
            return None
        if entry.get("operation_id") == command["grant"]["operation_id"]:
            if entry.get("operation_digest") != hashlib.sha256(raw.encode()).hexdigest():
                return {"ok": False, "code": "IDEMPOTENCY_CONFLICT"}
            return {"ok": True, "client_test": self._readback_body(command["password"], entry)}
        return None

    def _provision(self, raw: str, command: dict[str, Any]) -> dict[str, Any]:
        password = command["password"]
        entry = self.grants.get(password)
        idempotent = self._idempotent(raw, command, entry)
        if idempotent is not None:
            return idempotent
        grant = command["grant"]
        if entry is not None or grant["generation"] != "1" or grant["lease_seq"] != "1" or grant["revoked"]:
            return {"ok": False, "code": "CONFLICT"}
        for other in self.grants.values():
            if other["grant_id"] == grant["grant_id"] or other["registration_id"] == grant["registration_id"]:
                return {"ok": False, "code": "CONFLICT"}
        self.grants[password] = {
            "grant_id": grant["grant_id"],
            "registration_id": grant["registration_id"],
            "node_id": grant["node_id"],
            "public_key_spki": grant["public_key_spki"],
            "generation": 1,
            "lease_seq": 1,
            "expires_at": int(command["expires_at"]),
            "revoked": False,
            "operation_id": grant["operation_id"],
            "operation_digest": hashlib.sha256(raw.encode()).hexdigest(),
        }
        return {"ok": True, "client_test": self._readback_body(password, self.grants[password])}

    def _refresh(self, raw: str, command: dict[str, Any]) -> dict[str, Any]:
        password = command["password"]
        entry = self.grants.get(password)
        idempotent = self._idempotent(raw, command, entry)
        if idempotent is not None:
            return idempotent
        if entry is None:
            return {"ok": False, "code": "NOT_FOUND"}
        grant = command["grant"]
        if (
            entry["revoked"]
            or grant["generation"] != str(entry["generation"])
            or grant["lease_seq"] != str(entry["lease_seq"] + 1)
            or command.get("expected_seq") != str(entry["lease_seq"])
            or grant["revoked"]
        ):
            return {"ok": False, "code": "LEASE_CONFLICT"}
        entry.update(
            lease_seq=entry["lease_seq"] + 1,
            expires_at=int(command["expires_at"]),
            operation_id=grant["operation_id"],
            operation_digest=hashlib.sha256(raw.encode()).hexdigest(),
        )
        return {"ok": True, "client_test": self._readback_body(password, entry)}

    def _revoke(self, raw: str, command: dict[str, Any]) -> dict[str, Any]:
        password = command["password"]
        entry = self.grants.get(password)
        idempotent = self._idempotent(raw, command, entry)
        if idempotent is not None:
            return idempotent
        grant = command["grant"]
        if entry is None:
            if grant["generation"] != "2" or not grant["revoked"]:
                return {"ok": False, "code": "CONFLICT"}
            self.grants[password] = {
                "grant_id": grant["grant_id"],
                "registration_id": grant["registration_id"],
                "node_id": grant["node_id"],
                "public_key_spki": grant["public_key_spki"],
                "generation": 2,
                "lease_seq": int(grant["lease_seq"]),
                "expires_at": int(command["expires_at"]),
                "revoked": True,
                "operation_id": grant["operation_id"],
                "operation_digest": hashlib.sha256(raw.encode()).hexdigest(),
            }
        else:
            if grant["generation"] != str(entry["generation"] + 1) or not grant["revoked"]:
                return {"ok": False, "code": "LEASE_CONFLICT"}
            entry.update(
                generation=entry["generation"] + 1,
                revoked=True,
                operation_id=grant["operation_id"],
                operation_digest=hashlib.sha256(raw.encode()).hexdigest(),
            )
        return {"ok": True, "client_test": self._readback_body(password, self.grants[password])}
