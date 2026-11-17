#!/bin/sh
# Carries the FreeBSD part of the package repository from the FreeBSD jobs of a release to the
# job that publishes. A FreeBSD job merges its package and catalog into
# dist/repo/freebsd/FreeBSD:<major>:<cpu>/, and the upload of a CI artifact refuses the colons
# in those folder names, so one tar carries them. Runs on the host (make release-pack and
# make release-unpack).
# Usage: release-parts.sh pack RELEASE | release-parts.sh unpack
#   pack    writes dist/freebsd-repo-RELEASE.tar with the folders of the FreeBSD major version
#           of RELEASE: the catalog files and the package of this version, and not the packages
#           published before, which the publish job downloads itself.
#   unpack  extracts the tar of every release in packaging/freebsd/releases into dist/repo.
#           It stops before it extracts anything when one tar is missing: the old catalog of
#           that major version would stay, and the release would go out without its package.
# Variables: VERSION and PACKAGE_DATE as for the build; DIST_DIR (default dist); RELEASES_FILE
# (default packaging/freebsd/releases).
set -eu

mode="${1:-}"
freebsd_release="${2:-}"
case "$mode:$freebsd_release" in
    pack:?* | unpack:) ;;
    *)
        echo "usage: release-parts.sh pack RELEASE | release-parts.sh unpack" >&2
        exit 2
        ;;
esac

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
# package-lib.sh reads VERSION and PACKAGE_DATE and says what the package is called.
# shellcheck source=package-lib.sh
. scripts/package-lib.sh

dist="${DIST_DIR:-dist}"
releases="${RELEASES_FILE:-packaging/freebsd/releases}"

pack() {
    major="${freebsd_release%%.*}"
    [ -d "$dist/repo/freebsd" ] || fail "no repository folder in $dist/repo/freebsd"
    tar_file="$(cd "$dist" && pwd)/freebsd-repo-$freebsd_release.tar"
    cd "$dist/repo"
    set -- freebsd/FreeBSD:"$major":*
    [ -d "$1" ] || fail "no repository folder of FreeBSD $major in $dist/repo/freebsd"
    for folder in "$@"; do
        [ -f "$folder/All/$name-$pkgver.pkg" ] ||
            fail "$folder/All/$name-$pkgver.pkg is missing, make package-freebsd did not build it"
    done
    files="$(mktemp)"
    trap 'rm -f "$files"' EXIT
    find "$@" ! -type d ! \( -path '*/All/*.pkg' ! -name "$name-$pkgver.pkg" \) > "$files"
    tar -cf "$tar_file" -T "$files"
    echo "packed FreeBSD $freebsd_release into $tar_file"
}

unpack() {
    names="$(awk '!/^#/ && NF { print $1 }' "$releases")"
    for each in $names; do
        [ -f "$dist/freebsd-repo-$each.tar" ] ||
            fail "$dist/freebsd-repo-$each.tar is missing, the FreeBSD job of $each did not leave it"
    done
    mkdir -p "$dist/repo"
    for each in $names; do
        tar -xf "$dist/freebsd-repo-$each.tar" -C "$dist/repo"
        echo "unpacked FreeBSD $each into $dist/repo"
    done
}

"$mode"
