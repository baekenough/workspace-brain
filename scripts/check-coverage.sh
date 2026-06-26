#!/usr/bin/env sh
set -eu
profile="${1:-coverage.out}"
go test ./... -coverprofile="$profile"

# coretest is a test-helper package (no production code, only a contract-suite
# helper for other packages to import). Its t.Fatalf guard lines are never hit
# during passing test runs — that is the nature of assertion helpers. Exclude it
# from the total so the gate measures only production code.
filtered="${profile%.out}-filtered.out"
grep -v '^github\.com/sangyi/workspace-brain/internal/core/coretest/' "$profile" \
  > "$filtered"

total="$(go tool cover -func="$filtered" | awk '/^total:/ {print $3}')"
if [ "$total" != "100.0%" ]; then
  go tool cover -func="$filtered"
  echo "coverage must be 100.0%, got $total" >&2
  exit 1
fi
go tool cover -func="$filtered"
