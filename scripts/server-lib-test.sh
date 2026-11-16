#!/bin/sh
# Test of scripts/server-lib.sh. Runs on the host, no Docker, no sleeps (make server-lib-test,
# part of make check): the servers are small scripts that print what they were asked.
# The fake servers are written with single quotes on purpose: they expand when they run.
# shellcheck disable=SC2016
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failed=0

# shellcheck source=server-lib.sh
. "$root/scripts/server-lib.sh"

# $1 name, $2 wanted text, $3 text found.
expect() {
    if [ "$2" = "$3" ]; then
        printf 'ok   %s\n' "$1"
    else
        printf 'FAIL %s\n     want: %s\n     got:  %s\n' "$1" "$2" "$3"
        failed=$((failed + 1))
    fi
}

# fake FILE LABEL makes a program that prints its label, its arguments and its input.
fake() {
    printf '#!/bin/sh\necho "%s $* $(cat)"\n' "$2" >"$1"
    chmod 755 "$1"
}

# Sets hashed to what the hash command printed. A command that fails ends the test, because
# set -e is on.
hash_with() {
    printf 'secret\n' >"$work/input"
    hashed="$(server_hash_password "$1" <"$work/input")"
}

# A server that has the control program next to it: the command is there.
mkdir -p "$work/new"
fake "$work/new/splicefsd" server
fake "$work/new/splicefsdctl" control
hash_with "$work/new/splicefsd"
expect "the control program next to the server hashes" "control hash-password secret" "$hashed"

# An older server has no control program: it hashes itself.
mkdir -p "$work/old"
fake "$work/old/splicefsd" server
hash_with "$work/old/splicefsd"
expect "an older server hashes itself" "server hash-password secret" "$hashed"

# A control program that cannot run does not count as one.
mkdir -p "$work/broken"
fake "$work/broken/splicefsd" server
fake "$work/broken/splicefsdctl" control
chmod 644 "$work/broken/splicefsdctl"
hash_with "$work/broken/splicefsd"
expect "a control program that is not executable is ignored" "server hash-password secret" "$hashed"

# A folder with a space in its name works as well.
mkdir -p "$work/with space"
fake "$work/with space/splicefsd" server
fake "$work/with space/splicefsdctl" control
hash_with "$work/with space/splicefsd"
expect "folders with spaces work" "control hash-password secret" "$hashed"

[ "$failed" -eq 0 ] || { echo "$failed failed"; exit 1; }
echo "server programs: all checks as expected"
