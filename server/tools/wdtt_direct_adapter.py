"""Minimal private Minishop to WDTT Plus v15 adapter.

The service intentionally exposes only ensure/get/disable over one local
AF_UNIX socket.  WDTT credentials and raw responses never leave this process.
"""

from __future__ import annotations

import ipaddress
import hashlib
import sqlite3
import fcntl
import json
import os
import re
import signal
import socket
import stat
import struct
import sys
import time
import uuid
from contextlib import suppress
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from threading import Lock
from typing import Any, Final, Protocol
from urllib.parse import urlencode, urlsplit

ADAPTER_SOCKET: Final = Path("/run/terlimo-wdtt-adapter/adapter.sock")
WDTT_SOCKET: Final = Path("/run/wdtt/admin.sock")
CREDENTIAL_NAME: Final = "wdtt-main-password"
VK_HASH_CREDENTIAL_NAME: Final = "shared-vk-hash"
GRANT_NAMESPACE: Final = uuid.UUID("97ab0503-5926-51b2-9a62-741f65f11845")
MAX_MESSAGE_BYTES: Final = 16 * 1024
MAX_ADMIN_RESPONSE_BYTES: Final = 2 * 1024 * 1024
DEADLINE_SECONDS: Final = 5.0

_SUBSCRIPTION_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9:._-]{0,127}$")
_MARKER = re.compile(r"^tlm:[0-9a-f-]{36}$")
_PASSWORD = re.compile(r"^[ABCDEFGHJKLMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789]{16}$")
_PORTS = re.compile(r"^[0-9]{1,5},[0-9]{1,5},[0-9]{1,5}$")
_VK_HASH = re.compile(r"^[A-Za-z0-9_-]{43}$")
_VK_HASH_COUNT_MIN: Final = 1
_VK_HASH_COUNT_MAX: Final = 4
_WORKERS_PER_HASH: Final = 9
_VK_JOIN_HOSTS: Final = frozenset({"vk.ru", "vk.com"})
_VK_JOIN_PATH = re.compile(r"^/call/join/([A-Za-z0-9_-]{43})/?$")


class AdapterError(Exception):
    """An error whose stable code is safe to return and log."""

    def __init__(self, code: str, *, uncertain: bool = False) -> None:
        super().__init__(code)
        self.code = code
        self.uncertain = uncertain


@dataclass(frozen=True, slots=True)
class ServerInfo:
    host: str
    default_ports: str


@dataclass(frozen=True, slots=True)
class PasswordRecord:
    password: str
    label: str
    ports: str
    status: str
    expires_at: int
    server: ServerInfo
    vk_hash: str | None = None


class AdminBoundary(Protocol):
    def list_records(self) -> tuple[PasswordRecord, ...]: ...

    def details(self, password: str) -> PasswordRecord | None: ...

    def create(self, marker: str, expires_at: int, vk_hash: str) -> None: ...

    def set_vk_hash(self, password: str, vk_hash: str) -> None: ...

    def set_expiry(self, password: str, expires_at: int) -> None: ...

    def activate(self, password: str) -> None: ...

    def deactivate(self, password: str) -> None: ...


