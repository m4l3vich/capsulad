#!/bin/sh
# Stage and package the PA speaker app.
#
#   sh apps/pa/build.sh                # stage build/apps/org.capsulad.pa and package it
#   OUT=/tmp/pa sh apps/pa/build.sh    # change the output directory
#   PACKAGE=0 sh apps/pa/build.sh      # stage the package directory only
#
# The app is a MicroPython script package: nothing is compiled and no native
# runtime is needed to build it. Packaging uses the repository CLI, which must
# run on the build host; CAPSULAD_CLI=/path/to/capsulad-cli overrides the search.
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$HERE/../.." && pwd)
OUT=${OUT:-"$ROOT/build/apps"}
PACKAGE=${PACKAGE:-1}
CLI=${CAPSULAD_CLI:-}
APP_ID=org.capsulad.pa
PKG="$OUT/$APP_ID"

say() { printf '%s\n' "$*" >&2; }
die() { say "pa: $*"; exit 1; }

[ -f "$HERE/manifest.toml" ] || die "missing $HERE/manifest.toml"
[ -f "$HERE/main.py" ] || die "missing $HERE/main.py"
[ -f "$HERE/proto/pa.protoset" ] || die "missing $HERE/proto/pa.protoset"

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
  for candidate in "$ROOT/build/bin/capsulad-cli" "$ROOT/build/bin/capsulad-cli-host"; do
    if [ -x "$candidate" ] && "$candidate" version >/dev/null 2>&1; then
      CLI="$candidate"
      return 0
    fi
  done
  die "no usable capsulad-cli; run 'make capsulad capsulad-cli' or set CAPSULAD_CLI"
}

resolve_cli

rm -rf "$PKG"
mkdir -p "$PKG/proto" "$PKG/docs"
cp "$HERE/manifest.toml" "$HERE/main.py" "$PKG/"
cp "$HERE/proto/pa.proto" "$PKG/proto/pa.proto"
cp "$HERE/docs/speaker.md" "$PKG/docs/speaker.md"

# The descriptor is committed so the package builds without protoc; regenerate
# it when the schema changes and protoc is available, then prove it names the
# service the manifest declares.
if command -v protoc >/dev/null 2>&1; then
  protoc -I "$HERE/proto" --include_imports \
    --descriptor_set_out="$PKG/proto/pa.protoset" "$HERE/proto/pa.proto"
  protoc --decode=google.protobuf.FileDescriptorSet google/protobuf/descriptor.proto \
    < "$PKG/proto/pa.protoset" | grep -q 'name: "SpeakerService"' ||
    die "proto/pa.protoset does not declare SpeakerService"
else
  cp "$HERE/proto/pa.protoset" "$PKG/proto/pa.protoset"
  say "pa: protoc not found; packaging the committed proto/pa.protoset"
fi

if [ "$PACKAGE" = "1" ]; then
  ( cd "$OUT" && "$CLI" app package "$APP_ID" )
else
  say "pa: staged $PKG (PACKAGE=0, no archive written)"
fi
