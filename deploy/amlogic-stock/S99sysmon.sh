#!/bin/sh
# Framework boot service for the Amlogic A113X stock image (VK Capsula Mini).
#
# Install as /etc/init.d/S99sysmon.sh, replacing the vendor supervisor's script.
# BusyBox rcS expands /etc/init.d/S??* before /data is mounted (S02datamount) and
# before the overlay pivots (S04overlaymount), so a NEW overlay-only name never
# runs. Replacing an already-expanded name does: the name is in the list and the
# file resolved at run time comes from the overlay. S99sysmon.sh is the vendor
# supervisor (sysmon -> voice-agent) this framework replaces, so it is the safe
# name to wrap; S99btctl.sh keeps btctl untouched. `disable` writes the stock
# script back verbatim.
#
# The stock vendor assistant is stopped at runtime by `capsulad -supersede auto`
# (profile VendorProcesses: voice-agent), which also reaps a respawned copy. The
# Amlogic stock init script names for the vendor chain are not published, so this
# hook does not retire or restore them the way the Rockchip hook does.
#
# Disable temporarily:   /etc/init.d/S99sysmon.sh disable
# Enable again:          /etc/init.d/S99sysmon.sh enable
# `capsulad -health` exit codes:
#   0  framework API answers
#   1  no daemon / not serving
#   2  this binary predates -health; trust the running process

DIR=/data/local/capsulad
CAPSULAD=$DIR/capsulad
SOCKET=$DIR/run/hal.sock
PIDFILE=$DIR/run/capsulad.pid
FEEDER_PID=$DIR/run/watchdog.feeder
FEEDER_HANDBACK=$DIR/run/watchdog.handback
BOOTLOG=$DIR/boot.log
SELF=/etc/init.d/S99sysmon.sh
DISABLED_SELF=$DIR/boot/S99sysmon.sh.disabled
WRAPPER_COPY=$DIR/boot/S99sysmon.sh.wrapper
MARKER=$DIR/boot/disabled

log() { echo "$(date '+%Y-%m-%dT%H:%M:%S') capsulad: $*" >>"$BOOTLOG"; }

if [ -f "$BOOTLOG" ] && [ "$(wc -c <"$BOOTLOG")" -gt 65536 ]; then
    : >"$BOOTLOG"
fi

# This busybox has no `pgrep -x`, so match the daemon by comm plus cmdline,
# never by name alone. The detached watchdog feeder is a plain /bin/sh process
# that only writes to an inherited descriptor; a feeder left by an older release
# is a copy of the daemon (its __capsulad_watchdog_feeder subcommand) and must
# not be killed: it may own the armed timer.
is_daemon() {
    [ -n "$1" ] && [ -d "/proc/$1" ] || return 1
    [ "$(cat "/proc/$1/comm" 2>/dev/null)" = "capsulad" ] || return 1
    case "$(tr '\0' ' ' <"/proc/$1/cmdline" 2>/dev/null)" in
    *__capsulad_watchdog_feeder*) return 1 ;;
    esac
    return 0
}

daemon_pids() {
    pid=$(cat "$PIDFILE" 2>/dev/null)
    if is_daemon "$pid"; then
        echo "$pid"
        return 0
    fi
    for pid in $(pgrep capsulad 2>/dev/null); do
        is_daemon "$pid" && echo "$pid"
    done
}

running() { [ -n "$(daemon_pids)" ]; }

probe() {
    "$CAPSULAD" -health -socket "$SOCKET" >/dev/null 2>&1
    return $?
}

stop() {
    pids=$(daemon_pids)
    if [ -z "$pids" ]; then
        return 0
    fi
    for pid in $pids; do
        kill -TERM "$pid" 2>/dev/null
    done
    i=0
    while [ $i -lt 15 ]; do
        alive=""
        for pid in $pids; do
            is_daemon "$pid" && alive="$alive $pid"
        done
        [ -z "$alive" ] && return 0
        sleep 1
        i=$((i+1))
    done
    log "SIGTERM did not stop the daemon; sending SIGKILL"
    for pid in $alive; do
        kill -KILL "$pid" 2>/dev/null
    done
    sleep 1
}