class WDTTAdminClient:
    """Bounded, fixed-command client for the existing WDTT v15 admin socket."""

    def __init__(self, password: bytearray, socket_path: Path = WDTT_SOCKET) -> None:
        if not password or len(password) > 256 or b"\0" in password or b"\n" in password:
            raise AdapterError("credential_invalid")
        self._password = password
        self._socket_path = socket_path

    def list_records(self) -> tuple[PasswordRecord, ...]:
        body = self._call(["list"])
        values = body.get("passwords", [])
        if body.get("ok") is not True or not isinstance(values, list):
            raise AdapterError("wdtt_operation_failed")
        server = self._server(body.get("server"))
        if len(values) > 4096:
            raise AdapterError("wdtt_response_invalid")
        return tuple(self._record(value, server) for value in values)

    def details(self, password: str) -> PasswordRecord | None:
        self._require_password(password)
        body = self._call(["details", "--password", password])
        if body.get("ok") is False and body.get("code") == "not_found":
            return None
        if body.get("ok") is not True:
            raise AdapterError("wdtt_operation_failed")
        record = self._record(body.get("password"), self._server(body.get("server")))
        if record.password != password:
            raise AdapterError("wdtt_response_invalid")
        return record

    def create(self, marker: str, expires_at: int, vk_hash: str) -> None:
        if _MARKER.fullmatch(marker) is None:
            raise AdapterError("request_invalid")
        self._require_vk_hash(vk_hash)
        self._mutation(
            [
                "create",
                "--label",
                marker,
                "--expires-at",
                str(expires_at),
                "--vk-hash",
                vk_hash,
            ]
        )

    def set_vk_hash(self, password: str, vk_hash: str) -> None:
        self._require_password(password)
        self._require_vk_hash(vk_hash)
        self._mutation(["set-hash", "--password", password, "--vk-hash", vk_hash])

    def set_expiry(self, password: str, expires_at: int) -> None:
        self._require_password(password)
        self._mutation(["set-expiry", "--password", password, "--expires-at", str(expires_at)])

    def activate(self, password: str) -> None:
        self._require_password(password)
        self._mutation(["activate", "--password", password])

    def deactivate(self, password: str) -> None:
        self._require_password(password)
        self._mutation(["deactivate", "--password", password])

    def client_test(self, command: dict[str, Any]) -> dict[str, Any]:
        reply = self._call(["client-test", json.dumps(command, separators=(",", ":"))])
        if reply.get("ok") is not True:
            raise AdapterError(str(reply.get("code", "wdtt_operation_failed")))
        result = reply.get("client_test")
        if not isinstance(result, dict):
            raise AdapterError("wdtt_response_invalid")
        return result

    def _mutation(self, args: list[str]) -> None:
        body = self._call(args)
        if body.get("ok") is not True:
            code = body.get("code")
            stable_code = "capacity_full" if code == "capacity_full" else "wdtt_operation_failed"
            raise AdapterError(stable_code)

    def _call(self, args: list[str]) -> dict[str, Any]:
        password_text: str | None = None
        request = bytearray()
        response = bytearray()
        deadline = time.monotonic() + DEADLINE_SECONDS
        try:
            password_text = bytes(self._password).decode("utf-8", errors="strict")
            request.extend(
                json.dumps(
                    {"main_password": password_text, "args": args},
                    ensure_ascii=True,
                    separators=(",", ":"),
                ).encode("utf-8")
            )
            request.extend(b"\n")
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                self._set_timeout(client, deadline)
                client.connect(str(self._socket_path))
                self._set_timeout(client, deadline)
                client.sendall(request)
                while True:
                    self._set_timeout(client, deadline)
                    chunk = client.recv(min(65536, MAX_ADMIN_RESPONSE_BYTES + 1 - len(response)))
                    if not chunk:
                        break
                    response.extend(chunk)
                    if len(response) > MAX_ADMIN_RESPONSE_BYTES:
                        raise AdapterError("wdtt_response_invalid")
            if not response:
                raise AdapterError("wdtt_response_invalid")
            value = _strict_json(bytes(response))
            if not isinstance(value, dict) or not isinstance(value.get("ok"), bool):
                raise AdapterError("wdtt_response_invalid")
            return value
        except AdapterError:
            raise
        except (OSError, TimeoutError) as exc:
            raise AdapterError("wdtt_unavailable", uncertain=True) from exc
        except (UnicodeDecodeError, UnicodeEncodeError, ValueError) as exc:
            raise AdapterError("wdtt_response_invalid") from exc
        finally:
            password_text = None
            _wipe(request)
            _wipe(response)

    @staticmethod
    def _set_timeout(client: socket.socket, deadline: float) -> None:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise AdapterError("wdtt_unavailable", uncertain=True)
        client.settimeout(remaining)

    @staticmethod
    def _server(value: Any) -> ServerInfo:
        if not isinstance(value, dict):
            raise AdapterError("wdtt_response_invalid")
        host = value.get("effective_public_ip") or value.get("public_ip")
        ports = value.get("default_ports")
        if not isinstance(host, str) or not isinstance(ports, str):
            raise AdapterError("wdtt_response_invalid")
        return ServerInfo(host=host, default_ports=ports)

    @classmethod
    def _record(cls, value: Any, server: ServerInfo) -> PasswordRecord:
        if not isinstance(value, dict):
            raise AdapterError("wdtt_response_invalid")
        password = value.get("password")
        label = value.get("label", "")
        ports = value.get("ports") or server.default_ports
        status = value.get("status")
        expires_at = value.get("expires_at", 0)
        raw_vk_hash = value.get("vk_hash")
        vk_hash = None if raw_vk_hash is None or raw_vk_hash == "" else raw_vk_hash
        if (
            not isinstance(password, str)
            or _PASSWORD.fullmatch(password) is None
            or not isinstance(label, str)
            or len(label) > 256
            or not isinstance(ports, str)
            or _PORTS.fullmatch(ports) is None
            or status not in {"active", "deactivated", "expired", "expired_retained", "broken"}
            or isinstance(expires_at, bool)
            or not isinstance(expires_at, int)
            or expires_at < 0
            or (
                vk_hash is not None
                and (not isinstance(vk_hash, str) or not _is_canonical_vk_hashes(vk_hash))
            )
        ):
            raise AdapterError("wdtt_response_invalid")
        return PasswordRecord(password, label, ports, status, expires_at, server, vk_hash)

    @staticmethod
    def _require_password(value: str) -> None:
        if _PASSWORD.fullmatch(value) is None:
            raise AdapterError("request_invalid")

    @staticmethod
    def _require_vk_hash(value: str) -> None:
        try:
            normalized = _normalize_vk_hashes(value)
        except (TypeError, ValueError) as exc:
            raise AdapterError("request_invalid") from exc
        if normalized != value:
            raise AdapterError("request_invalid")


