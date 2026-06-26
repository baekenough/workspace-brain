.PHONY: build vet test race coverage-out coverage demo docker-build help

IMAGE ?= workspace-brain:local
COVERAGE_PROFILE ?= coverage.out

## Build the workspace-brain server binary.
build:
	go build ./cmd/workspace-brain

## Run Go vet static checks.
vet:
	go vet ./...

## Run unit tests.
test:
	go test ./...

## Run the race detector test suite.
race:
	go test -race ./...

## Generate a Go coverage profile.
coverage-out:
	go test ./... -coverprofile=$(COVERAGE_PROFILE)

## Generate and enforce the configured coverage threshold.
coverage: coverage-out
	./scripts/check-coverage.sh $(COVERAGE_PROFILE)

## Run the built-in demo flow.
demo:
	go run ./cmd/workspace-brain demo

## Build the local Docker image. Override IMAGE=name:tag.
docker-build:
	docker build -t $(IMAGE) .

## Show available Make targets.
help:
	@awk '/^## / { desc = substr($$0, 4); next } /^[a-zA-Z0-9_-]+:/ { if (desc) { split($$0, target, ":"); printf "%-16s %s\n", target[1], desc; desc = "" } }' $(MAKEFILE_LIST)
