#!/usr/bin/env python3
"""Protected local operator staging only. Explicit SOURCE operator claim scheduling; default staging dry-run.

PYTHONPATH=server python server/tools/delivery_stage.py --manifest FILE \
    --allowlist FILE --dsn-fd FD [--stage]
Default dry-run. Explicit claims: --schedule-claims --writer-fence-record FILE [--commit],
requires prior stage; only DB/outbox scheduling, never adapter I/O here.
DSN arrives through inherited FD, never command text/output.
"""
import argparse
import asyncio
import json
import os
import stat

import asyncpg
from terlimo_backend.db import Database
from terlimo_backend.delivery_plan import stage_manifest


def private_json(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode) or st.st_uid != os.geteuid() or st.st_mode & 0o077 or st.st_size > 256*1024:
            raise ValueError('owner-only bounded regular file required')
        with os.fdopen(fd, 'r', closefd=False) as stream:
            def pairs(entries):
                result = {}
                for key,value in entries:
                    if key in result:
                        raise ValueError('duplicate JSON key')
                    result[key] = value
                return result
            return json.load(stream, object_pairs_hook=pairs,
                             parse_constant=lambda _: (_ for _ in ()).throw(ValueError('nonfinite JSON')))
    finally:
        os.close(fd)


async def run(args):
    if sum(bool(x) for x in (args.stage,args.schedule_claims,args.activate_capacity))>1:
        raise ValueError('stage and claim scheduling are separate operator actions')
    manifest, allowlist = private_json(args.manifest), private_json(args.allowlist)
    with os.fdopen(os.dup(args.dsn_fd)) as stream:
        dsn = stream.read(8193).strip()
    if not dsn or len(dsn)>8192:
        raise ValueError('invalid DSN descriptor')
    c = await asyncpg.connect(dsn)
    try:
        await Database._init_connection(c)
        if args.activate_capacity:
            if args.writer_fence_record:raise ValueError('activation uses retained authenticated mapping proof')
            from terlimo_backend.common_capacity import activate
            result=await activate(c,manifest,allowlist,dry_run=not args.commit)
        elif args.schedule_claims:
            from terlimo_backend.external_delivery import schedule_claims
            if not args.writer_fence_record:
                raise ValueError('protected writer fence record required')
            result = await schedule_claims(c, __import__('uuid').UUID(manifest['id']), allowlist,
                private_json(args.writer_fence_record), dry_run=not args.commit)
        else:
            if args.commit or args.writer_fence_record:
                raise ValueError('claim scheduling option required')
            result = await stage_manifest(c,manifest,allowlist,dry_run=not args.stage)
        print(json.dumps(result,sort_keys=True))
    finally:
        await c.close()


if __name__ == '__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--manifest',required=True);p.add_argument('--allowlist',required=True)
    p.add_argument('--dsn-fd',type=int,required=True);p.add_argument('--stage',action='store_true')
    p.add_argument('--schedule-claims',action='store_true');p.add_argument('--writer-fence-record')
    p.add_argument('--activate-capacity',action='store_true')
    p.add_argument('--commit',action='store_true')
    try:
        asyncio.run(run(p.parse_args()))
    except Exception:
        # DB errors may include connection strings/values: don't print exception text.
        raise SystemExit('delivery operator scheduling failed; no adapter I/O was performed') from None
