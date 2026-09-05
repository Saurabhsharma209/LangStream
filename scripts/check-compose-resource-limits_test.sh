#!/usr/bin/env bash
#
# check-compose-resource-limits_test.sh — regression test for
# check-compose-resource-limits.sh.
#
# Pins the behaviors check-compose-resource-limits.sh's own header
# comment documents it must exhibit:
#
#   1. missing_file — no docker-compose.yml at all -> exit 2.
#   2. no_limit     — a langstream service with no memory limit at all
#                      (the exact gap this guard exists to catch) -> exit 1.
#   3. correct      — a 'memory:' entry present under deploy.resources.limits
#                      -> exit 0.
#
# Same fixture strategy as check-docker-arch_test.sh: copy the real script
# into a throwaway mktemp -d tree laid out like this repo (script derives
# REPO_ROOT from its own on-disk location), run it there, never touch the
# real script or the real repo's docker-compose.yml.
#
# Usage: scripts/check-compose-resource-limits_test.sh
# Exit codes: 0 = all cases behaved as expected, 1 = at least one didn't.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REAL_SCRIPT="$SCRIPT_DIR/check-compose-resource-limits.sh"

[[ -f "$REAL_SCRIPT" ]] || {
	echo "check-compose-resource-limits_test: cannot find $REAL_SCRIPT" >&2
	exit 1
}

TMPROOT="$(mktemp -d "${TMPDIR:-/tmp}/check-compose-resource-limits-test.XXXXXX")"
trap 'rm -rf "$TMPROOT"' EXIT

failures=0

install_script() {
	local root="$1"
	mkdir -p "$root/scripts"
	cp "$REAL_SCRIPT" "$root/scripts/check-compose-resource-limits.sh"
	chmod +x "$root/scripts/check-compose-resource-limits.sh"
}

run_case() {
	local name="$1" root="$2" expected="$3" pattern="${4:-}"
	local out rc

	set +e
	out="$("$root/scripts/check-compose-resource-limits.sh" 2>&1)"
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

# --- Case 1: missing_file -- no docker-compose.yml at all -----------------
case1="$TMPROOT/missing_file"
mkdir -p "$case1"
install_script "$case1"
run_case "missing_file" "$case1" 2 "does not exist"

# --- Case 2: no_limit -- the exact regression this guard exists to catch:
# no memory limit anywhere in the file ---------------------------------------
case2="$TMPROOT/no_limit"
mkdir -p "$case2"
install_script "$case2"
cat >"$case2/docker-compose.yml" <<-'COMPOSE'
version: "3.9"
services:
  langstream:
    image: langstream:local
    command: ["serve", "--addr", ":8080"]
    ports:
      - "8080:8080"
    restart: unless-stopped
COMPOSE
run_case "no_limit" "$case2" 1 "no 'deploy.resources.limits.memory'"

# --- Case 3: correct -- a memory limit is declared -------------------------
case3="$TMPROOT/correct"
mkdir -p "$case3"
install_script "$case3"
cat >"$case3/docker-compose.yml" <<-'COMPOSE'
version: "3.9"
services:
  langstream:
    image: langstream:local
    command: ["serve", "--addr", ":8080"]
    ports:
      - "8080:8080"
    restart: unless-stopped
    deploy:
      resources:
        limits:
          memory: 512M
          cpus: "1.0"
COMPOSE
run_case "correct" "$case3" 0 "check-compose-resource-limits: OK"

echo
if [[ "$failures" -eq 0 ]]; then
	echo "check-compose-resource-limits_test: OK -- all 3 cases behaved as expected"
	exit 0
else
	echo "check-compose-resource-limits_test: FAIL -- $failures case(s) did not behave as expected" >&2
	exit 1
fi
