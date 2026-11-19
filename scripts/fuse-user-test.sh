#!/bin/sh
# Test of scripts/fuse-user.sh, which writes the two files that let an ordinary user mount with
# fusermount3 inside an image. Runs on the host, no Docker (make fuse-user-test, part of make
# unit). What is checked is what the files say: the user and group the part runs as, and the
# setting an administrator makes for allow_other.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failed=0

# $1 case name, $2 file, $3 the line that must be in it exactly once.
has_line() {
    count="$(grep -cxF -- "$3" "$2" || true)"
    if [ "$count" -eq 1 ]; then
        printf 'ok   %s\n' "$1"
    else
        printf 'FAIL %s: "%s" is in %s %s times, want 1\n' "$1" "$3" "$2" "$count"
        failed=$((failed + 1))
    fi
}

"$root/scripts/fuse-user.sh" "$work/etc" 4242 4343

has_line "passwd names the user the part runs as" "$work/etc/passwd" "user:x:4242:4343:user:/tmp:/bin/sh"
has_line "passwd keeps root" "$work/etc/passwd" "root:x:0:0:root:/root:/bin/sh"
has_line "fuse.conf lets a user mount with allow_other" "$work/etc/fuse.conf" "user_allow_other"

# A second run for another user replaces the files: the folder is kept between runs.
"$root/scripts/fuse-user.sh" "$work/etc" 1001 118
has_line "passwd follows the user of the next run" "$work/etc/passwd" "user:x:1001:118:user:/tmp:/bin/sh"
if grep -q 4242 "$work/etc/passwd"; then
    echo "FAIL passwd still names the user of the first run"
    failed=$((failed + 1))
else
    echo "ok   passwd does not keep the user of the first run"
fi

# Without the numbers it uses the caller's own.
user_id="$(id -u)"
group_id="$(id -g)"
"$root/scripts/fuse-user.sh" "$work/own"
has_line "passwd names the caller by default" "$work/own/passwd" "user:x:$user_id:$group_id:user:/tmp:/bin/sh"

status=0
"$root/scripts/fuse-user.sh" >/dev/null 2>&1 || status=$?
if [ "$status" -eq 2 ]; then
    echo "ok   refuses a call without a folder"
else
    echo "FAIL a call without a folder ended with status $status, want 2"
    failed=$((failed + 1))
fi

[ "$failed" -eq 0 ] || { echo "$failed failed"; exit 1; }
echo "fuse user: all cases as expected"
