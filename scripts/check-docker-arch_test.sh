#!/usr/bin/env bash
#
# check-docker-arch_test.sh — regression test for check-docker-arch.sh.
#
# Pins the behaviors check-docker-arch.sh's own header comment documents
# it must exhibit:
#
#   1. missing_file      — no Dockerfile at all -> exit 2.
#   2. hardcoded_amd64   — 'go build' line hardcodes GOARCH=amd64 (the
#                          exact bug this guard exists to catch: an
#                          amd64-only binary silently mismatching a
#                          non-amd64 runtime base image on a non-amd64
#                          build host) -> exit 1.
#   3. missing_arg_decl  — 'go build' line references $TARGETARCH but the
#                          Dockerfile never declares 'ARG TARGETARCH' ->
#                          exit 1.
#   4. correct           — ARG TARGETOS/TARGETARCH declared and used on
#                          the go build line, no hardcoded literal -> exit 0.
#
# Same fixture strategy as check-dockerignore_test.sh: copy the real
# script into a throwaway mktemp -d tree laid out like this repo (script
# derives REPO_ROOT from its own on-disk location), run it there, never
# touch the real script or the real repo's Dockerfile.
#
# Usage: scripts/check-docker-arch_test.sh
# Exit codes: 0 = all cases behaved as expected, 1 = at least one didn't.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REAL_SCRIPT="$SCRIPT_DIR/check-docker-arch.sh"

[[ -f "$REAL_SCRIPT" ]] || {
	echo "check-docker-arch_test: cannot find $REAL_SCRIPT" >&2
	exit 1
}

TMPROOT="$(mktemp -d "${TMPDIR:-/tmp}/check-docker-arch-test.XXXXXX")"
trap 'rm -rf "$TMPROOT"' EXIT

failures=0

install_script() {
	local root="$1"
	mkdir -p "$root/scripts"
	cp "$REAL_SCRIPT" "$root/scripts/check-docker-arch.sh"
	chmod +x "$root/scripts/check-docker-arch.sh"
}

run_case() {
	local name="$1" root="$2" expected="$3" pattern="${4:-}"
	local out rc

	set +e
	out="$("$root/scripts/check-docker-arch.sh" 2>&1)"
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

# --- Case 1: missing_file -- no Dockerfile at all -------------------------
case1="$TMPROOT/missing_file"
mkdir -p "$case1"
install_script "$case1"
run_case "missing_file" "$case1" 2 "does not exist"

# --- Case 2: hardcoded_amd64 -- the exact regression this guard exists to
# catch: GOARCH pinned to a fixed literal regardless of build platform ---
case2="$TMPROOT/hardcoded_amd64"
mkdir -p "$case2"
install_script "$case2"
cat >"$case2/Dockerfile" <<-'DOCKERFILE'
FROM golang:1.22 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/langstream ./cmd/langstream
FROM gcr.io/distroless/base-debian12:nonroot AS runtime
COPY --from=builder /out/langstream /app/langstream
ENTRYPOINT ["/app/langstream"]
DOCKERFILE
run_case "hardcoded_amd64" "$case2" 1 "hardcodes GOOS/GOARCH"

# --- Case 3: missing_arg_decl -- references $TARGETARCH but never
# declares 'ARG TARGETARCH' ------------------------------------------------
case3="$TMPROOT/missing_arg_decl"
mkdir -p "$case3"
install_script "$case3"
cat >"$case3/Dockerfile" <<-'DOCKERFILE'
FROM golang:1.22 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/langstream ./cmd/langstream
FROM gcr.io/distroless/base-debian12:nonroot AS runtime
COPY --from=builder /out/langstream /app/langstream
ENTRYPOINT ["/app/langstream"]
DOCKERFILE
run_case "missing_arg_decl" "$case3" 1 "never declares 'ARG TARGETOS'"

# --- Case 4: correct -- ARG TARGETOS/TARGETARCH declared and used, no
# hardcoded literal ---------------------------------------------------------
case4="$TMPROOT/correct"
mkdir -p "$case4"
install_script "$case4"
cat >"$case4/Dockerfile" <<-'DOCKERFILE'
FROM golang:1.22 AS builder
WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/langstream ./cmd/langstream
FROM gcr.io/distroless/base-debian12:nonroot AS runtime
COPY --from=builder /out/langstream /app/langstream
ENTRYPOINT ["/app/langstream"]
DOCKERFILE
run_case "correct" "$case4" 0 "check-docker-arch: OK"

echo
if [[ "$failures" -eq 0 ]]; then
	echo "check-docker-arch_test: OK -- all 4 cases behaved as expected"
	exit 0
else
	echo "check-docker-arch_test: FAIL -- $failures case(s) did not behave as expected" >&2
	exit 1
fi