class DirectAdapter:
    """Idempotent adapter domain logic; all mutations are serialized."""

    def __init__(self, admin: AdminBoundary, vk_hashes: str, *, state_dir: Path | None = None,
                 v3_enabled: bool = False) -> None:
        try:
            normalized = _normalize_vk_hashes(vk_hashes)
        except (TypeError, ValueError) as exc:
            raise AdapterError("credential_invalid") from exc
        if normalized != vk_hashes:
            raise AdapterError("credential_invalid")
        self._admin = admin
        self._vk_hashes = normalized
        self._mutation_lock = Lock()
        self._journal = TargetJournal(state_dir) if state_dir is not None else None
        self._v3_enabled = v3_enabled

    def execute(self, request: dict[str, Any]) -> dict[str, Any]:
        if type(request) is not dict:
            raise AdapterError("request_invalid")
        if type(request.get("version")) is int and request["version"] == 3:
            with self._mutation_lock:
                if not self._v3_enabled or self._journal is None:
                    raise AdapterError("v3_disabled")
                return self._execute_v3(request)
        if request.get("version") == 2:
            if (
                set(request) != {"version", "operation", "command"}
                or request["operation"] != "client_test"
            ):
                raise AdapterError("request_invalid")
            command = request["command"]
            if not isinstance(command, dict) or command.get("operation") not in {
                "bootstrap_provision",
                "bootstrap_revoke",
                "grant_provision",
                "grant_get",
                "refresh_lease",
                "grant_revoke",
            }:
                raise AdapterError("operation_rejected")
            if not isinstance(self._admin, WDTTAdminClient):
                raise AdapterError("configuration_invalid")
            with self._mutation_lock:
                result = self._admin.client_test(command)
            return {"version": 2, "status": "ok", "result": result}
        operation, subscription_id, expires_at = _validate_request(request)
        grant_id = uuid.uuid5(GRANT_NAMESPACE, subscription_id)
        marker = f"tlm:{grant_id}"
        with self._mutation_lock:
            if operation != "get" and self._journal is not None and self._journal.target(subscription_id):
                raise AdapterError("target_claimed")
            if operation == "ensure":
                record, created = self._ensure(marker, expires_at)
            elif operation == "disable":
                record = self._disable(marker)
                created = False
            else:
                record = self._existing(marker)
                if record.vk_hash != self._vk_hashes:
                    raise AdapterError("wdtt_readback_failed")
                created = False
        return _success(grant_id, record, created=created, include_artifact=operation != "disable")

    def close(self) -> None:
        if self._journal is not None:
            self._journal.close()

    def _snapshot(self, marker: str) -> dict[str, Any]:
        row = self._find(marker)
        if row is None:
            return {"state":"absent", "expires_at":None}
        row = self._confirmed(row, marker)
        if row.vk_hash != self._vk_hashes or row.status == "broken":
            raise AdapterError("target_readback_conflict")
        return {"state":"active" if row.status == "active" else "inactive",
                "expires_at":row.expires_at}

    def _execute_v3(self, request: dict[str, Any]) -> dict[str, Any]:
        _validate_v3(request)
        journal = self._journal
        key = request['external_key']; marker = 'tlm:'+request['grant_id']
        target = journal.target(key)
        if request['operation'] == 'read':
            if target is None or target['fence_id'] != request['fence_id']:
                raise AdapterError('target_not_claimed')
            return self._v3_view(target, journal.operation(target['last_operation_id']), marker)
        digest = hashlib.sha256(json.dumps(request,sort_keys=True,separators=(',',':')).encode()).hexdigest()
        prior = journal.operation(request['operation_id'])
        if prior is not None:
            if prior['digest'] != digest:
                raise AdapterError('operation_conflict')
            if prior['phase'] != 'applied':
                self._recover_v3(prior, marker)
            return self._v3_view(journal.target(key),journal.operation(prior['operation_id']),marker)
        if request['operation'] == 'claim':
            if target is not None:
                raise AdapterError('target_claimed')
            base = self._snapshot(marker)
            expected = {'state':request['expected_state'],'expires_at':_v3_expiry(request['expected_expires_at'])}
            if base != expected:
                raise AdapterError('base_mismatch')
            journal.claim(request,digest,base)
        else:
            if target is None or target['fence_id'] != request['fence_id']:
                raise AdapterError('target_not_claimed')
            if journal.pending(key):
                raise AdapterError('operation_pending')
            if request['expected_revision'] != target['accepted_revision'] or request['revision'] <= target['accepted_revision']:
                raise AdapterError('revision_conflict')
            base = self._snapshot(marker)
            if not _snapshot_matches(base,target['snapshot']):
                raise AdapterError('target_readback_conflict')
            expiry = _v3_expiry(request['expires_at'])
            if request['desired_state'] == 'active' and expiry <= int(time.time()):
                raise AdapterError('expiry_elapsed')
            desired = {'state':request['desired_state'],'expires_at':expiry if expiry is not None else base['expires_at']}
            journal.intent(request,digest,base,desired)
            self._recover_v3(journal.operation(request['operation_id']),marker)
        return self._v3_view(journal.target(key),journal.operation(request['operation_id']),marker)

    def _recover_v3(self, op: dict[str, Any], marker: str) -> None:
        journal = self._journal
        try:
            current = self._snapshot(marker)
            # An unacknowledged admin mutation may still complete after a read. Never
            # accept a successor on that ambiguous basis; retain the same intent.
            if op['step'] == 'issued':
                journal.pending_result(op,'admin_completion_unknown'); return
            if current == op['desired']:
                journal.complete(op,current); return
            base, desired = op['base'],op['desired']
            allowed_states = {base['state'],desired['state']}
            allowed_expiries = {base['expires_at'],desired['expires_at']}
            if (not _snapshot_matches(current,base) and
                    (current['state'] not in allowed_states or current['expires_at'] not in allowed_expiries)):
                journal.pending_result(op,'target_readback_conflict',conflict=True); return
            if desired['state']=='active' and desired['expires_at'] <= int(time.time()):
                journal.pending_result(op,'expiry_elapsed',conflict=True); return
            if current['state']=='absent' and desired['state']=='inactive':
                journal.pending_result(op,'absent_inactive_unsupported',conflict=True); return
            def mutation(call):
                journal.step(op,'issued')
                # Only normal completion acknowledges the issued command.
                # Error classification alone does not prove admin completion.
                call()
                journal.step(op,'acked')
            if current['state']=='absent':
                mutation(lambda:self._admin.create(marker,desired['expires_at'],self._vk_hashes))
            else:
                row = self._existing(marker)
                if row.expires_at != desired['expires_at']:
                    mutation(lambda:self._admin.set_expiry(row.password,desired['expires_at']))
                row = self._existing(marker)
                if desired['state']=='active' and row.status != 'active':
                    mutation(lambda:self._admin.activate(row.password))
                elif desired['state']=='inactive' and row.status=='active':
                    mutation(lambda:self._admin.deactivate(row.password))
            current = self._snapshot(marker)
            if current != desired:
                journal.pending_result(op,'target_readback_conflict',conflict=True); return
            journal.complete(op,current)
        except AdapterError as error:
            journal.pending_result(op,error.code)

    def _v3_view(self, target, op, marker):
        pending = self._journal.pending(target['external_key'])
        try:
            current = self._snapshot(marker)
            current_ok = _snapshot_matches(current,target['snapshot'])
            current_revision = target['applied_revision'] if current_ok else None
            artifact = ''
            if current_ok and current['state']=='active':
                row = self._existing(marker)
                # Recheck the exact record used to emit the protected artifact.
                if row.status=='active' and row.expires_at==current['expires_at']:
                    artifact = _artifact(row)
                else:
                    raise AdapterError('target_readback_conflict')
            known_pending = pending and current['state'] in {pending['base']['state'],pending['desired']['state']} and current['expires_at'] in {pending['base']['expires_at'],pending['desired']['expires_at']}
            readback_code = None if (current_ok or known_pending) else 'target_readback_conflict'
        except AdapterError as error:
            current = None;current_revision = None;artifact='';readback_code=error.code
        pending = self._journal.pending(target['external_key'])
        state = 'conflict' if (readback_code=='target_readback_conflict' or (pending and pending['phase']=='conflict')) else 'pending' if (pending or readback_code) else 'applied'
        return {'version':3,'status':'ok','external_key':target['external_key'],'grant_id':target['grant_id'],
                'fence_id':target['fence_id'],'accepted_revision':target['accepted_revision'],
                'current_revision':current_revision,'current':current,'delivery_state':state,
                'code':readback_code or (pending['code'] if pending else None),
                'receipt':op['result'] if op and op['phase']=='applied' else None,
                'operation_id':op['operation_id'] if op else None,'body_digest':op['digest'] if op else None,
                'desired':op['desired'] if op else None,'artifact':artifact}

    def _ensure(self, marker: str, expires_at: int | None) -> tuple[PasswordRecord, bool]:
        if expires_at is None:
            raise AdapterError("request_invalid")
        record = self._find(marker)
        created = False
        if record is None:
            try:
                self._admin.create(marker, expires_at, self._vk_hashes)
            except AdapterError as exc:
                if not exc.uncertain:
                    raise
                record = self._find(marker)
                if record is None:
                    raise
            else:
                record = self._find(marker)
            if record is None:
                raise AdapterError("wdtt_readback_failed")
            created = True
        record = self._confirmed(record, marker)
        if created:
            if record.vk_hash != self._vk_hashes:
                raise AdapterError("wdtt_readback_failed")
        elif record.vk_hash != self._vk_hashes:
            self._admin.set_vk_hash(record.password, self._vk_hashes)
            record = self._readback(record.password, marker)
            if record.vk_hash != self._vk_hashes:
                raise AdapterError("wdtt_readback_failed")
        if record.expires_at != expires_at:
            self._admin.set_expiry(record.password, expires_at)
            record = self._readback(record.password, marker)
        if record.status != "active":
            self._admin.activate(record.password)
            record = self._readback(record.password, marker)
        if (
            record.expires_at != expires_at
            or record.status != "active"
            or record.vk_hash != self._vk_hashes
        ):
            raise AdapterError("wdtt_readback_failed")
        return record, created

    def _disable(self, marker: str) -> PasswordRecord:
        record = self._existing(marker)
        if record.status == "active":
            self._admin.deactivate(record.password)
            record = self._readback(record.password, marker)
        if record.status == "active":
            raise AdapterError("wdtt_readback_failed")
        return record

    def _existing(self, marker: str) -> PasswordRecord:
        record = self._find(marker)
        if record is None:
            raise AdapterError("not_found")
        return self._confirmed(record, marker)

    def _find(self, marker: str) -> PasswordRecord | None:
        matches = [record for record in self._admin.list_records() if record.label == marker]
        if len(matches) > 1:
            raise AdapterError("marker_ambiguous")
        return matches[0] if matches else None

    def _confirmed(self, record: PasswordRecord, marker: str) -> PasswordRecord:
        return self._readback(record.password, marker)

    def _readback(self, password: str, marker: str) -> PasswordRecord:
        record = self._admin.details(password)
        if record is None or record.label != marker:
            raise AdapterError("wdtt_readback_failed")
        return record


