#!/usr/bin/env bash
# Bounded HTTP readiness gate. Usage:
#   05_wait_readiness.sh [--url URL] [--attempts N] [--interval S] [--connect-timeout S] [--max-time S]
# Readiness is an HTTP check (API), never a single DB read. Exhaustion is a hard non-zero abort so
# the caller cannot silently continue to the next step.
set -euo pipefail
URL="http://127.0.0.1:18091/health/ready"; ATTEMPTS=60; INTERVAL=2; CONNECT_TIMEOUT=2; MAX_TIME=5
while [ $# -gt 0 ]; do
  case "$1" in
    --url) URL="$2"; shift 2 ;;
    --attempts) ATTEMPTS="$2"; shift 2 ;;
    --interval) INTERVAL="$2"; shift 2 ;;
    --connect-timeout) CONNECT_TIMEOUT="$2"; shift 2 ;;
    --max-time) MAX_TIME="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 64 ;;
  esac
done
for _ in $(seq 1 "$ATTEMPTS"); do
  code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout "$CONNECT_TIMEOUT" --max-time "$MAX_TIME" "$URL" || true)
  if [ "$code" = "200" ]; then echo "READINESS_OK $URL"; exit 0; fi
  sleep "$INTERVAL"
done
echo "READINESS_TIMEOUT $URL after $ATTEMPTS attempts" >&2
exit 1
