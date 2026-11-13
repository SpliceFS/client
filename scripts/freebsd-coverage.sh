#!/bin/sh
# Fails unless the coverage profiles a FreeBSD machine wrote add up to 100% of the
# statements of the FreeBSD build. It is the same gate as scripts/coverage.sh, applied to
# profiles that were written somewhere else. It runs inside the Go container
# (scripts/check-freebsd.sh), in the source folder.
set -eu

dir="${1:?usage: freebsd-coverage.sh PROFILE_DIR}"
profile="$(mktemp)"
trap 'rm -f "$profile"' EXIT

# One profile per package, each with its own header line; one header is kept.
echo "mode: atomic" > "$profile"
cat "$dir"/*.out | grep -v '^mode:' >> "$profile"

# The build tags of FreeBSD decide which files the profile names.
export GOOS=freebsd
total="$(go tool cover -func="$profile" | awk '$1 == "total:" {print $3}')"
if [ "$total" != "100.0%" ]; then
    echo "coverage on FreeBSD is $total, required 100.0%. Uncovered:" >&2
    go tool cover -func="$profile" | awk '$1 != "total:" && $3 != "100.0%"' >&2
    exit 1
fi
echo "coverage on FreeBSD: $total"