class TargetJournal:
    """Protected single-process SQLite delivery journal; never stores passwords/artifacts."""
    def __init__(self, directory: Path) -> None:
        self._db = None; self._lock_fd = None
        try:
            directory.mkdir(parents=True,mode=0o700,exist_ok=True)
            metadata = directory.lstat()
            if not stat.S_ISDIR(metadata.st_mode) or metadata.st_uid != os.geteuid() or metadata.st_mode & 0o077:
                raise AdapterError('journal_unavailable')
            self._lock_fd = os.open(directory/'journal.lock',os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW,0o600)
            fcntl.flock(self._lock_fd,fcntl.LOCK_EX|fcntl.LOCK_NB)
            path = directory/'delivery.sqlite3'
            fd = os.open(path,os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW,0o600)
            try:
                info = os.fstat(fd)
                if not stat.S_ISREG(info.st_mode) or info.st_uid!=os.geteuid() or info.st_mode&0o077 or info.st_nlink!=1:
                    raise AdapterError('journal_unavailable')
            finally:os.close(fd)
            self._db = sqlite3.connect(path,check_same_thread=False)
            self._db.execute('PRAGMA synchronous=FULL')
            if self._db.execute('PRAGMA quick_check').fetchone()[0]!='ok':
                raise AdapterError('journal_unavailable')
            with self._db:
                self._db.execute('CREATE TABLE IF NOT EXISTS targets (key TEXT PRIMARY KEY,doc TEXT NOT NULL)')
                self._db.execute('CREATE TABLE IF NOT EXISTS operations (id TEXT PRIMARY KEY,target TEXT NOT NULL,phase TEXT NOT NULL,doc TEXT NOT NULL)')
                self._db.execute("CREATE UNIQUE INDEX IF NOT EXISTS pending_target ON operations(target) WHERE phase!='applied'")
        except (OSError,sqlite3.Error,AdapterError) as error:
            self.close();raise AdapterError('journal_unavailable') from error

    def close(self):
        if self._db is not None:self._db.close();self._db=None
        if self._lock_fd is not None:os.close(self._lock_fd);self._lock_fd=None

    def _read(self,query,args):
        try:
            row = self._db.execute(query,args).fetchone()
            return json.loads(row[0]) if row else None
        except (sqlite3.Error,ValueError,AttributeError) as error:
            raise AdapterError('journal_unavailable') from error

    def target(self,key):return self._read('SELECT doc FROM targets WHERE key=?',(key,))
    def operation(self,key):return self._read('SELECT doc FROM operations WHERE id=?',(key,))
    def pending(self,key):return self._read("SELECT doc FROM operations WHERE target=? AND phase!='applied'",(key,))

    def _save(self,op,target=None):
        try:
            with self._db:
                if target is not None:
                    self._db.execute('INSERT INTO targets VALUES(?,?) ON CONFLICT(key) DO UPDATE SET doc=excluded.doc',
                                     (target['external_key'],json.dumps(target,sort_keys=True)))
                self._db.execute('INSERT INTO operations VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET phase=excluded.phase,doc=excluded.doc',
                                 (op['operation_id'],op['external_key'],op['phase'],json.dumps(op,sort_keys=True)))
        except (sqlite3.Error,AttributeError) as error:
            raise AdapterError('journal_unavailable') from error

    @staticmethod
    def _op(request,digest,base,desired):
        return {'operation_id':request['operation_id'],'external_key':request['external_key'],
                'grant_id':request['grant_id'],'fence_id':request['fence_id'],'revision':request['revision'],
                'digest':digest,'base':base,'desired':desired,'phase':'pending','step':'none','result':None,'code':None}

    def claim(self,request,digest,base):
        target = {'external_key':request['external_key'],'grant_id':request['grant_id'],'fence_id':request['fence_id'],
                  'accepted_revision':request['revision'],'applied_revision':request['revision'],'snapshot':base,'last_operation_id':request['operation_id']}
        op=self._op(request,digest,base,base);op['phase']='applied';op['result']=self._receipt(op,base)
        self._save(op,target)

    def intent(self,request,digest,base,desired):
        target=self.target(request['external_key']);target['accepted_revision']=request['revision'];target['last_operation_id']=request['operation_id']
        self._save(self._op(request,digest,base,desired),target)

    def step(self,op,step):op['step']=step;self._save(op)
    def pending_result(self,op,code,*,conflict=False):
        op['phase']='conflict' if conflict else 'pending';op['code']=code;self._save(op)
    def complete(self,op,current):
        target=self.target(op['external_key']);target['applied_revision']=op['revision'];target['snapshot']=current
        op['phase']='applied';op['code']=None;op['result']=self._receipt(op,current);self._save(op,target)
    @staticmethod
    def _receipt(op,current):
        return {k:op[k] for k in ('operation_id','digest','external_key','grant_id','fence_id','revision','desired')}|{'applied':current}


