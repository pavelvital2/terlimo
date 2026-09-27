"""Slice B adapters/worker integration: real cipher + bootstrap clients + worker wiring.

No live node/db/routes: the node client is faked, the transport command shape is asserted, and
the worker runs against the disposable test database only.
"""
from __future__ import annotations

import json
import uuid
from types import SimpleNamespace

import asyncpg
import pytest
from cryptography.fernet import Fernet

from terlimo_backend import management_handler
from terlimo_backend.db import Database
from terlimo_backend.gateway_adapter import GatewayAdminClient
from terlimo_backend.gateway_control import GatewayError
from terlimo_backend.onboarding_hour import (
    FernetSecretCipher,
    OnboardingHourHandlers,
    UnavailableSecretCipher,
    build_secret_cipher,
    create_intent,
    revoke_intent,
)
from terlimo_backend.worker import OutboxWorker

PROVISION = "gateway.bootstrap_provision"
REVOKE = "gateway.bootstrap_revoke"


async def _connect(url: str) -> asyncpg.Connection:
    connection = await asyncpg.connect(url, timeout=10)
    await connection.set_type_codec(
        "jsonb", schema="pg_catalog", encoder=json.dumps, decoder=json.loads
    )
    return connection


async def _seed(database_url: str, key: str = "terlimo-035-node") -> tuple[uuid.UUID, uuid.UUID]:
    import base64

    endpoints = {
        "node_id": key,
        "peer_ip": "127.0.0.1",
        "dtls_port": 57400,
        "wg_port": 57401,
        "dtls_spki_sha256": base64.urlsafe_b64encode(b"k" * 32).rstrip(b"=").decode(),
        "target_workers": 36,
    }
    connection = await _connect(database_url)
    try:
        installation_id = await connection.fetchval(
            """
            INSERT INTO installations
                (environment, platform, public_key_fingerprint, public_key_spki_b64, state)
            VALUES ('test', 'android', $1, 'spki', 'technical') RETURNING id
            """,
            uuid.uuid4().hex,
        )
        gateway_id = await connection.fetchval(
            """
            INSERT INTO gateways (gateway_key, environment, endpoints, capabilities,
                                  registry_state, confirmed_max_workers)
            VALUES ($1, 'test', $2::jsonb, '["managed"]'::jsonb, 'registered', 36)
            RETURNING id
            """,
            key,
            json.dumps(endpoints),
        )
        return installation_id, gateway_id
    finally:
        await connection.close()


class FlakyBootstrapClient:
    def __init__(self, fail_times: int = 0) -> None:
        self.fail_times = fail_times
        self.attempts: list[dict] = []
        self.provisions: list[dict] = []
        self.revokes: list[str] = []

    async def bootstrap_provision(self, *, credential_id, secret, expires_at, node_id):
        self.attempts.append({"credential_id": credential_id, "secret": secret})
        if self.fail_times > 0:
            self.fail_times -= 1
            raise GatewayError("GATEWAY_UNREACHABLE", "simulated")
        self.provisions.append({"credential_id": credential_id, "secret": secret, "node_id": node_id})
        return {"ok": True}

    async def bootstrap_revoke(self, *, credential_id):
        self.revokes.append(credential_id)


def test_fernet_cipher_roundtrip_and_fail_closed():
    key = Fernet.generate_key().decode()
    cipher = FernetSecretCipher(key)
    sealed = cipher.encrypt("s3cret-value")
    assert sealed != b"s3cret-value" and cipher.decrypt(sealed) == "s3cret-value"
    with pytest.raises(RuntimeError):
        FernetSecretCipher(Fernet.generate_key().decode()).decrypt(sealed)
    with pytest.raises(ValueError):
        FernetSecretCipher("not-a-key")
    unavailable = build_secret_cipher(SimpleNamespace(onboarding_secret_key=""))
    assert isinstance(unavailable, UnavailableSecretCipher)
    with pytest.raises(RuntimeError):
        unavailable.encrypt("x")


def test_management_handler_bootstrap_field_validation():
    config = management_handler.HandlerConfig()
    config.node_id = "terlimo-035-node"
    config.max_not_after = 86400
    secret = "s" * 32
    expires = int(__import__("time").time()) + 600
    request = {
        "v": 1,
        "op": "bootstrap_provision",
        "node_id": "terlimo-035-node",
        "fields": {"credential_id": "cred-1", "secret": secret, "expires_at": expires},
    }
    command = management_handler._validate_command(request, config)
    assert command == {
        "operation": "bootstrap_provision",
        "bootstrap": {"credential_id": "cred-1", "secret": secret, "expires_at": expires},
    }
    revoke = management_handler._validate_command(
        {
            "v": 1,
            "op": "bootstrap_revoke",
            "node_id": "terlimo-035-node",
            "fields": {"credential_id": "cred-1"},
        },
        config,
    )
    assert revoke == {"operation": "bootstrap_revoke", "bootstrap": {"credential_id": "cred-1"}}
    with pytest.raises(management_handler.HandlerError):
        management_handler._validate_command(
            {**request, "fields": {"credential_id": "c", "secret": "short", "expires_at": expires}},
            config,
        )
    with pytest.raises(management_handler.HandlerError):
        management_handler._validate_command(
            {**request, "fields": {**request["fields"], "unexpected": 1}}, config
        )


