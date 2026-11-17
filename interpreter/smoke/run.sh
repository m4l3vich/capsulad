#!/bin/sh
# End-to-end script-runtime smoke.
#
# Proves, on a simulator daemon, that both interpreter hosts:
#   * connect through the public libcapsulad ABI and report APP_READY;
#   * receive AppBus invocations;
#   * make a permitted unary framework call;
#   * exchange routed unary calls with each other;
#   * open and cancel a routed stream;
#   * terminate cleanly on Stop and on SIGKILL without orphan processes.
#
# It builds the daemon/CLI and expects the interpreter hosts in build/lib
# (run `make interpreter` first, or pass SKIP_INTERPRETER_BUILD=0 to build).
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
OUT="$ROOT/build/interp"
BIN="$OUT/bin"
LIB="$ROOT/build/lib"
DATA=${DATA:-"$OUT/smoke-data"}
LOG="$DATA/smoke.log"
PROBE_RUN=""
TARGET_RUN=""
DAEMON_PID=""
DAEMON="$BIN/capsulad"
CLI="$BIN/capsulad-cli"
SKIP_INTERPRETER_BUILD=${SKIP_INTERPRETER_BUILD:-1}

say() { printf '%s\n' "$*" >&2; }
die() { say "smoke: $*"; exit 1; }

