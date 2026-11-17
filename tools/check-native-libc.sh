#!/bin/sh
# Verify a native runtime's libc from its ELF dynamic section.
#
# usage: check-native-libc.sh LIBRARY musl|gnu
#
# The bundle gate must not guess from a string match inside the file: a shared
# library carries no program interpreter, so the evidence is the DT_NEEDED
# entries in its dynamic section. readelf is preferred; Apple's/LLVM objdump is
# the portable fallback and understands ELF on macOS. A library with no libc
# dependency at all is a static link and is rejected: a musl or glibc app cannot
# load it as a shared runtime.
set -eu

lib=${1:-}
want=${2:-}
[ -n "$lib" ] && [ -n "$want" ] || { echo "usage: $0 LIBRARY musl|gnu" >&2; exit 2; }
[ -f "$lib" ] || { echo "error: $lib is missing" >&2; exit 2; }

dynamic=""
if command -v readelf >/dev/null 2>&1 && readelf -d "$lib" >/dev/null 2>&1; then
    dynamic=$(readelf -d "$lib" 2>/dev/null || true)
elif command -v objdump >/dev/null 2>&1 && objdump -p "$lib" >/dev/null 2>&1; then
    dynamic=$(objdump -p "$lib" 2>/dev/null || true)
else
    echo "error: cannot inspect $lib: install readelf (binutils) or an ELF-capable objdump" >&2
    exit 3
fi

case "$want" in
musl) pattern='libc\.musl|ld-musl' ;;
gnu) pattern='libc\.so\.6' ;;
*)
    echo "error: unknown libc class $want (want musl or gnu)" >&2
    exit 2
    ;;
esac

if printf '%s\n' "$dynamic" | grep -F 'NEEDED' | grep -Eq "$pattern"; then
    echo "libc=$want"
    exit 0
fi

if printf '%s\n' "$dynamic" | grep -F 'NEEDED' | grep -Eq 'libc'; then
    echo "error: $lib is linked against a different libc than $want:" >&2
    printf '%s\n' "$dynamic" | grep -E 'NEEDED' >&2
    exit 1
fi

echo "error: $lib declares no libc dependency (static link); a $want userland cannot load it as a shared runtime. Rebuild with -C target-feature=-crt-static." >&2
exit 1
