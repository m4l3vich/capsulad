#!/bin/sh
# Development-only media provider for the web console.
#
# Builds internal/daemon/testdata/mediafixture for the host and installs it as
# the app `dev.media` in a simulator data directory, so the Overview media bar
# has a real standard MediaPlayerService provider to render and control. The
# fixture starts stopped; Play/Pause/Next/Previous then drive it through the
# same routed contract the Web UI and the physical keys use.
#
# It is a test fixture packaged for development, never a product application:
# do not install it on the speaker and do not copy it into a release.
#
# Usage: sh tools/dev-media.sh [DATA_DIR]        (default build/dev-web)
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DATA_DIR=${1:-build/dev-web}
APP_ID=dev.media
CLI=$ROOT/build/bin/capsulad-cli
PKG=$ROOT/build/dev-media/pkg-$APP_ID

[ -x "$CLI" ] || { echo "build the CLI first: make capsulad capsulad-cli" >&2; exit 1; }
[ -d "$DATA_DIR" ] || { echo "no simulator data dir at $DATA_DIR; create one with: capsulad-cli -data-dir $DATA_DIR auth init -hostname localhost -ip 127.0.0.1" >&2; exit 1; }

mkdir -p "$PKG"
if [ "$(go env GOOS)" = darwin ]; then
    (cd "$ROOT" && go build -ldflags '-linkmode=external' -o "$PKG/mediafixture" ./internal/daemon/testdata/mediafixture)
else
    (cd "$ROOT" && go build -o "$PKG/mediafixture" ./internal/daemon/testdata/mediafixture)
fi

# The simulator accepts host binaries regardless of the declared target, so the
# manifest keeps the device's aarch64/Linux values. No interface descriptor is
# declared: discovery advertises the service without the optional protoset.
cat >"$PKG/manifest.toml" <<'EOF'
id = "dev.media"
name = "Dev media"
version = "0.1.0"
description = "Development-only media provider for the Web UI. Runs internal/daemon/testdata/mediafixture on the simulator and publishes the standard MediaPlayerService contract."
background = true
start_on_boot = true
restart_on_failure = true
single_instance = true
provides = ["media_player"]

[entrypoint]
runtime = "native"
path = "mediafixture"
architecture = "aarch64"
abi = "linux-gnu"

[[interfaces]]
service = "capsulad.contracts.media.v1.MediaPlayerService"
version = "1.0.0"
EOF

# Reinstalling replaces the package atomically, but the CLI refuses while a run
# is alive, so stop any existing run first.
if [ -d "$DATA_DIR/apps/$APP_ID" ]; then
    for run in $("$CLI" -data-dir "$DATA_DIR" app runs "$APP_ID" 2>/dev/null |
                 sed -n "s/^$APP_ID  *\([^ ]*\) .*/\1/p"); do
        "$CLI" -data-dir "$DATA_DIR" app stop "$run" >/dev/null 2>&1 || true
    done
    "$CLI" -data-dir "$DATA_DIR" app remove "$APP_ID"
fi

"$CLI" -data-dir "$DATA_DIR" app install "$PKG"

cat <<EOF

Restart the simulator daemon so it rescans the package store:

  capsulad -platform simulator -data-dir $DATA_DIR \\
    -dev-admin $DATA_DIR/run/dev.sock -web-addr 127.0.0.1:8443

$APP_ID starts on boot. The Overview media bar then shows Dev media →
local.player (Aurora — Capsula, stopped); Play/Pause/Next/Previous work
through the same routed contract as the physical panel keys.
EOF
