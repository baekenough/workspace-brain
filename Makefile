.PHONY: test race coverage demo

test:
	go test ./...

race:
	go test -race ./...

coverage:
	./scripts/check-coverage.sh coverage.out

demo:
	go run ./cmd/workspace-brain demo
