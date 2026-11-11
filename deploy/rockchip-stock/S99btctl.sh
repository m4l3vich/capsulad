#!/bin/sh
# Boot hook for the framework daemon on the Rockchip stock image.
#
# rcS expands `/etc/init.d/S??*` once, before S23overlayfs.sh pivots into the
# /data overlay, so a script that exists only in the overlay (S60capsulad) is
# never in that pre-pivot list. The stock S99btctl.sh is a vendor no-op whose
# only line is "btctl is now controlled by voice-agent"; replacing it with this
# wrapper is the late, ordered entry point that actually runs after the pivot,
# after mount, microphone-gate, network and PulseAudio init.
#
# The real service lives in S60capsulad so it can be started, stopped or
# restarted by hand. `exec` replaces the rcS subshell, which is what a non-.sh
# style child would do anyway.
exec /etc/init.d/S60capsulad start
