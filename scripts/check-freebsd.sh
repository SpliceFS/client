#!/bin/sh
# Runs the tests on FreeBSD itself and fails unless they pass with 100% statement coverage.
# The code that exists only for FreeBSD cannot be tested anywhere else, and the code shared
# with Linux has to be proven on FreeBSD's own FUSE. The test programs are built for FreeBSD
# by scripts/freebsd-tests.sh; this script runs them in a virtual machine of every FreeBSD
# release in packaging/freebsd/images, as root, with the kernel modules listed in
# packaging/freebsd/test-modules loaded
# (scripts/freebsd-check-job.sh). It runs on the host (make check-freebsd) and needs
# Docker, curl and xz.
#
# Environment: FREEBSD limits the run to one release (for example 15.1); LINUX_IMAGE is the
# container image QEMU is installed into; VM_TIMEOUT is the most seconds one machine may run;
# FREEBSD_ONLY names one package folder (for example mount/fuse) to test alone, which is for
# work on that package: the coverage gate is then skipped, and the run says so.
set -eu

out="${1:?usage: check-freebsd.sh OUTPUT_DIR}"
image="${LINUX_IMAGE:?LINUX_IMAGE is not set}"
out="$(cd "$out" && pwd)"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

. scripts/freebsd-lib.sh

work="$out/freebsd-check"
archive="$work/$machine/in.tar"
[ -f "$archive" ] || fail "$archive does not exist; run freebsd-tests.sh first"

run_tests() {
    release="$1"
    disk="$2"
    # Not under $work: the container that built the test programs made that folder, as root
    # on Linux, and this script, which runs as the user, could not create anything in it.
    job="$out/freebsd-check-job/$release"
    results="$out/freebsd-check-results/$release"
    rm -rf "$results" 2>/dev/null || true
    mkdir -p "$results"
    freebsd_job_folder "$job" freebsd-check-job.sh "${FREEBSD_ONLY:-}"
    if [ -f packaging/freebsd/test-modules ]; then cp packaging/freebsd/test-modules "$job/"; fi
    echo "FreeBSD $release ($machine): running the tests"
    freebsd_run "$disk" "$job" "$results" "$archive"
    [ "$run_status" -eq 0 ] || fail "the tests failed on FreeBSD $release ($machine)"
    if [ -n "${FREEBSD_ONLY:-}" ]; then
        echo "ok   FreeBSD $release ($machine), $FREEBSD_ONLY only: coverage NOT checked"
        return 0
    fi
    # The profiles name source files, so the gate needs the Go tools and the source: it
    # runs in the Go container like every other build step.
    docker run --rm -v "$source_dir":/src:ro -v "$out":/out -v splicefs-gomod:/go/pkg/mod "$image" \
        sh -c "cd /src && sh scripts/freebsd-coverage.sh '/out/${results#"$out"/}/cover'" ||
        fail "coverage on FreeBSD $release ($machine) is not 100%"
    echo "ok   FreeBSD $release ($machine)"
}

freebsd_each_release run_tests
