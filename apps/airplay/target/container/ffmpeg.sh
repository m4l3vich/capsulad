#!/bin/sh
# Build the minimal static FFmpeg the AirPlay package links (AAC only).
#
# Runs inside the pinned builder image as root; writes /opt/ffmpeg. The source
# tarball was sha256-verified before it was copied in (see the Dockerfile).
set -eu

version=${AIRPLAY_FFMPEG_VERSION:?}
src=/build/ffmpeg.tar.xz

say() { printf 'ffmpeg: %s\n' "$*" >&2; }

# The single source of the configure line. Everything the AAC decoder needs and
# nothing else: no programs, no network, no demuxers/muxers/filters, no GPL
# component. libavformat is dropped too (the app decodes raw access units).
# --disable-autodetect keeps configure from picking up unrelated system
# libraries; --enable-pic keeps the archives usable in a PIE or shared link.
CONFIGURE="--prefix=/opt/ffmpeg --enable-static --disable-shared --disable-programs --disable-doc \
--disable-network --disable-everything --enable-decoder=aac --enable-parser=aac \
--disable-zlib --disable-bzlib --disable-lzma --disable-iconv --disable-xlib --disable-sdl2 \
--disable-avdevice --disable-swscale --disable-swresample --disable-postproc --disable-avfilter \
--disable-avformat --disable-autodetect --enable-pic --cc=gcc"

mkdir -p /build && cd /build
say "source: $src (${AIRPLAY_FFMPEG_URL:-url not recorded})"
tar xJf "$src"
cd "/build/ffmpeg-${version}"

# Keep the exact inputs next to the source copy: the LGPL offer that ships
# beside the package quotes them verbatim.
mkdir -p /opt/ffmpeg/src
printf '%s\n' "$CONFIGURE" > /opt/ffmpeg/src/configure-line.txt
{
  echo "version: $version"
  echo "url: ${AIRPLAY_FFMPEG_URL:-not recorded}"
  echo "sha256: ${AIRPLAY_FFMPEG_SHA256:-not recorded}"
  echo "tarball RELEASE: $(cat RELEASE 2>/dev/null || echo '?')"
  echo "tarball VERSION: $(cat VERSION 2>/dev/null || echo '?')"
} > /opt/ffmpeg/src/SOURCE.txt

say "configure $CONFIGURE"
# shellcheck disable=SC2086
./configure $CONFIGURE > /build/configure.log 2>&1 || { tail -40 /build/configure.log >&2; exit 1; }
tail -12 /build/configure.log >&2

say "make -j$(nproc)"
make -j"$(nproc)" > /build/make.log 2>&1 || { tail -40 /build/make.log >&2; exit 1; }
make install > /build/install.log 2>&1
# The source stays available for the LGPL offer that ships beside the package
# (build-target.sh copies it out); only the build tree and logs are dropped.
mkdir -p /opt/ffmpeg/src && cp "$src" "/opt/ffmpeg/src/ffmpeg-${version}.tar.xz"
cd / && rm -rf "/build/ffmpeg-${version}" /build/make.log /build/configure.log /build/install.log

# FFmpeg's configure puts -latomic in the .pc Libs because its atomics test
# wants the runtime library. Linking the shared libatomic would add a DT_NEEDED
# entry for a library a device rootfs need not carry, so point the flags at
# gcc's static archive: an archive never produces DT_NEEDED, and if nothing
# needs its symbols the linker ignores it.
LATOMIC=$(gcc -print-file-name=libatomic.a)
case "$LATOMIC" in
  /*) ;;
  *) echo "ffmpeg: could not locate a static libatomic.a" >&2; exit 1 ;;
esac
sed -i "s|-latomic|$LATOMIC|g" /opt/ffmpeg/lib/pkgconfig/libavcodec.pc /opt/ffmpeg/lib/pkgconfig/libavutil.pc

say "installed:"
ls -la /opt/ffmpeg/lib >&2
grep -H '^Libs:' /opt/ffmpeg/lib/pkgconfig/*.pc >&2
# A shared object here would mean the package silently carried a second library
# tree; refuse instead.
if ls /opt/ffmpeg/lib/*.so* >/dev/null 2>&1; then
  echo "ffmpeg: a static-only build produced shared libraries" >&2
  exit 1
fi
PKG_CONFIG_PATH=/opt/ffmpeg/lib/pkgconfig pkg-config --static --libs libavcodec libavutil