#!/bin/sh
# Emit the runtime metadata consumed by capsulad's native launcher.
# Usage: emit-runtime-metadata.sh <output.json> <library-filename>
# <library-filename> is relative to the runtime libdir, e.g. libcapsulad.so.1
# on Linux or libcapsulad.1.dylib on Darwin.
set -eu

if [ "$#" -lt 1 ]; then
    echo "usage: $0 <output.json> [library-filename]" >&2
    exit 2
fi

out=$1
lib=${2:-}
if [ -z "$lib" ]; then
    case "$(uname -s)" in
        Darwin) lib="libcapsulad.1.dylib" ;;
        *) lib="libcapsulad.so.1" ;;
    esac
fi

cat >"$out" <<EOF
{
  "abi_major": 1,
  "abi_minor": 0,
  "library": "$lib"
}
EOF
