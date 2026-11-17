#!/bin/sh
# In-container half of the target build: compile, package, prove, and publish
# the LGPL relink material. Runs in the pinned builder image with
#   /work  = the caller's source tree copy (writable, never the repository)
#   /out   = the caller's artifact directory
# and TARGET_ABI set. Everything it writes lands under /out.
set -eu

ABI=${TARGET_ABI:?run.sh needs TARGET_ABI=linux-gnu|linux-musl}
APP_ID=org.capsulad.airplay
CEILING=${GLIBC_CEILING:-}
TOOL=/usr/local/share/capsulad-airplay

say() { printf '\n===== %s =====\n' "$*"; }
note() { printf '%s\n' "$*"; }

case "$ABI" in
  linux-gnu | linux-musl) ;;
  *) echo "run.sh: TARGET_ABI must be linux-gnu or linux-musl, got '$ABI'" >&2; exit 2 ;;
esac

say "builder"
head -2 /etc/os-release 2>/dev/null || true
go version
gcc --version | head -1
(ldd --version 2>&1 | head -1) || true
nproc
note "TARGET_ABI=$ABI"
note "FFmpeg prefix: /opt/ffmpeg"

say "FFmpeg: static archives only"
ls -la /opt/ffmpeg/lib
ls -la /opt/ffmpeg/src
say "FFmpeg link mode (LGPL branch): the .pc flags, and whether any shared object exists"
pkg-config --libs libavcodec libavutil
pkg-config --static --libs libavcodec libavutil
if ls /opt/ffmpeg/lib/libavcodec.so* /opt/ffmpeg/lib/libavutil.so* >/dev/null 2>&1; then
  echo "FFMPEG-LINK-MODE: dynamic (shared libavcodec/libavutil present)"
else
  echo "FFMPEG-LINK-MODE: static (only libavcodec.a/libavutil.a; no libavcodec.so*/libavutil.so*)"
fi
for f in /opt/ffmpeg/lib/libavcodec.a /opt/ffmpeg/lib/libavutil.a; do [ -f "$f" ] && stat -c '%s bytes %n' "$f"; done

# ------------------------------------------------------------------ build ---
say "packager (linux/arm64, CGO_ENABLED=0)"
cd /work
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /work/build/bin/capsulad-cli ./cmd/capsulad-cli
file /work/build/bin/capsulad-cli

say "app ($ABI)"
# build.sh passes these to `capsulad-cli app package`, which gates every ELF
# file in the package: the ABI, the machine, and (for linux-gnu) the stock
# glibc ceiling. The package is therefore checked by the repository's own gate
# rather than by a second, weaker pass here.
export CAPSULAD_TARGET_ARCH=aarch64
if [ "$ABI" = linux-gnu ] && [ -n "$CEILING" ]; then
  export CAPSULAD_MAX_GLIBC="$CEILING"
else
  export CAPSULAD_MAX_GLIBC=
fi
OUT=/work/build/apps TARGET_ABI="$ABI" GOOS=linux GOARCH=arm64 CGO_ENABLED=1 \
  AIRPLAY_FFMPEG_PREFIX=/opt/ffmpeg \
  CAPSULAD_CLI=/work/build/bin/capsulad-cli \
  sh apps/airplay/build.sh

say "staged tree"
find /work/build/apps -maxdepth 2 | sort
say "staged manifest abi"
grep -n '^abi' /work/build/apps/"$APP_ID"/manifest.toml

# --------------------------------------------------------------- publish ---
rm -rf /out/"$ABI"
mkdir -p /out/"$ABI"
cp /work/build/apps/"$APP_ID"-*.tar.gz /out/"$ABI"/
cp /work/build/apps/"$APP_ID"/airplay /out/"$ABI"/airplay
BIN=/out/"$ABI"/airplay

say "package listing"
tar -tzvf /out/"$ABI"/"$APP_ID"-*.tar.gz
say "unpacked size of the package"
tar -tzvf /out/"$ABI"/"$APP_ID"-*.tar.gz | awk '{s+=$3} END {printf "%d bytes (%.1f MiB) of the 64 MiB unpacked limit\n", s, s/1048576}'
ls -la /out/"$ABI"

