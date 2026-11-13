#!/bin/sh
# Builds the FreeBSD packages and proves them, on FreeBSD itself: for every FreeBSD release
# in packaging/freebsd/images it starts that release as a virtual machine, where pkg(8)
# builds the package for amd64 and arm64, installs the one for the machine and checks that
# the service runs (scripts/freebsd-job.sh). It runs on the host (make package-freebsd)
# after package-binaries.sh and needs Docker, curl and xz.
#
# Environment: VERSION and PACKAGE_DATE as for every package; FREEBSD limits the run to
# one release (for example 15.1), empty means every release in the list; LINUX_IMAGE is
# the container image QEMU is installed into; VM_TIMEOUT is the most seconds one machine
# may run.
set -eu

out="${1:?usage: package-freebsd.sh OUTPUT_DIR}"
. scripts/package-lib.sh
image="${LINUX_IMAGE:?LINUX_IMAGE is not set}"
out="$(cd "$out" && pwd)"
. scripts/freebsd-lib.sh

for arch in amd64 arm64; do
    [ -f "$out/stage/freebsd-$arch/$program" ] ||
        fail "$out/stage/freebsd-$arch/$program does not exist; run package-binaries.sh first"
done

build_and_test() {
    release="$1"
    disk="$2"
    job="$out/freebsd-job/$release"
    freebsd_job_folder "$job" freebsd-job.sh "$name" "$program" "$pkgver" "$VERSION"
    for arch in amd64 arm64; do
        cp "$out/stage/freebsd-$arch/$program" "$job/$program-$arch"
    done
    cp "packaging/freebsd/$program.rc" packaging/freebsd/manifest.in packaging/freebsd/plist \
        packaging/freebsd/check-lib.sh "$job/"
    echo "FreeBSD $release ($machine): building and testing $name $pkgver"
    freebsd_run "$disk" "$job" "$out/freebsd" || fail "FreeBSD $release ($machine) failed"
    echo "ok   FreeBSD $release ($machine)"
}

freebsd_each_release build_and_test
ls "$out"/freebsd/*/*.pkg
