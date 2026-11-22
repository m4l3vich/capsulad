#!/bin/sh
# Assemble release assets from the outputs of `make core`, `make apps` and
# `make installer`. The Makefile stays three build targets; this script owns
# platform/boot-hook mapping, flat GitHub asset names and manifest staging.
#
#   tools/release.sh core      --libc gnu|musl --platform P --out DIR [--version V] [--profile release|debug]
#   tools/release.sh apps      --libc gnu|musl --platform P --out DIR [--version V] [--core V] [--profile ...]
#   tools/release.sh installer --out DIR [--version V]
#   tools/release.sh checksums --out DIR
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
CMD=${1:-}
[ -n "$CMD" ] || {
    sed -n '2,9p' "$0" >&2
    exit 2
}
shift

LIBC=
PLATFORM=
OUT=dist
VERSION=
CORE=
PROFILE=${PROFILE:-release}

while [ "$#" -gt 0 ]; do
    case "$1" in
    --libc) LIBC=${2:-}; shift 2 ;;
    --platform) PLATFORM=${2:-}; shift 2 ;;
    --out) OUT=${2:-}; shift 2 ;;
    --version) VERSION=${2:-}; shift 2 ;;
    --core) CORE=${2:-}; shift 2 ;;
    --profile) PROFILE=${2:-}; shift 2 ;;
    *)
        echo "release: unknown argument $1" >&2
        exit 2
        ;;
    esac
done

default_version() {
    if tag=$(git -C "$ROOT" describe --tags --exact-match 2>/dev/null); then
        printf '%s\n' "$tag" | sed -e 's/^v//' -e 's/^apps-//'
        return
    fi
    git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev
}

platform_libc() {
    case "$1" in
    rockchip-stock) echo gnu ;;
    amlogic-stock) echo gnu ;;
    amlogic-alpine) echo musl ;;
    *) return 1 ;;
    esac
}

boot_hook() {
    case "$1" in
    rockchip-stock) echo deploy/rockchip-stock/S99btctl.sh ;;
    amlogic-alpine) echo deploy/amlogic-alpine/capsulad ;;
    *) echo deploy/amlogic-stock/S99sysmon.sh ;;
    esac
}

need_platform() {
    [ -n "$PLATFORM" ] || { echo "release: --platform is required" >&2; exit 2; }
    want=$(platform_libc "$PLATFORM") || { echo "release: unknown platform $PLATFORM" >&2; exit 2; }
    [ -n "$LIBC" ] || { echo "release: --libc is required" >&2; exit 2; }
    [ "$LIBC" = "$want" ] || {
        echo "release: $PLATFORM uses $want, not $LIBC" >&2
        exit 2
    }
}

absolute_out() {
    mkdir -p "$OUT"
    OUT=$(CDPATH= cd -- "$OUT" && pwd)
}

run_stage() {
    tool=$1
    shift
    (cd "$ROOT/installer" && CGO_ENABLED=0 go run "./cmd/$tool" "$@")
}

