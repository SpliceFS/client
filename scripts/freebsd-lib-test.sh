#!/bin/sh
# Host test of freebsd-lib.sh: no download, no machine. freebsd_each_release must call its step
# for every release in the list, whatever names the step uses for its own variables (shell
# variables are global, so a step's loop variable must not replace the lib's).
set -eu
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fail() {
    echo "FAIL: $1" >&2
    exit 1
}
timed() {
    shift
    "$@"
}

# shellcheck source=freebsd-lib.sh
. "$root/scripts/freebsd-lib.sh"
# The lib sets machine from this computer; the list below names that machine, and its disks
# are already in the cache, so nothing is downloaded or unpacked.
out="$work/out"
mkdir -p "$work/packaging/freebsd" "$out/cache/freebsd"
for release in 14.5 15.1; do
    printf '%s %s 0 https://example.invalid/%s.raw.xz\n' "$release" "$machine" "$release" >>"$work/packaging/freebsd/images"
    : >"$out/cache/freebsd/$release.raw"
done

calls=""
# A step that loops with a variable named like the lib's own: each.
step() {
    for each in first second; do : "$each"; done
    calls="$calls $1"
}
# A step the lib cannot find ends this script with "not found" (set -e).
cd "$work"
freebsd_each_release step
[ "$calls" = " 14.5 15.1" ] || fail "the steps ran for [$calls], want [ 14.5 15.1]"
echo "freebsd-lib: every release ran its step"
