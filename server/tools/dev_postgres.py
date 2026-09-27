#!/usr/bin/env python3
"""Start an isolated TEST PostgreSQL (bundled binaries) and print its DSN.

Example:
    .venv/bin/python tools/dev_postgres.py --pgdata ./.test-pg
    # prints: DATABASE_URL=postgresql://postgres:@/postgres?host=...
    # then run in another terminal:
    #   DATABASE_URL=... .venv/bin/terlimo-migrate up
    #   DATABASE_URL=... .venv/bin/terlimo-api
    #   DATABASE_URL=... .venv/bin/terlimo-worker
"""

from __future__ import annotations

import argparse
import os
import shutil
import signal
import tempfile
import time
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser(description="Isolated TEST PostgreSQL for TERLIMO backend")
    parser.add_argument("--pgdata", default=".test-pg")
    parser.add_argument("--database", default="terlimo_backend_test")
    parser.add_argument("--keep", action="store_true")
    arguments = parser.parse_args()

    runtime_dir = Path(tempfile.mkdtemp(prefix="terlimo-dev-pg-runtime-"))
    runtime_dir.chmod(0o700)
    os.environ["XDG_RUNTIME_DIR"] = str(runtime_dir)
    import pgserver

    pgdata = Path(arguments.pgdata).resolve()
    server = pgserver.get_server(pgdata)
    server.ensure_pgdata_inited()
    server.ensure_postgres_running()
    dsn = server.get_uri()
    print("PostgreSQL is running (isolated datadir).", flush=True)
    print(f"DATABASE_URL={dsn}", flush=True)
    print(f"PGDATA={pgdata}", flush=True)
    print("Press Ctrl+C to stop.", flush=True)
    stop = {"requested": False}

    def _handle(_signum, _frame):
        stop["requested"] = True

    signal.signal(signal.SIGINT, _handle)
    signal.signal(signal.SIGTERM, _handle)
    while not stop["requested"]:
        time.sleep(0.5)
    server.cleanup()
    if not arguments.keep:
        shutil.rmtree(pgdata, ignore_errors=True)
    print("PostgreSQL stopped.", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
