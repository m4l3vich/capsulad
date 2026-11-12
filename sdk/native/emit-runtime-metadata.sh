#!/bin/sh
# Emit the runtime metadata consumed by capsulad's native launcher.
# Usage: emit-runtime-metadata.sh <output.json> <library-filename>
# <library-filename> is relative to the runtime libdir, e.g. libcapsula.so.1
# on Linux or libcapsula.1.dylib on Darwin.
set -eu

if [ "$#" -lt 1 ]; then
    echo "usage: $0 <output.json> [library-filename]" >&2
    exit 2
fi

out=$1
lib=${2:-}
if [ -z "$lib" ]; then
    case "$(uname -s)" in
        Darwin) lib="libcapsula.1.dylib" ;;
        *) lib="libcapsula.so.1" ;;
    esac
fi

cat >"$out" <<EOF
{
  "abi_major": 1,
  "abi_minor": 0,
  "capabilities": [
    "capsula.abi.v1",
    "capsula.transport.unix-grpc",
    "capsula.reflect.json-proto",
    "capsula.messages.raw-proto",
    "capsula.transport.unary",
    "capsula.transport.server-streaming",
    "capsula.transport.client-streaming",
    "capsula.transport.bidi",
    "capsula.bus.connect",
    "capsula.bus.events",
    "capsula.bus.notifications",
    "capsula.bus.routed-calls",
    "capsula.audio.capture",
    "capsula.audio.playback",
    "capsula.audio.pcm-fast-path",
    "capsula.descriptors.register",
    "capsula.deadlines",
    "capsula.cancel",
    "capsula.concurrent-handles",
    "capsula.service.device",
    "capsula.service.settings",
    "capsula.service.routing",
    "capsula.service.presentation",
    "capsula.service.audio",
    "capsula.service.input",
    "capsula.service.assistant",
    "capsula.service.application",
    "capsula.service.application-configuration",
    "capsula.service.app-interface",
    "capsula.service.appbus",
    "capsula.service.sensor",
    "capsula.service.system",
    "capsula.service.diagnostics",
    "capsula.service.asset",
    "capsula.service.bluetooth-media"
  ],
  "library": "$lib"
}
EOF
