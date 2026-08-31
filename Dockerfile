# syntax=docker/dockerfile:1

# ---- builder ------------------------------------------------------------
# Pin to the same Go version the project targets (go.mod: go 1.22) so local
# builds and CI match what ships in the image.
FROM golang:1.22 AS builder

WORKDIR /src

# TARGETOS/TARGETARCH are populated automatically by BuildKit/buildx to
# match whatever platform this stage is actually being built for (the
# host's native platform by default, or an explicit `--platform` if one
# is passed) -- see https://docs.docker.com/build/building/multi-platform/.
# Neither this stage nor the runtime stage below pins a `--platform`, so
# both resolve to the same platform at build time; hardcoding GOOS/GOARCH
# to a fixed value here (previously `GOARCH=amd64`, unconditionally) would
# silently mismatch that on any non-amd64 build host -- e.g. Apple Silicon
# dev laptops or an arm64 CI runner -- producing an amd64 binary COPYed
# into a non-amd64 runtime base image below, which fails at container
# start with "exec format error" rather than at build time. Using the
# build-args instead keeps the compiled binary's architecture matched to
# whatever platform this image build actually targets.
ARG TARGETOS
ARG TARGETARCH

# Cache module downloads separately from source changes. go.sum* (glob)
# tolerates the fact that no go.sum exists yet (no external deps as of
# Week 1) without failing the COPY.
COPY go.mod go.sum* ./
RUN go mod download

# See .dockerignore (Sprint 27) -- it keeps this COPY from pulling in
# .git/, docs, and other non-build content that would otherwise bust this
# layer's cache on every doc-only commit (this project's DEVLOG.md is
# edited on essentially every sprint) without changing anything the build
# below actually reads.
COPY . .

# Static binary: CGO disabled so it runs unmodified on the distroless base
# below (no libc dependency issues). GOOS/GOARCH come from the
# TARGETOS/TARGETARCH build args above (see their declaration), not a
# hardcoded literal, so the binary's architecture always matches the
# platform this image is being built for.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/langstream ./cmd/langstream

# ---- runtime --------------------------------------------------------------
# distroless base-debian12: no shell, no package manager, minimal attack
# surface — appropriate for a service handling live call audio.
FROM gcr.io/distroless/base-debian12:nonroot AS runtime

WORKDIR /app
COPY --from=builder /out/langstream /app/langstream

# Serves the observability dashboard (pkg/observability, shipped
# 2026-07-09) via `langstream serve --addr :8080`: `/` (HTML dashboard),
# `/dashboard.json` (same data as JSON), and `/metrics` (Prometheus
# scrape endpoint for glass-to-glass latency, error rate, and per-vendor
# cost). This was a placeholder port reservation through Week 2; it's live
# as of today (see docker-compose.yml `command:` override, which runs
# `langstream serve` instead of the default no-arg entrypoint).
EXPOSE 8080

# --- HEALTHCHECK ------------------------------------------------------
# Deliberately NOT adding a Docker-level HEALTHCHECK CMD-SHELL curl/wget
# probe here: this base image is distroless (no /bin/sh, no curl, no
# wget, no package manager to add them), so a shell-form HEALTHCHECK is
# impossible outright, and an exec-form HEALTHCHECK (which doesn't need a
# shell) still needs *some* binary in the image capable of making an HTTP
# request — the only executable we ship is /app/langstream itself, and it
# has no self-check subcommand today (adding one is a cmd/langstream
# change, owned by the Tech workstream, not SRE — flagged for the EM to
# route as a follow-up, e.g. `langstream healthcheck --addr :8080` doing
# an in-process GET /dashboard.json and exiting 0/1).
#
# Until that lands, rely on orchestrator-level health checking instead of
# a container-internal one:
#   - Kubernetes: use an httpGet livenessProbe/readinessProbe against
#     GET /dashboard.json. Kubelet performs that HTTP call itself, from
#     outside the container, so it needs no shell/curl inside the image
#     at all — this is the correct production mechanism, not a
#     Docker-level HEALTHCHECK.
#   - docker-compose (local dev): see the `healthcheck:` comment in
#     docker-compose.yml for the same constraint and the chosen fallback.
# HEALTHCHECK NONE marks this as an intentional decision, not an omission.
HEALTHCHECK NONE

USER nonroot:nonroot

ENTRYPOINT ["/app/langstream"]