async def test_admin_client_bootstrap_command_shape(monkeypatch):
    client = GatewayAdminClient(socket_path="/tmp/does-not-matter.sock", main_password="pw")
    captured: list[dict] = []

    async def fake_call(command):
        captured.append(command)
        return {"ok": True}

    monkeypatch.setattr(client, "call", fake_call)
    await client.bootstrap_provision(credential_id="cred-1", secret="s" * 32, expires_at=1234567890)
    assert captured[0] == {
        "operation": "bootstrap_provision",
        "bootstrap": {"credential_id": "cred-1", "secret": "s" * 32, "expires_at": 1234567890, "revoked": False},
    }
    await client.bootstrap_revoke(credential_id="cred-1")
    assert captured[1] == {
        "operation": "bootstrap_revoke",
        "bootstrap": {"credential_id": "cred-1"},
    }


async def _worker_env(migrated_url, settings_factory, client, cipher):
    key = Fernet.generate_key().decode()
    settings = settings_factory(migrated_url, onboarding_secret_key=key, gateway_local_admin_enabled=True)
    database = Database(settings)
    await database.connect()
    handlers = OnboardingHourHandlers(
        cipher=cipher, client_factory=lambda _key, _endpoints: client
    ).as_handlers()
    worker = OutboxWorker(database, settings, handlers=handlers, worker_id="onboarding-int")
    return settings, database, worker


async def test_worker_provision_retry_is_idempotent_and_keeps_secrets_out_of_outbox(
    migrated_url, settings_factory
):
    key = Fernet.generate_key().decode()
    cipher = FernetSecretCipher(key)
    installation_id, _ = await _seed(migrated_url)
    connection = await _connect(migrated_url)
    try:
        created = await create_intent(
            connection,
            installation_id=installation_id,
            request_key="int-key-1",
            environment="test",
            cipher=cipher,
        )
    finally:
        await connection.close()
    client = FlakyBootstrapClient(fail_times=1)
    _settings, database, worker = await _worker_env(migrated_url, settings_factory, client, cipher)
    try:
        assert await worker.run_once() is True
        connection = await _connect(migrated_url)
        try:
            op = await connection.fetchrow(
                "SELECT status, attempts FROM outbox_operations WHERE operation_type = $1", PROVISION
            )
            assert op["status"] == "pending" and int(op["attempts"]) == 1
            payload_text = await connection.fetchval(
                "SELECT payload::text FROM outbox_operations WHERE operation_type = $1", PROVISION
            )
            assert "secret" not in payload_text
            await connection.execute(
                "UPDATE outbox_operations SET available_at = now() WHERE operation_type = $1", PROVISION
            )
        finally:
            await connection.close()
        assert await worker.drain() == 1
        assert len(client.attempts) == 2 and len(client.provisions) == 1
        assert client.attempts[0]["secret"] == client.attempts[1]["secret"]
        connection = await _connect(migrated_url)
        try:
            row = await connection.fetchrow(
                "SELECT state, secret_enc, secret_hash FROM onboarding_intents WHERE id = $1",
                created["id"],
            )
            assert row["state"] == "ready"
            assert bytes(row["secret_enc"]) != client.provisions[0]["secret"].encode()
            assert cipher.decrypt(bytes(row["secret_enc"])) == client.provisions[0]["secret"]
            assert row["secret_hash"]
        finally:
            await connection.close()
    finally:
        await database.close()


async def test_worker_revoke_path_and_terminal_provision_failure(migrated_url, settings_factory):
    key = Fernet.generate_key().decode()
    cipher = FernetSecretCipher(key)
    installation_id, _ = await _seed(migrated_url, key="terlimo-035-node")
    connection = await _connect(migrated_url)
    try:
        created = await create_intent(
            connection,
            installation_id=installation_id,
            request_key="int-key-2",
            environment="test",
            cipher=cipher,
        )
        revoked = await revoke_intent(connection, intent_id=uuid.UUID(created["id"]))
        assert revoked["revoked"] is True
    finally:
        await connection.close()
    client = FlakyBootstrapClient()
    _settings, database, worker = await _worker_env(migrated_url, settings_factory, client, cipher)
    try:
        assert await worker.drain() == 2  # revoked intent: provision op is fenced, revoke runs
        assert client.revokes == [created["credential_id"]]
        assert client.provisions == []
        connection = await _connect(migrated_url)
        try:
            assert await connection.fetchval(
                "SELECT status FROM outbox_operations WHERE operation_type = $1", REVOKE
            ) == "done"
        finally:
            await connection.close()

        # terminal provision failure: bounded attempts -> dead, intent stays pending (documented)
        other_installation, _ = await _seed(migrated_url, key="terlimo-035-alt")
        connection = await _connect(migrated_url)
        try:
            second = await create_intent(
                connection,
                installation_id=other_installation,
                request_key="int-key-3",
                environment="test",
                cipher=cipher,
            )
            await connection.execute(
                "UPDATE outbox_operations SET max_attempts = 1 WHERE operation_type = $1 AND payload->>'intent_id' = $2",
                PROVISION,
                second["id"],
            )
        finally:
            await connection.close()
        always_failing = FlakyBootstrapClient(fail_times=10)
        _settings2, database2, worker2 = await _worker_env(
            migrated_url, settings_factory, always_failing, cipher
        )
        try:
            assert await worker2.run_once() is True
            connection = await _connect(migrated_url)
            try:
                status = await connection.fetchval(
                    "SELECT status FROM outbox_operations WHERE operation_type = $1 AND payload->>'intent_id' = $2",
                    PROVISION,
                    second["id"],
                )
                state = await connection.fetchval(
                    "SELECT state FROM onboarding_intents WHERE id = $1", second["id"]
                )
                assert status == "dead" and state == "pending"
            finally:
                await connection.close()
        finally:
            await database2.close()
    finally:
        await database.close()
