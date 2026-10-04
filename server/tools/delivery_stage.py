#!/usr/bin/env python3
"""Protected local operator staging only. No ownership proof/claim/access mutation.

PYTHONPATH=server python server/tools/delivery_stage.py --manifest FILE \
    --allowlist FILE --dsn-fd FD [--stage]
Default dry-run. DSN arrives through inherited FD, never command text/output.
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
    manifest, allowlist = private_json(args.manifest), private_json(args.allowlist)
    with os.fdopen(os.dup(args.dsn_fd)) as stream:
        dsn = stream.read(8193).strip()
    if not dsn or len(dsn)>8192:
        raise ValueError('invalid DSN descriptor')
    c = await asyncpg.connect(dsn)
    try:
        await Database._init_connection(c)
        result = await stage_manifest(c,manifest,allowlist,dry_run=not args.stage)
        print(json.dumps(result,sort_keys=True))
    finally:
        await c.close()


if __name__ == '__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--manifest',required=True);p.add_argument('--allowlist',required=True)
    p.add_argument('--dsn-fd',type=int,required=True);p.add_argument('--stage',action='store_true')
    try:
        asyncio.run(run(p.parse_args()))
    except Exception:
        # DB errors may include connection strings/values: don't print exception text.
        raise SystemExit('delivery staging failed; no access was requested') from None
