#!/bin/sh
# Table test of which versions scripts/archive-lib.sh accepts and what the archive of each is
# called. Runs on the host, no Docker (make archive-lib-test, part of make check).
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
failed=0

# $1 VERSION, $2 the archive name for linux-amd64 that it must give.
accepts() {
    got="$(VERSION="$1" PACKAGE_DATE=20261112 sh -c \
        '. scripts/archive-lib.sh; archive_name linux-amd64' 2>&1)" || true
    if [ "$got" = "$2" ]; then
        printf 'ok   %-28s %s\n' "$1" "$2"
    else
        printf 'FAIL %-28s want "%s" got "%s"\n' "$1" "$2" "$got"
        failed=$((failed + 1))
    fi
}

# $1 VERSION that must be refused; the message must name it.
refuses() {
    if got="$(VERSION="$1" PACKAGE_DATE=20261112 sh -c '. scripts/archive-lib.sh' 2>&1)"; then
        printf 'FAIL %-28s was accepted\n' "$1"
        failed=$((failed + 1))
    elif printf '%s' "$got" | grep -q -- "$1"; then
        printf 'ok   %-28s refused: %s\n' "$1" "$got"
    else
        printf 'FAIL %-28s refused with the wrong message: %s\n' "$1" "$got"
        failed=$((failed + 1))
    fi
}

accepts v1.2.3 splicefs-client-1.2.3-linux-amd64.tar.gz
accepts v1.2.3-rc.1 splicefs-client-1.2.3-rc.1-linux-amd64.tar.gz
accepts v1.2.3-beta.2 splicefs-client-1.2.3-beta.2-linux-amd64.tar.gz
accepts v1.2.3-alpha splicefs-client-1.2.3-alpha-linux-amd64.tar.gz
accepts v1.2.3-dirty splicefs-client-1.2.3-dirty-linux-amd64.tar.gz
accepts v1.2.3-rc.1-dirty splicefs-client-1.2.3-rc.1-dirty-linux-amd64.tar.gz
refuses v1.2.3-foo.1
refuses v1.2
refuses v1.2.3-rc.1.2
# What git describe prints between releases and without tags is not a version any more; a
# build that gets one has not read the file VERSION.
refuses v1.2.3-4-gabc1234
refuses abc1234
refuses abc1234-dirty
refuses dev

[ "$failed" -eq 0 ] || { echo "$failed failed"; exit 1; }
echo "archive-lib: versions accepted and refused as expected"