case "$CMD" in
core)
    need_platform
    [ "$PROFILE" = release ] || [ "$PROFILE" = debug ] || {
        echo "release: PROFILE must be release or debug" >&2
        exit 2
    }
    [ -n "$VERSION" ] || VERSION=$(default_version)
    absolute_out
    SRC="$ROOT/build/$LIBC/$PROFILE"
    for f in bin/capsulad bin/capsulad-cli lib/capsulad-native.json lib/capsulad-quickjs lib/capsulad-micropython; do
        [ -e "$SRC/$f" ] || {
            echo "release: missing $SRC/$f; run 'make core LIBC=$LIBC'" >&2
            exit 1
        }
    done
    lib=
    for f in "$SRC"/lib/libcapsulad.so.*.*; do
        [ -f "$f" ] || continue
        lib=$f
        break
    done
    [ -n "$lib" ] || {
        echo "release: no libcapsulad.so.*.* in $SRC/lib; run 'make core LIBC=$LIBC'" >&2
        exit 1
    }

    tmp=$(mktemp -d "${TMPDIR:-/tmp}/capsulad-core.XXXXXX")
    trap 'rm -rf "$tmp"' EXIT HUP INT TERM
    mkdir -p "$tmp/bundle/lib" "$tmp/bundle/apps" "$tmp/bundle/boot"
    # The build jobs upload a directory artifact, and the artifact service does
    # not preserve the executable bit. The bundle is the installer's deployment
    # input, so stage its modes explicitly instead of trusting the download.
    cp "$SRC/bin/capsulad" "$tmp/bundle/capsulad"
    cp "$SRC/bin/capsulad-cli" "$tmp/bundle/capsulad-cli"
    chmod 0755 "$tmp/bundle/capsulad" "$tmp/bundle/capsulad-cli"
    for f in "$SRC"/lib/*; do
        if [ -f "$f" ] && [ ! -L "$f" ]; then
            dest="$tmp/bundle/lib/$(basename "$f")"
            cp "$f" "$dest"
            case "$(basename "$f")" in
            capsulad-quickjs | capsulad-micropython | libcapsulad.so.*) chmod 0755 "$dest" ;;
            *) chmod 0644 "$dest" ;;
            esac
        fi
    done
    if [ -d "$SRC/include/capsulad" ]; then
        mkdir -p "$tmp/bundle/lib/include"
        cp -R "$SRC/include/capsulad" "$tmp/bundle/lib/include/"
    fi
    hook=$(boot_hook "$PLATFORM")
    cp "$ROOT/$hook" "$tmp/bundle/boot/$(basename "$hook")"
    chmod 0755 "$tmp/bundle/boot/$(basename "$hook")"
    (cd "$tmp/bundle" && COPYFILE_DISABLE=1 tar -czf "$tmp/deploy-bundle.tar.gz" .)

    run_stage stage-release \
        -out "$OUT" -version "$VERSION" -platform "$PLATFORM" \
        -bundle "$tmp/deploy-bundle.tar.gz" -flat
    ;;

apps)
    need_platform
    [ "$PROFILE" = release ] || [ "$PROFILE" = debug ] || {
        echo "release: PROFILE must be release or debug" >&2
        exit 2
    }
    [ -n "$VERSION" ] || VERSION=$(default_version)
    absolute_out
    SRC="$ROOT/build/$LIBC/$PROFILE/apps"
    [ -d "$SRC" ] || {
        echo "release: missing $SRC; run 'make apps LIBC=$LIBC'" >&2
        exit 1
    }
    count=0
    apps=
    for f in "$SRC"/org.capsulad.*-*.tar.gz; do
        [ -f "$f" ] || continue
        count=$((count + 1))
        if [ -n "$apps" ]; then
            apps="$apps,$f"
        else
            apps=$f
        fi
    done
    [ "$count" -eq 10 ] || {
        echo "release: expected 10 app archives in $SRC, found $count" >&2
        exit 1
    }
    set -- -out "$OUT" -version "$VERSION" -platform "$PLATFORM" -libc "$LIBC" -apps "$apps" -flat
    if [ -n "$CORE" ]; then
        set -- "$@" -core "$CORE"
    fi
    run_stage stage-apps "$@"
    ;;

installer)
    [ -n "$VERSION" ] || VERSION=$(default_version)
    absolute_out
    SRC="$ROOT/build/installer"
    for os in darwin linux windows; do
        for arch in amd64 arm64; do
            bin="$SRC/$os-$arch/capsulad-installer"
            if [ "$os" = windows ]; then
                bin="$bin.exe"
            fi
            [ -f "$bin" ] || {
                echo "release: missing $bin; run 'make installer'" >&2
                exit 1
            }
            if [ "$os" = windows ]; then
                command -v zip >/dev/null 2>&1 || {
                    echo "release: zip is required for the Windows installer" >&2
                    exit 1
                }
                zip -q -j "$OUT/capsulad-installer_${VERSION}_${os}_${arch}.zip" "$bin"
            else
                tmp=$(mktemp -d "${TMPDIR:-/tmp}/capsulad-installer.XXXXXX")
                cp "$bin" "$tmp/capsulad-installer"
                # The artifact service does not preserve the executable bit;
                # the released archive must contain a runnable installer.
                chmod 0755 "$tmp/capsulad-installer"
                tar -C "$tmp" -czf "$OUT/capsulad-installer_${VERSION}_${os}_${arch}.tar.gz" capsulad-installer
                rm -rf "$tmp"
            fi
        done
    done
    ;;

checksums)
    absolute_out
    (
        cd "$OUT"
        for f in *; do
            [ -f "$f" ] || continue
            if [ "$f" = SHA256SUMS ]; then
                continue
            fi
            if command -v sha256sum >/dev/null 2>&1; then
                sha256sum "$f"
            else
                shasum -a 256 "$f"
            fi
        done >SHA256SUMS
    )
    ;;

*)
    echo "release: unknown command $CMD" >&2
    exit 2
    ;;
esac
