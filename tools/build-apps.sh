#!/bin/sh
# Build the ten first-party app archives for one libc.
#
# AirPlay is built by its pinned buster/Alpine container recipe; the other nine
# run inside the libc builder. The result is a flat set of archives under
# build/<libc>/<profile>/apps.
#
# usage: tools/build-apps.sh gnu|musl release|debug
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
    echo "build-apps: LIBC must be gnu or musl" >&2
    exit 2
    ;;
esac
case "$PROFILE" in
release | debug) ;;
*)
    echo "build-apps: PROFILE must be release or debug" >&2
    exit 2
    ;;
esac

BUILDROOT="/src/build/$LIBC/$PROFILE"
APPS="$ROOT/build/$LIBC/$PROFILE/apps"
CACHE=${XDG_CACHE_HOME:-$HOME/.cache}/capsulad-build
mkdir -p "$APPS" "$CACHE"

# AirPlay's target recipe owns its toolchain and FFmpeg build. It must run on
# the host because it invokes Docker itself.
echo "apps: AirPlay ($LIBC)" >&2
OUT="$APPS/target" TARGET_ABI="$TARGET_ABI" \
    sh "$ROOT/apps/airplay/target/build-target.sh"
rm -f "$APPS"/org.capsulad.airplay-*.tar.gz
cp "$APPS/target/$TARGET_ABI"/org.capsulad.airplay-*.tar.gz "$APPS/"

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
    -e PROFILE="$PROFILE" \
    -e TARGET_ABI="$TARGET_ABI" \
    -e MAX_GLIBC="$MAX_GLIBC" \
    -e NATIVE_TARGET="$NATIVE_TARGET" \
    -e BUILDROOT="$BUILDROOT" \
    -e CAPSULAD_NATIVE_LIB_DIR="$BUILDROOT/lib" \
    "$IMAGE" sh -ec '
        make _apps GOOS=linux GOARCH=arm64 CC=gcc \
            PROFILE="$PROFILE" \
            TARGET_ABI="$TARGET_ABI" \
            MAX_GLIBC="$MAX_GLIBC" \
            NATIVE_TARGET="$NATIVE_TARGET" \
            BUILDROOT="$BUILDROOT" \
            BINDIR="$BUILDROOT/bin" \
            LIBDIR="$BUILDROOT/lib" \
            INCLUDEDIR="$BUILDROOT/include" \
            APPDIR="$BUILDROOT/apps" \
            CAPSULAD_NATIVE_LIB_DIR="$CAPSULAD_NATIVE_LIB_DIR"
    '

count=0
for f in "$APPS"/org.capsulad.*-*.tar.gz; do
    [ -f "$f" ] || continue
    count=$((count + 1))
done
if [ "$count" -ne 10 ]; then
    echo "build-apps: expected 10 app archives in $APPS, found $count" >&2
    ls -1 "$APPS" >&2 || true
    exit 1
fi
printf 'apps: %s/%s -> %s (10 archives)\n' "$LIBC" "$PROFILE" "$APPS"
