#!/bin/sh
# Stage and package the QuickJS Soundboard app on the build host.
#
#   sh apps/soundboard/build.sh                    # stage + package under build/apps
#   OUT=/tmp/apps sh apps/soundboard/build.sh      # another output directory
#   PACKAGE=0 sh apps/soundboard/build.sh          # stage the directory only
#   CAPSULAD_CLI=build/bin/capsulad-cli sh apps/soundboard/build.sh
#
# The package contains the manifest, main.js, this guide and the custom settings
# UI. Sounds are user assets under /assets, never package files; the test suite
# stays out of the installed artifact.
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$HERE/../.." && pwd)
APP_ID=org.capsulad.soundboard
OUT=${OUT:-"$ROOT/build/apps"}
PACKAGE=${PACKAGE:-1}
CLI=${CAPSULAD_CLI:-"$ROOT/build/bin/capsulad-cli"}
STAGE="$OUT/$APP_ID"

say() { printf '%s\n' "$*" >&2; }
die() { say "soundboard: $*"; exit 1; }

case "$PACKAGE" in
  0|1) ;;
  *) die "PACKAGE must be 0 or 1 (got '$PACKAGE')" ;;
esac

[ -f "$HERE/manifest.toml" ] || die "missing $HERE/manifest.toml"
[ -f "$HERE/main.js" ] || die "missing $HERE/main.js"
[ -f "$HERE/README.md" ] || die "missing $HERE/README.md"
[ -f "$HERE/ui/settings.html" ] || die "missing $HERE/ui/settings.html"

if [ "$PACKAGE" = "1" ]; then
  case "$CLI" in
    /*) ;;
    *) CLI="$PWD/$CLI" ;;
  esac
  [ -x "$CLI" ] ||
    die "capsulad-cli not found at $CLI; run 'make capsulad-cli' or set CAPSULAD_CLI"
fi

rm -rf "$STAGE"
rm -f "$OUT/$APP_ID"-*.tar.gz
mkdir -p "$STAGE/ui"
cp "$HERE/manifest.toml" "$HERE/main.js" "$HERE/README.md" "$STAGE/"
cp "$HERE/ui/settings.html" "$STAGE/ui/settings.html"
say "soundboard: staged $STAGE"

if [ "$PACKAGE" = "1" ]; then
  ( cd "$OUT" && "$CLI" app package "$APP_ID" ) || die "packaging failed with $CLI"
  say "soundboard: archive $OUT/$APP_ID-*.tar.gz"
else
  say "soundboard: PACKAGE=0, no archive written"
fi
