#!/bin/sh
# Build and package the Internet Radio tuner companion.
#
#   sh apps/radio/tuner/build.sh
#   OUT=/tmp/radio-tuner sh apps/radio/tuner/build.sh
#   PACKAGE=0 sh apps/radio/tuner/build.sh          # stage only
#   GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc sh apps/radio/tuner/build.sh
#
# Packaging happens on the build host. The device receives
# org.capsulad.radio.tuner-<version>.tar.gz.
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$HERE/../../.." && pwd)
OUT=${OUT:-"$ROOT/build/apps"}
APP_ID=org.capsulad.radio.tuner
GO=${GO:-go}
PACKAGE=${PACKAGE:-1}
CLI=${CAPSULAD_CLI:-}

say() { printf '%s\n' "$*" >&2; }
die() { say "radio-tuner: $*"; exit 1; }

[ -f "$ROOT/sdk/go/go.mod" ] || die "Go SDK not found at $ROOT/sdk/go"
LIB_DIR=${CAPSULAD_NATIVE_LIB_DIR:-"$ROOT/build/lib"}
[ -f "$LIB_DIR/capsulad-native.json" ] ||
  die "native runtime metadata missing at $LIB_DIR/capsulad-native.json; run 'make native'"
ls "$LIB_DIR"/libcapsulad* >/dev/null 2>&1 ||
  die "no libcapsulad shared library in $LIB_DIR; run 'make native'"

GOOS=${GOOS:-$($GO env GOOS)}
GOARCH=${GOARCH:-$($GO env GOARCH)}
CGO_ENABLED=${CGO_ENABLED:-1}
CC=${CC:-$($GO env CC)}
case "$GOOS" in
  darwin) LDFLAGS=${LDFLAGS:-"-s -w -linkmode=external"} ;;
  *)      LDFLAGS=${LDFLAGS:-"-s -w"} ;;
esac

HOST_GOOS=$($GO env GOHOSTOS)
HOST_GOARCH=$($GO env GOHOSTARCH)
HOST_LDFLAGS=
if [ "$HOST_GOOS" = darwin ]; then
  HOST_LDFLAGS="-linkmode=external"
fi

resolve_cli() {
  [ "$PACKAGE" = "1" ] || return 0
  if [ -n "$CLI" ]; then
    case "$CLI" in /*) ;; *) CLI="$PWD/$CLI" ;; esac
    [ -x "$CLI" ] || die "CAPSULAD_CLI=$CLI is not executable"
    return 0
  fi
  if [ "$GOOS/$GOARCH" = "$HOST_GOOS/$HOST_GOARCH" ] && [ -x "$ROOT/build/bin/capsulad-cli" ] &&
     "$ROOT/build/bin/capsulad-cli" version >/dev/null 2>&1; then
    CLI="$ROOT/build/bin/capsulad-cli"
    return 0
  fi
  CLI="$ROOT/build/bin/capsulad-cli-host"
  if [ -x "$CLI" ]; then
    return 0
  fi
  say "radio-tuner: building the host packager $CLI"
  ( cd "$ROOT" && GOOS="$HOST_GOOS" GOARCH="$HOST_GOARCH" CGO_ENABLED=1 \
      $GO build -trimpath -ldflags "-s -w $HOST_LDFLAGS" -o "$CLI" ./cmd/capsulad-cli ) ||
    die "failed to build the host capsulad-cli packager"
}

stage() {
  rm -rf "$OUT/$APP_ID"
  rm -f "$OUT/$APP_ID"-*.tar.gz
  mkdir -p "$OUT/$APP_ID/bin"
}

resolve_cli
stage
say "radio-tuner: building $APP_ID for $GOOS/$GOARCH (cgo=$CGO_ENABLED cc=$CC)"
( cd "$HERE" && GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED="$CGO_ENABLED" CC="$CC" \
    $GO build -trimpath -ldflags "$LDFLAGS" -o "$OUT/$APP_ID/bin/tuner" ./cmd/tuner ) ||
  die "build failed; a device build needs a matching C compiler (CC=aarch64-linux-gnu-gcc)"
cp "$HERE/manifest.toml" "$OUT/$APP_ID/manifest.toml"
# Stamp the package ABI for the target libc (source manifest stays the default).
if [ "${GOOS:-}" = linux ]; then
    sh "$HERE/../../../tools/stage-manifest-abi.sh" "$OUT/$APP_ID/manifest.toml" "${TARGET_ABI:-linux-gnu}"
fi
say "radio-tuner: staged $OUT/$APP_ID"

if [ "$PACKAGE" = "1" ]; then
if [ "${GOOS:-}" = linux ]; then
    staged_bin=$(find "$OUT/$APP_ID" -maxdepth 1 -type f -perm -u+x | head -1)
    [ -n "$staged_bin" ] || { echo "error: no staged executable to ABI-check" >&2; exit 1; }
    sh "$HERE/../../../tools/check-elf-interp.sh" "$staged_bin" "${TARGET_ABI:-linux-gnu}"
fi
  if [ "${GOOS:-}" = linux ]; then
    ( cd "$OUT" && "$CLI" app package "$APP_ID" -target-abi "${TARGET_ABI:-linux-gnu}" -max-glibc "${CAPSULAD_MAX_GLIBC:-}" -target-arch "${CAPSULAD_TARGET_ARCH:-}" )
  else
    ( cd "$OUT" && "$CLI" app package "$APP_ID" )
  fi || die "packaging failed with $CLI"
  say "radio-tuner: install with: capsulad-cli app install $OUT/$APP_ID-<version>.tar.gz"
else
  say "radio-tuner: PACKAGE=0, no archive written"
fi
