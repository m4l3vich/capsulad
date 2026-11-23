#!/bin/sh
# Build the AirPlay device packages for the target libc ABIs inside pinned
# containers, and publish the LGPL relink material beside them.
#
#   OUT=/tmp/airplay-target sh apps/airplay/target/build-target.sh
#   OUT=/tmp/airplay-target TARGET_ABI=linux-musl sh apps/airplay/target/build-target.sh
#   make airplay-target                       # OUT=build/apps/target
#
# The repository is never built in place: the tree is copied to a temporary
# directory, mounted at /work, and every artifact is written under $OUT.
#
# Environment:
#   OUT            (required) artifact directory. Absolute path.
#   TARGET_ABI     all (default) | linux-gnu | linux-musl
#   CACHE          downloaded-input cache
#                  (default ${XDG_CACHE_HOME:-$HOME/.cache}/capsulad-airplay-target)
#   RUNTIME_DIR    optional target libcapsulad.so.1 for the load smoke, looked up
#                  as <dir>/<abi>/libcapsulad.so.1 then <dir>/libcapsulad.so.1
#                  (the file's libc is detected; a mismatched one is skipped so
#                  the smoke never mistakes "wrong libc" for "app is broken").
#                  Default: $OUT/runtime
#   GLIBC_CEILING  override the stock glibc ceiling (default 2.28, see pins.sh)
#   KEEP=1         keep the temporary build tree
#   NO_CACHE=1     rebuild the container image from scratch (no layer cache)
#   PLATFORM       docker --platform (default linux/arm64)
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$HERE/../../.." && pwd)
# shellcheck source=pins.sh
. "$HERE/pins.sh"

PLATFORM=${PLATFORM:-$AIRPLAY_TARGET_PLATFORM}
ABIS=${TARGET_ABI:-all}
GLIBC_CEILING=${GLIBC_CEILING:-$AIRPLAY_TARGET_GNU_CEILING}

say() { printf 'airplay-target: %s\n' "$*" >&2; }
die() { printf 'airplay-target: %s\n' "$*" >&2; exit 1; }

