#!/bin/sh
# Host proof for the optional deployment-bundle CLI (BUNDLE_CLI).
#
# Assembles the bundle twice from stub build outputs, using the same
# `deploy-bundle` recipe as a real build, so this neither compiles Go nor
# touches build/. The deploy-side rule that a CLI-free bundle is accepted and
# leaves a separately installed CLI untouched is
# TestDeployWithoutCLILeavesInstalledCLI in cmd/capsulad-cli.
#
#   sh tools/check-deploy-bundle.sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

work=$(mktemp -d "${TMPDIR:-/tmp}/capsulad-bundle-check.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

mkdir -p "$work/bin" "$work/lib" "$work/include" "$work/apps"
cat > "$work/bin/capsulad" <<'EOF'
#!/bin/sh
echo 'capsulad stub (sdk 0.5)'
EOF
cat > "$work/bin/capsulad-cli" <<'EOF'
#!/bin/sh
echo 'capsulad-cli stub'
EOF
chmod 0755 "$work/bin/capsulad" "$work/bin/capsulad-cli"

# Paths contain no spaces (mktemp under TMPDIR), so unquoted expansion is the
# intended word split.
# shellcheck disable=SC2086
MAKE_ARGS="BUNDLE_NATIVE=0 BINDIR=$work/bin DEPLOYDIR=$work/bundle DEPLOY_TARBALL=$work/bundle.tar.gz LIBDIR=$work/lib INCLUDEDIR=$work/include APPDIR=$work/apps"

assemble() {
	make -o capsulad -o capsulad-cli deploy-bundle $MAKE_ARGS "$@"
}

in_tar() {
	tar -tzf "$work/bundle.tar.gz" | grep -qxF "./$1"
}

fail() {
	echo "check-deploy-bundle: $*" >&2
	exit 1
}

# Default: the CLI is built (prerequisite) and travels in the bundle.
make -n deploy-bundle $MAKE_ARGS > "$work/default-dry.log" 2>&1
grep -qF './cmd/capsulad-cli' "$work/default-dry.log" ||
	fail "default bundle does not build the CLI"
grep -qF "$work/bin/capsulad-cli $work/bundle/capsulad-cli" "$work/default-dry.log" ||
	fail "default bundle does not copy the CLI"

assemble > "$work/default.log" 2>&1
[ -f "$work/bundle/capsulad-cli" ] || fail "default bundle has no CLI"
[ -f "$work/bundle/capsulad" ] || fail "default bundle has no daemon"
in_tar capsulad-cli || fail "default tarball has no CLI"
in_tar capsulad || fail "default tarball has no daemon"

# Ambiguous values are rejected up front: the old mixed predicate built the CLI
# and then omitted it.
if make -n deploy-bundle $MAKE_ARGS BUNDLE_CLI=yes > "$work/invalid.log" 2>&1; then
	fail "BUNDLE_CLI=yes was accepted"
fi
grep -qF "BUNDLE_CLI must be 0 or 1, got 'yes'" "$work/invalid.log" ||
	fail "BUNDLE_CLI=yes did not explain the rejected value"

# Opt-in: the CLI is neither a prerequisite nor copied, and a stale CLI in a
# reused DEPLOYDIR is removed with the rest of the previous bundle.
rm -f "$work/bin/capsulad-cli"
printf '#!/bin/sh\n# stale CLI from a previous bundle\n' > "$work/bundle/capsulad-cli"
chmod 0755 "$work/bundle/capsulad-cli"

make -n deploy-bundle $MAKE_ARGS BUNDLE_CLI=0 > "$work/omit-dry.log" 2>&1
if grep -qF './cmd/capsulad-cli' "$work/omit-dry.log"; then
	fail "BUNDLE_CLI=0 still builds the CLI"
fi
if grep -qF "$work/bin/capsulad-cli" "$work/omit-dry.log"; then
	fail "BUNDLE_CLI=0 still copies the CLI"
fi

assemble BUNDLE_CLI=0 > "$work/omit.log" 2>&1
[ ! -e "$work/bundle/capsulad-cli" ] || fail "BUNDLE_CLI=0 kept a stale CLI"
if in_tar capsulad-cli; then
	fail "BUNDLE_CLI=0 tarball contains a CLI"
fi
in_tar capsulad || fail "BUNDLE_CLI=0 tarball has no daemon"
in_tar boot/S99btctl.sh || fail "BUNDLE_CLI=0 tarball has no boot hook"
grep -qF 'no CLI (BUNDLE_CLI=0)' "$work/omit.log" ||
	fail "BUNDLE_CLI=0 did not report the omission"

# Switching back on the same reused directory must restore the CLI rather than
# preserve the previous omission.
cat > "$work/bin/capsulad-cli" <<'EOF'
#!/bin/sh
echo 'capsulad-cli stub'
EOF
chmod 0755 "$work/bin/capsulad-cli"
assemble > "$work/default-again.log" 2>&1
[ -f "$work/bundle/capsulad-cli" ] || fail "switching back to the default lost the CLI"
in_tar capsulad-cli || fail "switching back to the default lost the CLI in the tarball"

echo "check-deploy-bundle: default and CLI-free bundles assembled as expected"
