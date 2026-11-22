#!/bin/sh
# Build the complete device core for one libc:
#   capsulad, capsulad-cli, libcapsulad + metadata, QuickJS and MicroPython.
#
# usage: tools/build-core.sh gnu|musl release|debug
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
LIBC=${1:-gnu}
PROFILE=${2:-release}

case "$LIBC" in
gnu)
    TARGET_ABI=linux-gnu
    NATIVE_TARGET=aarch64-unknown-linux-gnu
    MAX_GLIBC=2.28
    ;;
musl)
    TARGET_ABI=linux-musl
    NATIVE_TARGET=aarch64-unknown-linux-musl
    MAX_GLIBC=
    ;;
*)
    echo "build-core: LIBC must be gnu or musl" >&2
    exit 2
    ;;
esac
case "$PROFILE" in
release | debug) ;;
*)
    echo "build-core: PROFILE must be release or debug" >&2
    exit 2
    ;;
esac

VERSION=${VERSION:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null |
    sed -e 's/^v//' -e 's/^apps-//')}
[ -n "$VERSION" ] || VERSION=0.1.0-dev

BUILDROOT="/src/build/$LIBC/$PROFILE"
CACHE=${XDG_CACHE_HOME:-$HOME/.cache}/capsulad-build
mkdir -p "$CACHE"

IMAGE=$(sh "$ROOT/tools/build-image.sh" "$LIBC")

docker run --rm --platform linux/arm64 \
    --user "$(id -u):$(id -g)" \
    -v "$ROOT:/src" -w /src \
    -v "$CACHE:/cache" \
    -e HOME=/tmp \
    -e GIT_CONFIG_COUNT=1 \
    -e GIT_CONFIG_KEY_0=safe.directory \
    -e GIT_CONFIG_VALUE_0=/src \
    -e GOCACHE=/cache/go \
    -e GOMODCACHE=/cache/go-mod \
    -e CARGO_HOME=/cache/cargo \
    -e RUSTUP_HOME=/usr/local/rustup \
    -e VERSION="$VERSION" \
    -e LIBC="$LIBC" \
    -e PROFILE="$PROFILE" \
    -e TARGET_ABI="$TARGET_ABI" \
    -e MAX_GLIBC="$MAX_GLIBC" \
    -e NATIVE_TARGET="$NATIVE_TARGET" \
    -e BUILDROOT="$BUILDROOT" \
    "$IMAGE" sh -ec '
        make _core GOOS=linux GOARCH=arm64 \
            PROFILE="$PROFILE" \
            TARGET_ABI="$TARGET_ABI" \
            MAX_GLIBC="$MAX_GLIBC" \
            NATIVE_TARGET="$NATIVE_TARGET" \
            BUILDROOT="$BUILDROOT" \
            BINDIR="$BUILDROOT/bin" \
            LIBDIR="$BUILDROOT/lib" \
            INCLUDEDIR="$BUILDROOT/include"
        "$BUILDROOT/bin/capsulad" -version | grep -F "capsulad $VERSION"
        "$BUILDROOT/bin/capsulad-cli" version | grep -F "capsulad-cli $VERSION"
    '

printf 'core: %s/%s -> %s\n' "$LIBC" "$PROFILE" "$ROOT/build/$LIBC/$PROFILE"
