#!/bin/sh
# Runs the tests with the race detector and fails unless statement coverage is 100%.
set -eu

profile="${1:-coverage.out}"

go test -race -covermode=atomic -coverprofile="$profile" ./...

total="$(go tool cover -func="$profile" | awk '$1 == "total:" {print $3}')"
if [ "$total" != "100.0%" ]; then
    echo "coverage is $total, required 100.0%. Uncovered:" >&2
    go tool cover -func="$profile" | awk '$1 != "total:" && $3 != "100.0%"' >&2
    exit 1
fi
echo "coverage: $total"
