#!/bin/sh
# Refuse a Linux app/native build that would use the wrong C toolchain.
# A native Linux build (GOARCH == GOHOSTARCH) may use the distro's cc/gcc;
# a cross build must name a compiler for the target architecture and libc.
# usage: check-target-cc.sh GOOS GOARCH GOHOSTOS GOHOSTARCH TARGET_ABI [CC]
set -eu

goos=${1:-}
goarch=${2:-}
host=${3:-}
hostarch=${4:-}
abi=${5:-}
cc=${6:-}

[ "$goos" = linux ] || exit 0

if [ "$host" = linux ] && [ "$goarch" = "$hostarch" ]; then
    case "$abi" in
    linux-musl)
        case "$cc" in
        *gnu* | *glibc*)
            echo "error: TARGET_ABI=linux-musl cannot use a glibc C compiler ($cc)." >&2
            exit 1
            ;;
        esac
        ;;
    linux-gnu)
        case "$cc" in
        *musl*)
            echo "error: TARGET_ABI=linux-gnu cannot use a musl C compiler ($cc)." >&2
            exit 1
            ;;
        esac
        ;;
    esac
    exit 0
fi

case "$cc" in
*aarch64* | *arm64* | *x86_64* | *amd64*) ;;
*)
    echo "error: GOOS=linux/$goarch on a $host/$hostarch host needs a target C compiler (set CC, e.g. aarch64-linux-musl-gcc)." >&2
    exit 1
    ;;
esac

case "$abi" in
linux-musl)
    case "$cc" in
    *musl*) ;;
    *)
        echo "error: TARGET_ABI=linux-musl needs a musl C compiler, got CC=$cc" >&2
        exit 1
        ;;
    esac
    ;;
linux-gnu)
    case "$cc" in
    *musl*)
        echo "error: TARGET_ABI=linux-gnu cannot use a musl C compiler ($cc)" >&2
        exit 1
        ;;
    esac
    ;;
esac

exit 0
