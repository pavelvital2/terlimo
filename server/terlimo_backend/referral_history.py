"""Protected coverage importer; never called from public API or configured by env.

Caller must exclusively fence legacy/canonical writers during import/activation.
The manifest is a complete verified membership snapshot, not merely reward users.
"""
from __future__ import annotations

import hashlib
import json
from uuid import UUID

from . import referral, referral_rewards


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=True)


def manifest_sha256(value):
    return hashlib.sha256(encoded(value).encode()).hexdigest()


def _keys(value, keys):
    if not isinstance(value, dict) or set(value) != set(keys.split()):
        raise ValueError('invalid history shape')


def _text(value):
    if not isinstance(value, str) or not 1 <= len(value) <= 256 or any(ord(c) < 32 for c in value):
        raise ValueError('invalid history evidence')


def _tg(value):
    if type(value) is not int or not 0 < value < 2**63:
        raise ValueError('invalid verified identity')


def validate_manifest(document):
    _keys(document, 'schema epoch_id scope complete snapshot_watermark final_watermark writer_fence members rewards')
    if type(document['schema']) is not int or document['schema'] != 1 or document['scope'] not in ('test','production'):
        raise ValueError('invalid history schema/scope')
    if not isinstance(document['epoch_id'], str) or str(UUID(document['epoch_id'])) != document['epoch_id']:
        raise ValueError('invalid epoch')
    if type(document['complete']) is not bool:
        raise ValueError('invalid coverage')
    for key in ('snapshot_watermark','final_watermark','writer_fence'):
        _text(document[key])
    if not isinstance(document['members'], list) or not isinstance(document['rewards'], list):
        raise ValueError('invalid history rows')
    members = {}
    codes = set()
    for row in document['members']:
        _keys(row, 'telegram_id code code_absent_verified referred_by_telegram_id trial_used first_main_paid dispositions')
        _tg(row['telegram_id'])
        if row['telegram_id'] in members:
            raise ValueError('duplicate member')
        members[row['telegram_id']] = row
        if row['referred_by_telegram_id'] is not None:
            _tg(row['referred_by_telegram_id'])
            if row['referred_by_telegram_id'] == row['telegram_id']:
                raise ValueError('self attribution')
        for field in ('trial_used','first_main_paid','code_absent_verified'):
            if type(row[field]) is not bool:
                raise ValueError('invalid history flag')
        if row['code'] is not None:
            referral.normalize_code(row['code'])
            if row['code_absent_verified'] or row['code'] in codes:
                raise ValueError('conflicting code')
            codes.add(row['code'])
        _keys(row['dispositions'], 'trial first_main_paid')
        if any(v not in ('earned','not_earned','unresolved') for v in row['dispositions'].values()):
            raise ValueError('invalid reward disposition')
        for event, used in (('trial', row['trial_used']), ('first_main_paid', row['first_main_paid'])):
            if row['dispositions'][event] == 'earned' and (not used or row['referred_by_telegram_id'] is None):
                raise ValueError('earned reward without source/owner')
    for row in members.values():
        if document['complete'] and row['referred_by_telegram_id'] is not None and row['referred_by_telegram_id'] not in members:
            raise ValueError('incomplete inviter membership')
    receipts = set()
    sources = set()
    for receipt in document['rewards']:
        referral_rewards.validate_import_evidence(receipt)
        pair = (receipt['invitee_telegram_id'], receipt['event_kind'])
        member = members.get(pair[0])
        if pair in receipts or receipt['source_id'] in sources or member is None or member['referred_by_telegram_id'] != receipt['inviter_telegram_id']:
            raise ValueError('reward membership conflict')
        if member['dispositions'][pair[1]] != 'earned':
            raise ValueError('reward disposition conflict')
        receipts.add(pair)
        sources.add(receipt['source_id'])
    for tg, row in members.items():
        for event, disposition in row['dispositions'].items():
            if disposition == 'earned' and (tg,event) not in receipts:
                raise ValueError('missing earned receipt')
    return document


