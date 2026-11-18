#!/bin/sh
# Runs the tests with coverage and fails unless statement coverage is 100%. The race detector
# has a part of its own (make race); the part that mounts for real adds it with -race (make fuse).
#
# usage: coverage.sh [PROFILE [ARGUMENT ...]]    The arguments go to go test, after the profile
# flag: the packages to test, and any flag such as -race. They default to every package.
set -eu

profile="${1:-coverage.out}"
[ "$#" -eq 0 ] || shift
[ "$#" -gt 0 ] || set -- ./...

go test -coverprofile="$profile" "$@"

total="$(go tool cover -func="$profile" | awk '$1 == "total:" {print $3}')"
if [ "$total" != "100.0%" ]; then
    echo "coverage is $total, required 100.0%. Uncovered:" >&2
    go tool cover -func="$profile" | awk '$1 != "total:" && $3 != "100.0%"' >&2
    exit 1
fi
echo "coverage: $total"
