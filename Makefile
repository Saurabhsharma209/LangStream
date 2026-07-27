.PHONY: build test fmt fmt-check vet docker docker-run serve ci check-vendor-keys

BINARY := langstream
CMD_PATH := ./cmd/langstream
IMAGE := langstream:local

build:
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

# What CI runs. Keep this in sync with .github/workflows/ci.yml so
# `make ci` is a reliable local pre-push check. (CI's docker-build job is
# informational/parallel, not part of the local pre-push gate here - run
# `make docker` separately if you want to sanity-check the image too.)
ci: fmt-check vet test build check-vendor-keys
