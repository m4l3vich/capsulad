#!/bin/sh
# Build the QuickJS and MicroPython app hosts and stage them beside the shared
# native SDK in build/lib. Both engines are built from pinned upstream source;
# no interpreter is replaced with Node, CPython or a hand-written evaluator.
#
# Reproducible build inputs:
#   quickjs-ng v0.18.0  sha256 28bcd89aa3683fd2a946537e5ce3f11cc46b5cbf3383c7561aaef3df5ff9f1dd
#   micropython 1c6321  sha256 0fbc545074a0960fc6c64c161bcc883aa101889514eb99f2d52a2ed9cefdf18e
#
# Offline/cached builds may set QUICKJS_SRC and MICROPYTHON_SRC to already
# extracted matching source trees. The pins above are still checked only for
# downloads; a cache directory is trusted by the operator.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUT=${OUT:-"$ROOT/build/lib"}
WORK=${WORK:-"$ROOT/build/interp"}
# Workers change directory (MicroPython's embed make) and embed these paths in
# generated makefiles, so a relative OUT/WORK would be reinterpreted from the
# new cwd. Normalize both against the repository root.
case "$OUT" in /*) ;; *) OUT="$ROOT/$OUT" ;; esac
case "$WORK" in /*) ;; *) WORK="$ROOT/$WORK" ;; esac
COMMON="$ROOT/interpreter/common"
QUICKJS_DIR="$ROOT/interpreter/quickjs"
MICROPYTHON_DIR="$ROOT/interpreter/micropython"
INCLUDE="$ROOT/sdk/native/include"
TARGETS=${TARGETS:-"quickjs micropython"}
CC=${CC:-cc}
JOBS=${JOBS:-$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)}
UNAME=$(uname -s)

QJS_VERSION=0.18.0
QJS_SHA256=28bcd89aa3683fd2a946537e5ce3f11cc46b5cbf3383c7561aaef3df5ff9f1dd
QJS_URL="https://codeload.github.com/quickjs-ng/quickjs/tar.gz/refs/tags/v${QJS_VERSION}"
MP_COMMIT=1c63211817d9c5164542b94771634cf80b300fdf
MP_SHA256=0fbc545074a0960fc6c64c161bcc883aa101889514eb99f2d52a2ed9cefdf18e
MP_URL="https://codeload.github.com/micropython/micropython/tar.gz/${MP_COMMIT}"

say() { printf '%s\n' "$*" >&2; }
die() { say "interpreter build: $*"; exit 1; }

# Both hosts dlopen libcapsulad. glibc below 2.34 still ships dlopen/dlsym in
# libdl, so a Linux link needs -ldl; newer glibc folds them into libc and the
# extra flag is harmless.
DL_LIBS=
if [ "$UNAME" = Linux ]; then
  DL_LIBS="-ldl"
fi

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

download() {
  url=$1
  dest=$2
  want=$3
  if [ -f "$dest" ] && [ "$(sha256 "$dest")" = "$want" ]; then
    return 0
  fi
  command -v curl >/dev/null 2>&1 || die "curl is required to fetch $url"
  say "fetching $url"
  rm -f "$dest"
  curl -fL --retry 3 --connect-timeout 20 -o "$dest" "$url" ||
    die "cannot download $url; set the corresponding *_SRC to a pinned source tree"
  got=$(sha256 "$dest")
  [ "$got" = "$want" ] || die "sha256 mismatch for $url (got $got, want $want)"
}

extract_source() {
  tarball=$1
  dest=$2
  [ -d "$dest" ] && return 0
  mkdir -p "$dest"
  tar -xzf "$tarball" -C "$dest" --strip-components=1
}

build_quickjs() {
  command -v cmake >/dev/null 2>&1 || die "cmake is required for QuickJS"

  if [ -n "${QUICKJS_SRC:-}" ]; then
    qjs_src=$QUICKJS_SRC
  else
    mkdir -p "$WORK/cache" "$WORK/src"
    tarball="$WORK/cache/quickjs-${QJS_VERSION}.tar.gz"
    download "$QJS_URL" "$tarball" "$QJS_SHA256"
    qjs_src="$WORK/src/quickjs-${QJS_VERSION}"
    extract_source "$tarball" "$qjs_src"
  fi
  [ -f "$qjs_src/quickjs.h" ] || die "$qjs_src is not a QuickJS source tree"

  qjs_build="$WORK/quickjs-build"
  mkdir -p "$qjs_build"
  cmake -S "$qjs_src" -B "$qjs_build" \
    -DCMAKE_BUILD_TYPE=Release \
    -DBUILD_SHARED_LIBS=OFF \
    -DQJS_BUILD_EXAMPLES=OFF \
    -DQJS_BUILD_CLI_STATIC=OFF \
    -DQJS_ENABLE_INSTALL=OFF >&2
  cmake --build "$qjs_build" --target qjs -- -j"$JOBS" >&2

  extra=$DL_LIBS
  mkdir -p "$OUT"
  "$CC" -O2 -std=c11 -Wall -Wextra \
    -I"$INCLUDE" -I"$COMMON" -I"$qjs_src" \
    "$QUICKJS_DIR/host.c" "$COMMON/capsulad_host.c" \
    "$qjs_build/libqjs.a" -lm -lpthread $extra \
    -o "$OUT/capsulad-quickjs"
  chmod 0755 "$OUT/capsulad-quickjs"
  say "staged $OUT/capsulad-quickjs ($("$OUT/capsulad-quickjs" --version))"
}

generate_prelude() {
  python3 - "$MICROPYTHON_DIR/sdk.py" "$WORK/micropython/sdk_prelude.h" <<'PY'
import json
import sys

source_path, out_path = sys.argv[1], sys.argv[2]
with open(source_path, encoding="utf-8") as source:
    text = source.read()
with open(out_path, "w", encoding="utf-8") as out:
    out.write("/* generated from interpreter/micropython/sdk.py */\n")
    out.write("static const char sdk_prelude[] =\n")
    for line in text.splitlines(True):
        out.write("  " + json.dumps(line) + "\n")
    out.write("  ;\n")
