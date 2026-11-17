#!/bin/sh
# Verify an executable's ELF program interpreter for a target libc.
# usage: check-elf-interp.sh BINARY linux-gnu|linux-musl [require-dynamic]
# A static binary has no PT_INTERP and is accepted for either libc unless
# require-dynamic is given (a native-SDK app must be able to dlopen the shared
# runtime, and a static musl binary cannot). A dynamic one must name ld-musl-*
# (musl) or ld-linux-* (gnu).
set -eu
bin=${1:-}; abi=${2:-}; mode=${3:-}
[ -f "$bin" ] || { echo "error: $bin is missing" >&2; exit 2; }
case "$abi" in linux-gnu | linux-musl) ;; *) echo "error: unknown ABI '$abi'" >&2; exit 2 ;; esac
interp=""
readelf_ok=""
if command -v readelf >/dev/null 2>&1 && readelf -l "$bin" >/dev/null 2>&1; then
    readelf_ok=1
    interp=$(readelf -l "$bin" 2>/dev/null | sed -n 's|.*interpreter: \(.*\)\]|\1|p' | head -1)
    if [ -z "$interp" ]; then
        # readelf answered and named no interpreter: a static executable or a
        # shared object. No python3 is needed, which matters on target images
        # that carry binutils but no interpreter.
        if readelf -h "$bin" 2>/dev/null | grep -q 'Type:.*EXEC'; then
            interp="STATIC"
        else
            interp="DYN"
        fi
    fi
fi
if [ -z "$interp" ]; then
    if ! command -v python3 >/dev/null 2>&1; then
        echo "error: cannot read PT_INTERP from $bin: install readelf (binutils) or python3" >&2
        exit 3
    fi
    # Parse PT_INTERP from the program header table; a string search inside the
    # file is not evidence and is deliberately not used.
    interp=$(python3 - "$bin" <<'PYEOF'
import struct, sys
data = open(sys.argv[1], 'rb').read()
if data[:4] != b'\x7fELF':
    sys.exit(4)
is64 = data[4] == 2
if is64:
    phoff, = struct.unpack_from('<Q', data, 0x20)
    phentsize, phnum = struct.unpack_from('<HH', data, 0x36)
else:
    phoff, = struct.unpack_from('<I', data, 0x1c)
    phentsize, phnum = struct.unpack_from('<HH', data, 0x2a)
etype, = struct.unpack_from('<H', data, 16)
if phnum == 0 or phentsize == 0:
    print('STATIC' if etype == 2 else 'DYN')
    sys.exit(0)
for i in range(phnum):
    off = phoff + i * phentsize
    p_type, = struct.unpack_from('<I', data, off)
    if p_type != 3:
        continue
    if is64:
        p_offset, = struct.unpack_from('<Q', data, off + 8)
        p_filesz, = struct.unpack_from('<Q', data, off + 32)
    else:
        p_offset, = struct.unpack_from('<I', data, off + 4)
        p_filesz, = struct.unpack_from('<I', data, off + 16)
    if p_offset + p_filesz > len(data):
        sys.exit(5)
    print(data[p_offset:p_offset + p_filesz].rstrip(b'\x00').decode('latin1'))
    sys.exit(0)
print('STATIC' if etype == 2 else 'DYN')
PYEOF
    ) || { echo "error: $bin is not a readable ELF file" >&2; exit 1; }
fi
# These outcomes are produced by either the readelf or the python path, so they
# are handled once, outside the fallback.
if [ "$interp" = "DYN" ]; then
    echo "shared object (no program interpreter)"
    exit 0
fi
if [ "$interp" = "STATIC" ]; then
    if [ "$mode" = "require-dynamic" ]; then
        echo "error: $bin is statically linked; a native-SDK app must load the shared runtime, and a static $abi binary cannot dlopen it" >&2
        exit 1
    fi
    echo "static (no program interpreter)"
    exit 0
fi
case "$abi:$interp" in
linux-musl:*ld-musl-*) echo "interpreter=$interp"; exit 0 ;;
linux-gnu:*ld-linux-*) echo "interpreter=$interp"; exit 0 ;;
*) echo "error: $bin names $interp, which is not a $abi interpreter" >&2; exit 1 ;;
esac
