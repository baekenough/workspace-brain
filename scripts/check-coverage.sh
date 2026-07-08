#!/usr/bin/env sh
set -eu
profile="${1:-coverage.out}"
go test ./... -coverprofile="$profile"

# coretest is a test-helper package (no production code, only a contract-suite
# helper for other packages to import). Its t.Fatalf guard lines are never hit
# during passing test runs — that is the nature of assertion helpers. Exclude it
# from the total so the gate measures only production code.
#
# postgres is an integration-only package: all its tests carry //go:build integration
# and run against a real Postgres container via `go test -tags=integration`
# (testcontainers-go, no fixture/service setup required — see the CI
# `integration-test` job in .github/workflows/ci.yml and `make test-integration`).
# Real coverage is 74.1% under that tag as of 2026-07-08; it has zero
# default-build tests by design. Exclude it here so the unit gate remains
# meaningful for the packages it covers. This percentage is informational only
# (verified via `make test-integration`, not enforced by this script) — re-verify
# and update it whenever postgres integration tests change meaningfully, but a
# small drift does not fail any gate.
#
# qdrant is an integration-only package: its tests carry //go:build integration
# and run against a real Qdrant container via `go test -tags=integration`
# (testcontainers-go REST adapter). Real coverage is 98.8% under that tag as of
# 2026-07-08; it has zero default-build tests by design. Exclude it here for
# the same reason as postgres. Informational only, same as above.
#
# rabbitmq is an integration-only package (WB-06b): all its tests carry
# //go:build integration and run against a real RabbitMQ container via
# `go test -tags=integration` (testcontainers-go). Real coverage is 65.0%
# under that tag as of 2026-07-08; it has zero default-build tests by design.
# Exclude it here for the same reason as postgres and qdrant. Informational
# only, same as above.
filtered="${profile%.out}-filtered.out"
grep -vE '^github\.com/sangyi/workspace-brain/(internal/core/(coretest|postgres|qdrant)|internal/control/rabbitmq)/' "$profile" \
  > "$filtered"

total="$(go tool cover -func="$filtered" | awk '/^total:/ {print $3}')"
if [ "$total" != "100.0%" ]; then
  go tool cover -func="$filtered"
  echo "coverage must be 100.0%, got $total" >&2
  exit 1
fi
go tool cover -func="$filtered"
