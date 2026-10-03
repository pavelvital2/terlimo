"""Recovery server SOURCE: real handlers, fake IO, explicit public TEST key only."""
from contextlib import asynccontextmanager
from dataclasses import replace
import json
from pathlib import Path
import subprocess
import sys

import pytest
from aiohttp.test_utils import TestClient, TestServer
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from terlimo_backend import api, recovery_api, telegram_bot
from terlimo_backend.config import load_settings
from terlimo_backend.db import DatabaseUnavailable
from terlimo_backend.recovery_code import (
    DOMAIN, MAX_CODE, PublicRecoveryCode, RecoveryInvalid, RecoveryUnavailable,
    encode_b64, parse_seed, sign_seed, verify_code,
)
from terlimo_backend.service_relay import service_path_allowed
from terlimo_backend.session_auth import AuthError

VECTOR = json.loads((Path(__file__).parent / 'fixtures/recovery-v1-test-vector.json').read_text())
TEST_KEY = Ed25519PrivateKey.from_private_bytes(bytes.fromhex(VECTOR['private_test_seed_hex']))


@pytest.fixture
def configured(tmp_path, settings_factory):
    path = tmp_path / 'public-code.txt'
    path.write_text(VECTOR['recovery_code'] + '\n')
    settings = settings_factory('postgresql://TEST-NOT-USED/db', recovery_code_file=str(path),
                                recovery_verify_key_b64=VECTOR['public_key_b64'])
    return settings, path


def signed_raw(raw):
    return 'TR1.' + encode_b64(raw) + '.' + encode_b64(TEST_KEY.sign(DOMAIN + raw))


def test_shared_fixture_exact_and_compact():
    raw = VECTOR['payload_utf8'].encode()
    assert sign_seed(raw, TEST_KEY, 'test') == VECTOR['recovery_code']
    assert (DOMAIN + raw).hex() == VECTOR['signed_bytes_hex']
    assert verify_code(' \n' + VECTOR['recovery_code'] + '\n', VECTOR['public_key_b64'], 'test') == VECTOR['seed']
    assert len(VECTOR['recovery_code']) < MAX_CODE


def test_valid_noncanonical_json_verifies_original_bytes():
    raw = json.dumps(VECTOR['seed'], indent=2).encode()
    assert raw != VECTOR['payload_utf8'].encode()
    code = signed_raw(raw)
    assert verify_code(code, VECTOR['public_key_b64'], 'test') == VECTOR['seed']


@pytest.mark.parametrize('mutation', ['payload', 'signature', 'foreign_key', 'foreign_environment',
                                      'version', 'padding', 'whitespace', 'truncated', 'extra_segment', 'oversize'])
def test_corrupt_code_rejected(mutation):
    code, key, env = VECTOR['recovery_code'], VECTOR['public_key_b64'], 'test'
    parts = code.split('.')
    if mutation == 'payload':
        parts[1] = encode_b64(VECTOR['payload_utf8'].replace('192.0.2.123', '192.0.2.124').encode())
        code = '.'.join(parts)
    elif mutation == 'signature':
        parts[2] = encode_b64(bytes(64)); code = '.'.join(parts)
    elif mutation == 'foreign_key': key = encode_b64(bytes(32))
    elif mutation == 'foreign_environment': env = 'production'
    elif mutation == 'version': code = code.replace('TR1.', 'TR2.', 1)
    elif mutation == 'padding': code = 'TR1.' + parts[1] + '=.' + parts[2]
    elif mutation == 'whitespace': code = code[:15] + ' ' + code[15:]
    elif mutation == 'truncated': code = code[:-8]
    elif mutation == 'extra_segment': code += '.extra'
    elif mutation == 'oversize': code = 'A' * (MAX_CODE + 1)
    with pytest.raises(RecoveryInvalid): verify_code(code, key, env)


