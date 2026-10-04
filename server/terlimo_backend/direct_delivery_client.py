"""Protected Unix v3 transport only; caller owns persisted intent and delivery policy."""
from __future__ import annotations

import asyncio
import hashlib
import json
import re
import uuid
from dataclasses import dataclass, field
from datetime import UTC, datetime
from pathlib import Path
from typing import Literal

LIMIT = 16 * 1024
DEADLINE = 5.0
ENDPOINT = Path('/run/terlimo-wdtt-adapter/adapter.sock')
NAMESPACE = uuid.UUID('97ab0503-5926-51b2-9a62-741f65f11845')

class DeliveryError(Exception):
    def __init__(self, code: str, *, unresolved: bool = True):
        self.code = code
        self.unresolved = unresolved
        super().__init__(code)


def _invalid():
    raise DeliveryError('direct_wire_invalid')


def _integer(value):
    if type(value) is not int or not 0 <= value < 2**63:
        _invalid()
    return value


def _uuid(value):
    valid = False
    if type(value) is str:
        try:
            valid = str(uuid.UUID(value)) == value
        except (ValueError, AttributeError):
            pass
    if not valid:
        _invalid()
    return value


def _keys(value, keys):
    if type(value) is not dict or set(value) != set(keys.split()):
        _invalid()


def _json(body):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                _invalid()
            result[key] = value
        return result
    try:
        result = json.loads(body.decode('utf-8'), object_pairs_hook=pairs,
                            parse_constant=lambda _: _invalid())
    except (ValueError, UnicodeError, RecursionError):
        result = None
    if result is None:
        _invalid()
    return result


def _epoch(value):
    if type(value) is not str or len(value) > 40:
        _invalid()
    epoch = None
    try:
        parsed = datetime.fromisoformat(value.replace('Z', '+00:00'))
        if parsed.tzinfo is not None and parsed.utcoffset() == UTC.utcoffset(parsed) and not parsed.microsecond:
            epoch = int(parsed.timestamp())
    except (ValueError, OverflowError, OSError):
        pass
    return _integer(epoch)


@dataclass(frozen=True)
class Target:
    external_key: str
    grant_id: str
    fence_id: str

    def __post_init__(self):
        if type(self.external_key) is not str or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9:._-]{0,127}', self.external_key):
            _invalid()
        _uuid(self.grant_id)
        _uuid(self.fence_id)
        if self.grant_id != str(uuid.uuid5(NAMESPACE, self.external_key)):
            _invalid()


@dataclass(frozen=True)
class Snapshot:
    state: Literal['absent', 'active', 'inactive']
    expires_at: int | None

    def __post_init__(self):
        if self.state not in ('absent', 'active', 'inactive'):
            _invalid()
        if (self.state == 'absent' and self.expires_at is not None) or (self.state == 'active' and self.expires_at is None):
            _invalid()
        if self.expires_at is not None:
            _integer(self.expires_at)

    @classmethod
    def parse(cls, value):
        _keys(value, 'state expires_at')
        return cls(value['state'], value['expires_at'])


@dataclass(frozen=True)
class Request:
    """Exact caller bytes; construct only from an already persisted wire body."""
    body: bytes = field(repr=False)
    base: Snapshot | None = None
    target: Target = field(init=False)
    operation: Literal['claim', 'apply', 'read'] = field(init=False)
    operation_id: str | None = field(init=False)
    revision: int | None = field(init=False)
    desired: Snapshot | None = field(init=False)
    digest: str = field(init=False)

    def __post_init__(self):
        if self.base is not None and not isinstance(self.base, Snapshot):
            _invalid()
        if type(self.body) is not bytes or len(self.body) + 1 > LIMIT or b'\n' in self.body:
            _invalid()
        v = _json(self.body)
        if type(v) is not dict or type(v.get('version')) is not int or v['version'] != 3:
            _invalid()
        op = v.get('operation')
        common = 'version operation external_key grant_id fence_id '
        extras = {'claim': 'operation_id revision expected_state expected_expires_at',
                  'apply': 'operation_id revision expected_revision desired_state expires_at', 'read': ''}
        if type(op) is not str or op not in extras:
            _invalid()
        _keys(v, common + extras[op])
        target = Target(v['external_key'], v['grant_id'], v['fence_id'])
        desired = None
        if op != 'read':
            _uuid(v['operation_id']); _integer(v['revision'])
            if op == 'claim':
                desired = Snapshot(v['expected_state'], None if v['expected_expires_at'] is None else _epoch(v['expected_expires_at']))
                if (desired.state == 'absent') != (desired.expires_at is None):
                    _invalid()
            else:
                _integer(v['expected_revision'])
                if v['desired_state'] not in ('active', 'inactive'):
                    _invalid()
                expiry = _epoch(v['expires_at']) if v['expires_at'] is not None else None
                if expiry is None:
                    if v['desired_state'] != 'inactive' or not isinstance(self.base, Snapshot):
                        _invalid()
                    expiry = self.base.expires_at
                desired = Snapshot(v['desired_state'], expiry)
        for key, value in dict(target=target, operation=op, operation_id=v.get('operation_id'),
                               revision=v.get('revision'), desired=desired,
                               digest=hashlib.sha256(json.dumps(v, sort_keys=True, separators=(',', ':')).encode()).hexdigest()).items():
            object.__setattr__(self, key, value)


@dataclass(frozen=True)
class Receipt:
    operation_id: str
    digest: str
    target: Target
    revision: int
    desired: Snapshot
    applied: Snapshot


