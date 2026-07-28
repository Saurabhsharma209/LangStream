#!/usr/bin/env bash
#
# check-vendor-keys_test.sh — regression test for check-vendor-keys.sh.
#
# check-vendor-keys.sh (Sprint 16, 2026-07-27) was verified by hand against
# scratch fixtures once, then finalized with no automated test of its own:
# a future edit to its grep/awk parsing assumptions could silently break
# either detection direction and nobody would notice until a real
# vendor-key drift bug slipped through again -- the exact failure mode it
# exists to catch. This script closes that gap by pinning both failure
# directions check-vendor-keys.sh's own header comment documents it must
# catch, plus a healthy-repo control case:
#
#   1. in_sync             — a vendor backend's key is registered in a
#                             fake main.go's init() AND passed through by
#                             docker-compose.yml's environment: block ->
#                             must exit 0 (not just "doesn't crash" — a
#                             false positive here would be as bad as a
#                             missed real drift).
#   2. missing_from_compose — a vendor backend registered in init() whose
#                             *_API_KEY docker-compose.yml's environment:
#                             block does NOT pass through (the Sprint 15
#                             bug class) -> must exit 1.
#   3. stale_in_compose     — docker-compose.yml's environment: block
#                             passes through a *_API_KEY no registered
#                             vendor constructor reads anymore (the
#                             opposite bug) -> must exit 1.
#
# How it exercises check-vendor-keys.sh without editing that script's core
# logic: check-vendor-keys.sh derives REPO_ROOT purely from its own
# location on disk (`dirname "${BASH_SOURCE[0]}"`/..), so this test copies
# the real script into a throwaway `mktemp -d` fixture tree laid out
# exactly like this repo's relevant subset (cmd/langstream/main.go,
# pkg/<vendor>/, docker-compose.yml, scripts/) per case, then runs that
# copy from inside its own fixture tree. check-vendor-keys.sh itself is
# never modified, and its real no-argument invocation
# (`./scripts/check-vendor-keys.sh`, as used by `make check-vendor-keys`
# and ci.yml) is completely untouched by anything here.
#
# Usage: scripts/check-vendor-keys_test.sh
# Exit codes: 0 = all cases behaved as expected, 1 = at least one didn't.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REAL_SCRIPT="$SCRIPT_DIR/check-vendor-keys.sh"

[[ -f "$REAL_SCRIPT" ]] || {
	echo "check-vendor-keys_test: cannot find $REAL_SCRIPT" >&2
	exit 1
}

TMPROOT="$(mktemp -d "${TMPDIR:-/tmp}/check-vendor-keys-test.XXXXXX")"
trap 'rm -rf "$TMPROOT"' EXIT

failures=0

# write_vendor_pkg <fixture_root> <pkg_name> <constructor_name> <env_var>
#
# Fixture stand-in for a real vendor package like pkg/asr/deepgram.go:
# a package with one exported constructor that reads exactly one
# XXX_API_KEY via a literal os.Getenv(...) call, matching the assumption
# check-vendor-keys.sh's header comment documents it relies on.
write_vendor_pkg() {
	local root="$1" pkg="$2" fn="$3" key="$4"
	mkdir -p "$root/pkg/$pkg"
	cat >"$root/pkg/$pkg/fake.go" <<-EOF
	package $pkg

	import "os"

	// $fn is a fixture stand-in for a real vendor constructor (e.g.
	// pkg/asr/deepgram.go's NewDeepgramRecognizer). It exists only so
	// check-vendor-keys_test.sh can exercise check-vendor-keys.sh against
	// a self-contained fake package tree, independent of the real repo's
	// current six-vendor state.
	func $fn() (int, error) {
		_ = os.Getenv("$key")
		return 0, nil
	}
	EOF
}

# write_main_go <fixture_root> <call...>
#
# Fixture stand-in for cmd/langstream/main.go: an init() that calls one or
# more pkg.NewXxx() vendor constructors, matching the exact
# "func init() {" / trailing "}" column-0 layout check-vendor-keys.sh's
# awk-based extraction expects.
write_main_go() {
	local root="$1"
	shift
	mkdir -p "$root/cmd/langstream"
	{
		echo "package main"
		echo
		echo "func init() {"
		for call in "$@"; do
			echo "	$call"
		done
		echo "}"
	} >"$root/cmd/langstream/main.go"
}

