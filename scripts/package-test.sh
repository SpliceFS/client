#!/bin/sh
# Tests on every system in packaging/test-systems, each in its own throwaway container, and
# keeps going after a failure so one run shows every system that is broken. Runs on the
# host (make package-test, make repo-test, CI). Usage: package-test.sh [package|repo].
# "package" (the default) install-tests the packages with scripts/package-test-<format>.sh;
# "repo" tests the signed repository in dist/repo with scripts/repo-test-<format>.sh, which
# sees the repository at /repo and only the PUBLIC keys at /keys. Environment: VERSION and
# PACKAGE_DATE as for the build; PLATFORM (linux/amd64 or linux/arm64) tests that CPU type
# by emulation, empty means this machine's own; ONLY limits the run to one format or image.
set -eu

kind="${1:-package}"
case "$kind" in
    package | repo) ;;
    *) echo "usage: package-test.sh [package|repo]" >&2; exit 2 ;;
esac

: "${VERSION:?VERSION is not set}"
: "${PACKAGE_DATE:?PACKAGE_DATE is not set}"
root="$(cd "$(dirname "$0")/.." && pwd)"
# Named explicitly because after a run with another platform the local image tag can point
# at the other CPU type, and an empty setting would then silently test the wrong one.
platform="${PLATFORM:-$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')}"
mounts="-v $root/dist:/out"
if [ "$kind" = repo ]; then
    mounts="-v $root/dist/repo:/repo:ro -v $root/dist/keys/splicefs.asc:/keys/splicefs.asc:ro"
    mounts="$mounts -v $root/dist/keys/splicefs.rsa.pub:/keys/splicefs.rsa.pub:ro"
fi
systems="$(cat "$root/packaging/test-systems")"
log="$(mktemp)"
trap 'rm -f "$log"' EXIT
passed=0
failed=0

# A here-document keeps the counters in this shell; a pipe would run the loop in a subshell.
while read -r format image; do
    # Blank lines and comments are skipped; every other line is a system to test.
    case "$format" in "" | "#"*) continue ;; *) ;; esac
    if [ -n "${ONLY:-}" ] && [ "$ONLY" != "$format" ] && [ "$ONLY" != "$image" ]; then
        continue
    fi
    # A format without a script for this kind is not a failure. The rpm and apk repository
    # tests are not written yet; whoever adds scripts/repo-test-<format>.sh starts them here.
    if [ ! -f "$root/scripts/$kind-test-$format.sh" ]; then
        echo "skip $format $image (no $kind test yet)"
        continue
    fi
    # $mounts is split on purpose: it holds several -v options.
    # shellcheck disable=SC2086
    if docker run --rm --platform "$platform" \
        -e VERSION -e PACKAGE_DATE -v "$root":/src:ro $mounts -w /src \
        "$image" sh "scripts/$kind-test-$format.sh" /out >"$log" 2>&1; then
        echo "ok   $format $image"
        passed=$((passed + 1))
    else
        echo "FAIL $format $image"
        tail -n 15 "$log" | sed 's/^/     /'
        failed=$((failed + 1))
    fi
done <<LIST
$systems
LIST

echo "passed: $passed  failed: $failed"
[ "$failed" -eq 0 ]
