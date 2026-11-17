#!/bin/sh
# Tests scripts/release-parts.sh, which carries the FreeBSD part of the package repository
# from the FreeBSD jobs of a release to its publish job. Runs on the host, no Docker (make
# release-parts-test, part of make unit). The repository is a small hand-made folder.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
# Runs the command in a subshell, with its output in $work/output, and fails unless its status
# is $1 and its output contains $2. An empty text is no condition on the output.
expect() {
    want_status="$1"
    want_text="$2"
    shift 2
    status=0
    ("$@") >"$work/output" 2>&1 || status=$?
    [ "$status" -eq "$want_status" ] ||
        { cat "$work/output" >&2; fail "[$*] ended with status $status, want $want_status"; }
    [ -z "$want_text" ] || grep -qF -- "$want_text" "$work/output" ||
        { cat "$work/output" >&2; fail "[$*] did not say [$want_text]"; }
}

export VERSION=v1.2.3 PACKAGE_DATE=20260101
cd "$root"
# The name of the package of this version comes from the same library the build uses, which
# also provides fail.
# shellcheck source=package-lib.sh
. scripts/package-lib.sh
new_package="$name-$pkgver.pkg"

printf '14.5\n15.1\n' >"$work/releases"
export RELEASES_FILE="$work/releases"

# One published FreeBSD repository folder: an old package, the catalog that lists it, and
# the package of this version when $3 is yes. $1 is the repository root, $2 the folder name.
make_folder() {
    mkdir -p "$1/freebsd/$2/All"
    echo "old package" >"$1/freebsd/$2/All/$name-0.0.1.pkg"
    echo "catalog $4" >"$1/freebsd/$2/meta.conf"
    echo "data $4" >"$1/freebsd/$2/data.pkg"
    if [ "$3" = yes ]; then echo "new package $2" >"$1/freebsd/$2/All/$new_package"; fi
}

# A dist folder as a FreeBSD job leaves it: both CPU types of one major version merged.
make_dist() {
    rm -rf "$1"
    for abi in amd64 aarch64; do
        make_folder "$1/repo" "FreeBSD:$2:$abi" yes new
    done
}

pack() { DIST_DIR="$1" scripts/release-parts.sh pack "$2"; }
unpack() { DIST_DIR="$1" scripts/release-parts.sh unpack; }
listing() { tar -tf "$1" | sort; }

# --- pack -------------------------------------------------------------------------------

make_dist "$work/dist14" 14
expect 0 "" pack "$work/dist14" 14.5
want="freebsd/FreeBSD:14:aarch64/All/$new_package
freebsd/FreeBSD:14:aarch64/data.pkg
freebsd/FreeBSD:14:aarch64/meta.conf
freebsd/FreeBSD:14:amd64/All/$new_package
freebsd/FreeBSD:14:amd64/data.pkg
freebsd/FreeBSD:14:amd64/meta.conf"
got="$(listing "$work/dist14/freebsd-repo-14.5.tar")"
[ "$got" = "$want" ] || fail "the tar of 14.5 holds [$got], want [$want]"

# Only the folders of its own major version: another release's catalog must not be rolled
# back to the state this job downloaded.
make_dist "$work/dist-both" 14
make_folder "$work/dist-both/repo" FreeBSD:15:amd64 yes new
expect 0 "" pack "$work/dist-both" 14.5
tar -tf "$work/dist-both/freebsd-repo-14.5.tar" | grep -q 'FreeBSD:15' &&
    fail "the tar of 14.5 holds a folder of FreeBSD 15"

# A folder without the package of this version is not a result of this release.
make_dist "$work/dist-old" 14
rm "$work/dist-old/repo/freebsd/FreeBSD:14:aarch64/All/$new_package"
expect 1 "FreeBSD:14:aarch64/All/$new_package is missing" pack "$work/dist-old" 14.5
expect 1 "no repository folder of FreeBSD 15" pack "$work/dist14" 15.1
expect 2 "usage: release-parts.sh" pack "$work/dist14" ""

# --- unpack -----------------------------------------------------------------------------

# The fetched repository, with the state before this release for both major versions.
fetched() {
    rm -rf "$work/fetched"
    for major in 14 15; do
        for abi in amd64 aarch64; do
            make_folder "$work/fetched/repo" "FreeBSD:$major:$abi" no old
        done
    done
    cp -R "$work/fetched" "$work/fetched-before"
}

make_dist "$work/dist15" 15
pack "$work/dist15" 15.1
fetched

# Both tars must be there before anything is changed: a missing one would otherwise leave
# the old catalog of its major version in place and publish without its package.
mkdir "$work/parts"
cp "$work/dist14/freebsd-repo-14.5.tar" "$work/parts/"
cp -R "$work/fetched/repo" "$work/parts/repo"
expect 1 "freebsd-repo-15.1.tar is missing" unpack "$work/parts"
diff -r "$work/fetched/repo" "$work/parts/repo" >/dev/null || fail "a failed unpack changed the repository"

cp "$work/dist15/freebsd-repo-15.1.tar" "$work/parts/"
expect 0 "" unpack "$work/parts"
for major in 14 15; do
    for abi in amd64 aarch64; do
        folder="$work/parts/repo/freebsd/FreeBSD:$major:$abi"
        grep -qx "new package FreeBSD:$major:$abi" "$folder/All/$new_package" ||
            fail "the new package is not in FreeBSD:$major:$abi"
        grep -qx "catalog new" "$folder/meta.conf" || fail "the catalog of FreeBSD:$major:$abi is not the new one"
        grep -qx "old package" "$folder/All/$name-0.0.1.pkg" || fail "an old package changed in FreeBSD:$major:$abi"
    done
done

echo "release parts: all cases as expected"
