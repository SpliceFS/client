# shellcheck shell=sh
# The scripts that source this file read these variables; this file reads none of them.
# shellcheck disable=SC2034
# Sourced by the scripts that build the programs and make the release files from them. Says
# what is built and what a release file is called, and refuses a version that cannot be
# released.
: "${VERSION:?VERSION is not set}"
: "${BUILD_DATE:?BUILD_DATE is not set}"

# The name of the program set: the archives of a release are called after it.
name=splicefs-client
# Every program in cmd/ is built and archived.
programs=""
for cmd_folder in cmd/*; do programs="$programs $(basename "$cmd_folder")"; done
programs="${programs# }"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

# The file name of the plain program archive (archive-programs.sh) of the system $1, written
# <os>-<arch>. The version is the one the Makefile made, without the v.
archive_name() {
    printf '%s-%s-%s.tar.gz\n' "$name" "${VERSION#v}" "$1"
}

# Prints the systems make build-all put in the folder $1/bin, one <os>-<arch> to a line. The
# build decides what the release is made for, so the files that are made from its output ask
# it here and keep no list of their own.
built_systems() {
    [ -d "$1/bin" ] || fail "missing $1/bin, run make build-all first"
    found="$(cd "$1/bin" && LC_ALL=C ls)"
    [ -n "$found" ] || fail "$1/bin holds no system, run make build-all first"
    echo "$found"
}

# VERSION is the version in the file VERSION as the Makefile makes it: v1.2.3 for a release,
# v1.2.3-rc.1 for a pre-release, and a trailing -dirty when the tree has uncommitted changes.
# The package repository makes every package version from the one in a release file name, and
# Alpine takes only these pre-release words. Anything else is refused, which also catches a
# build that was given what git describe prints and so has not read the file VERSION.
release="${VERSION%-dirty}"
printf '%s' "$release" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|pre|rc)(\.?[0-9]+)?)?$' ||
    fail "version $VERSION is not vMAJOR.MINOR.PATCH[-alpha|-beta|-pre|-rc[.N]]"
