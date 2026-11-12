#!/bin/sh
# Framework boot service for the Rockchip stock image.
#
# This is the ONLY capsulad-specific boot file. It is deployed over the stock
# /etc/init.d/S99btctl.sh, which rcS already executes after mount, microphone
# enable, network and PulseAudio init. The name is inherited from the vendor
# boot list: rcS expands /etc/init.d/S??* before S23overlayfs.sh pivots into
# /data, so a script that exists only in the overlay is never executed.
#
# Everything this deployment needs is in this file: data/run preparation,
# Bluetooth UART IRQ affinity, retirement and restoration of the vendor
# assistant init scripts, daemon start/stop, health gating and vendor fallback.
# There is no separate S60capsulad service and no hidden init hook.
#
# Disable temporarily:   /etc/init.d/S99btctl.sh disable
# Enable again:          /data/capsdemo/boot/S99btctl.sh.disabled enable
#   disable stops the daemon, restores the vendor init scripts, starts the
#   vendor assistant chain and moves this hook to $DIR/boot/. With the hook
#   absent and the vendor scripts restored, the next boot behaves like stock.
# Restore manually:      run `disable`, then restore the firmware's original
#   S99btctl.sh (vendor no-op) from the device backup if a pristine hook is
#   required. Deleting this file alone is safe only when the vendor scripts
#   have already been restored.
#
# `capsulad -health` exit codes:
#   0  framework API answers
#   1  no daemon / not serving
#   2  this binary predates -health (the first upgrade); trust the process

DIR=/data/capsdemo
CAPSULAD=$DIR/capsulad
SOCKET=$DIR/run/hal.sock
PIDFILE=$DIR/run/capsulad.pid
BOOTLOG=$DIR/boot.log
INITDIR=/etc/init.d
SELF=$INITDIR/S99btctl.sh
DISABLED_SELF=$DIR/boot/S99btctl.sh.disabled
MARKER=$DIR/boot/disabled
VENDOR_BACKUP=$DIR/vendor-init
VENDOR_STOCK=$VENDOR_BACKUP/stock
VENDOR=$VENDOR_BACKUP/S97voice-agent.sh
VENDOR_SCRIPTS="S97voice-agent.sh S99sysmon.sh"

log() { echo "$(date '+%Y-%m-%dT%H:%M:%S') capsulad: $*" >>"$BOOTLOG"; }

# Keep the boot log from becoming a second unbounded log file.
if [ -f "$BOOTLOG" ] && [ "$(wc -c <"$BOOTLOG")" -gt 65536 ]; then
    : >"$BOOTLOG"
fi

# This busybox has no `pgrep -x`, and the detached watchdog feeder shares the
# `capsulad` process name. Match the daemon by comm plus cmdline, never by
# name alone: killing the feeder can reset the board once the timer is armed.
is_daemon() {
    [ -n "$1" ] && [ -d "/proc/$1" ] || return 1
    [ "$(cat "/proc/$1/comm" 2>/dev/null)" = "capsulad" ] || return 1
    case "$(tr '\0' ' ' <"/proc/$1/cmdline" 2>/dev/null)" in
    *__capsula_watchdog_feeder*) return 1 ;;
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

# A pre-upgrade binary cannot answer -health; a running process is the best
# available signal for that one transition, never for a normal start.
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

# The stock assistant init scripts are moved aside while the daemon owns the
# hardware, so they cannot start a second owner of the input key/watchdog in
# the same boot. Copies are kept in $VENDOR_STOCK for exact restoration.
retire_vendor() {
    mkdir -p "$VENDOR_STOCK"
    for script in $VENDOR_SCRIPTS; do
        [ -f "$INITDIR/$script" ] || continue
        if [ ! -f "$VENDOR_STOCK/$script" ]; then
            cp -p "$INITDIR/$script" "$VENDOR_STOCK/$script" 2>/dev/null || \
                log "could not preserve $INITDIR/$script"
        fi
        if [ ! -f "$VENDOR_BACKUP/$script" ]; then
            cp -p "$INITDIR/$script" "$VENDOR_BACKUP/$script" 2>/dev/null || true
        fi
        rm -f "$INITDIR/$script"
        log "retired $script while the framework owns the device"
    done
}

# restore_vendor puts the stock assistant scripts back. It prefers the pristine
# copy and falls back to the preserved chain in $VENDOR_BACKUP.
restore_vendor() {
    for script in $VENDOR_SCRIPTS; do
        source="$VENDOR_STOCK/$script"
        [ -f "$source" ] || source="$VENDOR_BACKUP/$script"
        if [ -f "$source" ]; then
            cp -p "$source" "$INITDIR/$script" 2>/dev/null || \
                log "could not restore $INITDIR/$script"
        fi
    done
}

# vendor_start restores and starts the preserved vendor assistant chain. This
# is the documented recovery path when the framework cannot become healthy.
vendor_start() {
    restore_vendor
    if [ -x "$VENDOR" ]; then
        "$VENDOR" start
        log "started the vendor assistant fallback"
    else
        log "vendor fallback $VENDOR is missing"
    fi
}

# The RK3308 routes the Bluetooth UART interrupt to CPU0, which also runs the
# daemon and the wakeword inference. Under load hci_uart reports framing errors
# and A2DP drops 100 ms chunks. Move the UART IRQ to CPU3.
pin_bluetooth_irq() {
    irq_line=$(grep serial /proc/interrupts 2>/dev/null | head -1)
    [ -n "$irq_line" ] || return 0
    irq=${irq_line%%:*}
    irq=$(echo "$irq" | tr -d ' ')
    if [ -w "/proc/irq/$irq/smp_affinity" ]; then
        echo 8 > "/proc/irq/$irq/smp_affinity" 2>/dev/null
    fi
}

start() {
    if [ -f "$MARKER" ]; then
        log "disabled by $MARKER; leaving the vendor chain in control"
        restore_vendor
        if ! running && [ -x "$VENDOR" ]; then
            vendor_start
        fi
        return 0
    fi
    pin_bluetooth_irq
    probe
    case $? in
    0)
        retire_vendor
        return 0
        ;;
    2)
        if running; then
            retire_vendor
            return 0
        fi
        ;;
    esac
    if running; then
        log "a daemon process exists but does not answer; restarting it"
        stop
    fi
    mkdir -p "$DIR/run" "$DIR/boot"
    retire_vendor
    log "starting $CAPSULAD"
    nohup "$CAPSULAD" -platform rockchip-stock -data-dir "$DIR" \
        -web-addr 0.0.0.0:8443 >>"$BOOTLOG" 2>&1 &
    if wait_up; then
        log "daemon healthy"
        return 0
    fi
    log "daemon did not become healthy; falling back to the vendor assistant"
    stop
    vendor_start
    return 1
}

disable() {
    stop
    restore_vendor
    mkdir -p "$DIR/boot"
    : >"$MARKER"
    vendor_start
    if [ -f "$SELF" ]; then
        mv "$SELF" "$DISABLED_SELF"
        log "disabled; hook moved to $DISABLED_SELF"
    else
        log "disabled; hook already absent"
    fi
    return 0
}

enable() {
    if [ -f "$DISABLED_SELF" ] && [ ! -f "$SELF" ]; then
        cp -p "$DISABLED_SELF" "$SELF" || {
            log "could not restore the hook to $SELF"
            return 1
        }
        rm -f "$DISABLED_SELF"
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
