#!/bin/sh
# Refuse a Linux app/native cross build that would use the host C toolchain.
# usage: check-target-cc.sh GOOS GOARCH GOHOSTOS TARGET_ABI [CC]
set -eu
goos=${1:-}; goarch=${2:-}; host=${3:-}; abi=${4:-}; cc=${5:-}
[ "$goos" = linux ] || exit 0
if [ "$host" = linux ] && [ -z "$cc" ]; then
    exit 0 # a native Linux compiler is the right one
fi
case "$cc" in
*aarch64* | *arm64* | *x86_64* | *amd64*) ;;
*)
    echo "error: GOOS=linux/$goarch on a $host host needs a target C compiler (set CC, e.g. aarch64-linux-musl-gcc)." >&2
    exit 1
    ;;
esac
case "$abi" in
linux-musl) case "$cc" in *musl*) ;; *) echo "error: TARGET_ABI=linux-musl needs a musl C compiler, got CC=$cc" >&2; exit 1 ;; esac ;;
linux-gnu) case "$cc" in *musl*) echo "error: TARGET_ABI=linux-gnu cannot use a musl C compiler ($cc)" >&2; exit 1 ;; esac ;;
esac
exit 0
