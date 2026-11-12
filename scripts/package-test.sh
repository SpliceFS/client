#!/bin/sh
# Install-tests every package on every system in packaging/test-systems, each in its own
# throwaway container, and keeps going after a failure so one run shows every system that
# is broken. Runs on the host (make package-test, CI). Environment: VERSION and
# PACKAGE_DATE as for the build; PLATFORM (linux/amd64 or linux/arm64) tests that CPU type
# by emulation, empty means this machine's own; ONLY limits the run to one format or image.
set -eu

: "${VERSION:?VERSION is not set}"
: "${PACKAGE_DATE:?PACKAGE_DATE is not set}"
root="$(cd "$(dirname "$0")/.." && pwd)"
log="$(mktemp)"
trap 'rm -f "$log"' EXIT
passed=0
failed=0

# A here-document keeps the counters in this shell; a pipe would run the loop in a subshell.
while read -r format image; do
    case "$format" in "" | "#"*) continue ;; esac
    if [ -n "${ONLY:-}" ] && [ "$ONLY" != "$format" ] && [ "$ONLY" != "$image" ]; then
        continue
    fi
    if docker run --rm ${PLATFORM:+--platform "$PLATFORM"} \
        -e VERSION -e PACKAGE_DATE -v "$root":/src:ro -v "$root/dist":/out -w /src \
        "$image" sh "scripts/package-test-$format.sh" /out >"$log" 2>&1; then
        echo "ok   $format $image"
        passed=$((passed + 1))
    else
        echo "FAIL $format $image"
        tail -n 15 "$log" | sed 's/^/     /'
        failed=$((failed + 1))
    fi
done <<LIST
$(cat "$root/packaging/test-systems")
LIST

echo "passed: $passed  failed: $failed"
[ "$failed" -eq 0 ]
