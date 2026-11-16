# shellcheck shell=sh
# What the test scripts need from a server program, in one place so that the scripts that start
# servers ask the same way. The file is sourced, so it runs nothing itself.

# server_hash_password SERVER reads a password on standard input and prints its hash. SERVER is
# the path of the server program. Newer servers keep the command in the control program built
# into the same folder (splicefsdctl); older ones have it in the server program itself, and
# the tests run against both.
server_hash_password() {
    control="$(dirname "$1")/splicefsdctl"
    if [ -x "$control" ]; then
        "$control" hash-password
    else
        "$1" hash-password
    fi
}
