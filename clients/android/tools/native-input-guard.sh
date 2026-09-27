#!/usr/bin/env bash
# Shared, dependency-light guards for the prebuilt native input and its APK entry.
# Sourced by build-test-android.sh and exercised by tools/native-input-guard.test.sh.
#
# These guards never build native code; they only fail fast on an invalid input.

# guard_require_elf_input <path> [label]
# Requires: regular file, executable, non-empty, ELF magic (0x7f 'E' 'L' 'F').
guard_require_elf_input() {
    local path="$1" label="${2:-native input}"
    if [[ ! -e "$path" ]]; then
        echo "ERROR: $label is missing: $path. Build it first with ./build-test-android.sh (native-first)." >&2
        return 1
    fi
    if [[ ! -f "$path" ]]; then
        echo "ERROR: $label is not a regular file: $path." >&2
        return 1
    fi
    if [[ ! -x "$path" ]]; then
        echo "ERROR: $label is not executable: $path. Re-run ./build-test-android.sh to rebuild it." >&2
        return 1
    fi
    local size
    size="$(wc -c <"$path")"
    if [[ "$size" -le 0 ]]; then
        echo "ERROR: $label is empty: $path. Re-run ./build-test-android.sh to rebuild it." >&2
        return 1
    fi
    local magic
    magic="$(head -c4 "$path" | od -An -tx1 | tr -d ' \n')"
    if [[ "$magic" != "7f454c46" ]]; then
        echo "ERROR: $label is not an ELF file: $path (magic=$magic). Re-run ./build-test-android.sh." >&2
        return 1
    fi
    echo "OK: $label verified: $path ($size bytes, ELF)"
}

# guard_require_apk_elf_entry <apk> <zip-entry> [label]
# Requires: APK exists, contains the exact ZIP entry, entry is non-empty, entry is ELF.
# Reads entry size and the first four bytes via Python's zipfile, so a large
# entry never sends SIGPIPE to a producer under `set -o pipefail`.
guard_require_apk_elf_entry() {
    local apk="$1" entry="$2" label="${3:-APK native entry}"
    if [[ ! -f "$apk" ]]; then
        echo "ERROR: APK missing: $apk." >&2
        return 1
    fi
    local info
    info="$(python3 - "$apk" "$entry" <<'PY'
import sys, zipfile
apk, entry = sys.argv[1], sys.argv[2]
try:
    with zipfile.ZipFile(apk) as z:
        try:
            item = z.getinfo(entry)
        except KeyError:
            print("MISSING")
            sys.exit(0)
        with z.open(entry) as handle:
            head = handle.read(4)
    print("OK {0} {1}".format(item.file_size, head.hex()))
except Exception:
    print("ERROR")
PY
)" || {
        echo "ERROR: $label could not inspect $apk." >&2
        return 1
    }
    case "$info" in
        MISSING)
            echo "ERROR: $label missing exact ZIP entry '$entry' in $apk." >&2
            return 1
            ;;
        ERROR|"")
            echo "ERROR: $label could not read '$entry' in $apk." >&2
            return 1
            ;;
    esac
    local size magic
    size="$(printf '%s\n' "$info" | awk '{ print $2 }')"
    magic="$(printf '%s\n' "$info" | awk '{ print $3 }')"
    if [[ -z "$size" || "$size" -le 0 ]]; then
        echo "ERROR: $label has empty ZIP entry '$entry' in $apk." >&2
        return 1
    fi
    if [[ "$magic" != "7f454c46" ]]; then
        echo "ERROR: $label ZIP entry '$entry' in $apk is not ELF (magic=$magic)." >&2
        return 1
    fi
    echo "OK: $label verified: $apk :: $entry ($size bytes, ELF)"
}
