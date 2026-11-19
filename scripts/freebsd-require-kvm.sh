#!/bin/sh
# Stops at once, and says why, on a computer that cannot give a FreeBSD machine hardware
# virtualization. make check-freebsd runs it before it builds anything, so that nobody waits
# for a build to learn that the machine cannot start.
set -eu

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

. scripts/freebsd-lib.sh
freebsd_require_kvm
