#!/bin/sh
# End-to-end check for the PA speaker app on a simulator daemon.
#
# Proves, with the real MicroPython host and the real framework audio service:
#   * the package installs and the app reports ready at boot;
#   * a WAV streamed from another script app plays and is drained;
#   * a WAV split across request frames plays the same audio;
#   * unsupported encodings, a too-long clip, a truncated file, an oversized
#     frame, a frame with no WAV bytes and a stalled upload are rejected with
#     the documented status;
#   * cancelling the call discards the audio, and the app serves the next call;
#   * two queued calls are served one after another (never two streams at once);
#   * an administrator setting change takes effect: with max_seconds=1 the exact
#     bound plays and one frame over it is rejected;
#   * the app holds no playback stream between calls.
#
# It needs the host daemon/CLI and the interpreter runtime, but it builds
# nothing: use `make capsulad capsulad-cli` and `make interpreter-micropython`
# first. Proof level: simulator API and PCM submission. It does not prove
# acoustic output.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
OUT=${OUT:-"$ROOT/build/pa-e2e"}
DATA=${DATA:-"$OUT/data"}
LOG="$DATA/pa-e2e.log"
LIB=${CAPSULAD_NATIVE_LIB_DIR:-"$ROOT/build/lib"}
CLI=${CAPSULAD_CLI:-"$ROOT/build/bin/capsulad-cli"}
DAEMON=${CAPSULAD_DAEMON:-"$ROOT/build/bin/capsulad"}
APP=org.capsulad.pa
CALLER=org.capsulad.pa.test-caller
DAEMON_PID=""
CALLER_RUN=""

say() { printf '%s\n' "$*" >&2; }
die() { say "pa-e2e: $*"; exit 1; }
cli() { "$CLI" -data-dir "$DATA" -native-lib-dir "$LIB" "$@"; }

cleanup() {
  if [ -x "$CLI" ] && [ -n "$CALLER_RUN" ]; then
    cli app stop "$CALLER_RUN" >/dev/null 2>&1 || true
  fi
  kill "${DAEMON_PID:-0}" >/dev/null 2>&1 || true
  sleep 1
  pkill -f "$LIB/capsulad-micropython" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

[ -x "$CLI" ] || die "missing $CLI; run 'make capsulad capsulad-cli' or set CAPSULAD_CLI"
[ -x "$DAEMON" ] || die "missing $DAEMON; run 'make capsulad' or set CAPSULAD_DAEMON"
[ -x "$LIB/capsulad-micropython" ] || die "missing $LIB/capsulad-micropython; run 'make interpreter-micropython'"
ls "$LIB"/libcapsulad* >/dev/null 2>&1 || die "missing libcapsulad in $LIB; run 'make native'"

say "packaging the app"
sh "$ROOT/apps/pa/build.sh" >/dev/null
ARCHIVE=$(ls "$ROOT"/build/apps/org.capsulad.pa-*.tar.gz | tail -n 1)
[ -f "$ARCHIVE" ] || die "build.sh wrote no archive"

rm -rf "$DATA"
mkdir -p "$DATA"
"$CLI" -data-dir "$DATA" auth init -password pa-e2e-pass -hostname localhost -ip 127.0.0.1 >/dev/null
cli app install "$ARCHIVE" >/dev/null
cli app install "$ROOT/apps/pa/tests/caller" >/dev/null

"$DAEMON" -platform simulator -data-dir "$DATA" -native-lib-dir "$LIB" \
  -dev-admin "$DATA/dev.sock" -log-file "$LOG" >"$OUT/daemon.out" 2>&1 &
DAEMON_PID=$!

tries=0
while ! "$DAEMON" -data-dir "$DATA" -health >/dev/null 2>&1; do
  tries=$((tries + 1))
  [ "$tries" -lt 150 ] || die "daemon did not become healthy (log: $LOG)"
  sleep 0.1
done

wait_log() {
  marker=$1
  limit=${2:-200}
  tries=0
  while ! grep -q "$marker" "$LOG" 2>/dev/null; do
    tries=$((tries + 1))
    [ "$tries" -lt "$limit" ] || die "timed out waiting for: $marker (log: $LOG)"
    sleep 0.1
  done
}

say "waiting for the PA speaker to start at boot"
wait_log "pa: ready app=$APP"

say "phase main: accepted files, rejections, interruption, queued calls, stall"
CALLER_RUN=$(cli app start "$CALLER" phase=main | sed -n 's/.*run=\([^ ]*\).*/\1/p')
[ -n "$CALLER_RUN" ] || die "caller did not report a run id"
wait_log "pa-caller: DONE main" 400

say "checking the main-phase results"
grep -q "pa-caller: DONE main ok=15 fail=0" "$LOG" || die "main cases failed; see $LOG"
! grep -q "pa-caller: FAIL" "$LOG" || die "a caller case failed; see $LOG"
grep -q "pa: played call=" "$LOG" || die "the app never logged a played call"
grep -q "pa: rejected call=" "$LOG" || die "the app never logged a rejection"
grep -q "pa: cancelled call=" "$LOG" || die "cancellation was not observed by the app"
grep -q "pa: stalled call=" "$LOG" || die "the stalled upload was not observed by the app"
grep -q "carries no WAV bytes" "$LOG" || die "the no-progress frame rejection was not observed"
! grep -q "pa: failed call=" "$LOG" || die "a call failed unexpectedly; see $LOG"
grep -q "playback stream=" "$LOG" || die "the framework audio service never opened a playback stream"

say "checking that one clip is served at a time"
# The app's own playback streams must strictly alternate open/close, which is
# what proves a queued second call waits for the first to finish.
awk '/org\.capsulad\.pa +playback stream=.*opened/ {count++; if (depth) bad=1; depth=1}
     /org\.capsulad\.pa +playback stream=.*closed/ {closes++; depth=0}
     END {if (bad || count != closes || count < 6) exit 1}' "$LOG" ||
  die "playback streams overlapped or leaked (see $LOG)"

say "lowering max_seconds to 1 s through the administrator CLI"
cli settings app "$APP" max_seconds=1 >/dev/null
wait_log "pa: settings revision=" 100
grep -q "pa: settings revision=[0-9]* max_seconds=1" "$LOG" ||
  die "the app did not apply max_seconds=1 (settings event shape); see $LOG"

say "phase settings: the bound is enforced"
CALLER_RUN=$(cli app start "$CALLER" phase=settings | sed -n 's/.*run=\([^ ]*\).*/\1/p')
[ -n "$CALLER_RUN" ] || die "caller did not report a run id"
wait_log "pa-caller: DONE settings" 300

grep -q "pa-caller: DONE settings ok=3 fail=0" "$LOG" || die "settings cases failed; see $LOG"
! grep -q "pa-caller: FAIL" "$LOG" || die "a caller case failed; see $LOG"
grep -q "max_seconds=1 allows 32000" "$LOG" ||
  die "the lowered bound was not used for the rejection; see $LOG"

say "checking that the app is still ready and idle"
cli app runs "$APP" | grep -q "APP_READY" || die "the PA speaker is not APP_READY after the suite"

say "PASS: install, boot, play, split play, rejections, no-progress frames, stall, cancellation, queued calls, setting bound"
