#!/bin/sh
# Rewrite the single top-level `abi = "..."` line of a staged manifest.
# usage: stage-manifest-abi.sh MANIFEST TARGET_ABI   (TARGET_ABI: linux-gnu|linux-musl)
# The source manifest keeps its default; only the copy that becomes a package is
# stamped, and the packager itself never rewrites the field.
set -eu
manifest=${1:-}
abi=${2:-}
case "$abi" in
linux-gnu | linux-musl) ;;
*)
    echo "error: TARGET_ABI must be linux-gnu or linux-musl, got '$abi'" >&2
    exit 2
    ;;
esac
case "${GOOS:-}" in
"" | linux) ;;
*)
    echo "error: refusing to stamp abi=$abi into a $GOOS build; TARGET_ABI only applies to GOOS=linux artifacts" >&2
    exit 1
    ;;
esac
[ -f "$manifest" ] || { echo "error: $manifest is missing" >&2; exit 2; }
count=$(grep -c '^abi[[:space:]]*=' "$manifest" || true)
[ "$count" = "1" ] || { echo "error: $manifest has $count top-level abi lines, want 1" >&2; exit 1; }
tmp=$manifest.tmp.$$
sed "s|^abi[[:space:]]*=.*|abi = \"$abi\"|" "$manifest" >"$tmp"
mv "$tmp" "$manifest"
grep -q "^abi = \"$abi\"$" "$manifest" || { echo "error: staging $abi into $manifest failed" >&2; exit 1; }
