#!/usr/bin/env bash
# Target guard for the STEP03.5 dump import. Env: WORK, PSQL, PGCTL, LOG.
# Aborts (non-zero) before any DROP when the live server is not the exact package target.
set -euo pipefail
: "${WORK:?WORK required}"; : "${PSQL:?PSQL required}"; : "${PGCTL:?PGCTL required}"; : "${LOG:?LOG required}"
umask 077
export PGHOST PGPORT PGUSER PGDATABASE
eval "$(python3 - "$WORK" <<'PY'
import json, sys, urllib.parse as u
state = json.load(open(sys.argv[1] + "/state.json"))
parts = u.urlsplit(state["database_url"]); query = u.parse_qs(parts.query)
print(f'PGHOST={query["host"][0]!r}')
print(f'PGPORT={parts.port if parts.port else ""!r}')
print(f'PGUSER={parts.username!r}')
print('PGDATABASE=postgres')
PY
)"
"$PGCTL" -D "$WORK/pg" status >>"$LOG" 2>&1
DD=$("$PSQL" -v ON_ERROR_STOP=1 -Atc "SELECT pg_catalog.current_setting('data_directory')" 2>>"$LOG")
USD=$("$PSQL" -v ON_ERROR_STOP=1 -Atc "SELECT pg_catalog.current_setting('unix_socket_directories')" 2>>"$LOG")
LA=$("$PSQL" -v ON_ERROR_STOP=1 -Atc "SELECT pg_catalog.current_setting('listen_addresses')" 2>>"$LOG")
[ "$(readlink -f "$DD")" = "$(readlink -f "$WORK/pg")" ] || { echo "TARGET_MISMATCH data_directory=$DD" >&2; exit 1; }
IFS=',' read -ra SOCKS <<<"$USD"
for sock in "${SOCKS[@]}"; do
  [ "$(readlink -f "$sock")" = "$(readlink -f "$WORK/pg")" ] || { echo "TARGET_MISMATCH socket=$sock" >&2; exit 1; }
done
[ "$LA" = "" ] || { echo "TARGET_MISMATCH listen_addresses=$LA (TCP not expected)" >&2; exit 1; }
PID=$(head -1 "$WORK/pg/postmaster.pid")
tr '\0' ' ' < "/proc/$PID/cmdline" | grep -q -- "-D $WORK/pg" || { echo "TARGET_MISMATCH postmaster cmdline" >&2; exit 1; }
[ "$(stat -c %u "/proc/$PID")" = "$(id -u)" ] || { echo "TARGET_MISMATCH postmaster owner" >&2; exit 1; }
echo TARGET_OK
