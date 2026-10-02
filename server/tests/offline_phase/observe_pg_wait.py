"""OPTIONAL future authorized observer; never run implicitly or query other backends.

Usage: exact captured --pid, --seconds <=10; existing local peer access only.
No query text, lock owner, address, application name or identifiers collected.
Absence of a wait sample cannot prove absence of locks, commit or I/O latency.
"""
import argparse,asyncio,json,re,time
from datetime import UTC,datetime
import asyncpg

def project(row):
    allowed_states={'active','idle','idle in transaction','idle in transaction (aborted)','disabled'}
    state=row['state'] if row['state'] in allowed_states else 'UNKNOWN'
    def enum(value):
        return value if isinstance(value,str) and re.fullmatch('[A-Za-z_]{1,64}',value) else 'UNKNOWN'
    return {'state':state,'wait_type':enum(row['wait_event_type']),'wait_event':enum(row['wait_event'])}

async def observe(pid,seconds):
    connection=await asyncpg.connect(host='/home/pavel/step036-device-stage/pkg/pg',user='postgres',database='terlimo_036',timeout=1)
    rows=[];identity=None;started=time.monotonic()
    try:
        for _ in range(40):
            if time.monotonic()-started>=seconds:break
            try:
                async with connection.transaction(readonly=True):
                    await connection.execute("SET LOCAL statement_timeout='300ms'")
                    await connection.execute("SET LOCAL lock_timeout='100ms'")
                    row=await connection.fetchrow('SELECT backend_start,state,wait_event_type,wait_event FROM pg_stat_activity WHERE pid=$1 AND datname=current_database()',pid)
                if row is None:
                    rows.append({'utc':datetime.now(UTC).isoformat(),'status':'unavailable'});break
                if identity is None:identity=row['backend_start']
                if row['backend_start']!=identity:
                    rows.append({'utc':datetime.now(UTC).isoformat(),'status':'PID_REUSED'});break
                rows.append({'utc':datetime.now(UTC).isoformat(),**project(row)})
            except (asyncpg.PostgresError,asyncio.TimeoutError):
                rows.append({'utc':datetime.now(UTC).isoformat(),'status':'unavailable'});break
            await asyncio.sleep(min(.25,max(0,seconds-(time.monotonic()-started))))
    finally:await connection.close(timeout=1)
    return {'pg_backend_pid':pid,'samples':rows,'limits':'Single exact PID, fresh readonly transaction per sample, <=40samples/~4Hz/10s; absent or unavailable samples do not rule out waits.'}

if __name__=='__main__':
    parser=argparse.ArgumentParser();parser.add_argument('--pid',type=int,required=True);parser.add_argument('--seconds',type=float,default=10)
    args=parser.parse_args()
    if not 0<args.pid<=2**31-1 or not 0<args.seconds<=10:parser.error('PID positive int; duration >0 and <=10')
    print(json.dumps(asyncio.run(asyncio.wait_for(observe(args.pid,args.seconds),timeout=13))))
