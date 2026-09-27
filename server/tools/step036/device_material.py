#!/usr/bin/env python3
"""Prepare private STEP036 device material from the exact TEST A VK source; never print secrets."""
from __future__ import annotations

import argparse
import base64
import hashlib
import importlib.util
import json
import os
import secrets
from datetime import UTC, datetime, timedelta
from pathlib import Path

from cryptography import x509
from cryptography.fernet import Fernet
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID

SOURCE = Path('/etc/terlimo/client-test/backend/config.json')
SOURCE_ID = 'terlimo-test-193-5-251-217'
SOURCE_IP = '193.5.251.217'


def _helper():
    path = Path('/home/pavel/projects/terlimo-backend/tools/step035/provision_trusted_vk.py')
    spec = importlib.util.spec_from_file_location('step035_vk_source', path)
    module = importlib.util.module_from_spec(spec)
    assert spec and spec.loader
    spec.loader.exec_module(module)
    return module


def _write(path: Path, content: bytes, owner: int) -> None:
    if path.exists():
        raise RuntimeError(f'material already exists: {path.name}')
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        stream.write(content)
        stream.flush()
        os.fsync(stream.fileno())
    os.chown(path, owner, -1)


def _hashes() -> list[str]:
    helper = _helper()
    node, _ = helper._select_node(SOURCE, SOURCE_ID, SOURCE_IP)
    return helper._validate_hashes(node, 4)


def _pin(cert: x509.Certificate) -> str:
    der = cert.public_key().public_bytes(serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)
    return base64.urlsafe_b64encode(hashlib.sha256(der).digest()).rstrip(b'=').decode('ascii')


def prepare(work: Path) -> dict:
    owner = work.stat().st_uid
    private = work / 'secrets'
    creds = work / 'gateway' / 'creds'
    private.mkdir(mode=0o700, exist_ok=True)
    creds.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(private, 0o700)
    os.chmod(work / 'gateway', 0o700)
    os.chmod(creds, 0o700)
    os.chown(private, owner, -1)
    os.chown(work / 'gateway', owner, -1)
    os.chown(creds, owner, -1)
    hashes = _hashes()
    now = datetime.now(UTC)
    key = ec.generate_private_key(ec.SECP256R1())
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, 'terlimo-036-device-test')])
    cert = (x509.CertificateBuilder().subject_name(name).issuer_name(name)
            .public_key(key.public_key()).serial_number(x509.random_serial_number())
            .not_valid_before(now - timedelta(hours=1)).not_valid_after(now + timedelta(days=14))
            .sign(key, hashes_module()))
    _write(creds / 'wl-test-dtls-cert', cert.public_bytes(serialization.Encoding.PEM), owner)
    _write(creds / 'wl-test-dtls-key', key.private_bytes(serialization.Encoding.PEM,
           serialization.PrivateFormat.PKCS8, serialization.NoEncryption()), owner)
    _write(private / 'service-seed', secrets.token_urlsafe(32).encode(), owner)
    _write(private / 'onboarding-key', Fernet.generate_key(), owner)
    _write(private / 'vk-hashes.json', json.dumps(hashes).encode(), owner)
    return {'ok': True, 'node_id': 'terlimo-036-node', 'vk_count': len(hashes),
            'dtls_pin': _pin(cert), 'cert_not_after': cert.not_valid_after_utc.isoformat()}


def hashes_module():
    return hashes.SHA256()


def profile(work: Path) -> dict:
    helper = _helper()
    gateway = work / 'gateway'
    if helper._node_running(gateway):
        raise RuntimeError('node must be stopped before VK profile update')
    source = _hashes()
    cached = json.loads((work / 'secrets' / 'vk-hashes.json').read_text())
    if cached != source:
        raise RuntimeError('protected VK source changed after preparation')
    target = gateway / 'passwords.json'
    if not target.is_file():
        raise RuntimeError('node profile missing; first materialization start/stop required')
    result = helper.update_profile_file(target, source)
    return {'ok': True, 'node_id': 'terlimo-036-node', 'vk_count': result['hashes'],
            'backup_file': result['backup']}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument('action', choices=['prepare', 'profile'])
    parser.add_argument('--work', required=True)
    args = parser.parse_args()
    work = Path(args.work).resolve(strict=True)
    if os.geteuid() != 0 or work.is_symlink() or work.name != 'pkg' or not (work / '.step036-owner.json').is_file():
        raise SystemExit('owned STEP036 work and root required')
    print(json.dumps(prepare(work) if args.action == 'prepare' else profile(work), sort_keys=True))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