def _snapshot_matches(current,expected):
    if current==expected:return True
    # Natural expiry of an already confirmed active revision is not reactivation.
    return (expected['state']=='active' and current['state']=='inactive' and
            current['expires_at']==expected['expires_at'] and current['expires_at']<=int(time.time()))


def _v3_expiry(value):
    if value is None:return None
    if not isinstance(value,str) or len(value)>40:
        raise AdapterError('request_invalid')
    try:
        parsed=datetime.fromisoformat(value.replace('Z','+00:00'));epoch=int(parsed.timestamp())
        if parsed.tzinfo is None or parsed.utcoffset()!=UTC.utcoffset(parsed) or parsed.microsecond or not 0<=epoch<=2**63-1:
            raise ValueError()
    except (ValueError,OverflowError,OSError) as error:
        raise AdapterError('request_invalid') from error
    return epoch


def _validate_v3(value):
    common={'version','operation','external_key','grant_id','fence_id'}
    op=value.get('operation')
    extra={'operation_id','revision','expected_state','expected_expires_at'} if op=='claim' else {'operation_id','revision','expected_revision','desired_state','expires_at'} if op=='apply' else set()
    if op not in ('claim','apply','read') or set(value)!=common|extra:
        raise AdapterError('request_invalid')
    if not isinstance(value['external_key'],str) or not _SUBSCRIPTION_ID.fullmatch(value['external_key']):
        raise AdapterError('request_invalid')
    for key in ('grant_id','fence_id')+(() if op=='read' else ('operation_id',)):
        try:
            if type(value[key]) is not str or str(uuid.UUID(value[key]))!=value[key]:raise ValueError()
        except (ValueError,AttributeError) as error:raise AdapterError('request_invalid') from error
    if value['grant_id']!=str(uuid.uuid5(GRANT_NAMESPACE,value['external_key'])):
        raise AdapterError('target_identity_conflict')
    if op=='read':return
    if type(value['revision']) is not int or not 0<=value['revision']<2**63:
        raise AdapterError('request_invalid')
    if op=='claim':
        expiry=_v3_expiry(value['expected_expires_at'])
        if value['expected_state'] not in ('absent','active','inactive') or (value['expected_state']=='absent')!=(expiry is None):
            raise AdapterError('request_invalid')
    else:
        expiry=_v3_expiry(value['expires_at'])
        if (type(value['expected_revision']) is not int or not 0<=value['expected_revision']<2**63 or
                value['desired_state'] not in ('active','inactive') or (value['desired_state']=='active' and expiry is None)):
            raise AdapterError('request_invalid')


