#!/bin/sh
# Stage and package the Simon Says QuickJS app on the build host.
#
#   sh apps/simon/build.sh                    # stage + package under build/apps
#   OUT=/tmp/apps sh apps/simon/build.sh      # another output directory
#   PACKAGE=0 sh apps/simon/build.sh          # stage the directory, write no archive
#   CAPSULAD_CLI=build/bin/capsulad-cli sh apps/simon/build.sh
#
# The package directory contains manifest.toml, main.js and the app's root
# README.md; the test suite stays out of the installed artifact. Packaging runs
# the repository CLI, which validates the manifest and the artifact limits.
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$HERE/../.." && pwd)
APP_ID=org.capsulad.simon
OUT=${OUT:-"$ROOT/build/apps"}
PACKAGE=${PACKAGE:-1}
CLI=${CAPSULAD_CLI:-"$ROOT/build/bin/capsulad-cli"}
STAGE="$OUT/$APP_ID"

say() { printf '%s\n' "$*" >&2; }
die() { say "simon: $*"; exit 1; }

case "$PACKAGE" in
  0|1) ;;
  *) die "PACKAGE must be 0 or 1 (got '$PACKAGE')" ;;
esac

[ -f "$HERE/manifest.toml" ] || die "missing $HERE/manifest.toml"
[ -f "$HERE/main.js" ] || die "missing $HERE/main.js"
[ -f "$HERE/README.md" ] || die "missing $HERE/README.md"

if [ "$PACKAGE" = "1" ]; then
  [ -x "$CLI" ] ||
    die "capsulad-cli not found at $CLI; run 'make capsulad-cli' or set CAPSULAD_CLI"
fi

rm -rf "$STAGE"
rm -f "$OUT/$APP_ID"-*.tar.gz
mkdir -p "$STAGE"
cp "$HERE/manifest.toml" "$HERE/main.js" "$HERE/README.md" "$STAGE/"
say "simon: staged $STAGE"

if [ "$PACKAGE" = "1" ]; then
  ( cd "$OUT" && "$CLI" app package "$APP_ID" ) || die "packaging failed with $CLI"
  say "simon: archive $OUT/$APP_ID-*.tar.gz"
else
  say "simon: PACKAGE=0, no archive written"
fi