async def import_manifest(connection, document, *, expected_sha256, scope, activate=False, dry_run=True):
    """Validate full batch and DB conflicts atomically. Dry-run rolls all writes back.

    Activation is explicit and only permitted for a complete manifest of the caller's
    explicit deployment scope. An exact replay cannot reset reward progress.
    """
    validate_manifest(document)
    sha = manifest_sha256(document)
    if sha != expected_sha256 or document['scope'] != scope or type(activate) is not bool or type(dry_run) is not bool:
        raise ValueError('history manifest/scope mismatch')
    epoch = UUID(document['epoch_id'])
    tx = connection.transaction()
    await tx.start()
    try:
        await connection.execute("SELECT pg_advisory_xact_lock(hashtextextended('referral-history-import',0))")
        if await connection.fetchval('SELECT 1 FROM referral_history_epochs WHERE active AND id<>$1',epoch):
            raise ValueError('active coverage requires explicit reconciliation, not another epoch')
        prior = await connection.fetchrow('SELECT * FROM referral_history_epochs WHERE id=$1',epoch)
        if prior is not None and prior['manifest_sha256'] != sha:
            raise ValueError('history epoch conflict')
        if activate and not document['complete']:
            raise ValueError('incomplete coverage cannot activate')
        if prior is None:
            # Import verified mappings only; never manufacture an installation/account.
            for row in document['members']:
                account = await connection.fetchrow("SELECT * FROM accounts WHERE telegram_id=$1 AND status='verified'", row['telegram_id'])
                if account is None:
                    raise ValueError('verified legacy account mapping missing')
                if row['code'] is not None and account['referral_code'] not in (None,row['code']):
                    raise ValueError('canonical code conflict')
                if row['code_absent_verified'] and account['referral_code'] is not None:
                    raise ValueError('canonical code absence conflict')
                inviter = None
                if row['referred_by_telegram_id'] is not None:
                    inviter = await connection.fetchval("SELECT id FROM accounts WHERE telegram_id=$1 AND status='verified'",row['referred_by_telegram_id'])
                    if inviter is None:
                        raise ValueError('verified inviter mapping missing')
                if account['referred_by_account_id'] not in (None,inviter):
                    raise ValueError('canonical attribution conflict')
                if await connection.fetchval('SELECT 1 FROM referral_history_staging WHERE telegram_id=$1', row['telegram_id']):
                    raise ValueError('existing staging proof requires reconciliation')
                if row['code'] is not None and await connection.fetchval('SELECT 1 FROM accounts WHERE referral_code=$1 AND id<>$2',row['code'],account['id']):
                    raise ValueError('code owner conflict')
            await connection.execute('''INSERT INTO referral_history_epochs
                (id,scope,manifest_sha256,complete,snapshot_watermark,final_watermark,writer_fence,manifest)
                VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb)''',epoch,scope,sha,document['complete'],document['snapshot_watermark'],document['final_watermark'],document['writer_fence'],encoded(document))
            for row in document['members']:
                await connection.execute('''INSERT INTO referral_history_staging
                    (telegram_id,code,referred_by_telegram_id,proven_new,trial_used,first_main_paid,source_sha256,
                     epoch_id,code_absent_verified,reward_dispositions)
                    VALUES($1,$2,$3,false,$4,$5,$6,$7,$8,$9::jsonb)''',
                    row['telegram_id'],row['code'],row['referred_by_telegram_id'],row['trial_used'],row['first_main_paid'],sha,
                    epoch,row['code_absent_verified'],encoded(row['dispositions']))
        # Exercise all DB reward conflicts even for staged-only/dry-run batches,
        # but never leave a runnable WAITING receipt before explicit activation.
        validation = connection.transaction()
        await validation.start()
        try:
            await referral_rewards.import_reward_receipts(connection,document['rewards'],sha,require_evidence=True)
        finally:
            await validation.rollback()
        if activate:
            if await connection.fetchval('SELECT 1 FROM referral_history_epochs WHERE active AND id<>$1',epoch):
                raise ValueError('another coverage epoch already active')
            await referral_rewards.import_reward_receipts(connection,document['rewards'],sha,require_evidence=True)
            # Block already-used legacy benefits immediately at activation, even
            # before a user's first info/registration-triggered initialization.
            for member in sorted(document['members'], key=lambda row: row['telegram_id']):
                account = await connection.fetchval(
                    "SELECT id FROM accounts WHERE telegram_id=$1 AND status='verified' FOR UPDATE",member['telegram_id'])
                if account is None:
                    raise ValueError('verified mapping changed before activation')
                await connection.execute("""INSERT INTO referral_benefits
                    (account_id,imported_trial_used,imported_first_main_paid) VALUES($1,$2,$3)
                    ON CONFLICT(account_id) DO UPDATE SET
                    imported_trial_used=referral_benefits.imported_trial_used OR EXCLUDED.imported_trial_used,
                    imported_first_main_paid=referral_benefits.imported_first_main_paid OR EXCLUDED.imported_first_main_paid""",
                    account,member['trial_used'],member['first_main_paid'])
            await connection.execute('UPDATE referral_history_epochs SET active=true,activated_at=coalesce(activated_at,now()) WHERE id=$1',epoch)
        # Never initialize accounts during staging: this is the ordinary initializer's job.
        if dry_run:
            await tx.rollback()
        else:
            await tx.commit()
    except BaseException:
        try:
            await tx.rollback()
        except Exception:
            pass
        raise
    return {'manifest_sha256':sha,'epoch_id':str(epoch),'members':len(document['members']),
            'dry_run':dry_run,'activation_requested':activate}


def main():
    """python -m terlimo_backend.referral_history; protected files, default dry-run."""
    import argparse
    import asyncio
    import os
    import stat
    from pathlib import Path
    import asyncpg

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--manifest', required=True)
    parser.add_argument('--sha256', required=True, help='SHA256 of canonical JSON (sorted keys, compact ASCII)')
    parser.add_argument('--scope', choices=('test','production'), required=True)
    parser.add_argument('--dsn-file', required=True)
    parser.add_argument('--commit', action='store_true')
    parser.add_argument('--activate', action='store_true')
    args = parser.parse_args()

    def private_text(name):
        path = Path(name)
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd) as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.geteuid():
                raise ValueError('input must be owner-only regular file')
            return stream.read()

    def unique(pairs):
        result = {}
        for key,value in pairs:
            if key in result:
                raise ValueError('duplicate manifest key')
            result[key] = value
        return result

    async def run():
        document = json.loads(private_text(args.manifest), object_pairs_hook=unique)
        connection = await asyncpg.connect(private_text(args.dsn_file).strip())
        try:
            receipt = await import_manifest(connection,document,expected_sha256=args.sha256,
                scope=args.scope,activate=args.activate,dry_run=not args.commit)
            print(encoded(receipt))  # Counts/digest only; no identity/DSN/manifest rows.
        finally:
            await connection.close()
    try:
        asyncio.run(run())
    except Exception:
        parser.exit(1, 'History import rejected; no input or database details printed.\n')


if __name__ == '__main__':
    main()
