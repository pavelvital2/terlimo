#!/usr/bin/env python3
"""Exact additive STEP036 TEST vhost; apply/rollback only after root review."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import stat
import subprocess
import tempfile
from datetime import UTC, datetime
from pathlib import Path

CADDY = Path('/usr/local/libexec/terlimo-caddy')
CONFIG = Path('/etc/terlimo-minishop-edge/Caddyfile')
BLOCK = '''\n# STEP036 isolated mobile TEST; owned vhost (no current A/B route changes)
step036.193-5-251-217.sslip.io {
    bind 193.5.251.217
    reverse_proxy 127.0.0.1:18092
}
'''


def sha(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def validate(path: str) -> None:
    subprocess.run(['sudo', '-n', str(CADDY), 'validate', '--config', path,
                    '--adapter', 'caddyfile'], check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)


def restart() -> None:
    # This Caddy has admin off, so its reload API is unavailable.
    subprocess.run(['sudo', '-n', 'systemctl', 'restart', 'terlimo-minishop-edge'], check=True)


def replace_exact(raw: bytes, expected_sha: str, owner: os.stat_result) -> str:
    """Validate and replace one preimage, retaining its mode and ownership."""
    fd, name = tempfile.mkstemp(prefix='.Caddyfile.step036-', dir=str(CONFIG.parent))
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(raw)
            stream.flush()
            os.fchown(stream.fileno(), owner.st_uid, owner.st_gid)
            os.fchmod(stream.fileno(), stat.S_IMODE(owner.st_mode))
            os.fsync(stream.fileno())
        validate(name)
        if sha(CONFIG.read_bytes()) != expected_sha:
            raise RuntimeError('Caddyfile changed after plan/validation; refusing replacement')
        os.replace(name, CONFIG)
        directory = os.open(CONFIG.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if Path(name).exists():
            Path(name).unlink()
    return sha(raw)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument('action', choices=['plan', 'apply', 'rollback'])
    parser.add_argument('--backup-dir', default='/home/pavel/step036-device-stage/caddy-backups')
    parser.add_argument('--expect-sha256', help='required exact current Caddyfile hash for mutation')
    args = parser.parse_args()
    raw = CONFIG.read_bytes()
    text = raw.decode('utf-8')
    if text.count(BLOCK) > 1:
        raise SystemExit('duplicate STEP036 vhost')
    if args.action == 'plan':
        print(json.dumps({'action':'plan','config':str(CONFIG),'pre_sha256':sha(raw),
                          'owned_block_present':BLOCK in text,'candidate_host':'step036.193-5-251-217.sslip.io',
                          'candidate_upstream':'127.0.0.1:18092'},sort_keys=True))
        return 0
    if os.geteuid() != 0:
        raise SystemExit('apply/rollback require root')
    if not args.expect_sha256 or sha(raw) != args.expect_sha256:
        raise SystemExit('Caddyfile pre-SHA mismatch or missing --expect-sha256; no write/restart')
    owner = CONFIG.stat()
    if args.action == 'apply':
        if BLOCK in text or 'step036.193-5-251-217.sslip.io {' in text:
            raise SystemExit('STEP036 vhost already exists or conflicts')
        replacement = text.rstrip() + '\n' + BLOCK
    else:
        if BLOCK not in text:
            raise SystemExit('exact owned STEP036 block absent; refusing rollback')
        replacement = text.replace(BLOCK, '', 1)
    backup_dir = Path(args.backup_dir)
    backup_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(backup_dir,0o700)
    stamp = datetime.now(UTC).strftime('%Y%m%dT%H%M%SZ')
    backup = backup_dir / f'Caddyfile.pre-{args.action}-{stamp}'
    with backup.open('xb') as stream:
        stream.write(raw)
    os.chmod(backup,0o600)
    replacement_raw = replacement.encode()
    post_sha = sha(replacement_raw)
    replaced = False
    try:
        replace_exact(replacement_raw, sha(raw), owner)
        replaced = True
        restart()
    except BaseException as error:
        # A pre-write guard failure never restarts or rewrites the live config.
        if replaced:
            try:
                replace_exact(raw, post_sha, owner)
                restart()
            except BaseException as restore_error:
                error.add_note(f'preimage restore/restart failed: {restore_error!r}')
        raise
    print(json.dumps({'action':args.action,'backup':str(backup),'pre_sha256':sha(raw),
                      'post_sha256':sha(CONFIG.read_bytes())},sort_keys=True))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
