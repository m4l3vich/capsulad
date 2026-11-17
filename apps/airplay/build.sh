#!/bin/sh
# Build and package the AirPlay receiver app.
#
#   sh apps/airplay/build.sh                    # build + package
#   OUT=/tmp/apps sh apps/airplay/build.sh
#   PACKAGE=0 sh apps/airplay/build.sh          # stage the directory only
#   GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc sh apps/airplay/build.sh
#
# The binary is compiled on the build host and packaged with the repository
# CLI; a cross build therefore needs a host-executable packager, which is what
# CAPSULAD_CLI overrides.
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$HERE/../.." && pwd)
APP_ID=org.capsulad.airplay
OUT=${OUT:-"$ROOT/build/apps"}
# PACKAGE: 1 = package, 0 = stage only, auto (default) = package only for a
# Linux target, because a host binary cannot be installed on the device.
PACKAGE=${PACKAGE:-auto}
GO=${GO:-go}
CLI=${CAPSULAD_CLI:-}

say() { printf '%s\n' "$*" >&2; }
die() { say "airplay: $*"; exit 1; }

[ -f "$ROOT/sdk/go/go.mod" ] && [ -d "$ROOT/sdk/go/capsulad" ] ||
  die "Go SDK not found at $ROOT/sdk/go; restore sdk/go or set up the repository root"

# The native runtime is injected by the launcher; the app never bundles it. A
# package that cannot run is not worth producing, so refuse when it is absent.
LIB_DIR=${CAPSULAD_NATIVE_LIB_DIR:-"$ROOT/build/lib"}
[ -f "$LIB_DIR/capsulad-native.json" ] ||
  die "native runtime metadata missing at $LIB_DIR/capsulad-native.json; build it with 'make native' (or set CAPSULAD_NATIVE_LIB_DIR)"
ls "$LIB_DIR"/libcapsulad* >/dev/null 2>&1 ||
  die "no libcapsulad shared library in $LIB_DIR; build it with 'make native' (or set CAPSULAD_NATIVE_LIB_DIR)"

# AAC decoding links libavcodec/libavutil, the same decoder the upstream
# receiver links for AAC-LC buffered streams. It is a build dependency, not a
# bundled receiver: the transport, timing and buffering stay in this app.
if ! pkg-config --exists libavcodec libavutil 2>/dev/null; then
  die "FFmpeg development libraries not found (pkg-config libavcodec libavutil).\
 Install the FFmpeg headers/libraries or point PKG_CONFIG_PATH at them. A device
 package needs an aarch64 build of libavcodec/libavutil (see apps/airplay/README.md)."
fi

# The staged manifest must describe the artifact. A native app is not portable
# across libc ABIs, and this app additionally links a native decoder, so the
# ABI is an explicit build input rather than the source manifest's default.
TARGET_ABI=${TARGET_ABI:-linux-gnu}
case "$TARGET_ABI" in
  linux-gnu | linux-musl) ;;
  *) die "TARGET_ABI must be linux-gnu or linux-musl, not '$TARGET_ABI'" ;;
esac

GOOS=${GOOS:-$($GO env GOOS)}
GOARCH=${GOARCH:-$($GO env GOARCH)}
CGO_ENABLED=${CGO_ENABLED:-1}
CC_WAS_SET=${CC:-}
CC=${CC:-$($GO env CC)}

