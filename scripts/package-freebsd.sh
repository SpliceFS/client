#!/bin/sh
# Builds the FreeBSD packages and proves them, on FreeBSD itself: for every FreeBSD release
# in packaging/freebsd/images it starts that release as a virtual machine, where pkg(8)
# builds the package for amd64 and arm64, installs the one for the machine and checks that
# the service runs (scripts/freebsd-job.sh). It runs on the host (make package-freebsd)
# after package-binaries.sh and scripts/signing-keys.sh and needs Docker, curl and xz.
#
# The machine also builds the signed package repository: it gets the keys and the packages
# published before (OUTPUT_DIR/repo/freebsd/FreeBSD:<major>:<cpu>/) and returns the folders
# repo/FreeBSD:<major>:<cpu>/ with the new package and the catalog pkg repo wrote. They are merged
# into OUTPUT_DIR/repo/freebsd/, which scripts/publish.sh sends.
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

[ -f "$out/keys/splicefs.rsa" ] && [ -f "$out/keys/splicefs.rsa.pub" ] ||
    fail "the signing keys are missing in $out/keys; run scripts/signing-keys.sh first"
published="$out/repo/freebsd"

# Writes the input archive of one machine: the signing keys and, when they exist, the folders
# of the packages published before for this FreeBSD major version.
input_archive() {
    major="$1"
    archive="$2"
    stage="$out/freebsd-input/$major"
    rm -rf "$stage"
    mkdir -p "$stage/keys" "$stage/repo"
    cp "$out/keys/splicefs.rsa" "$out/keys/splicefs.rsa.pub" "$stage/keys/"
    for abiarch in amd64 aarch64; do
        if [ -d "$published/FreeBSD:$major:$abiarch" ]; then
            cp -R "$published/FreeBSD:$major:$abiarch" "$stage/repo/"
        fi
    done
    # macOS attaches attributes of its own to files, and its tar would store them; FreeBSD's tar
    # then reports an error for each. Both tar programs know this option, and the variable
    # stops the macOS one from adding "._" files.
    COPYFILE_DISABLE=1 tar --no-xattrs -cf "$archive" -C "$stage" .
}

# Merges the repository folders the machine returned into the published ones. The catalog
# files are replaced, packages are added. A published package is never replaced by a different
# file: a package under a name that exists must be byte for byte the same one.
merge_repo() {
    result="$1"
    for src in "$result"/repo/FreeBSD:*; do
        [ -d "$src" ] || continue
        dest="$published/$(basename "$src")"
        mkdir -p "$dest/All"
        for pkgfile in "$src"/All/*.pkg; do
            target="$dest/All/$(basename "$pkgfile")"
            if [ -f "$target" ]; then
                cmp -s "$pkgfile" "$target" ||
                    fail "$target is published and differs from the package just built; a published package is never replaced"
            else
                cp "$pkgfile" "$target"
            fi
        done
        # Catalog files keep no old content: only what this run wrote stays.
        for old in "$dest"/*; do
            [ "$(basename "$old")" = All ] || rm -rf "$old"
        done
        for new in "$src"/*; do
            [ "$(basename "$new")" = All ] || cp -R "$new" "$dest/"
        done
    done
}

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
    major="${release%%.*}"
    archive="$out/freebsd-input/$release.tar"
    mkdir -p "$out/freebsd-input"
    input_archive "$major" "$archive"
    # Folders left by an earlier run must not be merged again as if this run had made them.
    rm -rf "$out"/freebsd/repo/FreeBSD:"$major":*
    freebsd_run "$disk" "$job" "$out/freebsd" "$archive"
    [ "$run_status" -eq 0 ] || fail "FreeBSD $release ($machine) failed"
    merge_repo "$out/freebsd"
    # Merged, so the copy goes: its folder names hold colons, which the file upload of a CI
    # job refuses, and dist/freebsd is what such a job keeps.
    rm -rf "$out/freebsd/repo"
    echo "ok   FreeBSD $release ($machine)"
}

freebsd_each_release build_and_test
ls "$out"/freebsd/*/*.pkg "$published"/*/meta.conf
