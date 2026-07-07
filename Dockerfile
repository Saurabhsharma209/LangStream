# syntax=docker/dockerfile:1

# ---- builder ------------------------------------------------------------
# Pin to the same Go version the project targets (go.mod: go 1.22) so local
# builds and CI match what ships in the image.
FROM golang:1.22 AS builder

WORKDIR /src

# Cache module downloads separately from source changes. go.sum* (glob)
# tolerates the fact that no go.sum exists yet (no external deps as of
# Week 1) without failing the COPY.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# Static binary: CGO disabled so it runs unmodified on the distroless base
# below (no libc dependency issues).
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/langstream ./cmd/langstream

# ---- runtime --------------------------------------------------------------
# distroless base-debian12: no shell, no package manager, minimal attack
# surface — appropriate for a service handling live call audio.
FROM gcr.io/distroless/base-debian12:nonroot AS runtime

WORKDIR /app
COPY --from=builder /out/langstream /app/langstream

# Reserved for the future HTTP API (health checks, /metrics, control plane).
EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/app/langstream"]