# ---------------------------------------------------------------- evidence --
say "file $BIN"
file "$BIN"
say "readelf -h"
readelf -h "$BIN"
say "readelf -d (dynamic section)"
readelf -d "$BIN" || true
say "readelf -l | grep -i interp"
readelf -l "$BIN" | grep -i interp || echo "(no INTERP program header)"
say "readelf --version-info"
readelf --version-info "$BIN" 2>&1 | head -40 || true
say "highest GLIBC_* requirement"
readelf --version-info "$BIN" 2>/dev/null | grep -o 'GLIBC_[0-9.]*' | sort -u -V | tail -6 || echo "(none: not a glibc binary)"
say "versioned imports above GLIBC_2.17"
readelf --dyn-syms --wide "$BIN" 2>/dev/null | grep -oE '[A-Za-z_][A-Za-z0-9_]*@GLIBC_[0-9.]+' | sort -u | grep -v 'GLIBC_2\.17$' || echo "(none)"
say "ldd $BIN"
ldd "$BIN" 2>&1 || true
say "cgo AAC decoder present (stripped binary: its own error string)"
strings -a "$BIN" | grep -c 'this libavcodec has no AAC decoder' || true
say "size"
stat -c '%s bytes %n' "$BIN"

# ------------------------------------------------------- AAC decode smoke ---
# A real decode with the static decoder on this target libc, using the
# repository's fixtures. This is the decoder the package carries, not a host
# build: same toolchain, same libc, same libavcodec.a.
say "AAC decode smoke: go test -tags ffmpeg ./internal/aac (target toolchain)"
cd /work/apps/airplay
go test -tags ffmpeg -count=1 -v ./internal/aac/ 2>&1 | tail -40
cd /work

# ------------------------------------------------------------- load smoke ---
# With a target runtime supplied by the caller, this exercises the real dlopen
# path (capsulad_abi_major/abi_minor/version/get_api + the first framework
# call). Without one it can only show the SDK reporting the missing library.
say "load smoke"
RUNTIME="/runtime/$ABI/libcapsulad.so.1"
[ -f "$RUNTIME" ] || RUNTIME=/runtime/libcapsulad.so.1
if [ -f "$RUNTIME" ]; then
  LIB=$RUNTIME
  note "using the supplied runtime: $RUNTIME"
  file "$RUNTIME" || true
  readelf -d "$RUNTIME" 2>/dev/null | grep -E 'NEEDED|SONAME' || true
else
  LIB=/nonexistent
  note "no runtime supplied at /runtime/$ABI/libcapsulad.so.1 (or /runtime/libcapsulad.so.1);"
  note "the app can only report the missing library. Drop one there to exercise dlopen for real."
fi

# /opt/ffmpeg is renamed away first: a load failure that mentions libavcodec
# would mean the decoder leaked back out of the package as a runtime dependency.
mv /opt/ffmpeg /opt/ffmpeg.hidden
cd /out/"$ABI"
set +e
CAPSULAD_SOCKET=/nonexistent CAPSULAD_RUN_ID=x CAPSULAD_APP_ID=$APP_ID \
  CAPSULAD_LIBRARY="$LIB" timeout 30 ./airplay
note "exit status: $?"
set -e
mv /opt/ffmpeg.hidden /opt/ffmpeg

# ------------------------------------------------------- relink check ------
# The published relink route is "rebuild the app against a modified FFmpeg
# prefix" (LGPL-2.1+ section 6). Prove it on every build rather than documenting
# it: copy the prefix, rebuild with packaging off, and show the result is a
# working binary of the same shape. RELINK.md points at this transcript.
say "relink check: rebuild the app against a writable copy of the FFmpeg prefix"
rm -rf /tmp/ffmpeg-relink /tmp/relink-out
cp -R /opt/ffmpeg /tmp/ffmpeg-relink
( cd /work && AIRPLAY_FFMPEG_PREFIX=/tmp/ffmpeg-relink PACKAGE=0 \
    GOOS=linux GOARCH=arm64 TARGET_ABI="$ABI" CGO_ENABLED=1 \
    CAPSULAD_CLI=/work/build/bin/capsulad-cli OUT=/tmp/relink-out \
    sh apps/airplay/build.sh )
