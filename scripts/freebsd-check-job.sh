#!/bin/sh
# Runs inside the FreeBSD machine as root, started by the first-boot script: unpacks the
# archive of test programs and source from the input disk and runs every test program in
# the folder of its package, as go test would. The coverage profiles go to the output folder.
#
#   freebsd-check-job.sh <input folder> <output folder> [package folder]
#
# With a package folder (for example mount/fuse) only that package is tested.
set -u

out="$2"
only="${3:-}"
work=/tmp/work
mkdir -p "$work" "$out/cover"

# The mounts under test are FUSE mounts. The module may be loaded already.
kldload fusefs 2>/dev/null || true
kldstat -q -m fusefs || { echo "FAIL: the fusefs kernel module is not loaded"; exit 1; }

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
echo "all tests passed on $(freebsd-version) ($(uname -m))"