cleanup() {
  "$CLI" -data-dir "$DATA" app stop "${PROBE_RUN:-}" >/dev/null 2>&1 || true
  "$CLI" -data-dir "$DATA" app stop "${TARGET_RUN:-}" >/dev/null 2>&1 || true
  kill "${DAEMON_PID:-0}" >/dev/null 2>&1 || true
  sleep 1
  pkill -f "$LIB/capsulad-quickjs" >/dev/null 2>&1 || true
  pkill -f "$LIB/capsulad-micropython" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

mkdir -p "$BIN"
if [ "$SKIP_INTERPRETER_BUILD" != "1" ]; then
  sh "$ROOT/interpreter/build.sh"
fi
[ -x "$LIB/capsulad-quickjs" ] || die "missing $LIB/capsulad-quickjs; run make interpreter"
[ -x "$LIB/capsulad-micropython" ] || die "missing $LIB/capsulad-micropython; run make interpreter"

say "host terminal-event queue test"
CC=${CC:-cc}
"$CC" -std=c11 -Wall -Wextra -pthread \
  -I"$ROOT/sdk/native/include" -I"$ROOT/interpreter/common" \
  "$ROOT/interpreter/common/capsulad_host_test.c" \
  -o "$OUT/capsulad-host-test"
"$OUT/capsulad-host-test" || die "host terminal-event queue test failed"

say "building daemon and CLI"
(cd "$ROOT" && go build -ldflags '-linkmode=external' -o "$DAEMON" ./cmd/capsulad)
(cd "$ROOT" && go build -ldflags '-linkmode=external' -o "$CLI" ./cmd/capsulad-cli)

rm -rf "$DATA"
mkdir -p "$DATA"
"$CLI" -data-dir "$DATA" auth init -password smokepass -hostname localhost -ip 127.0.0.1 >/dev/null
"$CLI" -data-dir "$DATA" -native-lib-dir "$LIB" app install "$ROOT/interpreter/smoke/apps/micropython-target" >/dev/null
"$CLI" -data-dir "$DATA" -native-lib-dir "$LIB" app install "$ROOT/interpreter/smoke/apps/quickjs-probe" >/dev/null

"$DAEMON" -platform simulator -data-dir "$DATA" -native-lib-dir "$LIB" \
  -dev-admin "$DATA/dev.sock" -log-file "$LOG" >"$OUT/daemon.out" 2>&1 &
DAEMON_PID=$!
tries=0
while ! "$DAEMON" -data-dir "$DATA" -health >/dev/null 2>&1; do
  tries=$((tries + 1))
  [ "$tries" -lt 100 ] || die "daemon did not become healthy"
  sleep 0.1
done

start() { "$CLI" -data-dir "$DATA" -native-lib-dir "$LIB" app start "$1"; }
run_of() { start "$1" | sed -n 's/.*run=\([^ ]*\).*/\1/p'; }
wait_log() {
  marker=$1
  tries=0
  while ! grep -q "$marker" "$LOG" 2>/dev/null; do
    tries=$((tries + 1))
    [ "$tries" -lt 200 ] || die "timed out waiting for log marker: $marker"
    sleep 0.1
  done
}
wait_log_since() {
  marker=$1
  start_line=$2
  tries=0
  while :; do
    if tail -n +"$((start_line + 1))" "$LOG" 2>/dev/null | grep -q "$marker"; then
      return 0
    fi
    tries=$((tries + 1))
    [ "$tries" -lt 200 ] || die "timed out waiting for log marker after restart: $marker"
    sleep 0.1
  done
}
wait_gone() {
  pid=$1
  tries=0
  while kill -0 "$pid" 2>/dev/null; do
    tries=$((tries + 1))
    [ "$tries" -lt 100 ] || die "process $pid did not exit"
    sleep 0.1
  done
}

TARGET_RUN=$(run_of org.example.micropython-smoke)
PROBE_RUN=""
wait_log "SMOKE-READY micropython"
PROBE_RUN=$(run_of org.example.quickjs-smoke)
wait_log "SMOKE-READY quickjs"
wait_log "SMOKE-PUBLIC-API quickjs"
wait_log "SMOKE-ROUTED-OK micropython"
wait_log "SMOKE-ROUTED-OK quickjs"
wait_log "SMOKE-CANCEL-CLIENT quickjs"
wait_log "SMOKE-TRYRECV quickjs"
wait_log "SMOKE-TRYRECV-BYTES quickjs"
wait_log "SMOKE-TRYRECV-NEG quickjs"
wait_log "SMOKE-TRYRECV-OK quickjs"
wait_log "SMOKE-STREAM-OK micropython"
wait_log "SMOKE-INVOCATION quickjs"
wait_log "SMOKE-INVOCATION micropython"

PROBE_PID=$(ps -ax -o pid=,command= | awk -v p="$PROBE_RUN" '$0 ~ p {print $1; exit}')
# The run ID is not part of argv; resolve by app id instead.
PROBE_PID=$(ps -ax -o pid=,command= | awk -v app="$DATA/apps/org.example.quickjs-smoke/main.js" 'index($0, app) {print $1; exit}')
TARGET_PID=$(ps -ax -o pid=,command= | awk -v app="$DATA/apps/org.example.micropython-smoke/main.py" 'index($0, app) {print $1; exit}')
[ -n "$PROBE_PID" ] || die "QuickJS host process not found"
[ -n "$TARGET_PID" ] || die "MicroPython host process not found"

say "stop cleanup"
"$CLI" -data-dir "$DATA" app stop "${PROBE_RUN:-}" >/dev/null
wait_gone "$PROBE_PID"
! "$CLI" -data-dir "$DATA" app runs | grep -q "$PROBE_RUN" || die "stopped run still listed"

say "crash cleanup"
PROBE_RUN=$(run_of org.example.quickjs-smoke)
wait_log "SMOKE-READY quickjs"
PROBE_PID=$(ps -ax -o pid=,command= | awk -v app="$DATA/apps/org.example.quickjs-smoke/main.js" 'index($0, app) {print $1; exit}')
[ -n "$PROBE_PID" ] || die "QuickJS host process not found after restart"
kill -9 "$PROBE_PID"
wait_gone "$PROBE_PID"
tries=0
while "$CLI" -data-dir "$DATA" app runs | grep -q "$PROBE_RUN"; do
  tries=$((tries + 1))
  [ "$tries" -lt 100 ] || die "crashed run still listed"
  sleep 0.1
done

say "target stop cleanup"
"$CLI" -data-dir "$DATA" app stop "${TARGET_RUN:-}" >/dev/null
wait_gone "$TARGET_PID"

say "restart target for daemon-crash disconnect delivery"
restart_line=$(wc -l <"$LOG")
TARGET_RUN=$(run_of org.example.micropython-smoke)
wait_log_since "SMOKE-READY micropython" "$restart_line"
TARGET_PID=$(ps -ax -o pid=,command= | awk -v app="$DATA/apps/org.example.micropython-smoke/main.py" 'index($0, app) {print $1; exit}')
[ -n "$TARGET_PID" ] || die "MicroPython host process not found after restart"

say "daemon crash: terminal event cleanup"
kill -9 "${DAEMON_PID:-0}"
wait "$DAEMON_PID" 2>/dev/null || true
# The bus socket is gone without a supervisor left to SIGTERM the hosts. Both
# hosts must observe the terminal disconnect and exit on their own.
wait_gone "$PROBE_PID"
wait_gone "$TARGET_PID"
sleep 1
if pgrep -f "$LIB/capsulad-quickjs" >/dev/null 2>&1 ||
   pgrep -f "$LIB/capsulad-micropython" >/dev/null 2>&1; then
  die "interpreter host survived daemon shutdown"
fi

say "PASS: QuickJS + MicroPython READY, unary, events, routed calls, stream cancel, stop/crash cleanup, daemon-crash disconnect cleanup"
