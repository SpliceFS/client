#!/bin/sh
# Runs inside the FreeBSD machine as root, started by the first-boot script: unpacks the
# archive of test programs and source from the input disk and runs every test program in
# the folder of its package, as go test would. The coverage profiles go to the output folder.
#
#   freebsd-check-job.sh <input folder> <output folder> [package folder]
#
# With a package folder (for example mount/fuse) only that package is tested.
set -u

in="$1"
out="$2"
only="${3:-}"
work=/tmp/work
mkdir -p "$work" "$out/cover"

# Kernel modules the tests need, one per line in packaging/freebsd/test-modules, which the
# job folder holds when the program has such a file. A module may be loaded already.
if [ -f "$in/test-modules" ]; then
    grep -v -e '^#' -e '^$' "$in/test-modules" | while read -r module; do
        kldload "$module" 2>/dev/null || true
        kldstat -q -m "$module" || { echo "FAIL: the $module kernel module is not loaded"; exit 1; }
    done || exit 1
fi

# The fourth disk is the archive itself, not a filesystem.
tar -xf /dev/vtbd3 -C "$work" || { echo "FAIL: cannot read the test archive"; exit 1; }
cd "$work" || exit 1

failed=""
while read -r name rel <&3; do
    if [ -n "$only" ] && [ "$only" != "$rel" ]; then continue; fi
    echo "=== $rel"
    if (cd "$rel" && "$work/tests/$name.test" -test.coverprofile="$out/cover/$name.out" -test.timeout=10m); then
        :
    else
        failed="$failed $rel"
    fi
done 3< tests/list

if [ -n "$failed" ]; then
    echo "FAIL: tests failed in:$failed"
    exit 1
fi
os_version="$(freebsd-version)"
cpu="$(uname -m)"
echo "all tests passed on $os_version ($cpu)"
