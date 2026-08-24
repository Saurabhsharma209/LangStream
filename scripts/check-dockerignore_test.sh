#!/usr/bin/env bash
#
# check-dockerignore_test.sh — regression test for check-dockerignore.sh.
#
# Pins the three behaviors check-dockerignore.sh's own header comment
# documents it must exhibit:
#
#   1. missing_file    — no .dockerignore at all -> exit 2.
#   2. missing_pattern — .dockerignore exists but is missing one of the
#                        required exclusion patterns (e.g. someone strips
#                        the "*.md" line while editing it) -> exit 1.
#   3. complete        — .dockerignore has all required patterns -> exit 0.
#
# Same fixture strategy as check-vendor-keys_test.sh: copy the real script
# into a throwaway mktemp -d tree laid out like this repo (script derives
# REPO_ROOT from its own on-disk location), run it there, never touch the
# real script or the real repo's .dockerignore.
#
# Usage: scripts/check-dockerignore_test.sh
# Exit codes: 0 = all cases behaved as expected, 1 = at least one didn't.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REAL_SCRIPT="$SCRIPT_DIR/check-dockerignore.sh"

[[ -f "$REAL_SCRIPT" ]] || {
	echo "check-dockerignore_test: cannot find $REAL_SCRIPT" >&2
	exit 1
}

TMPROOT="$(mktemp -d "${TMPDIR:-/tmp}/check-dockerignore-test.XXXXXX")"
trap 'rm -rf "$TMPROOT"' EXIT

failures=0

install_script() {
	local root="$1"
	mkdir -p "$root/scripts"
	cp "$REAL_SCRIPT" "$root/scripts/check-dockerignore.sh"
	chmod +x "$root/scripts/check-dockerignore.sh"
}

run_case() {
	local name="$1" root="$2" expected="$3" pattern="${4:-}"
	local out rc

	set +e
	out="$("$root/scripts/check-dockerignore.sh" 2>&1)"
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

# --- Case 1: missing_file -- no .dockerignore at all ----------------------
case1="$TMPROOT/missing_file"
mkdir -p "$case1"
install_script "$case1"
run_case "missing_file" "$case1" 2 "does not exist"

# --- Case 2: missing_pattern -- .dockerignore exists but is missing the
# "*.md" line (the DEVLOG.md-cache-busting case this whole check exists to
# prevent a regression of) --------------------------------------------------
case2="$TMPROOT/missing_pattern"
mkdir -p "$case2"
install_script "$case2"
cat >"$case2/.dockerignore" <<-'DOCKERIGNORE'
.git/
.github/
DOCKERIGNORE
run_case "missing_pattern" "$case2" 1 '\*\.md'

# --- Case 3: complete -- all required patterns present --------------------
case3="$TMPROOT/complete"
mkdir -p "$case3"
install_script "$case3"
cat >"$case3/.dockerignore" <<-'DOCKERIGNORE'
.git/
*.md
docs/
.github/
DOCKERIGNORE
run_case "complete" "$case3" 0 "check-dockerignore: OK"

echo
if [[ "$failures" -eq 0 ]]; then
	echo "check-dockerignore_test: OK -- all 3 cases behaved as expected"
	exit 0
else
	echo "check-dockerignore_test: FAIL -- $failures case(s) did not behave as expected" >&2
	exit 1
fi
