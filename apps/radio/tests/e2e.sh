#!/bin/sh
# End-to-end for the Internet Radio app: the real simulator daemon, the real
# MicroPython host, the shipped app and the test tuner fixture
# (tests/fixture-tuner), which pushes synthetic PCM over the same raw-protobuf
# contract a real tuner uses.
#
# Proves: package install, boot start, launch invocation, tuner discovery,
# Tune/Stop, Push backpressure and refusal, the speaker stream, the segment
# presentation, settings-driven station switch, media-provider discovery and a
# clean stop. No network station is contacted.
#
# Needs build/bin/{capsulad,capsulad-cli} and
# build/lib/{libcapsulad.*,capsulad-micropython}. KEEP=1 keeps the data dir.
set -eu

APP_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
REPO=$(CDPATH= cd -- "$APP_DIR/../.." && pwd)
LIB=${LIB:-"$REPO/build/lib"}
BIN=${BIN:-"$REPO/build/bin"}
CLI=${CLI:-"$BIN/capsulad-cli"}
DAEMON=${DAEMON:-"$BIN/capsulad"}
KEEP=${KEEP:-0}
DATA=${DATA:-}

say() { printf '%s\n' "$*" >&2; }
die() { say "e2e: $*"; exit 1; }

[ -x "$DAEMON" ] || die "missing $DAEMON (run 'make capsulad')"
[ -x "$CLI" ] || die "missing $CLI (run 'make capsulad-cli')"
[ -x "$LIB/capsulad-micropython" ] || die "missing $LIB/capsulad-micropython (run 'make interpreter')"
ls "$LIB"/libcapsulad.* >/dev/null 2>&1 || die "missing libcapsulad in $LIB (run 'make native')"

if [ -z "$DATA" ]; then
  DATA=$(mktemp -d "${TMPDIR:-/tmp}/radio-e2e.XXXXXX")
fi
LOG="$DATA/daemon.log"
DAEMON_PID=""
RADIO_RUN=""
TUNER_RUN=""

cleanup() {
  if [ -n "$RADIO_RUN" ]; then
    "$CLI" -data-dir "$DATA" app stop "$RADIO_RUN" >/dev/null 2>&1 || true
  fi
  if [ -n "$TUNER_RUN" ]; then
    "$CLI" -data-dir "$DATA" app stop "$TUNER_RUN" >/dev/null 2>&1 || true
  fi
  if [ -n "$DAEMON_PID" ]; then
    kill "$DAEMON_PID" >/dev/null 2>&1 || true
  fi
  sleep 1
  pkill -f "$LIB/capsulad-micropython" >/dev/null 2>&1 || true
  if [ "$KEEP" = "1" ]; then
    say "e2e: kept data dir $DATA"
  else
    rm -rf "$DATA"
  fi
}
trap cleanup EXIT INT TERM

cli() { "$CLI" -data-dir "$DATA" -native-lib-dir "$LIB" "$@"; }
start_app() { cli app start "$@"; }

wait_log() {
  marker=$1
  tries=0
  while ! grep -q "$marker" "$LOG" 2>/dev/null; do
    tries=$((tries + 1))
    [ "$tries" -lt 400 ] || die "timed out waiting for log marker: $marker"
    sleep 0.1
  done
  say "e2e: ok  $marker"
}

wait_count() {
  marker=$1
  want=$2
  tries=0
  while [ "$(grep -c "$marker" "$LOG" 2>/dev/null || echo 0)" -lt "$want" ]; do
    tries=$((tries + 1))
    [ "$tries" -lt 400 ] || die "timed out waiting for $want x $marker"
    sleep 0.1
  done
  say "e2e: ok  ${want}x $marker"
}

run_id_of() {
  cli app runs "$1" 2>/dev/null | sed -n 's/.*\(run_[A-Za-z0-9][A-Za-z0-9_-]*\).*/\1/p' | head -n 1
}

say "e2e: data dir $DATA"
mkdir -p "$DATA"
cli auth init -password radiotest -hostname localhost -ip 127.0.0.1 >/dev/null
cli app install "$APP_DIR/micropython" >/dev/null
cli app install "$APP_DIR/tests/fixture-tuner" >/dev/null
say "e2e: installed org.capsulad.radio and org.capsulad.radio.testtuner"

"$DAEMON" -platform simulator -data-dir "$DATA" -native-lib-dir "$LIB" \
  -dev-admin "$DATA/run/dev.sock" -log-file "$LOG" >"$DATA/daemon.out" 2>&1 &
DAEMON_PID=$!
tries=0
while ! "$DAEMON" -data-dir "$DATA" -health >/dev/null 2>&1; do
  tries=$((tries + 1))
  [ "$tries" -lt 100 ] || die "daemon did not become healthy"
  sleep 0.1
