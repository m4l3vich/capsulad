#!/bin/sh
# Build and package the Internet Radio app (single native package).
#
#   sh apps/radio/build.sh
#   OUT=/tmp/radio sh apps/radio/build.sh
#   PACKAGE=0 sh apps/radio/build.sh          # stage only
#   GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc sh apps/radio/build.sh
#
# The binary is compiled and packaged on the build host. The device receives
# org.capsulad.radio-<version>.tar.gz; it needs the shared native runtime that
# the launcher injects (make native).
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$HERE/../.." && pwd)
OUT=${OUT:-"$ROOT/build/apps"}
APP_ID=org.capsulad.radio
PKG="$OUT/$APP_ID"
GO=${GO:-go}
PACKAGE=${PACKAGE:-1}
CLI=${CAPSULAD_CLI:-}

say() { printf '%s\n' "$*" >&2; }
die() { say "radio: $*"; exit 1; }

[ -f "$ROOT/sdk/go/go.mod" ] || die "Go SDK not found at $ROOT/sdk/go"
[ -f "$HERE/manifest.toml" ] || die "missing $HERE/manifest.toml"
[ -f "$HERE/ui/settings.html" ] || die "missing $HERE/ui/settings.html"
[ -f "$HERE/README.md" ] || die "missing $HERE/README.md (the packaged app guide)"

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
  say "radio: building the host packager $CLI"
  ( cd "$ROOT" && GOOS="$HOST_GOOS" GOARCH="$HOST_GOARCH" CGO_ENABLED=1 \
      $GO build -trimpath -ldflags "-s -w $HOST_LDFLAGS" -o "$CLI" ./cmd/capsulad-cli ) ||
    die "failed to build the host capsulad-cli packager"
}

resolve_cli
rm -rf "$PKG"
rm -f "$OUT/$APP_ID"-*.tar.gz
mkdir -p "$PKG/bin" "$PKG/ui" "$PKG/proto"

say "radio: building $APP_ID for $GOOS/$GOARCH (cgo=$CGO_ENABLED cc=$CC)"
( cd "$HERE" && GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED="$CGO_ENABLED" CC="$CC" \
    $GO build -trimpath -ldflags "$LDFLAGS" -o "$PKG/bin/radio" ./cmd/radio ) ||
  die "build failed; a device build needs a matching C compiler (CC=aarch64-linux-gnu-gcc)"

cp "$HERE/manifest.toml" "$PKG/manifest.toml"
cp "$HERE/README.md" "$PKG/README.md"
cp "$HERE/ui/settings.html" "$PKG/ui/settings.html"
cp "$HERE/proto/media.protoset" "$PKG/proto/media.protoset"
cp "$HERE/proto/media.md" "$PKG/proto/media.md"
if [ "${GOOS:-}" = linux ]; then
  sh "$ROOT/tools/stage-manifest-abi.sh" "$PKG/manifest.toml" "${TARGET_ABI:-linux-gnu}"
fi
say "radio: staged $PKG"

if [ "$PACKAGE" = "1" ]; then
  if [ "${GOOS:-}" = linux ]; then
    [ -x "$PKG/bin/radio" ] || { echo "error: no staged executable at $PKG/bin/radio to ABI-check" >&2; exit 1; }
    sh "$ROOT/tools/check-elf-interp.sh" "$PKG/bin/radio" "${TARGET_ABI:-linux-gnu}"
    ( cd "$OUT" && "$CLI" app package "$APP_ID" -target-abi "${TARGET_ABI:-linux-gnu}" \
        -max-glibc "${CAPSULAD_MAX_GLIBC:-}" -target-arch "${CAPSULAD_TARGET_ARCH:-}" )
  else
    ( cd "$OUT" && "$CLI" app package "$APP_ID" )
  fi || die "packaging failed with $CLI"
  say "radio: install with: capsulad-cli app install $OUT/$APP_ID-<version>.tar.gz"
else
  say "radio: PACKAGE=0, no archive written"
fi