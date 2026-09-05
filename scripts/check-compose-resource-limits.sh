#!/usr/bin/env bash
#
# check-compose-resource-limits.sh — CI guard that docker-compose.yml's
# langstream service declares a memory limit, so a runaway container on
# this host can't starve every other process sharing it.
#
# Why this matters: docker-compose.yml already sets `restart:
# unless-stopped` (recovers from a process-level crash) but, until this
# check existed, had no `deploy.resources.limits` block. Without a memory
# limit, a leak or a runaway metric-cardinality blowup in
# pkg/observability (unbounded label values feeding LatencyRecorder, e.g.
# a caller-supplied stage/vendor string) has no ceiling: the container can
# consume host memory until the kernel OOM-killer picks a victim, which on
# a shared box may not even be this container. Compose v2 (i.e. `docker
# compose`, not the legacy python `docker-compose`) has applied
# `deploy.resources.limits` outside Swarm since 2022, so this is a real,
# enforced ceiling for local `docker compose up` runs, not swarm-only dead
# config.
#
# This does not start a container (same "don't do the slow/heavy thing in
# a CI gate" reasoning as check-dockerignore.sh/check-docker-arch.sh) — it
# statically checks docker-compose.yml for the expected block.
#
# Usage: scripts/check-compose-resource-limits.sh
# Exit codes: 0 = a memory limit is present, 1 = missing, 2 =
# docker-compose.yml itself is missing.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$REPO_ROOT/docker-compose.yml"

if [[ ! -f "$COMPOSE_FILE" ]]; then
	echo "check-compose-resource-limits: FAIL -- $COMPOSE_FILE does not exist." >&2
	exit 2
fi

if ! grep -qE '^[[:space:]]*memory:[[:space:]]*\S+' "$COMPOSE_FILE"; then
	echo "check-compose-resource-limits: FAIL -- docker-compose.yml has no 'deploy.resources.limits.memory' (or equivalent 'memory:') entry for the langstream service. Without a memory ceiling, a leak or unbounded metric-cardinality growth in pkg/observability can consume all host memory before the kernel OOM-killer intervenes." >&2
	echo "  Fix: add a 'deploy: resources: limits: memory: <value>' block under the langstream service." >&2
	exit 1
fi

echo "check-compose-resource-limits: OK -- docker-compose.yml's langstream service declares a memory limit."