@pytest.mark.parametrize('field,value', [
    ('secret', 'DUMMY'), ('version', True), ('revision', '01'), ('revision', '9'*20),
    ('environment', ''), ('peer_ip', 'host.example'), ('peer_ip', 'fe80::1%eth0'),
    ('dtls_port', False), ('dtls_port', 65536), ('dtls_spki_sha256', 'bad'),
    ('service_classifier', 'bad classifier'), ('vk_hashes', []), ('vk_hashes', ['x']*5),
    ('vk_hashes', ['x'*129]), ('vk_hashes', ['x\ny']), ('stream_id', -1), ('stream_id', 9223372036854775808),
])
def test_even_signed_invalid_seed_rejected(field, value):
    seed = dict(VECTOR['seed']); seed[field] = value
    raw = json.dumps(seed).encode()
    with pytest.raises(RecoveryInvalid): verify_code(signed_raw(raw), VECTOR['public_key_b64'], 'test')


@pytest.mark.parametrize('raw', [b'{"version":1,"version":1}', b'[]', b'null', b'\xff', b'{}'*4097])
def test_malformed_duplicate_or_oversize_seed(raw):
    with pytest.raises(RecoveryInvalid): parse_seed(raw, 'test')


def test_duplicate_signed_fields_rejected():
    raw = VECTOR['payload_utf8'].replace('"version":1', '"version":1,"version":1').encode()
    with pytest.raises(RecoveryInvalid): verify_code(signed_raw(raw), VECTOR['public_key_b64'], 'test')


def test_public_file_cache_reload_and_invalid_never_reissues(configured, monkeypatch):
    settings, path = configured
    loader = PublicRecoveryCode(settings)
    assert loader.get() == VECTOR['recovery_code']
    # A cached unchanged file does not re-verify or open.
    def fail_open(*args, **kwargs): raise AssertionError('cache should avoid file read')
    with monkeypatch.context() as patch:
        patch.setattr(Path, 'open', fail_open)
        assert loader.get() == VECTOR['recovery_code']
    seed = dict(VECTOR['seed'], revision='43')
    newer = sign_seed(json.dumps(seed).encode(), TEST_KEY, 'test')
    path.write_text(newer)
    assert loader.get() == newer
    path.write_text('TR1.invalid.code')
    with pytest.raises(RecoveryUnavailable): loader.get()
    path.write_text(VECTOR['recovery_code']); assert loader.get() == VECTOR['recovery_code']
    path.unlink()
    with pytest.raises(RecoveryUnavailable): loader.get()


@pytest.mark.parametrize('kind', ['absent_file', 'absent_key', 'wrong_key', 'wrong_environment', 'oversize', 'unicode'])
def test_public_config_unavailable(configured, kind):
    settings, path = configured
    if kind == 'absent_file': settings = replace(settings, recovery_code_file='')
    elif kind == 'absent_key': settings = replace(settings, recovery_verify_key_b64='')
    elif kind == 'wrong_key': settings = replace(settings, recovery_verify_key_b64=encode_b64(bytes(32)))
    elif kind == 'wrong_environment': settings = replace(settings, environment='production')
    elif kind == 'oversize': path.write_text('x'*3503)
    elif kind == 'unicode': path.write_text('не код')
    with pytest.raises(RecoveryUnavailable): PublicRecoveryCode(settings).get()