# A cross build must never pick up the host's FFmpeg: libavcodec for glibc and
# for musl are different artifacts, and a relabelled binary is not a musl app.
AIRPLAY_FFMPEG_PREFIX=${AIRPLAY_FFMPEG_PREFIX:-}
if [ -n "$AIRPLAY_FFMPEG_PREFIX" ]; then
  case "$AIRPLAY_FFMPEG_PREFIX" in
    /*) ;;
    *) die "AIRPLAY_FFMPEG_PREFIX must be an absolute path" ;;
  esac
  [ -d "$AIRPLAY_FFMPEG_PREFIX/lib/pkgconfig" ] ||
    die "AIRPLAY_FFMPEG_PREFIX=$AIRPLAY_FFMPEG_PREFIX has no lib/pkgconfig with libavcodec.pc"
  PKG_CONFIG_LIBDIR="$AIRPLAY_FFMPEG_PREFIX/lib/pkgconfig:$AIRPLAY_FFMPEG_PREFIX/share/pkgconfig"
  PKG_CONFIG_PATH=
  export PKG_CONFIG_LIBDIR PKG_CONFIG_PATH
  CGO_CFLAGS="${CGO_CFLAGS:-} -I$AIRPLAY_FFMPEG_PREFIX/include"
  CGO_LDFLAGS="${CGO_LDFLAGS:-} -L$AIRPLAY_FFMPEG_PREFIX/lib"
  export CGO_CFLAGS CGO_LDFLAGS
fi

# A fully static musl binary cannot dlopen the launcher-injected libcapsulad at
# all ("Dynamic loading not supported"), so it would pass an interpreter check
# and still be unusable. Refuse the combination up front.
if [ "$GOOS" = linux ] && [ "$TARGET_ABI" = linux-musl ]; then
  case " ${CGO_LDFLAGS:-} " in
    *" -static "*)
      die "TARGET_ABI=linux-musl with -static cannot dlopen the injected libcapsulad; link the musl package dynamically (musl's loader is the only dynamic dependency)"
      ;;
  esac
fi

if [ "$GOOS" != "$($GO env GOHOSTOS)" ]; then
  [ -n "$AIRPLAY_FFMPEG_PREFIX" ] || [ -n "${PKG_CONFIG_LIBDIR:-}" ] ||
    die "cross-building for $GOOS needs a $TARGET_ABI FFmpeg: set AIRPLAY_FFMPEG_PREFIX (or PKG_CONFIG_LIBDIR) to the target sysroot; the host's libavcodec is a different libc ABI"
  if [ -z "$CC_WAS_SET" ]; then
    CANDIDATE=
    if [ "$GOARCH" = arm64 ]; then
      case "$TARGET_ABI" in
        linux-gnu) CANDIDATE=aarch64-linux-gnu-gcc ;;
        linux-musl) CANDIDATE=aarch64-linux-musl-gcc ;;
      esac
    fi
    if [ -n "$CANDIDATE" ] && command -v "$CANDIDATE" >/dev/null 2>&1; then
      CC=$CANDIDATE
    else
      die "no $TARGET_ABI cross compiler for $GOARCH: install ${CANDIDATE:-the target C compiler} or pass CC=...; the host compiler is never used for a $GOOS build"
    fi
  fi
fi

LDFLAGS="-s -w"
case "$GOOS" in
  linux) LDFLAGS="-s -w" ;;
  darwin) LDFLAGS="-s -w -linkmode=external" ;;
  *) die "unsupported GOOS $GOOS" ;;
esac

if [ -z "$CLI" ]; then
  if [ "$GOOS/$GOARCH" = "$($GO env GOHOSTOS)/$($GO env GOHOSTARCH)" ] && [ -x "$ROOT/build/bin/capsulad-cli" ]; then
    CLI="$ROOT/build/bin/capsulad-cli"
  elif [ -x "$ROOT/build/bin/capsulad-cli-host" ]; then
    CLI="$ROOT/build/bin/capsulad-cli-host"
  else
    say "airplay: building a host packager with the repository CLI"
    ( cd "$ROOT" && CGO_ENABLED=0 $GO build -trimpath -o "$ROOT/build/bin/capsulad-cli-host" ./cmd/capsulad-cli ) ||
      die "could not build capsulad-cli; set CAPSULAD_CLI"
    CLI="$ROOT/build/bin/capsulad-cli-host"
  fi
fi

dir="$OUT/$APP_ID"
rm -rf "$dir"
rm -f "$OUT/$APP_ID"-*.tar.gz
mkdir -p "$dir/proto" "$dir/docs"

say "airplay: building for $GOOS/$GOARCH ($TARGET_ABI) with the FFmpeg AAC decoder (cgo=$CGO_ENABLED cc=$CC)"
[ -n "$AIRPLAY_FFMPEG_PREFIX" ] && say "airplay: FFmpeg prefix $AIRPLAY_FFMPEG_PREFIX"
( cd "$HERE" && GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED="$CGO_ENABLED" CC="$CC" \
    $GO build -trimpath -tags ffmpeg -ldflags "$LDFLAGS" -o "$dir/airplay" ./cmd/airplay ) ||
  die "native build failed; a cross build needs a matching C compiler/sysroot (CC=...) and a target libavcodec (CGO_CFLAGS/CGO_LDFLAGS or PKG_CONFIG_PATH)"

cp "$HERE/manifest.toml" "$dir/manifest.toml"
if [ "$GOOS" = linux ]; then
  # Only the copy that becomes a package is stamped, through the shared helper
  # every app uses; the source manifest keeps the glibc default.
  sh "$HERE/../../tools/stage-manifest-abi.sh" "$dir/manifest.toml" "$TARGET_ABI"
  say "airplay: staged manifest declares abi = $TARGET_ABI"
else
  say "airplay: host build ($GOOS/$GOARCH) for development; the staged manifest keeps abi = $TARGET_ABI but is not a device artifact"
fi
cp "$HERE/proto/media.protoset" "$dir/proto/media.protoset"
cp "$HERE/docs/media.md" "$dir/docs/media.md"
# Attribution and licence notices (Shairport Sync MIT constants, the FFmpeg
# LGPL link and the repository's licence status) ship with the package.
cp "$HERE/NOTICE" "$dir/NOTICE"
say "airplay: staged $dir (with NOTICE)"

if [ "$PACKAGE" = "auto" ]; then
  if [ "$GOOS" = linux ]; then PACKAGE=1; else PACKAGE=0; fi
fi

if [ "$PACKAGE" = "1" ]; then
  if [ "$GOOS" != linux ]; then
    [ "${AIRPLAY_ALLOW_HOST_PACKAGE:-0}" = "1" ] ||
      die "refusing to package a $GOOS build as a Linux package: the staged binary is not a target artifact. Build with GOOS=linux TARGET_ABI=linux-gnu|linux-musl and AIRPLAY_FFMPEG_PREFIX=..., use PACKAGE=0 for a staging-only development build, or set AIRPLAY_ALLOW_HOST_PACKAGE=1 for a local packaging smoke test (the archive is then not installable on the device)."
    say "airplay: packaging a host development build (AIRPLAY_ALLOW_HOST_PACKAGE=1); it is not installable on the device"
  else
    staged_bin=$(find "$dir" -maxdepth 1 -type f -perm -u+x | head -1)
    [ -n "$staged_bin" ] || die "no staged executable to ABI-check"
    # Fail closed when the interpreter does not match the manifest's libc: a
    # glibc-linked binary can never be shipped as a musl package. A musl build
    # must also be dynamically linked: musl's static libc.a stubs dlopen out
    # ("Dynamic loading not supported"), so a static musl app could not reach
    # the launcher-injected libcapsulad at all. The shared gate owns that rule
    # (tools/check-elf-interp.sh require-dynamic) so every native app agrees.
    abi_mode=
    case "$TARGET_ABI" in linux-musl) abi_mode=require-dynamic ;; esac
    sh "$HERE/../../tools/check-elf-interp.sh" "$staged_bin" "$TARGET_ABI" $abi_mode ||
      die "the staged binary does not match abi = $TARGET_ABI"
    say "airplay: staged binary matches abi = $TARGET_ABI"
  fi
  # A Linux target package goes through the shared gates: ELF class/machine must
  # match the target, the interpreter must match the manifest's libc, and a
  # glibc build is held to the device's symbol ceiling (2.28 on the stock
  # Amlogic image). A static binary passes either way. A host development
  # package (AIRPLAY_ALLOW_HOST_PACKAGE=1) must pass none of these flags: the
  # gate only applies to a Linux target, and a Mach-O binary would be rejected.
  if [ "$GOOS" = linux ]; then
    ( cd "$OUT" && "$CLI" app package "$APP_ID" \
        -target-abi "${TARGET_ABI:-linux-gnu}" \
        -max-glibc "${CAPSULAD_MAX_GLIBC:-}" \
        -target-arch "${CAPSULAD_TARGET_ARCH:-}" ) ||
      die "packaging failed"
  else
    # A host development package must not be judged as a Linux artifact: a
    # parent Makefile may export TARGET_ABI/CAPSULAD_TARGET_ARCH for its own
    # Linux targets, and the packager turns those env defaults into gate flags.
    ( cd "$OUT" && env -u TARGET_ABI -u CAPSULAD_MAX_GLIBC -u CAPSULAD_TARGET_ARCH \
        "$CLI" app package "$APP_ID" ) ||
      die "packaging failed"
  fi
  say "airplay: packaged $OUT/$APP_ID-<version>.tar.gz"
else
  say "airplay: staged directory only (PACKAGE=$PACKAGE, no archive written)"
fi
