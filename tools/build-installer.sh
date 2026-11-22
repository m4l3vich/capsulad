#!/bin/sh
# Build the terminal installer for every supported host.
#
# usage: tools/build-installer.sh release|debug
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
PROFILE=${1:-release}
case "$PROFILE" in
release) LDFLAGS="-s -w" ;;
debug) LDFLAGS="" ;;
*)
    echo "build-installer: PROFILE must be release or debug" >&2
    exit 2
    ;;
esac

VERSION=${VERSION:-$(cd "$ROOT" && git describe --tags --always --dirty 2>/dev/null |
    sed -e 's/^v//' -e 's/^apps-//')}
[ -n "$VERSION" ] || VERSION=0.1.0-dev
OUT="$ROOT/build/installer"

for os in darwin linux windows; do
    for arch in amd64 arm64; do
        ext=
        if [ "$os" = windows ]; then
            ext=.exe
        fi
        dir="$OUT/$os-$arch"
        mkdir -p "$dir"
        echo "installer: $os/$arch" >&2
        (
            cd "$ROOT/installer"
            CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
                go build -trimpath \
                -ldflags "$LDFLAGS -X main.version=$VERSION" \
                -o "$dir/capsulad-installer$ext" .
        )
    done
done

printf 'installer: %s -> %s\n' "$VERSION" "$OUT"
