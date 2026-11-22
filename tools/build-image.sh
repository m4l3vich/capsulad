#!/bin/sh
# Print the pinned libc builder image tag, building it when necessary.
# usage: tools/build-image.sh gnu|musl
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
LIBC=${1:-}
case "$LIBC" in
gnu | musl) ;;
*)
    echo "build-image: LIBC must be gnu or musl" >&2
    exit 2
    ;;
esac

DOCKERFILE="$ROOT/tools/images/$LIBC.Dockerfile"
[ -f "$DOCKERFILE" ] || {
    echo "build-image: missing $DOCKERFILE" >&2
    exit 1
}

if command -v sha256sum >/dev/null 2>&1; then
    HASH=$(sha256sum "$DOCKERFILE" | awk '{print $1}')
else
    HASH=$(shasum -a 256 "$DOCKERFILE" | awk '{print $1}')
fi
IMAGE="capsulad-build:$LIBC-$(printf '%s' "$HASH" | cut -c1-12)"

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    echo "build-image: building $IMAGE" >&2
    docker build --platform linux/arm64 -t "$IMAGE" -f "$DOCKERFILE" "$ROOT/tools/images" >&2
fi

printf '%s\n' "$IMAGE"
