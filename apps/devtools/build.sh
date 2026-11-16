#!/bin/sh
# Build and package the development-only devtools apps.
#
#   sh apps/devtools/build.sh              # all three apps, each packaged
#   APP=dev.hardware sh apps/devtools/build.sh
#   OUT=/tmp/devapps sh apps/devtools/build.sh
#   PACKAGE=0 sh apps/devtools/build.sh    # stage directories only
#
# Each app is compiled and packaged on the build host: the package directory
# and its <app-id>-<version>.tar.gz land side by side under OUT. Packaging uses
# the repository CLI; a cross build packages with a host-built CLI (override
# with CAPSULAD_CLI=/path/to/capsulad-cli).
#
# The native app uses the Go SDK's cgo loader. GOOS/GOARCH/CC follow the
# dumbassistant convention: the host build needs no cross compiler, while a
# device build must pass a target compiler (for example CC=aarch64-linux-gnu-gcc).
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$HERE/../.." && pwd)
OUT=${OUT:-"$ROOT/build/devapps"}
APP=${APP:-all}
GO=${GO:-go}
PACKAGE=${PACKAGE:-1}
CLI=${CAPSULAD_CLI:-}

say() { printf '%s\n' "$*" >&2; }
die() { say "devapps: $*"; exit 1; }

# Hard prerequisites. The native runtime (libcapsulad + capsulad-native.json) is
# not linked into the app, but the app cannot run without the launcher loading
# it, so refuse to produce a package that looks usable when it is not.
SDK_DIR="$ROOT/sdk/go"
[ -f "$SDK_DIR/go.mod" ] && [ -d "$SDK_DIR/capsulad" ] ||
  die "Go SDK not found at $SDK_DIR; restore sdk/go or set up the repository root"
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

# Packaging always runs on the build host. A cross build therefore needs a CLI
# built for the host, which the repository Makefile also provides.
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
  say "devapps: building the host packager $CLI"
  ( cd "$ROOT" && GOOS="$HOST_GOOS" GOARCH="$HOST_GOARCH" CGO_ENABLED=1 \
      $GO build -trimpath -ldflags "-s -w $HOST_LDFLAGS" -o "$CLI" ./cmd/capsulad-cli ) ||
    die "failed to build the host capsulad-cli packager (set CAPSULAD_CLI to an existing CLI)"
}

# pack ID: the artifact always lands beside the package directory under OUT.
pack() {
  if [ "$PACKAGE" != "1" ]; then
    say "devapps: PACKAGE=0, skipping the $1 archive"
    return 0
  fi
  ( cd "$OUT" && "$CLI" app package "$1" ) ||
    die "packaging $1 failed with $CLI; set CAPSULAD_CLI to a host-executable CLI"
}

want() {
  [ "$APP" = "all" ] || [ "$APP" = "$1" ]
}

[ "$APP" = "all" ] || [ "$APP" = "dev.hardware" ] || [ "$APP" = "dev.quickjs" ] || [ "$APP" = "dev.micropython" ] ||
  die "APP must be all, dev.hardware, dev.quickjs or dev.micropython (got '$APP')"

case "$PACKAGE" in
  0|1) ;;
  *) die "PACKAGE must be 0 or 1 (got '$PACKAGE')" ;;
esac

stage() {
  rm -rf "$OUT/$1"
  rm -f "$OUT/$1"-*.tar.gz
  mkdir -p "$OUT/$1"
}

resolve_cli

if want dev.hardware; then
  src="$HERE/native"
  dir="$OUT/dev.hardware"
  stage dev.hardware
  say "devapps: building dev.hardware for $GOOS/$GOARCH (cgo=$CGO_ENABLED cc=$CC)"
  ( cd "$src" && GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED="$CGO_ENABLED" CC="$CC" \
      $GO build -trimpath -ldflags "$LDFLAGS" -o "$dir/devtools" ./cmd/devtools ) ||
    die "native build failed; a cross build needs a matching C compiler/sysroot (CC=...)"
  cp "$src/manifest.toml" "$dir/manifest.toml"
  mkdir -p "$dir/proto"
  if command -v protoc >/dev/null 2>&1; then
    protoc -I "$src/proto" --descriptor_set_out="$dir/proto/devtools.protoset" "$src/proto/devtools.proto" ||
      die "protoc failed to produce devtools.protoset"
  else
    [ -f "$src/proto/devtools.protoset" ] ||
      die "protoc is unavailable and $src/proto/devtools.protoset is missing; install protoc or commit the descriptor"
    say "devapps: protoc not found; using the committed devtools.protoset"
    cp "$src/proto/devtools.protoset" "$dir/proto/devtools.protoset"
  fi
  cp "$src/proto/devtools.proto" "$dir/proto/devtools.proto"
  say "devapps: staged $dir"
  pack dev.hardware
fi

if want dev.quickjs; then
  stage dev.quickjs
  cp "$HERE/quickjs/manifest.toml" "$OUT/dev.quickjs/manifest.toml"
  cp "$HERE/quickjs/main.js" "$OUT/dev.quickjs/main.js"
  say "devapps: staged $OUT/dev.quickjs"
  pack dev.quickjs
fi

if want dev.micropython; then
  stage dev.micropython
  cp "$HERE/micropython/manifest.toml" "$OUT/dev.micropython/manifest.toml"
  cp "$HERE/micropython/main.py" "$OUT/dev.micropython/main.py"
  say "devapps: staged $OUT/dev.micropython"
  pack dev.micropython
fi

if [ ! -x "$LIB_DIR/capsulad-quickjs" ] || [ ! -x "$LIB_DIR/capsulad-micropython" ]; then
  say "devapps: note: script hosts are not both installed in $LIB_DIR; run 'make interpreter' before starting dev.quickjs/dev.micropython"
fi

if [ "$PACKAGE" = "1" ]; then
  say "devapps: artifacts and package directories under $OUT"
  say "  install: capsulad-cli app install $OUT/<app-id>-<version>.tar.gz"
else
  say "devapps: package directories under $OUT (PACKAGE=0, no archives written)"
  say "  install: capsulad-cli app install $OUT/<app-id> (development convenience)"
fi