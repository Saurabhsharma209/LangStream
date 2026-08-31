#!/usr/bin/env bash
#
# check-docker-arch.sh — CI guard that Dockerfile's builder-stage `go
# build` RUN line never hardcodes GOARCH (or GOOS) to a fixed literal
# (e.g. "GOARCH=amd64"), and instead cross-compiles using the
# TARGETOS/TARGETARCH build-args BuildKit/buildx populates automatically
# to match whatever platform the image is actually being built for.
#
# Why this matters: neither this Dockerfile's builder stage nor its
# runtime stage (`FROM gcr.io/distroless/base-debian12:nonroot`) pins an
# explicit `--platform`, so both default to the host's native platform at
# build time. If the builder stage's `go build` hardcodes GOARCH to a
# fixed value, that only matches the runtime base image's platform when
# the build host happens to be that same architecture. On any other build
# host — an Apple Silicon (arm64) dev laptop, or an arm64 CI runner — the
# result is an amd64 binary COPYed into a non-amd64 runtime image, which
# fails at container start with "exec format error" rather than at build
# time, silently defeating `make docker`/`docker compose up --build`/the
# CI `docker-build` job's own purpose of catching broken image builds
# before they reach main.
#
# This does not build an image (see check-dockerignore.sh's own header
# comment for the same reasoning: image builds are comparatively slow/
# heavy for a CI gate) — it statically checks the builder stage's `go
# build` RUN line in Dockerfile.
#
# Usage: scripts/check-docker-arch.sh
# Exit codes: 0 = Dockerfile uses TARGETOS/TARGETARCH build-args (no
# hardcoded arch literal), 1 = a hardcoded GOOS/GOARCH literal was found
# on the `go build` line, 2 = Dockerfile itself is missing.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOCKERFILE="$REPO_ROOT/Dockerfile"

if [[ ! -f "$DOCKERFILE" ]]; then
	echo "check-docker-arch: FAIL -- $DOCKERFILE does not exist." >&2
	exit 2
fi

# Find the RUN line(s) that actually invoke `go build` (the compile step),
# ignoring comments.
build_lines="$(grep -v '^[[:space:]]*#' "$DOCKERFILE" | grep -E 'GOARCH=|GOOS=' || true)"

if [[ -z "$build_lines" ]]; then
	echo "check-docker-arch: FAIL -- no GOOS=/GOARCH= assignment found on any Dockerfile build line; expected the builder stage's 'go build' RUN line to set both from \$TARGETOS/\$TARGETARCH." >&2
	exit 1
fi

# A hardcoded literal looks like GOARCH=amd64, GOARCH=arm64, GOOS=linux,
# etc. -- anything that isn't a $VAR/${VAR} reference. The correct form
# uses the buildx-populated build-args: GOARCH=$TARGETARCH / GOOS=$TARGETOS
# (with or without braces).
bad_lines="$(grep -E '(GOARCH|GOOS)=[a-zA-Z0-9_]+' <<<"$build_lines" | grep -vE '(GOARCH|GOOS)=\$\{?TARGET(ARCH|OS)\}?' || true)"

if [[ -n "$bad_lines" ]]; then
	echo "check-docker-arch: FAIL -- Dockerfile hardcodes GOOS/GOARCH to a fixed literal instead of using \$TARGETOS/\$TARGETARCH (buildx-populated build-args that match the platform actually being built for). This mismatches the runtime base image's platform on any non-matching build host (e.g. arm64 dev laptop or CI runner), producing 'exec format error' at container start rather than at build time." >&2
	echo "  offending line(s):" >&2
	sed 's/^/    /' <<<"$bad_lines" >&2
	echo "  Fix: declare 'ARG TARGETOS' / 'ARG TARGETARCH' in the builder stage and use GOOS=\$TARGETOS GOARCH=\$TARGETARCH on the go build line." >&2
	exit 1
fi

# Also require that TARGETOS/TARGETARCH are actually declared as ARGs
# before use, so a copy-paste that references the vars without declaring
# them (silently expanding to empty strings, which 'go build' treats as
# "use host defaults" -- not necessarily wrong, but not the intended,
# explicit cross-compile-safe behavior either) is caught too.
if ! grep -qE '^[[:space:]]*ARG[[:space:]]+TARGETOS[[:space:]]*$' "$DOCKERFILE"; then
	echo "check-docker-arch: FAIL -- Dockerfile references \$TARGETOS but never declares 'ARG TARGETOS' in the builder stage." >&2
	exit 1
fi
if ! grep -qE '^[[:space:]]*ARG[[:space:]]+TARGETARCH[[:space:]]*$' "$DOCKERFILE"; then
	echo "check-docker-arch: FAIL -- Dockerfile references \$TARGETARCH but never declares 'ARG TARGETARCH' in the builder stage." >&2
	exit 1
fi

echo "check-docker-arch: OK -- Dockerfile's go build uses \$TARGETOS/\$TARGETARCH (both declared as ARGs), no hardcoded GOOS/GOARCH literal."
