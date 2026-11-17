#!/bin/sh
# Live end-to-end with the real native tuner companion (apps/radio/tuner) and a
# real MP3 station. Proves: the tuner package installs and runs at boot, the
# radio discovers it, Tune/Stop, sustained real-time PCM (the daemon's speaker
# stream keeps accepting chunks), the circle key reaching the radio through the
# standard media contract, and a clean stop.
#
# Needs network access to the station and a built tuner package
# (build/apps/org.capsulad.radio.tuner-*.tar.gz; sh apps/radio/tuner/build.sh).
# The deterministic no-network test is tests/e2e.sh.
set -eu

APP_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
REPO=$(CDPATH= cd -- "$APP_DIR/../.." && pwd)
LIB=${LIB:-"$REPO/build/lib"}
BIN=${BIN:-"$REPO/build/bin"}
CLI=${CLI:-"$BIN/capsulad-cli"}
DAEMON=${DAEMON:-"$BIN/capsulad"}
KEEP=${KEEP:-0}
DATA=${DATA:-}
STATION=${STATION:-1}
TUNER_ARCHIVE=${TUNER_ARCHIVE:-$(ls "$REPO"/build/apps/org.capsulad.radio.tuner-*.tar.gz 2>/dev/null | head -n 1)}

say() { printf '%s\n' "$*" >&2; }
die() { say "e2e-tuner: $*"; exit 1; }

[ -x "$DAEMON" ] || die "missing $DAEMON (run 'make capsulad')"
[ -x "$CLI" ] || die "missing $CLI (run 'make capsulad-cli')"
[ -x "$LIB/capsulad-micropython" ] || die "missing $LIB/capsulad-micropython (run 'make interpreter')"
[ -n "$TUNER_ARCHIVE" ] || die "no tuner package; build it with 'sh apps/radio/tuner/build.sh'"

if [ -z "$DATA" ]; then
  DATA=$(mktemp -d "${TMPDIR:-/tmp}/radio-live.XXXXXX")
fi
LOG="$DATA/daemon.log"
DAEMON_PID=""
RADIO_RUN=""

cleanup() {
  if [ -n "$RADIO_RUN" ]; then
    "$CLI" -data-dir "$DATA" app stop "$RADIO_RUN" >/dev/null 2>&1 || true
  fi
  if [ -n "$DAEMON_PID" ]; then
    kill "$DAEMON_PID" >/dev/null 2>&1 || true
  fi
  sleep 1
  pkill -f "$LIB/capsulad-micropython" >/dev/null 2>&1 || true
  pkill -f "$DATA" >/dev/null 2>&1 || true
  if [ "$KEEP" = "1" ]; then
    say "e2e-tuner: kept data dir $DATA"
  else
    rm -rf "$DATA"
  fi
}
trap cleanup EXIT INT TERM

cli() { "$CLI" -data-dir "$DATA" -native-lib-dir "$LIB" "$@"; }

wait_log() {
  marker=$1
  tries=0
  while ! grep -q "$marker" "$LOG" 2>/dev/null; do
    tries=$((tries + 1))
    [ "$tries" -lt 600 ] || die "timed out waiting for log marker: $marker"
    sleep 0.1
  done
  say "e2e-tuner: ok  $marker"
}

wait_count() {
  marker=$1
  want=$2
  tries=0
  while [ "$(grep -c "$marker" "$LOG" 2>/dev/null || echo 0)" -lt "$want" ]; do
    tries=$((tries + 1))
    [ "$tries" -lt 600 ] || die "timed out waiting for $want x $marker"
    sleep 0.1
  done
  say "e2e-tuner: ok  ${want}x $marker"
}

tap_key() {
  cli dev key play_pause true >/dev/null
  cli dev key play_pause false >/dev/null
}

