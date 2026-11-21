#!/bin/sh
# Build and package the Home Assistant integration app.
#
#   sh apps/homeassistant/build.sh              # build + package
#   OUT=/tmp/apps sh apps/homeassistant/build.sh
#   PACKAGE=0 sh apps/homeassistant/build.sh    # stage the directory only
#   GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc sh apps/homeassistant/build.sh
#
# The binary is compiled on the build host and packaged with the repository
# CLI; a cross build therefore needs a host-executable packager, which is what
# CAPSULAD_CLI overrides. GOOS/GOARCH/CGO_ENABLED/CC follow the dumbassistant
# convention: the default is the host, and a device build needs a matching C
# cross compiler.
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$HERE/../.." && pwd)
APP_ID=org.capsulad.homeassistant
OUT=${OUT:-"$ROOT/build/apps"}
PACKAGE=${PACKAGE:-1}
GO=${GO:-go}
CLI=${CAPSULAD_CLI:-}

say() { printf '%s\n' "$*" >&2; }
die() { say "homeassistant: $*"; exit 1; }

[ -f "$ROOT/sdk/go/go.mod" ] && [ -d "$ROOT/sdk/go/capsulad" ] ||
  die "Go SDK not found at $ROOT/sdk/go; restore sdk/go or set up the repository root"

# The native runtime is injected by the launcher; the app never bundles it. A
# package that cannot run is not worth producing, so refuse when it is absent.
LIB_DIR=${CAPSULAD_NATIVE_LIB_DIR:-"$ROOT/build/lib"}
[ -f "$LIB_DIR/capsulad-native.json" ] ||
  die "native runtime metadata missing at $LIB_DIR/capsulad-native.json; build it with 'make native' (or set CAPSULAD_NATIVE_LIB_DIR)"
ls "$LIB_DIR"/libcapsulad* >/dev/null 2>&1 ||
  die "no libcapsulad shared library in $LIB_DIR; build it with 'make native' (or set CAPSULAD_NATIVE_LIB_DIR)"

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
    case "$CLI" in
      /*) ;;
      *) CLI="$PWD/$CLI" ;;
    esac
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
  say "homeassistant: building the host packager $CLI"
  ( cd "$ROOT" && GOOS="$HOST_GOOS" GOARCH="$HOST_GOARCH" CGO_ENABLED=1 \
      $GO build -trimpath -ldflags "-s -w $HOST_LDFLAGS" -o "$CLI" ./cmd/capsulad-cli ) ||
    die "failed to build the host capsulad-cli packager (set CAPSULAD_CLI to an existing CLI)"
}

resolve_cli

dir="$OUT/$APP_ID"
rm -rf "$dir"
rm -f "$OUT/$APP_ID"-*.tar.gz
mkdir -p "$dir/proto" "$dir/docs"

say "homeassistant: building for $GOOS/$GOARCH (cgo=$CGO_ENABLED cc=$CC)"
( cd "$HERE" && GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED="$CGO_ENABLED" CC="$CC" \
    $GO build -trimpath -ldflags "$LDFLAGS" -o "$dir/homeassistant" ./cmd/homeassistant ) ||
  die "native build failed; a cross build needs a matching C compiler/sysroot (CC=...)"

cp "$HERE/manifest.toml" "$dir/manifest.toml"
# The package root README.md is the operator-facing user guide shown in the
# Web UI; it is not the interface documentation beside the descriptor.
cp "$HERE/README.md" "$dir/README.md"
# Stamp the package ABI for the target libc (source manifest stays the default).
if [ "${GOOS:-}" = linux ]; then
    sh "$HERE/../../tools/stage-manifest-abi.sh" "$dir/manifest.toml" "${TARGET_ABI:-linux-gnu}"
fi
cp "$HERE/proto/homeassistant.proto" "$dir/proto/homeassistant.proto"
cp "$HERE/proto/homeassistant.protoset" "$dir/proto/homeassistant.protoset"
cp "$HERE/docs/bridge.md" "$dir/docs/bridge.md"
say "homeassistant: staged $dir"

if [ "$PACKAGE" = "1" ]; then
if [ "${GOOS:-}" = linux ]; then
    staged_bin=$(find "$dir" -maxdepth 1 -type f -perm -u+x | head -1)
    [ -n "$staged_bin" ] || { echo "error: no staged executable to ABI-check" >&2; exit 1; }
    sh "$HERE/../../tools/check-elf-interp.sh" "$staged_bin" "${TARGET_ABI:-linux-gnu}"
fi
  if [ "${GOOS:-}" = linux ]; then
    ( cd "$OUT" && "$CLI" app package "$APP_ID" -target-abi "${TARGET_ABI:-linux-gnu}" -max-glibc "${CAPSULAD_MAX_GLIBC:-}" -target-arch "${CAPSULAD_TARGET_ARCH:-}" )
  else
    ( cd "$OUT" && "$CLI" app package "$APP_ID" )
  fi ||
    die "packaging failed with $CLI; set CAPSULAD_CLI to a host-executable CLI"
  say "homeassistant: package and archive under $OUT"
  say "  install: capsulad-cli app install $OUT/$APP_ID-*.tar.gz"
else
  say "homeassistant: staged directory only (PACKAGE=0, no archive written)"
fi