def _validate_request(value: dict[str, Any]) -> tuple[str, str, int | None]:
    if not isinstance(value, dict):
        raise AdapterError("request_invalid")
    operation = value.get("operation")
    expected = {"version", "operation", "subscription_id"}
    if operation == "ensure":
        expected.add("expires_at")
    if set(value) != expected or value.get("version") != 1:
        raise AdapterError("request_invalid")
    subscription_id = value.get("subscription_id")
    if not isinstance(subscription_id, str) or _SUBSCRIPTION_ID.fullmatch(subscription_id) is None:
        raise AdapterError("request_invalid")
    if operation not in {"ensure", "get", "disable"}:
        raise AdapterError("operation_rejected")
    expires_at = _parse_expiry(value.get("expires_at")) if operation == "ensure" else None
    return operation, subscription_id, expires_at


def _parse_expiry(value: Any) -> int:
    if not isinstance(value, str) or len(value) > 40:
        raise AdapterError("request_invalid")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise AdapterError("request_invalid") from exc
    if parsed.tzinfo is None or parsed.utcoffset() != UTC.utcoffset(parsed):
        raise AdapterError("request_invalid")
    epoch = int(parsed.timestamp())
    if epoch <= int(datetime.now(UTC).timestamp()) or epoch > 2**63 - 1:
        raise AdapterError("request_invalid")
    return epoch