[ -n "${OUT:-}" ] || die "set OUT=<artifact directory>; packages and the LGPL offer are written there, never into the repository"
case "$OUT" in /*) ;; *) die "OUT must be an absolute path (got '$OUT')" ;; esac
command -v docker >/dev/null 2>&1 || die "docker is required (the builds run in the pinned images)"

case "$ABIS" in
  all) ABIS="linux-gnu linux-musl" ;;
  linux-gnu | linux-musl) ;;
  *) die "TARGET_ABI must be all, linux-gnu or linux-musl (got '$ABIS')" ;;
esac

mkdir -p "$OUT" || die "cannot create OUT=$OUT"
OUT=$(CDPATH= cd -- "$OUT" && pwd)

CACHE=${CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/capsulad-airplay-target}
mkdir -p "$CACHE"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/capsulad-airplay-target.XXXXXX")
cleanup() {
  if [ "${KEEP:-0}" = 1 ]; then say "kept the build tree at $WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT INT TERM

# ------------------------------------------------------------ pinned inputs --
sha_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}
sha_file() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$@"
  else
    sha256sum "$@"
  fi
}

fetch_pinned() { # URL SHA256 DEST
  url=$1 sha=$2 dest=$3
  if [ -f "$dest" ] && [ "$(sha_of "$dest")" = "$sha" ]; then
    say "cached $(basename "$dest") ($sha)"
    return 0
  fi
  rm -f "$dest"
  say "fetch $url"
  curl -fsSL --retry 3 --retry-delay 2 -o "$dest.part" "$url" ||
    { rm -f "$dest.part"; die "download failed: $url"; }
  [ "$(sha_of "$dest.part")" = "$sha" ] ||
    { rm -f "$dest.part"; die "sha256 mismatch for $url (expected $sha)"; }
  mv "$dest.part" "$dest"
  say "verified $(basename "$dest") sha256 $sha"
}

GO_ZIP="$CACHE/toolchain-go${AIRPLAY_TARGET_GO_VERSION}.linux-arm64.zip"
FFMPEG_TAR="$CACHE/ffmpeg-${AIRPLAY_TARGET_FFMPEG_VERSION}.tar.xz"
fetch_pinned "$AIRPLAY_TARGET_GO_URL" "$AIRPLAY_TARGET_GO_SHA256" "$GO_ZIP"
fetch_pinned "$AIRPLAY_TARGET_FFMPEG_URL" "$AIRPLAY_TARGET_FFMPEG_SHA256" "$FFMPEG_TAR"

# ----------------------------------------------------------- build context --
CTX=$WORK/ctx
mkdir -p "$CTX/container" "$CTX/deps/repo/apps/airplay" "$CTX/deps/repo/sdk/go" "$WORK/src"
cp "$HERE/container/ffmpeg.sh" "$HERE/container/run.sh" "$CTX/container/"
cp "$GO_ZIP" "$CTX/toolchain-go${AIRPLAY_TARGET_GO_VERSION}.linux-arm64.zip"
cp "$FFMPEG_TAR" "$CTX/ffmpeg-${AIRPLAY_TARGET_FFMPEG_VERSION}.tar.xz"

# The module graph is downloaded while building the image (go.sum pins every
# module), so a package build itself needs no network.
cp "$ROOT/go.mod" "$ROOT/go.sum" "$CTX/deps/repo/"
cp "$ROOT/apps/airplay/go.mod" "$ROOT/apps/airplay/go.sum" "$CTX/deps/repo/apps/airplay/"
cp -R "$ROOT/sdk/go/." "$CTX/deps/repo/sdk/go/"

say "copying the working tree to $WORK/src (the repository is never built in place)"
(
  cd "$ROOT" &&
    tar -cf - \
      --exclude='./.git' \
      --exclude='./_capsula-mini*' \
      --exclude='./build' \
      --exclude='./sdk/native' \
      --exclude='./sdk/rust' \
      --exclude='./sdk/cpp' \
      .
) | (cd "$WORK/src" && tar -xf -) || die "could not copy the working tree"

# build.sh refuses to stage a package without native-runtime metadata, and the
# runtime itself is launcher-injected (never linked). Stage the metadata plus an
# empty placeholder so the check reflects "the launcher provides this", not a
# build product of this recipe.
mkdir -p "$WORK/src/build/lib"
printf '%s\n' '{"abi_major":1,"min_minor":0,"note":"launcher-injected runtime; not part of the package"}' \
  > "$WORK/src/build/lib/capsulad-native.json"
: > "$WORK/src/build/lib/libcapsulad.so.1"

# ------------------------------------------------------ runtime for smoke ---
pick_runtime() { # ABI -> prints a matching path, or nothing
  abi=$1
  for cand in "$RUNTIME_DIR/$abi/libcapsulad.so.1" "$RUNTIME_DIR/libcapsulad.so.1"; do
    [ -f "$cand" ] || continue
    if grep -qa 'libc\.musl' "$cand"; then libc=musl; else libc=gnu; fi
    case "$abi:$libc" in
      linux-musl:musl | linux-gnu:gnu) printf '%s\n' "$cand"; return 0 ;;
    esac
  done
  return 1
}

# --------------------------------------------------------------- one ABI ----
build_abi() {
  abi=$1
  image="capsulad-airplay-target:$abi"
  say "=== $abi: building $image from $abi/Dockerfile ==="

  # NO_CACHE=1 rebuilds the image from scratch (the layers are content-addressed,
  # so a corrupted image layer would otherwise survive a normal rebuild).
  build_cache=
  [ "${NO_CACHE:-0}" = 1 ] && build_cache=--no-cache
  # shellcheck disable=SC2086
  docker build $build_cache --platform "$PLATFORM" -f "$HERE/$abi/Dockerfile" -t "$image" \
    --build-arg GO_VERSION="$AIRPLAY_TARGET_GO_VERSION" \
    --build-arg GO_SHA256="$AIRPLAY_TARGET_GO_SHA256" \
    --build-arg FFMPEG_VERSION="$AIRPLAY_TARGET_FFMPEG_VERSION" \
    --build-arg FFMPEG_URL="$AIRPLAY_TARGET_FFMPEG_URL" \
    --build-arg FFMPEG_SHA256="$AIRPLAY_TARGET_FFMPEG_SHA256" \
    --build-arg APK_MIRROR="$AIRPLAY_TARGET_MUSL_APK_MIRROR" \
    --build-arg SNAPSHOT="$AIRPLAY_TARGET_GNU_SNAPSHOT" \
    --build-arg GOPROXY_DOWNLOAD="$AIRPLAY_TARGET_GOPROXY" \
    "$CTX" || die "$abi: image build failed"

  mkdir -p "$OUT/$abi"
  run_args=""
  if [ -n "$RUNTIME_DIR" ] && runtime=$(pick_runtime "$abi"); then
    say "$abi: load smoke will use $runtime"
    run_args="-v $runtime:/runtime/$abi/libcapsulad.so.1:ro"
  else
    say "$abi: no $abi runtime supplied; the load smoke can only show the missing library"
  fi

  say "=== $abi: running the build ==="
  # The transcript is teed to the work directory, not to $OUT/$abi: the
  # container removes and recreates /out/<abi>, so a file opened there first
  # would be unlinked while it is still being written.
  # The container runs as root so it can hide /opt/ffmpeg for the runtime
  # dependency proof. It writes the package, binary and LGPL material into the
  # host-mounted $OUT and its build tree into $WORK, so hand those back to the
  # invoking user before exiting; otherwise the caller's evidence copy and
  # cleanup fail on a CI runner.
  # shellcheck disable=SC2086
  docker run --rm --platform "$PLATFORM" \
    -e HOST_UID="$(id -u)" \
    -e HOST_GID="$(id -g)" \
    -v "$WORK/src:/work" \
    -v "$OUT:/out" \
    $run_args \
    -e TARGET_ABI="$abi" \
    -e GLIBC_CEILING="$GLIBC_CEILING" \
    "$image" 2>&1 | tee "$WORK/evidence-$abi.txt"

  # A failed container must fail the recipe: the pipeline above reports tee's
  # status, so the container's own end marker is what proves it finished.
  grep -q "===== DONE $abi =====" "$WORK/evidence-$abi.txt" || {
    tail -40 "$WORK/evidence-$abi.txt" >&2
    die "$abi: the build did not reach its end marker"
  }

  # A silent container exit with no package must fail the recipe too.
  pkg=$(ls "$OUT/$abi"/org.capsulad.airplay-*.tar.gz 2>/dev/null | head -1) ||
    die "$abi: no package was written to $OUT/$abi"
  [ -n "$pkg" ] || die "$abi: no package was written to $OUT/$abi"
  cp "$WORK/evidence-$abi.txt" "$OUT/$abi/evidence.txt"
  chmod 644 "$OUT/$abi/evidence.txt" 2>/dev/null || true

  # The package is the deliverable, so its manifest is checked here and not
  # only inside the container.
  staged_abi=$(tar -xzOf "$pkg" manifest.toml | sed -n 's/^abi = "\(.*\)"$/\1/p' | head -1)
  [ "$staged_abi" = "$abi" ] ||
    die "$abi: $pkg declares abi \"$staged_abi\"; refusing to publish it"

  ( cd "$OUT/$abi" && sha_file ./*.tar.gz ./airplay > SHA256SUMS )
  say "=== $abi: artifact ready ==="
  sed -n '1,10p' "$OUT/$abi/SHA256SUMS" >&2
}

RUNTIME_DIR=${RUNTIME_DIR:-$OUT/runtime}

for abi in $ABIS; do
  build_abi "$abi"
done

# --------------------------------------------------------------- summary ----
say "done"
for abi in $ABIS; do
  pkg=$(ls "$OUT/$abi"/org.capsulad.airplay-*.tar.gz | head -1)
  say "  $abi: $pkg ($(wc -c <"$pkg" | tr -d ' ') B)"
  say "         binary  $OUT/$abi/airplay ($(wc -c <"$OUT/$abi/airplay" | tr -d ' ') B)"
  say "         evidence $OUT/$abi/evidence.txt"
  say "         LGPL    $OUT/lgpl/$abi/ (objects.tar.gz, libavcodec.a, libavutil.a,"
  say "                 ffmpeg-${AIRPLAY_TARGET_FFMPEG_VERSION}.tar.xz, RELINK.md, NOTICE)"
done