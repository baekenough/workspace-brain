# Contributing to workspace-brain

## Prerequisites

- Go 1.25.11 or a compatible Go 1.25 toolchain.
- Docker only if you are working on the container image.

## Build and test

```bash
# Build the server binary
go build ./...

# Run unit tests
go test ./...

# Run with the race detector (required before any PR)
go test -race ./...

# Run Go vet static checks
go vet ./...

# Enforce 100% coverage gate
./scripts/check-coverage.sh coverage.out
```

Make targets wrap the same commands:

```bash
make build        # go build ./cmd/workspace-brain
make test         # go test ./...
make race         # go test -race ./...
make vet          # go vet ./...
make coverage     # generates coverage.out and enforces 100%
make demo         # go run ./cmd/workspace-brain demo
make docker-build # build the distroless container image
```

CI runs `make vet`, `make test`, `make race`, `make coverage`, `make docker-build`, and `govulncheck ./...` on every push and pull request. All gates must pass before merge.

The coverage gate is strict: `./scripts/check-coverage.sh` fails unless total coverage is exactly `100.0%`. Add tests for any new code path before opening a PR.

## Workflow

1. Branch from `main`:

   ```bash
   git checkout main
   git pull
   git checkout -b feature/<short-description>
   ```

2. Make your changes. Keep commits focused and use [Conventional Commits](https://www.conventionalcommits.org/):
   - `feat:` — new capability
   - `fix:` — bug fix
   - `docs:` — documentation only
   - `chore:` — maintenance (deps, CI, tooling)
   - `refactor:` — code restructure without behaviour change
   - `test:` — test-only changes

3. Run the full quality gate locally:

   ```bash
   go test -race ./...
   go vet ./...
   ./scripts/check-coverage.sh coverage.out
   ```

4. Open a pull request to `main`. The PR description should explain what changed and why. Reference any related issue with `Closes #N`.

## Scope notes

- `pkg/brainapi/` is the surface-neutral public contract. Changes there affect all adapters.
- `internal/control/gateway/` owns ID generation, binding resolution, and authorization.
- `internal/core/memory/` is the local RAG core. Keep it free of HTTP, Slack, or surface knowledge.
- Do not add new runtime dependencies (PostgreSQL, Qdrant, RabbitMQ, etc.) outside the roadmap seam pattern.
- Do not modify `.github/workflows/ci.yml` without a separate discussion; CI stability is a project invariant.