def _success(
    grant_id: uuid.UUID,
    record: PasswordRecord,
    *,
    created: bool,
    include_artifact: bool,
) -> dict[str, Any]:
    return {
        "version": 1,
        "status": "ok",
        "grant_id": str(grant_id),
        "state": record.status,
        "expires_at": (
            datetime.fromtimestamp(record.expires_at, UTC).isoformat().replace("+00:00", "Z")
        ),
        "created": created,
        "artifact": _artifact(record) if include_artifact else "",
    }


def _artifact(record: PasswordRecord) -> str:
    host = _validated_host(record.server.host)
    if _PASSWORD.fullmatch(record.password) is None:
        raise AdapterError("artifact_invalid")
    ports = record.ports or record.server.default_ports
    if _PORTS.fullmatch(ports) is None:
        raise AdapterError("artifact_invalid")
    values = tuple(int(value) for value in ports.split(","))
    if any(value < 1 or value > 65535 for value in values):
        raise AdapterError("artifact_invalid")
    if not isinstance(record.vk_hash, str) or not _is_canonical_vk_hashes(record.vk_hash):
        raise AdapterError("artifact_invalid")
    hash_count = len(record.vk_hash.split(","))
    query = urlencode(
        (
            ("v", "1"),
            ("host", host),
            ("dtls", str(values[0])),
            ("wg", str(values[1])),
            ("local", str(values[2])),
            ("password", record.password),
            ("hashes", record.vk_hash),
            ("max_workers", str(_WORKERS_PER_HASH * hash_count)),
        )
    )
    return f"wdtt://connect?{query}"


def _validated_host(value: str) -> str:
    try:
        parsed = ipaddress.ip_address(value)
    except ValueError as exc:
        raise AdapterError("artifact_invalid") from exc
    if parsed.version != 4:
        raise AdapterError("artifact_invalid")
    return str(parsed)


def _strict_json(source: bytes) -> Any:
    def unique_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
        result: dict[str, Any] = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate field")
            result[key] = value
        return result

    return json.loads(
        source.decode("utf-8", errors="strict"),
        object_pairs_hook=unique_pairs,
        parse_constant=lambda _value: (_ for _ in ()).throw(ValueError("invalid constant")),
    )


def _load_credential(name: str, *, maximum: int = 256) -> bytearray:
    if name not in {CREDENTIAL_NAME, VK_HASH_CREDENTIAL_NAME}:
        raise AdapterError("credential_unavailable")
    directory = os.environ.get("CREDENTIALS_DIRECTORY", "")
    if not directory:
        raise AdapterError("credential_unavailable")
    path = Path(directory) / name
    metadata = path.stat(follow_symlinks=False)
    if (
        not stat.S_ISREG(metadata.st_mode)
        or metadata.st_uid != 0
        or metadata.st_nlink != 1
        or metadata.st_mode & 0o077
        or not 1 <= metadata.st_size <= maximum
    ):
        raise AdapterError("credential_unavailable")
    value = bytearray(metadata.st_size)
    with path.open("rb", buffering=0) as handle:
        if handle.readinto(value) != metadata.st_size or handle.read(1):
            _wipe(value)
            raise AdapterError("credential_unavailable")
    if value.endswith(b"\n"):
        value.pop()
    if not value or b"\0" in value or b"\n" in value:
        _wipe(value)
        raise AdapterError("credential_unavailable")
    return value


def _normalize_vk_hashes(value: str) -> str:
    if not isinstance(value, str):
        raise TypeError("hashes must be text")
    parts = [part.strip() for part in value.split(",")]
    if not _VK_HASH_COUNT_MIN <= len(parts) <= _VK_HASH_COUNT_MAX:
        raise ValueError("hash count invalid")
    normalized = [_normalize_vk_hash_item(part) for part in parts]
    if len(set(normalized)) != len(normalized):
        raise ValueError("hash duplicate")
    return ",".join(normalized)


def _normalize_vk_hash_item(value: str) -> str:
    if _VK_HASH.fullmatch(value) is not None:
        return value
    if not value or len(value) > 1024 or any(character.isspace() for character in value):
        raise ValueError("hash invalid")
    try:
        parsed = urlsplit(value)
    except ValueError as exc:
        raise ValueError("hash invalid") from exc
    matched = _VK_JOIN_PATH.fullmatch(parsed.path)
    if parsed.scheme != "https" or parsed.netloc not in _VK_JOIN_HOSTS or matched is None:
        raise ValueError("hash invalid")
    return matched.group(1)


def _is_canonical_vk_hashes(value: str) -> bool:
    try:
        return _normalize_vk_hashes(value) == value
    except (TypeError, ValueError):
        return False


def _load_vk_hashes() -> str:
    value = _load_credential(VK_HASH_CREDENTIAL_NAME, maximum=8192)
    try:
        try:
            normalized = bytes(value).decode("ascii", errors="strict")
        except UnicodeDecodeError as exc:
            raise AdapterError("credential_invalid") from exc
        try:
            normalized = _normalize_vk_hashes(normalized)
        except (TypeError, ValueError) as exc:
            raise AdapterError("credential_invalid") from exc
        if not normalized:
            raise AdapterError("credential_invalid")
        return normalized
    finally:
        _wipe(value)