def test_operator_tool_explicit_pem_path(configured, tmp_path):
    seed = tmp_path / 'seed.json'; seed.write_text(VECTOR['payload_utf8'])
    pem = tmp_path / 'DUMMY-TEST-KEY.pem'
    pem.write_bytes(TEST_KEY.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8,
                                          serialization.NoEncryption()))
    out, pub = tmp_path / 'code.txt', tmp_path / 'verifier.txt'
    args = [sys.executable, '-m', 'terlimo_backend.recovery_sign', '--seed-file', str(seed),
            '--private-key-file', str(pem), '--environment', 'test', '--output', str(out),
            '--public-key-output', str(pub)]
    result = subprocess.run(args, capture_output=True, text=True)
    assert result.returncode == 0, result.stderr
    assert not result.stdout
    assert out.read_text().strip() == VECTOR['recovery_code']
    assert pub.read_text().strip() == VECTOR['public_key_b64']
    # Signed output is not fabricated for corrupt seed or absent private input.
    seed.write_text('{"version":1}')
    out.unlink(); pub.unlink()
    result = subprocess.run(args, capture_output=True, text=True)
    assert result.returncode == 2 and not out.exists() and not pub.exists()
    assert 'DUMMY-TEST-KEY' not in result.stderr


def test_config_load_public_only(monkeypatch):
    monkeypatch.delenv('DATABASE_URL', raising=False)
    monkeypatch.setenv('RECOVERY_CODE_FILE', '/TEST/public.txt')
    monkeypatch.setenv('RECOVERY_VERIFY_KEY_B64', VECTOR['public_key_b64'])
    settings = load_settings(require_database=False)
    assert settings.database_url == ''
    assert settings.recovery_code_file == '/TEST/public.txt'
    assert settings.recovery_verify_key_b64 == VECTOR['public_key_b64']


class FakeDB:
    def __init__(self, connection=None, unavailable=False):
        self.connection, self.unavailable, self.acquisitions = connection, unavailable, 0
    async def ensure_ready(self): return not self.unavailable
    @asynccontextmanager
    async def acquire(self):
        self.acquisitions += 1
        if self.unavailable: raise DatabaseUnavailable('TEST DOWN')
        yield self.connection


class UnlinkedConnection:
    """Actual authenticate_session, no entitlement, no Telegram/account binding."""
    async def fetchrow(self, query, *args):
        from datetime import UTC, datetime, timedelta
        if 'FROM sessions' in query:
            return {'id':'TEST-session', 'account_id':None, 'installation_id':'TEST-install',
                    'scopes':[], 'generation':1, 'expires_at':datetime.now(UTC)+timedelta(minutes=1),
                    'revoked_at':None, 'binding_id':None, 'binding_generation':None,
                    'public_key_fingerprint':'TEST-fingerprint', 'environment':'test',
                    'installation_state':'technical'}
        if "kind = 'onboarding_hour'" in query: return None
        raise AssertionError('unexpected query: ' + query)


async def app_client(settings, db):
    app = api.create_app(settings, db)
    # Run real route registration/middleware; no live startup tasks/listeners/DB.
    app.on_startup.clear(); app.on_cleanup.clear()
    client = TestClient(TestServer(app)); await client.start_server()
    return client


async def test_service_seed_real_handler_actual_auth_without_grant(configured):
    settings, _path = configured
    db = FakeDB(UnlinkedConnection())
    client = await app_client(settings, db)
    try:
        response = await client.get(recovery_api.SERVICE_SEED_PATH,
                                    headers={'Authorization':'Bearer TEST-only', 'X-Request-ID':'TEST-header'})
        body = await response.json()
        assert response.status == 200 and body['status'] == 'ok'
        assert set(body) == {'request_id','server_time','schema_version','status','recovery_code'}
        assert body['recovery_code'] == VECTOR['recovery_code']
        assert response.headers['X-Request-ID'] == 'TEST-header'
        assert db.acquisitions == 1
        assert 'access' not in body and 'seed' not in body
    finally: await client.close()