done
say "e2e: daemon healthy"

# The radio declares background=true, so boot already started it.
wait_log "radio: ready app=org.capsulad.radio"
wait_log "radio: capabilities display=True speaker=True"
TUNER_RUN=$(start_app org.capsulad.radio.testtuner | sed -n 's/.*\(run_[A-Za-z0-9][A-Za-z0-9_-]*\).*/\1/p')
wait_log "testtuner: ready"

# Launch invocation selects and plays station 1 (the manifest default list)
# through the tuner discovered via AppInterfaceService.
start_app org.capsulad.radio station=1 >/dev/null
wait_log "radio: tuning SomaFM Groove Salad"
wait_log "testtuner: tune url=https://ice1.somafm.com/groovesalad-128-mp3"
wait_log "radio: speaker stream"
wait_log "radio: now playing"
wait_log "radio: display capture cap_"
wait_log "testtuner: pushed 25 chunks then finished"
wait_log "radio: state stopped station stream ended"

# A second launch invocation selects station 2 and plays it.
start_app org.capsulad.radio station=2 >/dev/null
wait_log "radio: tuning Radio Paradise Main Mix"
wait_log "testtuner: tune url=https://stream.radioparadise.com/mp3-192"

# Long session so the admin settings path can switch a *live* station: 120
# chunks = 24 s of audio, paced by the player's backpressure.
cli settings app org.capsulad.radio.testtuner chunks=120 >/dev/null
wait_log "testtuner: settings player=org.capsulad.radio chunks=120"
start_app org.capsulad.radio station=1 >/dev/null
wait_count "radio: tuning SomaFM Groove Salad" 2
wait_log "radio: state playing station audio"
playing_before=$(grep -c "radio: state playing station audio" "$LOG" 2>/dev/null || echo 0)
cli settings app org.capsulad.radio station=2 >/dev/null
wait_log "radio: tuning Radio Paradise Main Mix (station changed)"
# The circle key only reaches a *playing* provider, so wait for this session's
# first accepted chunk before tapping.
wait_count "radio: state playing station audio" $((playing_before + 1))

# The device's circle key reaches the app through the standard media contract.
cli dev key play_pause true >/dev/null
cli dev key play_pause false >/dev/null
wait_log "radio: state paused paused"
cli dev key play_pause true >/dev/null
cli dev key play_pause false >/dev/null
wait_log "radio: state buffering resumed"

# The standard media contract is discoverable while the app runs.
cli media list | grep -q "org.capsulad.radio" || die "radio is not listed as a media provider"
say "e2e: ok  media provider listed"

if grep -q "radio: error\|radio: state error" "$LOG"; then
  grep "radio: error\|radio: state error" "$LOG" >&2
  die "the radio reported an error"
fi
if grep -q "display frame failed\|display unavailable\|display stream ended" "$LOG"; then
  grep "display frame failed\|display unavailable\|display stream ended" "$LOG" >&2
  die "the segment presentation was rejected"
fi
wait_log "radio: display visible=True"

# The daemon really rendered the radio's frames: routing selected the capture
# and the output loop wrote the presentation.
grep -q "routing .*org.capsulad.radio.*display.segment.front selected app=org.capsulad.radio" "$LOG" \
  || die "the radio never owned the segment display"
grep -q "output .*org.capsulad.radio .*display.segment.front wrote presentation=" "$LOG" \
  || die "the daemon never wrote the radio's segment frames"
say "e2e: ok  daemon wrote the radio's segment frames"

# Autonomous watchdog: kill the tuner with no user interaction at all. The
# radio must leave PLAYING by itself (tuner liveness), not wait for a press.
TUNER_RUN=$(run_id_of org.capsulad.radio.testtuner)
[ -n "$TUNER_RUN" ] || die "no tuner run id"
cli app stop "$TUNER_RUN" >/dev/null
wait_log "radio: playback failed: tuner stopped"
wait_log "radio: state error tuner stopped"
say "e2e: ok  watchdog left PLAYING after the tuner died, with no user activity"

RADIO_RUN=$(run_id_of org.capsulad.radio)
[ -n "$RADIO_RUN" ] || die "no radio run id"
cli app stop "$RADIO_RUN" >/dev/null
tries=0
while cli app runs org.capsulad.radio 2>/dev/null | grep -q "APP_READY\|APP_STARTING"; do
  tries=$((tries + 1))
  [ "$tries" -lt 100 ] || die "radio did not stop"
  sleep 0.1
done
say "e2e: ok  radio stopped cleanly"

say "e2e: PASS (simulator, MicroPython host, raw-protobuf tuner contract)"
