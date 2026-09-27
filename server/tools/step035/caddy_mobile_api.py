#!/usr/bin/env python3
"""Remove exactly our reviewed mobile-API block from the live Caddyfile (stop procedure).

Context-checked: the file must contain the exact vhost and our exact block; any drift refuses.
Backup of the current file, validate, restart (the unit's reload is broken by design because
the Caddyfile sets `admin off`), then check /help and the mobile path.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import time
from pathlib import Path

CADDYFILE = Path("/etc/terlimo-minishop-edge/Caddyfile")
VHOST_HEAD = "terlimo.193-5-251-217.sslip.io {"
BLOCK = """\t# STEP03.5 mobile API -> isolated 03.4 backend (owner-reviewed additive route)
\t@mobile_api path /api/mobile/v1/*
\treverse_proxy @mobile_api 127.0.0.1:18091
"""


def has_block(text: str) -> bool:
    return BLOCK in text


def remove_block(text: str) -> tuple[str, bool]:
    if BLOCK not in text:
        return text, False
    if VHOST_HEAD not in text:
        raise RuntimeError("vhost context missing: refusing to edit foreign configuration")
    return text.replace(BLOCK, "", 1), True


def run(args: list[str], *, check: bool = True) -> str:
    result = subprocess.run(args, capture_output=True, text=True, check=False)
    if check and result.returncode != 0:
        raise RuntimeError(f"{' '.join(args)}: {result.stderr.strip()[:200]}")
    return result.stdout


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=["remove", "status"])
    arguments = parser.parse_args()
    text = run(["sudo", "-n", "cat", str(CADDYFILE)])
    if arguments.action == "status":
        print(json.dumps({"block_present": has_block(text), "vhost_present": VHOST_HEAD in text}))
        return 0
    if not has_block(text):
        print(json.dumps({"ok": True, "removed": False, "reason": "block already absent"}))
        return 0
    updated, removed = remove_block(text)
    backup = f"{CADDYFILE}.pre-remove.{int(time.time())}"
    run(["sudo", "-n", "cp", str(CADDYFILE), backup])
    subprocess.run(
        ["sudo", "-n", "tee", str(CADDYFILE)], input=updated, text=True, check=True
    )
    run(["sudo", "-n", "/usr/local/libexec/terlimo-caddy", "validate", "--config", str(CADDYFILE)])
    run(["sudo", "-n", "systemctl", "restart", "terlimo-minishop-edge"])
    time.sleep(2)
    help_code = run(
        [
            "curl",
            "-s",
            "-o",
            "/dev/null",
            "-w",
            "%{http_code}",
            "https://terlimo.193-5-251-217.sslip.io/help",
        ]
    )
    mobile_code = run(
        [
            "curl",
            "-s",
            "-o",
            "/dev/null",
            "-w",
            "%{http_code}",
            "https://terlimo.193-5-251-217.sslip.io/api/mobile/v1/gateways",
        ]
    )
    print(
        json.dumps(
            {
                "ok": True,
                "removed": removed,
                "backup": backup,
                "help": help_code,
                "mobile_after_removal": mobile_code,
            }
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
