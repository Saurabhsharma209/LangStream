.PHONY: build test fmt fmt-check vet docker docker-run serve ci check-vendor-keys test-vendor-key-guard

BINARY := langstream
CMD_PATH := ./cmd/langstream
IMAGE := langstream:local

# go build ./... first so this target has the same full-repo compile
# coverage as CI's `go build` step (.github/workflows/ci.yml) - building
# only $(CMD_PATH) would miss a break in e.g. examples/ or
# tools/latency_benchmark that CI would still catch, defeating the point
# of `make ci` as a reliable local pre-push mirror of CI (see the `ci:`
# target's own comment below). The second line still produces the
# bin/$(BINARY) binary that `make docker`/`make serve` expect.
build:
	go build ./...
	go build -o bin/$(BINARY) $(CMD_PATH)

test:
	go test ./... -race

fmt:
	gofmt -w $$(find . -type f -name '*.go' -not -path './vendor/*')

fmt-check:
	@unformatted="$$(gofmt -l $$(find . -type f -name '*.go' -not -path './vendor/*'))"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not gofmt'd:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet:
	go vet ./...

docker:
	docker build -t $(IMAGE) .

# Run the dashboard server locally without building a container - fast
# inner loop for pkg/observability changes. Mirrors the `command:`
# override in docker-compose.yml (`langstream serve --addr :8080`).
serve:
	go run $(CMD_PATH) serve --addr :8080

# Build the image and bring the service up via compose (dashboard on
# localhost:8080, per the `command:`/`ports:` in docker-compose.yml).
# Requires the Docker daemon; use `make serve` instead if you just want
# the dashboard without container overhead.
docker-run:
	docker compose up --build

# Guards against the vendor-API-key-drift bug class found in Sprint 15
# (2026-07-23): a real vendor backend registered in cmd/langstream/main.go's
# init() whose API key env var isn't (or is no longer) passed through by
# docker-compose.yml's environment: block. See
# scripts/check-vendor-keys.sh's own header comment for the full story.
check-vendor-keys:
	./scripts/check-vendor-keys.sh

# Regression test for check-vendor-keys.sh itself (Sprint 17, 2026-07-28):
# that guard was hand-verified once against scratch fixtures and then
# never pinned, so a future edit to its grep/awk parsing could silently
# break either drift-detection direction with nobody noticing until a
# real vendor-key bug slipped through again. This builds throwaway
# fixture repos in a mktemp -d tempdir and asserts check-vendor-keys.sh's
# exit code for the in-sync case and both drift directions
# (missing-from-compose, stale-in-compose) it exists to catch. See
# scripts/check-vendor-keys_test.sh's own header comment for details.
test-vendor-key-guard:
	./scripts/check-vendor-keys_test.sh

# What CI runs. Keep this in sync with .github/workflows/ci.yml so
# `make ci` is a reliable local pre-push check. (CI's docker-build job is
# informational/parallel, not part of the local pre-push gate here - run
# `make docker` separately if you want to sanity-check the image too.)
ci: fmt-check vet test build check-vendor-keys test-vendor-key-guard