async def test_service_seed_auth_and_unavailable_envelope(configured, monkeypatch):
    settings, path = configured
    db = FakeDB(UnlinkedConnection())
    client = await app_client(settings, db)
    try:
        response = await client.get(recovery_api.SERVICE_SEED_PATH)
        assert response.status == 401 and (await response.json())['code'] == 'SESSION_INVALID'
        assert db.acquisitions == 0
        async def revoked(*args, **kwargs): raise AuthError('DEVICE_REVOKED', 403)
        with monkeypatch.context() as patch:
            patch.setattr(recovery_api, 'authenticate_session', revoked)
            response = await client.get(recovery_api.SERVICE_SEED_PATH, headers={'Authorization':'Bearer TEST'})
            assert response.status == 403 and (await response.json())['code'] == 'DEVICE_REVOKED'
        path.write_text('invalid')
        response = await client.get(recovery_api.SERVICE_SEED_PATH, headers={'Authorization':'Bearer TEST'})
        body = await response.json()
        assert response.status == 503 and body['code'] == 'RECOVERY_UNAVAILABLE' and body['retryable']
        assert set(body) == {'request_id','server_time','schema_version','status','code','retryable'}
        db.unavailable = True
        response = await client.get(recovery_api.SERVICE_SEED_PATH, headers={'Authorization':'Bearer TEST'})
        assert response.status == 503 and (await response.json())['code'] == 'TEMPORARILY_UNAVAILABLE'
        for method in ('post', 'head'):
            response = await getattr(client, method)(recovery_api.SERVICE_SEED_PATH)
            assert response.status == 405
    finally: await client.close()


def update(num, text):
    return {'update_id':num, 'message':{'chat':{'id':123456}, 'text':text, 'from':{'username':'TEST'}}}


class FakeTransport:
    def __init__(self, updates): self.updates, self.sent = updates, []
    async def get_updates(self, offset, timeout): return [u for u in self.updates if u['update_id'] >= offset]
    async def send_message(self, chat_id, text, *, reply_markup=None):
        self.sent.append((chat_id, text, reply_markup))


async def test_bot_real_runner_public_before_db_and_copyable(configured):
    settings, _path = configured
    settings = replace(settings, database_url='', telegram_bot_username='TESTbot')
    db = FakeDB(unavailable=True)
    transport = FakeTransport([update(1, '/start'), update(2, '/recovery'),
                               update(3, telegram_bot.RECOVERY_BUTTON), update(4, '/recovery@TESTbot'),
                               update(5, '/recovery@OTHERbot'), update(6, '/recovery extra')])
    runner = telegram_bot.RegistrationBotRunner(settings, db, transport)
    outcomes = await runner.handle_once()
    assert [o['state'] for o in outcomes] == ['missing_token', 'recovery', 'recovery', 'recovery']
    assert db.acquisitions == 0
    assert transport.sent[0][2] == telegram_bot.RECOVERY_MENU
    assert all(text == VECTOR['recovery_code'] and markup is None for _, text, markup in transport.sent[1::2])
    assert all(text == telegram_bot.RECOVERY_INSTRUCTION for _, text, _ in transport.sent[2::2])
    assert await runner.handle_once() == []  # offset preserved


async def test_bot_invalid_config_and_registration_db_down_do_not_block_public(configured):
    settings, path = configured
    db = FakeDB(unavailable=True)
    transport = FakeTransport([update(1, '/start TEST-token'), update(2, '/recovery')])
    runner = telegram_bot.RegistrationBotRunner(settings, db, transport)
    outcomes = await runner.handle_once()
    assert [o['state'] for o in outcomes] == ['registration_unavailable', 'recovery']
    assert db.acquisitions == 0
    path.write_text('invalid')
    transport.updates.append(update(3, '/recovery'))
    outcomes = await runner.handle_once()
    assert outcomes[0]['state'] == 'recovery_unavailable' and 'TR1.' not in outcomes[0]['reply']
    assert db.acquisitions == 0