say "e2e-tuner: data dir $DATA"
say "e2e-tuner: tuner package $TUNER_ARCHIVE"
mkdir -p "$DATA"
cli auth init -password radiotest -hostname localhost -ip 127.0.0.1 >/dev/null
cli app install "$APP_DIR/micropython" >/dev/null
cli app install "$TUNER_ARCHIVE" >/dev/null
say "e2e-tuner: installed the radio and the native tuner"

"$DAEMON" -platform simulator -data-dir "$DATA" -native-lib-dir "$LIB" \
  -dev-admin "$DATA/run/dev.sock" -log-file "$LOG" >"$DATA/daemon.out" 2>&1 &
DAEMON_PID=$!
tries=0
while ! "$DAEMON" -data-dir "$DATA" -health >/dev/null 2>&1; do
  tries=$((tries + 1))
  [ "$tries" -lt 100 ] || die "daemon did not become healthy"
  sleep 0.1
done
say "e2e-tuner: daemon healthy"

wait_log "radio: ready app=org.capsulad.radio"
wait_log "radio-tuner: ready app=org.capsulad.radio.tuner"

cli app start org.capsulad.radio station="$STATION" >/dev/null
wait_log "radio: state buffering"
wait_log "radio: tuning"
wait_log "radio-tuner: session .* accepted station="
wait_log "radio-tuner: session .* playing station="
wait_log "radio: state playing station audio"
wait_log "radio: speaker stream"
# Sustained real-time audio: 50 chunks is about ten seconds at 200 ms each.
wait_log "radio: session .* playing 50 chunks"

# The circle key reaches the radio through the standard media contract.
tap_key
wait_log "radio: state paused paused"
wait_log "radio-tuner: session .* refused by player: paused"
tap_key
wait_log "radio: state buffering resumed"
wait_count "radio-tuner: session .* playing station=" 2

# A live station switch exercises the authorized Stop path: the player asks the
# tuner to drop the session and then tunes the new station.
cli settings app org.capsulad.radio station=2 >/dev/null
wait_log "radio: tuning Radio Paradise Main Mix (station changed)"
wait_log "radio-tuner: session .* stopped (station changed)"
wait_log "radio-tuner: session .* accepted station=\"Radio Paradise Main Mix\""
wait_count "radio: state playing station audio" 2
wait_log "radio: session .* playing 50 chunks"

if grep -q "radio: state error\|radio: error" "$LOG"; then
  grep "radio: state error\|radio: error" "$LOG" >&2
  die "the radio reported an error"
fi
if grep -q "radio-tuner: .*failed" "$LOG"; then
  grep "radio-tuner: .*failed" "$LOG" >&2
  die "the tuner reported a failure"
fi

# Autonomous watchdog: kill the native tuner with no user interaction; the
# radio must leave PLAYING by itself within the liveness window.
TUNER_RUN=$(cli app runs org.capsulad.radio.tuner 2>/dev/null |
  sed -n 's/.*\(run_[A-Za-z0-9][A-Za-z0-9_-]*\).*/\1/p' | head -n 1)
[ -n "$TUNER_RUN" ] || die "no tuner run id"
cli app stop "$TUNER_RUN" >/dev/null
wait_log "radio: playback failed: tuner stopped"
wait_log "radio: state error tuner stopped"
say "e2e-tuner: ok  watchdog left PLAYING after the tuner died, with no user activity"

RADIO_RUN=$(cli app runs org.capsulad.radio 2>/dev/null |
  sed -n 's/.*\(run_[A-Za-z0-9][A-Za-z0-9_-]*\).*/\1/p' | head -n 1)
[ -n "$RADIO_RUN" ] || die "no radio run id"
cli app stop "$RADIO_RUN" >/dev/null
tries=0
while cli app runs org.capsulad.radio 2>/dev/null | grep -q "APP_READY\|APP_STARTING"; do
  tries=$((tries + 1))
  [ "$tries" -lt 100 ] || die "radio did not stop"
  sleep 0.1
done
say "e2e-tuner: ok  radio stopped cleanly"
say "e2e-tuner: PASS (simulator, native tuner, live MP3 station)"