# write_compose <fixture_root> <env_var...>
#
# Fixture stand-in for docker-compose.yml's environment: block, indented
# the same way (4-space environment:, 6-space "- KEY=..." entries,
# 4-space terminating sibling key) as the real file so
# check-vendor-keys.sh's awk block-extraction sees the same shape.
write_compose() {
	local root="$1"
	shift
	{
		echo 'version: "3.9"'
		echo
		echo "services:"
		echo "  langstream:"
		echo "    image: fake:local"
		echo "    environment:"
		for key in "$@"; do
			echo "      - ${key}=\${${key}:-}"
		done
		echo "    restart: unless-stopped"
	} >"$root/docker-compose.yml"
}

# install_script <fixture_root>
#
# Copies (not symlinks) the real script into the fixture so each test run
# always exercises whatever check-vendor-keys.sh currently contains.
install_script() {
	local root="$1"
	mkdir -p "$root/scripts"
	cp "$REAL_SCRIPT" "$root/scripts/check-vendor-keys.sh"
	chmod +x "$root/scripts/check-vendor-keys.sh"
}

# run_case <name> <fixture_root> <expected_exit_code> [<expected_output_pattern>]
run_case() {
	local name="$1" root="$2" expected="$3" pattern="${4:-}"
	local out rc

	set +e
	out="$("$root/scripts/check-vendor-keys.sh" 2>&1)"
	rc=$?
	set -e

	if [[ "$rc" -ne "$expected" ]]; then
		echo "FAIL [$name]: expected exit $expected, got $rc" >&2
		echo "  output:" >&2
		sed 's/^/    /' <<<"$out" >&2
		failures=$((failures + 1))
		return
	fi

	if [[ -n "$pattern" ]] && ! grep -q "$pattern" <<<"$out"; then
		echo "FAIL [$name]: exit code $rc matched, but output didn't contain expected pattern: $pattern" >&2
		echo "  output:" >&2
		sed 's/^/    /' <<<"$out" >&2
		failures=$((failures + 1))
		return
	fi

	if [[ -n "$pattern" ]]; then
		echo "PASS [$name]: exit $rc, output matched /$pattern/"
	else
		echo "PASS [$name]: exit $rc"
	fi
}

# --- Case 1: in_sync -- both vendors registered, both keys in compose -----
case1="$TMPROOT/in_sync"
mkdir -p "$case1"
write_vendor_pkg "$case1" vendora NewFakeRecognizer VENDORA_API_KEY
write_vendor_pkg "$case1" vendorb NewFakeSynthesizer VENDORB_API_KEY
write_main_go "$case1" "vendora.NewFakeRecognizer()" "vendorb.NewFakeSynthesizer()"
write_compose "$case1" VENDORA_API_KEY VENDORB_API_KEY
install_script "$case1"
run_case "in_sync" "$case1" 0 "check-vendor-keys: OK"

# --- Case 2: missing_from_compose -- vendorb is registered in init() but
# its key is never passed through docker-compose.yml (the Sprint 15 bug
# class this script was originally built to prevent a repeat of) ----------
case2="$TMPROOT/missing_from_compose"
mkdir -p "$case2"
write_vendor_pkg "$case2" vendora NewFakeRecognizer VENDORA_API_KEY
write_vendor_pkg "$case2" vendorb NewFakeSynthesizer VENDORB_API_KEY
write_main_go "$case2" "vendora.NewFakeRecognizer()" "vendorb.NewFakeSynthesizer()"
write_compose "$case2" VENDORA_API_KEY
install_script "$case2"
run_case "missing_from_compose" "$case2" 1 "VENDORB_API_KEY"

# --- Case 3: stale_in_compose -- docker-compose.yml still passes a key no
# registered vendor constructor reads anymore (the opposite bug) ----------
case3="$TMPROOT/stale_in_compose"
mkdir -p "$case3"
write_vendor_pkg "$case3" vendora NewFakeRecognizer VENDORA_API_KEY
write_main_go "$case3" "vendora.NewFakeRecognizer()"
write_compose "$case3" VENDORA_API_KEY VENDORB_API_KEY
install_script "$case3"
run_case "stale_in_compose" "$case3" 1 "VENDORB_API_KEY"

echo
if [[ "$failures" -eq 0 ]]; then
	echo "check-vendor-keys_test: OK -- all 3 cases behaved as expected"
	exit 0
else
	echo "check-vendor-keys_test: FAIL -- $failures case(s) did not behave as expected" >&2
	exit 1
fi
