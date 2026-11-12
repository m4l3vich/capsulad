#!/bin/sh
# Regenerates the vendored C ABI header used by the cgo binding.
#
# The published capsula/sdk module must build without the rest of the
# repository, so it carries an exact copy of the canonical native header.
# Packaging/CI should run this after sdk/native/include/capsula/capsula.h
# changes and fail on drift:
#
#   sdk/go/regenerate-abi.sh          # rewrite the copy
#   sdk/go/regenerate-abi.sh --check  # exit 1 if the copy is stale
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
source_header="$here/../native/include/capsula/capsula.h"
target="$here/capsula/abi.h"
banner='/* Code generated from sdk/native/include/capsula/capsula.h by sdk/go/regenerate-abi.sh. DO NOT EDIT. */'

if [ ! -f "$source_header" ]; then
    echo "regenerate-abi: canonical header not found: $source_header" >&2
    exit 1
fi

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
{
    printf '%s\n' "$banner"
    cat "$source_header"
} >"$tmp"

if [ "${1:-}" = "--check" ]; then
    if cmp -s "$tmp" "$target"; then
        exit 0
    fi
    echo "regenerate-abi: $target is stale; run sdk/go/regenerate-abi.sh" >&2
    exit 1
fi

mv "$tmp" "$target"
trap - EXIT
