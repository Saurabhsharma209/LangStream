.PHONY: build test fmt fmt-check vet docker ci

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

# What CI runs. Keep this in sync with .github/workflows/ci.yml so
# `make ci` is a reliable local pre-push check.
ci: fmt-check vet test build
