# Testing

workspace-brain treats tests as release gates. Keep the local deterministic path fully covered before changing adapters, retrieval, source loading, or operational behavior.

## Coverage policy

The project requires 100% statement coverage for committed Go code.

- Run the configured coverage script for the source of truth.
- Add or update tests with every behavior change.
- Do not lower coverage to merge incomplete behavior.
- Keep tests deterministic. Avoid external network, Slack, OpenAI, PostgreSQL, Qdrant, or RabbitMQ dependencies in the default suite.

### Integration-only package exclusion

`scripts/check-coverage.sh` excludes four packages from the 100% statement-coverage
total. This is a deliberate policy, not a loophole:

| Package | Why it is excluded |
|---|---|
| `internal/core/coretest` | Test-helper package only (contract-suite assertions for other packages to import). Its `t.Fatalf` guard lines are never hit on a passing run — that is inherent to assertion helpers, not missing coverage. |
| `internal/core/postgres` | Adapter with `//go:build integration` tests only. Zero default-build tests by design; see below. |
| `internal/core/qdrant` | Adapter with `//go:build integration` tests only. Zero default-build tests by design; see below. |
| `internal/control/rabbitmq` | Adapter with `//go:build integration` tests only. Zero default-build tests by design; see below. |

The three adapter packages carry `//go:build integration` on every test file, so
`go test ./...` (the default suite) compiles their production code but runs zero
tests against it — there is nothing to include in the default coverage profile
in the first place. The gate stays meaningful for the packages it does cover
(deterministic core, control-plane, adapters with default-build tests) by
excluding packages that have no default-build tests, rather than diluting the
100% target with untested-by-design production code.

These packages are not untested — they are covered under the `integration`
build tag, exercised via [testcontainers-go](https://golang.testcontainers.org/)
against real Postgres/Qdrant/RabbitMQ containers. Their real, integration-tag
coverage is tracked informationally in comments in `scripts/check-coverage.sh`
(not enforced as a gate): postgres 74.1%, qdrant 98.8%, rabbitmq 65.0% (as of
2026-07-08). Re-verify these numbers with `make test-integration` (see below)
whenever you touch adapter tests, and update the comment if it has drifted —
drift does not fail CI, but a stale number is misleading to the next reader.

### Running integration tests locally

```bash
make test-integration
```

This runs `go test -tags=integration -race` against the three adapter
packages. Each test starts its own ephemeral Postgres/Qdrant/RabbitMQ
container via testcontainers-go and tears it down afterward — no manual
`docker compose up` or fixture setup is required, only a working Docker
daemon.

## Required local commands

Run these commands from the repository root:

```bash
go test ./...
go test -race ./...
go test -coverprofile=coverage.out ./...
./scripts/check-coverage.sh coverage.out
go vet ./...
go run ./cmd/workspace-brain demo
docker build -t workspace-brain:local .
```

Expected demo output shape:

```text
tenant=tenant-3c4997260a941958762c934928ad17ba job=job-616eec1b005fb0258433d73b843f8380 answer=...
```

You can also use Make targets for the same gates:

```bash
make test
make race
make coverage
make vet
make demo
make docker-build
```

## CI expectations

CI must fail when any required gate fails.

Required checks (`test` job in `.github/workflows/ci.yml`):

| Check | Purpose |
|---|---|
| `go test ./...` | Unit and integration behavior across packages (default build; deterministic core only). |
| `go test -race ./...` | Race safety for memory core, job store, server shutdown, and adapters. |
| `go test -coverprofile=coverage.out ./...` | Generate coverage data. |
| `./scripts/check-coverage.sh coverage.out` | Enforce 100% coverage on packages that have default-build tests (see exclusion policy above). |
| `go vet ./...` | Catch suspicious Go constructs. |
| `govulncheck ./...` | Detect known vulnerabilities in reachable code. |
| `docker build -t workspace-brain:local .` | Verify the committed Dockerfile builds. |

CI uses the local deterministic core by default in the `test` job. Adapters
with a real backing service (Postgres, Qdrant, RabbitMQ) are exercised in a
separate `integration-test` job, gated behind `-tags=integration`. Unlike
external SaaS providers (Slack, OpenAI), these adapters need no external
credentials or fixtures: their tests provision their own ephemeral containers
via testcontainers-go, so the `integration-test` job only needs a working
Docker daemon (present by default on `ubuntu-latest` runners) — see
`make test-integration` above. Providers that genuinely require external
credentials (Slack, OpenAI) remain out of the default CI path and are not
covered by an automated job here.

## Smoke test expectations

A release smoke test must prove that the selected frontend adapter reaches the same gateway and data core contract.

Minimum server-mode smoke path:

1. Start the server with `API_TOKEN` and, when testing persistence, `DATA_PATH`.
2. Call `GET /healthz` and expect JSON status `ok`.
3. Call `GET /readyz` and expect JSON status `ready`.
4. Create one tenant project as an admin.
5. Ingest one bounded source, such as `text://demo`.
6. Read the returned job status.
7. Ask a question and verify the answer includes grounded source behavior.
8. Toggle project state off and confirm serving operations fail safely, if the release changes admin behavior.

Example JSON adapter commands:

```bash
export API_TOKEN='dev-token'
export ADDR=':8080'
go run ./cmd/workspace-brain
```

In another shell:

```bash
curl -sS http://localhost:8080/healthz
curl -sS http://localhost:8080/readyz
curl -sS http://localhost:8080/api/commands \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"binding_key":"web:space:S1","principal":{"source":"web","id":"admin","roles":["admin"]},"text":"create Demo"}'
curl -sS http://localhost:8080/api/commands \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1","roles":["member"]},"text":"ingest text://demo"}'
curl -sS http://localhost:8080/api/commands \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1","roles":["member"]},"text":"status job-616eec1b005fb0258433d73b843f8380"}'
curl -sS http://localhost:8080/api/commands \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1","roles":["member"]},"text":"ask demo"}'
```

Slack smoke tests should cover Slack as an adapter only: signature verification, command routing, safe user-facing errors, and the same core create/ingest/ask/status flow through the gateway.

## Test areas to protect

Keep coverage around these boundaries:

- `pkg/brainapi`: contract types, validation, and safe errors.
- `internal/control/frontend`: shared command parsing and response rendering.
- `internal/control/gateway`: binding resolution, authorization, source loading, job lifecycle, and safe access errors.
- `internal/control/jobs`: tenant-scoped job state and reconciliation.
- `internal/control/sources`: file, HTTP(S), inline, raw, text, size, timeout, and empty-content guards.
- `internal/core/memory`: tenant isolation, deterministic retrieval, grounded spans, discovery filtering, project state, and JSON persistence.
- `internal/control/httpapi`: bearer token validation, strict JSON decoding, and response status mapping.
- `internal/control/slack`: adapter verification and manifest behavior.
- `internal/control/ops`: health and readiness responses.
- `internal/platform/config` and `internal/platform/server`: typed environment parsing, timeouts, and graceful shutdown.

## When coverage fails

1. Read the missing lines in the generated coverage report.
2. Add a focused test for the behavior branch.
3. Keep production code simple enough to test directly.
4. Re-run the full required local commands before release.
