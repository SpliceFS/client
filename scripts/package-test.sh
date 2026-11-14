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
    package) title="Package install tests" ;;
    repo) title="Repository install tests" ;;
    *) echo "usage: package-test.sh [package|repo]" >&2; exit 2 ;;
esac

: "${VERSION:?VERSION is not set}"
: "${PACKAGE_DATE:?PACKAGE_DATE is not set}"
root="$(cd "$(dirname "$0")/.." && pwd)"
# package-lib.sh reads files relative to the repository root.
cd "$root"
. scripts/package-lib.sh
. scripts/timing.sh
timing_begin
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
# The timing report is printed on every way out, so a failed run still shows where time went.
trap 'rm -f "$log"; timing_report "$title"' EXIT
passed=0
failed=0

# Runs one system's test in its own container. The container's output goes to the log, so the
# redirection is part of this function and not of the timed call, which prints to the console.
# $mounts is split on purpose: it holds several -v options.
run_system_test() {
    # shellcheck disable=SC2086
    docker run --rm --platform "$platform" \
        -e VERSION -e PACKAGE_DATE -v "$root":/src:ro $mounts -w /src \
        "$image" sh "scripts/$kind-test-$format.sh" /out >"$log" 2>&1
}

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
    # A registry failure must not look like a failed package test.
    # Errexit is off for the call to read its status; pull_image checks every command itself.
    set +e
    timed "$image: pull the image" pull_image "$platform" "$image"
    pulled=$?
    set -e
    if [ "$pulled" -ne 0 ]; then
        echo "FAIL $format $image"
        echo "     could not download the image $image (3 tries)"
        failed=$((failed + 1))
        continue
    fi
    # Errexit is off for the call to read its status; the function is one docker command.
    set +e
    timed "$image: $kind test" run_system_test
    ran=$?
    set -e
    if [ "$ran" -eq 0 ]; then
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