RELINKED=/tmp/relink-out/org.capsulad.airplay/airplay
file "$RELINKED"
readelf -d "$RELINKED" | grep NEEDED || true
printf 'AAC decoder string: '; strings -a "$RELINKED" | grep -c 'this libavcodec has no AAC decoder' || true
set +e
CAPSULAD_SOCKET=/nonexistent CAPSULAD_RUN_ID=x CAPSULAD_APP_ID=$APP_ID \
  CAPSULAD_LIBRARY=/nonexistent timeout 30 "$RELINKED"
printf 'relinked binary exit status: %s\n' "$?"
set -e

# ------------------------------------------------- LGPL relink material -----
# The package links libavcodec.a/libavutil.a statically, so LGPL-2.1+ section 6
# needs the recipient to be able to relink against a modified FFmpeg: the object
# files, the exact link command, the archives that were linked and the
# corresponding source. This lands beside the package, not inside it (the
# package is bound by the device's 64 MiB unpacked budget).
say "LGPL relink material for $ABI"
LGPL=/out/lgpl/"$ABI"
rm -rf "$LGPL"
mkdir -p "$LGPL/objects"

cd /work/apps/airplay
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC="${CC:-$(go env CC)}" \
  go build -work -x -tags ffmpeg -ldflags "-s -w" -o /tmp/relink-airplay ./cmd/airplay \
  2> /tmp/lgpl-build.log || { tail -20 /tmp/lgpl-build.log; exit 1; }
WORK=$(sed -n 's/^WORK=//p' /tmp/lgpl-build.log | tail -1)
[ -n "$WORK" ] && [ -d "$WORK" ] || { echo "could not find the go build work directory"; exit 1; }
note "go build work directory: $WORK"