PY
}

build_micropython() {
  if [ -n "${MICROPYTHON_SRC:-}" ]; then
    mp_src=$MICROPYTHON_SRC
  else
    mkdir -p "$WORK/cache" "$WORK/src"
    tarball="$WORK/cache/micropython-${MP_COMMIT}.tar.gz"
    download "$MP_URL" "$tarball" "$MP_SHA256"
    mp_src="$WORK/src/micropython-${MP_COMMIT}"
    extract_source "$tarball" "$mp_src"
  fi
  [ -f "$mp_src/py/mpconfig.h" ] || die "$mp_src is not a MicroPython source tree"

  mp_work="$WORK/micropython"
  mp_pkg="$mp_work/micropython_embed"
  mkdir -p "$mp_work"
  cp "$MICROPYTHON_DIR/mpconfigport.h" "$mp_work/mpconfigport.h"
  cat >"$mp_work/embed.mk" <<EOF
MICROPYTHON_TOP := $mp_src
SRC_QSTR += $MICROPYTHON_DIR/modcapsulad.c
CFLAGS += -I$MICROPYTHON_DIR -I$COMMON -I$INCLUDE
include \$(MICROPYTHON_TOP)/ports/embed/embed.mk
EOF

  rm -rf "$mp_work/build-embed" "$mp_pkg" "$WORK/micropython/sdk_prelude.h"
  (cd "$mp_work" && make -f embed.mk PACKAGE_DIR=micropython_embed >&2)
  [ -f "$mp_pkg/port/embed_util.c" ] || die "MicroPython embed package was not generated"

  generate_prelude
  cp "$mp_pkg/genhdr/moduledefs.h" "$mp_work/moduledefs.generated.h"

  objdir="$mp_work/obj"
  rm -rf "$objdir"
  mkdir -p "$objdir"
  # modcapsulad uses clock_gettime/CLOCK_MONOTONIC, which a strict -std=c99
  # build hides unless the POSIX feature set is requested.
  cflags="-O2 -std=c99 -D_POSIX_C_SOURCE=200809L -fno-common -I$mp_work -I$mp_pkg -I$MICROPYTHON_DIR -I$COMMON -I$INCLUDE"
  count=0
  for source in $(find "$mp_pkg/py" "$mp_pkg/extmod" "$mp_pkg/shared/runtime" "$mp_pkg/port" -name '*.c' | sort); do
    count=$((count + 1))
    "$CC" $cflags -c "$source" -o "$objdir/$count-$(basename "$source" .c).o"
  done
  "$CC" $cflags -c "$COMMON/capsulad_host.c" -o "$objdir/capsulad_host.o"
  "$CC" $cflags -c "$MICROPYTHON_DIR/modcapsulad.c" -o "$objdir/modcapsulad.o"
  "$CC" $cflags -c "$MICROPYTHON_DIR/host.c" -o "$objdir/host.o"

  mkdir -p "$OUT"
  "$CC" "$objdir"/*.o -lm -lpthread $DL_LIBS -o "$OUT/capsulad-micropython"
  chmod 0755 "$OUT/capsulad-micropython"
  say "staged $OUT/capsulad-micropython ($("$OUT/capsulad-micropython" --version))"
}

mkdir -p "$OUT" "$WORK"
for target in $TARGETS; do
  case "$target" in
    quickjs) build_quickjs ;;
    micropython) build_micropython ;;
    *) die "unknown target $target (expected quickjs or micropython)" ;;
  esac
done
