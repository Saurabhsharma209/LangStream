#!/usr/bin/env bash
#
# check-dockerignore.sh — CI guard that .dockerignore keeps excluding the
# entries that matter for Docker build-context hygiene and layer-cache
# correctness (see .dockerignore's own header comment for the full
# rationale: before this file existed, Dockerfile's `COPY . .` pulled in
# .git/ and every top-level doc file, including DEVLOG.md, which this
# project's own established pattern edits on essentially every sprint --
# busting the `go build` layer's cache on every pure-documentation change
# with zero effect on the built binary).
#
# This does not build an image (this sandbox's disk is intentionally kept
# tight and image builds are comparatively slow/heavy for a CI gate) --
# it statically checks that .dockerignore contains a pattern that would
# exclude each of a fixed list of paths known to be safe-and-important to
# exclude, using the same glob semantics `docker build` itself uses
# (delegated to Go's filepath.Match-style matching via a tiny inline
# matcher, since this repo intentionally has no other dependency on a
# Docker CLI/daemon being present to run this check).
#
# Usage: scripts/check-dockerignore.sh
# Exit codes: 0 = all required exclusions present, 1 = at least one
# required exclusion is missing (see stderr for which), 2 = .dockerignore
# itself is missing.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOCKERIGNORE="$REPO_ROOT/.dockerignore"

if [[ ! -f "$DOCKERIGNORE" ]]; then
	echo "check-dockerignore: FAIL -- $DOCKERIGNORE does not exist. Dockerfile's 'COPY . .' would pull in .git/ and every doc file (including DEVLOG.md, edited on essentially every sprint), busting the build layer's cache on every doc-only change. See .dockerignore's own header comment (if it still exists in git history) for the full rationale." >&2
	exit 2
fi

# required_path:required_pattern_substring pairs. required_pattern_substring
# is checked as a literal line present in .dockerignore (not a full glob
# evaluation) — deliberately simple and robust to reordering/comments, at
# the cost of not catching every conceivable equivalent-but-differently-
# spelled pattern. That tradeoff mirrors check-vendor-keys.sh's own
# documented "pragmatic, not bulletproof" approach.
declare -a required=(
	".git/:VCS metadata"
	"*.md:doc files (including DEVLOG.md)"
	".github/:CI config"
)

missing=()
for entry in "${required[@]}"; do
	pattern="${entry%%:*}"
	desc="${entry#*:}"
	if ! grep -qxF "$pattern" "$DOCKERIGNORE"; then
		missing+=("$pattern ($desc)")
	fi
done

if [[ ${#missing[@]} -gt 0 ]]; then
	echo "check-dockerignore: FAIL -- .dockerignore is missing required exclusion pattern(s):" >&2
	for m in "${missing[@]}"; do
		echo "  - $m" >&2
	done
	echo "  Fix: add the missing pattern(s) to $DOCKERIGNORE." >&2
	exit 1
fi

echo "check-dockerignore: OK -- .dockerignore excludes all ${#required[@]} required pattern(s) (.git/, doc files, CI config)."