async def test_registration_handler_regression_and_mixed_updates(configured, monkeypatch):
    settings, _ = configured
    calls, conn = [], object()
    async def confirm(connection, actual_settings, **kwargs):
        assert connection is conn and actual_settings is settings
        calls.append(kwargs)
        if kwargs['token'] == 'bad':
            from terlimo_backend.auth_api import ApiError
            raise ApiError('REGISTRATION_INVALID', http=400)
        return {'trial_available':True, 'trial_reason':'within_hour_no_prior_trial'}
    monkeypatch.setattr(telegram_bot, 'confirm_registration', confirm)
    db = FakeDB(conn)
    transport = FakeTransport([update(1, '/start good'), update(2, '/recovery'), update(3, '/start bad')])
    outcomes = await telegram_bot.RegistrationBotRunner(settings, db, transport).handle_once()
    assert [o['state'] for o in outcomes] == ['registered','recovery','REGISTRATION_INVALID']
    assert [c['token'] for c in calls] == ['good','bad'] and db.acquisitions == 2
    assert 'Регистрация подтверждена' in outcomes[0]['reply']
    assert all(c['telegram_id'] == 123456 and c['telegram_username'] == 'TEST' for c in calls)
    # Existing registration route handlers remain registered, alongside the new exact GET.
    app = api.create_app(settings, db)
    paths = {r.resource.canonical for r in app.router.routes()}
    assert '/api/mobile/v1/registration/telegram/link' in paths
    assert '/api/mobile/v1/internal/telegram/registration/confirm' in paths
    assert recovery_api.SERVICE_SEED_PATH in paths


def test_python_exact_guard():
    assert service_path_allowed('GET', recovery_api.SERVICE_SEED_PATH)
    for method in ('POST','DELETE','HEAD','PUT'):
        assert not service_path_allowed(method, recovery_api.SERVICE_SEED_PATH)
    for suffix in ('/','/other','?url=http://TEST','%2f'):
        assert not service_path_allowed('GET', recovery_api.SERVICE_SEED_PATH + suffix)


async def test_bot_startup_does_not_connect_application_db(configured, monkeypatch):
    settings, _ = configured
    settings = replace(settings, database_url='', telegram_bot_token='TEST-NOT-REAL')
    db = FakeDB(unavailable=True)
    closed = []
    async def connect(): raise AssertionError('public recovery must not connect DB at startup')
    async def close(): closed.append('db')
    db.connect, db.close = connect, close
    transport = FakeTransport([update(1, '/recovery')])
    async def close_transport(): closed.append('transport')
    transport.close = close_transport
    monkeypatch.setattr(telegram_bot, 'Database', lambda actual: db)
    monkeypatch.setattr(telegram_bot, 'AiohttpTelegramTransport', lambda token: transport)
    async def run_one(runner): await runner.handle_once()
    monkeypatch.setattr(telegram_bot.RegistrationBotRunner, 'run_forever', run_one)
    assert telegram_bot.bot_ready(settings)  # No Telegram binding/registration key required.
    await telegram_bot._amain(settings)
    assert db.acquisitions == 0 and closed == ['transport', 'db']
    assert transport.sent == [(123456, VECTOR['recovery_code'], None), (123456, telegram_bot.RECOVERY_INSTRUCTION, None)]


@pytest.mark.parametrize('invalid', [False, True])
async def test_bot_missing_invalid_config_real_handler(configured, invalid):
    settings, path = configured
    if invalid: path.write_text('TR1.bad.bad')
    else: path.unlink()
    db, transport = FakeDB(unavailable=True), FakeTransport([update(1, '/recovery')])
    outcomes = await telegram_bot.RegistrationBotRunner(settings, db, transport).handle_once()
    assert db.acquisitions == 0 and outcomes[0]['state'] == 'recovery_unavailable'
    assert len(transport.sent) == 1 and 'TR1.' not in transport.sent[0][1]


def test_existing_arm64_go_stream_bound_preserved():
    seed = dict(VECTOR['seed'], stream_id=9223372036854775807)
    code = sign_seed(json.dumps(seed).encode(), TEST_KEY, 'test')
    assert verify_code(code, VECTOR['public_key_b64'], 'test') == seed
