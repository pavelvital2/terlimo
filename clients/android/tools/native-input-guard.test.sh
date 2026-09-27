#!/usr/bin/env bash
# Deterministic, offline checks for tools/native-input-guard.sh.
# Proves missing/non-file/non-executable/non-ELF inputs fail and a valid ELF passes,
# and that the APK entry guard requires the exact entry with nonzero ELF content.
# Runs no Gradle/Android build.
set -euo pipefail

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/native-input-guard.sh
source "$here/native-input-guard.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fails=0

expect_ok() {
    local name="$1"; shift
    if "$@" >/dev/null 2>&1; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (expected success)"; fails=$((fails + 1))
    fi
}

expect_fail() {
    local name="$1"; shift
    if "$@" >/dev/null 2>&1; then
        echo "FAIL: $name (expected failure)"; fails=$((fails + 1))
    else
        echo "PASS: $name"
    fi
}

# --- input fixtures ---
valid_elf="$tmp/valid.so"
printf '\x7fELF\x02\x01\x01\x00payloadpayload' >"$valid_elf"
chmod +x "$valid_elf"

elf_nonexec="$tmp/elf_nonexec.so"
printf '\x7fELF\x02\x01\x01\x00payload' >"$elf_nonexec"
chmod -x "$elf_nonexec"

nonelf="$tmp/nonelf.so"
printf 'not-an-elf-payload' >"$nonelf"
chmod +x "$nonelf"

empty="$tmp/empty.so"
: >"$empty"
chmod +x "$empty"

missing="$tmp/does-not-exist.so"
adir="$tmp/a-directory.so"
mkdir -p "$adir"

expect_fail "missing input fails" guard_require_elf_input "$missing" "test input"
expect_fail "directory input fails" guard_require_elf_input "$adir" "test input"
expect_fail "non-executable ELF fails" guard_require_elf_input "$elf_nonexec" "test input"
expect_fail "non-ELF executable fails" guard_require_elf_input "$nonelf" "test input"
expect_fail "empty executable fails" guard_require_elf_input "$empty" "test input"
expect_ok "valid ELF executable passes" guard_require_elf_input "$valid_elf" "test input"

# --- APK fixtures (real ZIP files via python, no Android tooling) ---
python3 - "$tmp" <<'PY'
import sys, zipfile, os
tmp = sys.argv[1]
entry = "lib/arm64-v8a/libterlimo.so"
def make(name, entries):
    with zipfile.ZipFile(os.path.join(tmp, name), "w") as z:
        for n, data in entries.items():
            z.writestr(n, data)
make("good.apk", {entry: b"\x7fELF\x02\x01\x01\x00payload"})
make("missing.apk", {"classes.dex": b"dex"})
make("empty.apk", {entry: b""})
make("nonelf.apk", {entry: b"not-elf"})
# Large entry reproduces the `set -o pipefail` SIGPIPE case: a producer (unzip)
# writing far more than the 4 bytes read by the guard must not abort the guard.
big = b"\x7fELF\x02\x01\x01\x00" + b"\x00" * (8 * 1024 * 1024)
with zipfile.ZipFile(os.path.join(tmp, "big.apk"), "w", zipfile.ZIP_DEFLATED) as z:
    z.writestr(entry, big)
PY

expect_ok "APK with valid ELF entry passes" guard_require_apk_elf_entry "$tmp/good.apk" "lib/arm64-v8a/libterlimo.so" "test apk"
expect_ok "APK with large ELF entry passes under pipefail" guard_require_apk_elf_entry "$tmp/big.apk" "lib/arm64-v8a/libterlimo.so" "test apk"
expect_fail "APK without entry fails" guard_require_apk_elf_entry "$tmp/missing.apk" "lib/arm64-v8a/libterlimo.so" "test apk"
expect_fail "APK with empty entry fails" guard_require_apk_elf_entry "$tmp/empty.apk" "lib/arm64-v8a/libterlimo.so" "test apk"
expect_fail "APK with non-ELF entry fails" guard_require_apk_elf_entry "$tmp/nonelf.apk" "lib/arm64-v8a/libterlimo.so" "test apk"
expect_fail "missing APK fails" guard_require_apk_elf_entry "$tmp/nope.apk" "lib/arm64-v8a/libterlimo.so" "test apk"

echo
if [[ "$fails" -eq 0 ]]; then
    echo "RESULT: ALL PASS"
    exit 0
fi
echo "RESULT: $fails FAILURE(S)"
exit 1
