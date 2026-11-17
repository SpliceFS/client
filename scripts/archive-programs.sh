#!/bin/sh
# Makes the plain program archives of THIS version into DIST_DIR: one tar.gz for each system
# and CPU type that make build-all put in DIST_DIR/bin. Each holds the programs built for that
# system, LICENSE and README.txt (packaging/plain), and nothing that installs or runs them:
# no service file, no mount helper, no install script. They are for people who run SpliceFS by
# hand on a system the packages do not cover. Runs on the host after make build-all (make
# archives) and needs only tar and gzip. Variables: VERSION and PACKAGE_DATE as for the build;
# DIST_DIR (default dist).
#
# The same programs give the same bytes, so a rebuild can be compared with a release: the
# members are sorted, owner and group are 0, modes are fixed, every time is the date
# PACKAGE_DATE at 00:00 UTC, and gzip stores neither a name nor a time.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
# package-lib.sh reads VERSION and PACKAGE_DATE and says what the archives are called.
# shellcheck source=package-lib.sh
. scripts/package-lib.sh

dist="${DIST_DIR:-$root/dist}"
bin="$dist/bin"

case "$PACKAGE_DATE" in
    [0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]) ;;
    *) fail "PACKAGE_DATE $PACKAGE_DATE is not a date written YYYYMMDD" ;;
esac
# The names are ours and hold no spaces, so the list is split on purpose.
systems="$(built_systems "$dist")"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM

# Writes to the standard output the tar of the members listed in the file $2, which are in the
# folder $1. The two tar programs have their own options for the same thing. ustar has no
# extended headers, so nothing of the machine that built it gets in. COPYFILE_DISABLE stops the
# tar of macOS from adding the hidden ._ members that carry file attributes.
if tar --version 2>/dev/null | grep -q 'GNU tar'; then
    tar_members() {
        tar --format=ustar --no-recursion --owner=0 --group=0 --numeric-owner \
            -cf - -C "$1" -T "$2"
    }
else
    tar_members() {
        COPYFILE_DISABLE=1 tar --format ustar --no-recursion --uid 0 --gid 0 \
            --uname '' --gname '' -cf - -C "$1" -T "$2"
    }
fi

# Puts the folder of the archive of the system $1 in the folder $2, with the programs, the
# license and the README in it, and the modes and times that make the archive the same every
# time.
stage_system() {
    programs="$(cd "$bin/$1" && LC_ALL=C ls)"
    [ -n "$programs" ] || fail "$bin/$1 holds no programs, run make build-all first"
    mkdir -p "$2"
    cp "$bin/$1"/* "$2/"
    cp LICENSE packaging/plain/README.txt "$2/"
    chmod 755 "$2" "$2"/*
    chmod 644 "$2/LICENSE" "$2/README.txt"
    TZ=UTC touch -t "${PACKAGE_DATE}0000" "$2" "$2"/*
}

for system in $systems; do
    file="$(archive_name "$system")"
    top="${file%.tar.gz}"
    stage_system "$system" "$work/$system/$top"
    (cd "$work/$system" && find "$top" | LC_ALL=C sort) > "$work/$system.list"
    # The tar is written to a file before it is compressed: a pipeline would hide a failed tar.
    tar_members "$work/$system" "$work/$system.list" > "$work/$system.tar"
    gzip -n -9 -c "$work/$system.tar" > "$work/$file"
    mv "$work/$file" "$dist/$file"
    echo "made $dist/$file"
done
