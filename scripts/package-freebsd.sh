#!/bin/sh
# Builds the FreeBSD packages and proves them, on FreeBSD itself: for every FreeBSD release
# in packaging/freebsd/releases it starts that release as a virtual machine (its prepared
# image, scripts/freebsd-lib.sh), where pkg(8) builds the package for amd64 and for arm64,
# installs the amd64 one and checks that the service runs (scripts/freebsd-job.sh). The
# machine is amd64, so the arm64 package is built there for another ABI and is not installed;
# scripts/freebsd-package-check.sh shows that it is an arm64 package. It runs on the host
# (make package-freebsd) after scripts/build-all.sh and scripts/signing-keys.sh and needs
# Docker and hardware virtualization (KVM).
#
# The machine also builds the signed package repository: it gets the keys and the packages
# published before (OUTPUT_DIR/repo/freebsd/FreeBSD:<major>:<cpu>/) and returns the folders
# repo/FreeBSD:<major>:<cpu>/ with the new package and the catalog pkg repo wrote. They are merged
# into OUTPUT_DIR/repo/freebsd/, which scripts/publish.sh sends.
#
# Environment: VERSION and PACKAGE_DATE as for every package; FREEBSD limits the run to
# one release (for example 15.1), empty means every release in the list; FREEBSD_IMAGE is
# the image of a release, with @RELEASE@ where the release goes; LINUX_IMAGE is the Go image,
# where the files of the repository are handled and the packages are checked; VM_TIMEOUT is
# the most seconds one machine may run.
set -eu

out="${1:?usage: package-freebsd.sh OUTPUT_DIR}"
. scripts/package-lib.sh
# shellcheck source=scripts/timing.sh
. scripts/timing.sh
timing_begin
trap 'timing_report "FreeBSD packages"' EXIT
image="${LINUX_IMAGE:?LINUX_IMAGE is not set}"
out="$(cd "$out" && pwd)"
. scripts/freebsd-lib.sh

for arch in amd64 arm64; do
    for each in $programs; do
        [ -f "$out/bin/freebsd-$arch/$each" ] ||
            fail "$out/bin/freebsd-$arch/$each does not exist; run build-all.sh first"
    done
done

[ -f "$out/keys/splicefs.rsa" ] && [ -f "$out/keys/splicefs.rsa.pub" ] ||
    fail "the signing keys are missing in $out/keys; run scripts/signing-keys.sh first"
published="$out/repo/freebsd"

# Runs one step of scripts/freebsd-repo-files.sh as root in the Linux container, where the
# signing key and the files a machine returned can be read and removed. On Linux the host
# user can do neither: both belong to root.
repo_files() {
    source_folder="$(pwd)"
    docker run --rm --user 0 -v "$source_folder":/src:ro -v "$out":/out "$image" \
        sh /src/scripts/freebsd-repo-files.sh "$@"
}

# Looks at what the machine built, in the Go container, which has the tools. The package of
# the CPU type the machine does not have is never installed, so this is where a package that
# names one ABI and holds a program of another is stopped. Every program of the package is
# looked at, one call each.
check_packages() {
    source_folder="$(pwd)"
    user_id="$(id -u)"
    group_id="$(id -g)"
    for abiarch in amd64 aarch64; do
        for each in $programs; do
            docker run --rm --user "$user_id:$group_id" -v "$source_folder":/src:ro -v "$out":/out:ro "$image" \
                sh /src/scripts/freebsd-package-check.sh "/out/freebsd/FreeBSD-$1-$abiarch/$name-$pkgver.pkg" \
                "$1" "$abiarch" "$each"
        done
    done
}

build_and_test() {
    release="$1"
    job="$out/freebsd-job/$release"
    freebsd_job_folder "$job" freebsd-job.sh "$name" "$program" "$pkgver" "$VERSION"
    for arch in amd64 arm64; do
        for each in $programs; do
            cp "$out/bin/freebsd-$arch/$each" "$job/$each-$arch"
        done
    done
    cp "packaging/freebsd/$program.rc" packaging/freebsd/manifest.in packaging/freebsd/plist \
        packaging/freebsd/check-lib.sh "$job/"
    echo "FreeBSD $release: building $name $pkgver for amd64 and arm64, installing the amd64 package"
    major="${release%%.*}"
    archive="$out/freebsd-input/$release.tar"
    timed "FreeBSD $release: prepare the repository input" repo_files input "$major" "freebsd-input/$release.tar"
    timed "FreeBSD $release: machine run" freebsd_run "$release" "$job" "$out/freebsd" "$archive"
    # Removed before the result is looked at, so that a failed run leaves no key file either.
    repo_files remove-input "freebsd-input/$release.tar"
    [ "$run_status" -eq 0 ] || fail "FreeBSD $release failed"
    timed "FreeBSD $release: check the packages" check_packages "$major"
    timed "FreeBSD $release: merge the repository" repo_files merge
    echo "ok   FreeBSD $release"
}

freebsd_each_release build_and_test
ls "$out"/freebsd/*/*.pkg "$published"/*/meta.conf
