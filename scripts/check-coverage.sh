#!/usr/bin/env sh
set -eu
profile="${1:-coverage.out}"
go test ./... -coverprofile="$profile"
total="$(go tool cover -func="$profile" | awk '/^total:/ {print $3}')"
if [ "$total" != "100.0%" ]; then
  go tool cover -func="$profile"
  echo "coverage must be 100.0%, got $total" >&2
  exit 1
fi
go tool cover -func="$profile"
