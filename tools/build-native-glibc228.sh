#!/bin/sh
# Build the shared native runtime against glibc 2.28 for the Amlogic stock image.
#
# The stock image ships glibc 2.28; a bullseye/bullseye-era Rust build links
# GLIBC_2.29+ and cannot load there. This script builds inside Debian buster
# (glibc 2.28) with a current Rust toolchain (official rustc binaries target
# glibc 2.17) using the archived buster repositories, then verifies the symbol
# ceiling and emits the runtime metadata into build/lib (the normal LIBDIR).
#
# usage: tools/build-native-glibc228.sh [OUTDIR]
# Requires Docker with linux/arm64 support. No ABI change: the C ABI stays 1.0.
set -eu
out=${1:-build/lib}
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$root/$out"
docker run --rm --platform linux/arm64 \
    -v "$root":/src -w /src/sdk/native debian:buster sh -c '
set -e
sed -i "s|deb.debian.org/debian buster|archive.debian.org/debian buster|g; s|deb.debian.org/debian-security buster/updates|archive.debian.org/debian-security buster/updates|g" /etc/apt/sources.list
printf "Acquire::Check-Valid-Until \"false\";\n" > /etc/apt/apt.conf.d/99archive
apt-get update -qq
# xz-utils/unzip are required for rustup toolchain extraction; without them it
# fails silently. The rustup log is surfaced on failure.
apt-get install -y -qq curl ca-certificates build-essential pkg-config xz-utils unzip >/dev/null
if ! command -v rustc >/dev/null 2>&1; then
    # Toolchain pinned; the rustup bootstrap script itself is fetched over TLS
# (checksum pinning would require an upstream-published hash).
curl --proto =https --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain 1.100.0 >/tmp/rustup.log 2>&1 || {
        tail -20 /tmp/rustup.log >&2
        echo "error: rustup install failed; see the log above" >&2
        exit 1
    }
fi
. "$HOME/.cargo/env"
cargo build --locked --release --target aarch64-unknown-linux-gnu
so=target/aarch64-unknown-linux-gnu/release/libcapsulad.so
max=$(readelf --version-info "$so" | grep -o "GLIBC_[0-9.]*" | sed "s/GLIBC_//" | sort -V | tail -1)
[ -n "$max" ] || { echo "no GLIBC versions found" >&2; exit 1; }
if [ "$(printf "%s\n2.28\n" "$max" | sort -V | tail -1)" != "2.28" ]; then
    echo "error: runtime needs GLIBC_$max, above the stock 2.28 ceiling" >&2
    exit 1
fi
echo "max GLIBC symbol: $max"
readelf -d "$so" | grep NEEDED
cp "$so" /src/'"$out"'/libcapsulad.so.1.0.0
'
cd "$root/$out"
ln -sfn libcapsulad.so.1.0.0 libcapsulad.so.1
ln -sfn libcapsulad.so.1 libcapsulad.so
sh "$root/sdk/native/emit-runtime-metadata.sh" "$root/$out/capsulad-native.json" libcapsulad.so.1
echo "installed $root/$out/libcapsulad.so.1.0.0"
