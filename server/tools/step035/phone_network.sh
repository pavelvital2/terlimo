#!/usr/bin/env bash
# Thin wrapper: transactional network apply/rollback/status/egress-check.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "$HERE/../../.venv/bin/python" "$HERE/phone_network.py" "$@"