# release_feeder asks a detached watchdog feeder to hand the timer back before a
# new owner opens /dev/watchdog, so the open does not fail with EBUSY.
release_feeder() {
    [ -f "$FEEDER_PID" ] || return 0
    set -- $(cat "$FEEDER_PID" 2>/dev/null)
    if [ -n "$1" ]; then
        if [ ! -d "/proc/$1" ]; then
            rm -f "$FEEDER_PID"
            return 0
        fi
        case "$(tr '\0' ' ' <"/proc/$1/cmdline" 2>/dev/null)" in
        *capsulad-watchdog-feeder* | *__capsulad_watchdog_feeder*) ;;
        *)
            rm -f "$FEEDER_PID"
            return 0
            ;;
        esac
    fi
    : >"$FEEDER_HANDBACK"
    i=0
    while [ $i -lt 5 ]; do
        [ -f "$FEEDER_PID" ] || break
        sleep 1
        i=$((i+1))
    done
    rm -f "$FEEDER_HANDBACK"
    if [ -f "$FEEDER_PID" ]; then
        log "the watchdog feeder pid=$1 still holds the timer"
    fi
}

wait_up() {
    i=0
    while [ $i -lt 20 ]; do
        probe
        case $? in
        0) return 0 ;;
        2) running && return 0 ;;
        esac
        sleep 1
        i=$((i+1))
    done
    return 1
}

# vendor_start mirrors the stock S99sysmon.sh body (non-secret), used when the
# framework is disabled or fails its health gate.
vendor_start() {
    start-stop-daemon -S -x sysmon -b -- -logtosyslog 2>/dev/null && \
        log "started the vendor sysmon supervisor"
}

vendor_stop() {
    start-stop-daemon -K -x sysmon -o 2>/dev/null || true
}

start() {
    if [ -f "$MARKER" ]; then
        log "disabled by $MARKER; leaving the vendor supervisor in control"
        vendor_start
        return 0
    fi
    mkdir -p "$DIR/boot"
    if [ ! -f "$WRAPPER_COPY" ] && [ -f "$SELF" ]; then
        cp -p "$SELF" "$WRAPPER_COPY" 2>/dev/null || log "could not save the wrapper copy"
    fi
    probe
    case $? in
    0)
        return 0
        ;;
    2)
        running && return 0
        ;;
    esac
    if running; then
        log "a daemon process exists but does not answer; restarting it"
        stop
    fi
    mkdir -p "$DIR/run" "$DIR/boot"
    release_feeder
    log "starting $CAPSULAD"
    nohup "$CAPSULAD" -platform amlogic-stock -data-dir "$DIR" \
        -web-addr 0.0.0.0:8443 >>"$BOOTLOG" 2>&1 &
    if wait_up; then
        log "daemon healthy"
        return 0
    fi
    log "daemon did not become healthy; leaving the stock supervisor in control"
    stop
    release_feeder
    vendor_stop
    vendor_start
    return 1
}

disable() {
    stop
    release_feeder
    mkdir -p "$DIR/boot"
    : >"$MARKER"
    if [ -f "$SELF" ]; then
        cp -p "$SELF" "$DISABLED_SELF" 2>/dev/null || true
        cat >"$SELF" <<'VENDOR'
#!/bin/sh
#
# Start/stop sysmon
#


case "$1" in
  start)
    echo "Starting sysmon..."
    start-stop-daemon -S -x sysmon -b -- -logtosyslog
    ;;
  stop)
    echo "Stopping sysmon..."
    start-stop-daemon -K -x sysmon -o
    ;;
  restart)
    "$0" stop
    sleep 1
    "$0" start
    ;;
  *)
    echo "Usage: $0 {start|stop|restart}"
    exit 1
esac
VENDOR
        chmod 755 "$SELF"
        log "disabled; stock S99sysmon.sh restored"
    fi
    vendor_stop
    vendor_start
    return 0
}

enable() {
    if [ -f "$WRAPPER_COPY" ]; then
        cp -p "$WRAPPER_COPY" "$SELF" || {
            log "could not restore the wrapper to $SELF"
            return 1
        }
    fi
    rm -f "$MARKER"
    start
}

case "$1" in
start) start ;;
stop) stop ;;
restart)
    stop
    start
    ;;
status) probe ;;
disable) disable ;;
enable) enable ;;
*)
    echo "usage: $0 {start|stop|restart|status|disable|enable}"
    exit 1
    ;;
esac