def _wipe(value: bytearray) -> None:
    for index in range(len(value)):
        value[index] = 0


def _peer_identity(connection: socket.socket) -> tuple[int, int]:
    if not hasattr(socket, "SO_PEERCRED"):
        raise AdapterError("peer_rejected")
    raw = connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, struct.calcsize("3i"))
    _pid, uid, gid = struct.unpack("3i", raw)
    return uid, gid


class AdapterServer:
    def __init__(self, adapter: DirectAdapter, expected_uid: int, expected_gid: int) -> None:
        self._adapter = adapter
        self._expected_identity = (expected_uid, expected_gid)
        self._socket_gid = expected_gid
        self._stop = False

    def stop(self, _signum: int, _frame: Any) -> None:
        self._stop = True

    def serve(self) -> None:
        ADAPTER_SOCKET.parent.mkdir(mode=0o750, parents=True, exist_ok=True)
        os.chown(ADAPTER_SOCKET.parent, 0, self._socket_gid)
        os.chmod(ADAPTER_SOCKET.parent, 0o710)
        if ADAPTER_SOCKET.exists() or ADAPTER_SOCKET.is_symlink():
            if not stat.S_ISSOCK(ADAPTER_SOCKET.lstat().st_mode):
                raise AdapterError("socket_path_unsafe")
            ADAPTER_SOCKET.unlink()
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
            listener.bind(str(ADAPTER_SOCKET))
            os.chown(ADAPTER_SOCKET, 0, self._socket_gid)
            os.chmod(ADAPTER_SOCKET, 0o660)
            listener.listen(8)
            listener.settimeout(0.5)
            while not self._stop:
                try:
                    connection, _address = listener.accept()
                except TimeoutError:
                    continue
                with connection:
                    self._handle(connection)
        ADAPTER_SOCKET.unlink(missing_ok=True)

    def _handle(self, connection: socket.socket) -> None:
        response: dict[str, Any]
        request = None
        try:
            if _peer_identity(connection) != self._expected_identity:
                raise AdapterError("peer_rejected")
            connection.settimeout(DEADLINE_SECONDS)
            payload = bytearray()
            while b"\n" not in payload:
                chunk = connection.recv(MAX_MESSAGE_BYTES + 1 - len(payload))
                if not chunk:
                    raise AdapterError("request_invalid")
                payload.extend(chunk)
                if len(payload) > MAX_MESSAGE_BYTES:
                    raise AdapterError("request_too_large")
            line, separator, remainder = payload.partition(b"\n")
            if not separator or remainder:
                raise AdapterError("request_invalid")
            request = _strict_json(bytes(line))
            if not isinstance(request, dict):
                raise AdapterError("request_invalid")
            response = self._adapter.execute(request)
        except (AdapterError, OSError, TimeoutError, UnicodeDecodeError, ValueError) as exc:
            code = exc.code if isinstance(exc, AdapterError) else "request_invalid"
            response = {"version": 3 if isinstance(request,dict) and type(request.get("version")) is int and request["version"]==3 else 1, "status": "error", "code": code}
        encoded = json.dumps(response, separators=(",", ":"), ensure_ascii=True).encode() + b"\n"
        if len(encoded) <= MAX_MESSAGE_BYTES:
            with suppress(OSError):
                connection.sendall(encoded)


def _required_identity(name: str) -> int:
    value = os.environ.get(name, "")
    if not value.isdigit():
        raise AdapterError("configuration_invalid")
    normalized = int(value)
    if normalized < 1 or normalized > 2**31 - 1:
        raise AdapterError("configuration_invalid")
    return normalized


def main() -> int:
    if len(sys.argv) != 1:
        return 2
    credential = bytearray()
    adapter = None
    vk_hashes = ""
    try:
        credential = _load_credential(CREDENTIAL_NAME)
        vk_hashes = _load_vk_hashes()
        adapter = DirectAdapter(WDTTAdminClient(credential), vk_hashes,
            state_dir=Path(os.environ.get("WDTT_ADAPTER_STATE_DIR", "/var/lib/terlimo-wdtt-adapter")),
            v3_enabled=os.environ.get("WDTT_ADAPTER_V3_ENABLED", "false") == "true")
        server = AdapterServer(
            adapter,
            _required_identity("WDTT_ADAPTER_EXPECTED_UID"),
            _required_identity("WDTT_ADAPTER_EXPECTED_GID"),
        )
        signal.signal(signal.SIGTERM, server.stop)
        signal.signal(signal.SIGINT, server.stop)
        server.serve()
        return 0
    except (AdapterError, OSError):
        return 1
    finally:
        if adapter is not None:
            adapter.close()
        vk_hashes = ""
        _wipe(credential)


if __name__ == "__main__":
    raise SystemExit(main())