@dataclass(frozen=True)
class Response:
    target: Target
    operation_id: str
    body_digest: str
    desired: Snapshot
    accepted_revision: int
    current_revision: int | None
    current: Snapshot | None
    delivery_state: Literal['pending', 'applied', 'conflict']
    code: str | None
    receipt: Receipt | None
    _artifact: str = field(repr=False, compare=False)

    def artifact_for_projection(self) -> str:
        """Explicit secret access. Never log or persist the returned value."""
        return self._artifact

    def historical_fulfilled(self, intent: Request) -> bool:
        return intent.operation == 'apply' and self.receipt is not None and self.receipt.operation_id == intent.operation_id and self.receipt.digest == intent.digest

    def current_usable(self, intent: Request, *, now: int) -> bool:
        _integer(now)
        return (self.historical_fulfilled(intent) and self.delivery_state == 'applied' and
                self.code is None and self.current_revision == intent.revision and
                self.accepted_revision == intent.revision and self.current == intent.desired and
                self.current.state == 'active' and self.current.expires_at > now and bool(self._artifact))


def parse_response(body: bytes, expected: Request) -> Response:
    if len(body) + 1 > LIMIT:
        _invalid()
    v = _json(body)
    if type(v) is not dict:
        _invalid()
    if v.get('status') == 'error':
        _keys(v, 'version status code')
        if type(v['version']) is not int or v['version'] not in (1, 3) or type(v['code']) is not str or not re.fullmatch('[a-z_]{1,80}', v['code']):
            _invalid()
        # No exception echoes peer-controlled values or artifacts.
        raise DeliveryError('direct_peer_error')
    _keys(v, 'version status external_key grant_id fence_id accepted_revision current_revision current delivery_state code receipt operation_id body_digest desired artifact')
    if type(v['version']) is not int or v['version'] != 3 or v['status'] != 'ok' or expected.operation == 'read':
        _invalid()
    target = Target(v['external_key'], v['grant_id'], v['fence_id'])
    desired = Snapshot.parse(v['desired'])
    if target != expected.target or v['operation_id'] != expected.operation_id or v['body_digest'] != expected.digest or desired != expected.desired:
        _invalid()
    _uuid(v['operation_id'])
    accepted = _integer(v['accepted_revision'])
    current_revision = None if v['current_revision'] is None else _integer(v['current_revision'])
    current = None if v['current'] is None else Snapshot.parse(v['current'])
    if current is not None and (current.state == 'absent') != (current.expires_at is None):
        _invalid()
    if accepted < expected.revision or (current_revision is not None and (current is None or current_revision > accepted)):
        _invalid()
    if type(v['delivery_state']) is not str or v['delivery_state'] not in ('pending', 'applied', 'conflict'):
        _invalid()
    if v['code'] is not None and (type(v['code']) is not str or not re.fullmatch('[a-z_]{1,80}', v['code'])):
        _invalid()
    if type(v['artifact']) is not str:
        _invalid()
    receipt = None
    if v['receipt'] is not None:
        r = v['receipt']
        _keys(r, 'operation_id digest external_key grant_id fence_id revision desired applied')
        rt = Target(r['external_key'], r['grant_id'], r['fence_id'])
        rd, ra = Snapshot.parse(r['desired']), Snapshot.parse(r['applied'])
        revision = _integer(r['revision'])
        if rt != target or r['operation_id'] != expected.operation_id or r['digest'] != expected.digest or revision != expected.revision or rd != desired or ra != desired:
            _invalid()
        receipt = Receipt(r['operation_id'], r['digest'], rt, revision, rd, ra)
    if v['delivery_state'] == 'applied' and (receipt is None or v['code'] is not None):
        _invalid()
    if v['artifact'] and (current is None or current.state != 'active' or current_revision is None):
        _invalid()
    return Response(target, v['operation_id'], v['body_digest'], desired, accepted,
                    current_revision, current, v['delivery_state'], v['code'], receipt, v['artifact'])


class DirectDeliveryClient:
    def __init__(self, endpoint: Path = ENDPOINT, *, opener=None):
        if not isinstance(endpoint, Path) or not endpoint.is_absolute():
            raise DeliveryError('direct_endpoint_invalid', unresolved=False)
        self._endpoint = endpoint
        self._opener = opener or asyncio.open_unix_connection

    async def claim(self, request: Request) -> Response:
        return await self._exchange(request, 'claim', request)

    async def apply(self, request: Request) -> Response:
        return await self._exchange(request, 'apply', request)

    async def read(self, request: Request, *, expected: Request) -> Response:
        if request.target != expected.target or expected.operation not in ('claim', 'apply'):
            _invalid()
        return await self._exchange(request, 'read', expected)

    async def _exchange(self, request, operation, expected):
        if not isinstance(request, Request) or request.operation != operation:
            _invalid()
        writer = None
        try:
            async with asyncio.timeout(DEADLINE):
                reader, writer = await self._opener(path=str(self._endpoint))
                writer.write(request.body + b'\n')
                await writer.drain()
                payload = bytearray()
                while True:
                    chunk = await reader.read(LIMIT + 1 - len(payload))
                    if not chunk:
                        break
                    payload.extend(chunk)
                    if len(payload) > LIMIT:
                        _invalid()
                line, separator, trailing = payload.partition(b'\n')
                if not separator or trailing:
                    _invalid()
                return parse_response(bytes(line), expected)
        except asyncio.CancelledError:
            raise
        except (OSError, TimeoutError):
            raise DeliveryError('direct_transport_unresolved') from None
        finally:
            if writer is not None:
                writer.close()