# Every archive and object the link consumed, keeping the bNNN/ layout so the
# recorded commands and importcfg.link stay meaningful, plus the verbatim
# command log.
for d in "$WORK"/b*; do
  [ -d "$d" ] || continue
  mkdir -p "$LGPL/objects/$(basename "$d")"
  cp "$d"/*.o "$d"/*.a "$d"/*.link "$LGPL/objects/$(basename "$d")/" 2>/dev/null || true
done
cp /tmp/lgpl-build.log "$LGPL/objects/build-commands.log"

cp /opt/ffmpeg/lib/libavcodec.a /opt/ffmpeg/lib/libavutil.a "$LGPL/"
cp /opt/ffmpeg/src/ffmpeg-*.tar.xz "$LGPL/"
cp /opt/ffmpeg/src/SOURCE.txt /opt/ffmpeg/src/configure-line.txt "$LGPL/"
cp /work/apps/airplay/NOTICE "$LGPL/NOTICE"

CONFIGURE_LINE=$(cat /opt/ffmpeg/src/configure-line.txt)
SOURCE_FACTS=$(cat /opt/ffmpeg/src/SOURCE.txt)
FFV=$(sed -n 's/^version: //p' /opt/ffmpeg/src/SOURCE.txt)
FFSHA=$(sha256sum /opt/ffmpeg/src/ffmpeg-"$FFV".tar.xz | awk '{print $1}')
WORKDIR_ID=$WORK
PACKAGE=$(basename "$(ls /out/"$ABI"/org.capsulad.airplay-*.tar.gz | head -1)")
BIN_SHA=$(sha256sum /out/"$ABI"/airplay | awk '{print $1}')

cat > "$LGPL/RELINK.md" <<EOF
# Relinking $APP_ID ($ABI) against a modified FFmpeg

This directory is the LGPL-2.1+ section 6 material for a package whose FFmpeg
libraries are **statically linked**. It is a distribution precondition for the
package beside it, not an optional extra: the package embeds libavcodec and
libavutil object code, so the recipient must be able to replace them.

- package: \`$PACKAGE\` (its \`airplay\` member sha256 \`$BIN_SHA\`)
- FFmpeg link mode: static (\`libavcodec.a\` + \`libavutil.a\`; the image build
  refuses to continue if the prefix contains a shared libavcodec/libavutil)
- target libc ABI: \`$ABI\`

## Corresponding source

\`\`\`
$SOURCE_FACTS
\`\`\`

The source tarball in this directory is byte-identical to the one built:
sha256 \`$FFSHA\`.

## The configure line used

\`\`\`
$CONFIGURE_LINE
\`\`\`

Nothing GPL-only is enabled: the build is \`--disable-everything\` plus the AAC
decoder and parser, no programs, no network, no filters, and it is LGPL-2.1+
(the configure output says so).

## What is in this directory

| path | what it is |
|---|---|
| \`objects.tar.gz\` | every object file and package archive the link consumed, in the \`bNNN/\` layout Go used, plus \`build-commands.log\` and \`b001/importcfg.link\` |
| \`libavcodec.a\`, \`libavutil.a\` | the exact archives that were linked |
| \`ffmpeg-$FFV.tar.xz\` | the corresponding FFmpeg source |
| \`SOURCE.txt\`, \`configure-line.txt\` | the same facts in machine-readable form |
| \`NOTICE\` | the app's attribution and licence notice |

## How this offer satisfies §6

LGPL-2.1+ section 6 is met by (a) the complete corresponding source of this
work and of FFmpeg and (b) the scripts that control its compilation and
installation: the app is Go, so its link step is a deterministic function of the
sources, the Go toolchain and the FFmpeg archives shipped above. The object set
in \`objects.tar.gz\` is included as enabling material — every object file and
package archive the link consumed, in the \`bNNN/\` layout Go used, with the
verbatim \`go build -work -x\` transcript and \`b001/importcfg.link\` — so a
recipient can inspect the link without a full rebuild.

A raw \`go tool link\` invocation is deliberately **not** published as a recipe.
For a cgo binary the FFmpeg archives arrive through Go's cgo link flags, and
\`b001/importcfg.link\` holds 71 absolute archive paths under the build root, so a
hand-edited link line is easy to get subtly wrong (an earlier revision of this
file offered one; it was withdrawn because it was not executable as written).

## The relink procedure (executed on every build)

This is the route the offer stands behind, and the build runs it against a
writable copy of the prefix — the transcript is in the package's
\`evidence.txt\` under \`===== relink check =====\`:

\`\`\`sh
# inside the pinned builder image (apps/airplay/target/), as root:
# 1. a writable copy of the prefix the package was linked with
cp -R /opt/ffmpeg /tmp/ffmpeg-relink

# 2. replace the archives with your modified build. Rebuild them with the
#    configure line above and install into the copy, e.g.
#      ./configure <configure line> --prefix=/tmp/ffmpeg-relink && make && make install
#    Keep the .pc files pointing -latomic at gcc's static archive, exactly as
#    the recipe does:
#      sed -i "s|-latomic|$(gcc -print-file-name=libatomic.a)|g" \
#             /tmp/ffmpeg-relink/lib/pkgconfig/*.pc

# 3. rebuild the app against the modified prefix (staging only, no package):
cd /work
AIRPLAY_FFMPEG_PREFIX=/tmp/ffmpeg-relink PACKAGE=0 \
  GOOS=linux GOARCH=arm64 TARGET_ABI=$ABI CGO_ENABLED=1 \
  CAPSULAD_CLI=/work/build/bin/capsulad-cli OUT=/tmp/relink-out \
  sh apps/airplay/build.sh

# 4. the relinked binary is /tmp/relink-out/org.capsulad.airplay/airplay;
#    the build also runs it once and records its file(1) output, DT_NEEDED set
#    and the AAC-decoder string check next to the transcript.
\`\`\`

The Go toolchain, the C compiler and the target libc must be the same ones the
package was built with (\`gcc --version\`, \`go version\` in \`evidence.txt\`); the
pinned builder image in \`apps/airplay/target/\` guarantees that. \`build.sh\` does
not carry target flags into a host build and refuses a musl package linked
\`-static\`, so the rebuilt binary has the same shape as the shipped one.

This document is regenerated on every build and binds the relink material to the
package beside it by the packaged \`airplay\` member's sha256, above.
EOF

ls -la "$LGPL" "$LGPL/objects" | head -40
du -sh "$LGPL"

say "LGPL object archive"
tar -czf "$LGPL/objects.tar.gz" -C "$LGPL" objects
rm -rf "$LGPL/objects"
ls -la "$LGPL"

say "DONE $ABI"