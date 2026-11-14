#!/bin/sh
# One client against several servers, all real programs on this machine. compat-test.sh
# proves that one client works with one server; this script proves what only a cluster
# shows: the folder listing is the union of the servers, a read uses only the servers that
# hold the winning version, and a server that is in the node list but not running does not
# break either.
#
#   cluster-test.sh SPLICEFSD SPLICEFS
#
# Up to five servers share users, signing keys and the node list through one folder (each has
# its state folder's "shared" as a link to it); their own data folders are separate. The
# client's cluster host is server 1. The helpers are in cluster-lib.sh and the scenarios in
# cluster-scenarios/, sourced in name order. Every scenario starts from stopped programs,
# empty data folders, an empty cache, an empty node list and default client settings, and
# prints "ok: <name>" when all its checks passed.
#
# CLUSTER_ONLY=text runs only the scenarios whose name contains the text. CLUSTER_PORT is the
# port of server 1 (18490 if unset); server N listens on the Nth port from there.
#
# It needs Linux, root and /dev/fuse (a mount is made), and it exits with status 2 where
# they are missing. Everything lives in one temporary folder that is removed at the end,
# and so are the programs and the mount, whatever happens. Status 1 means a check failed;
# the last lines of the client log and of every server log are printed then.
set -eu

usage() {
    echo "usage: $0 SPLICEFSD SPLICEFS" >&2
    exit 2
}

[ "$#" -eq 2 ] || usage

system="$(uname -s)"
user_id="$(id -u)"
if [ "$system" != Linux ] || [ "$user_id" -ne 0 ] || [ ! -c /dev/fuse ]; then
    echo "cluster-test.sh needs Linux, root and /dev/fuse" >&2
    exit 2
fi

# The programs run from inside nothing but their own arguments, so a relative path is made
# absolute once here.
case "$1" in
    /*) server_bin="$1" ;;
    *) server_bin="$PWD/$1" ;;
esac
case "$2" in
    /*) client_bin="$2" ;;
    *) client_bin="$PWD/$2" ;;
esac

# The last word of "splicefs v0.4.4" is the version.
client_version="$("$client_bin" --version)"
server_version="$("$server_bin" --version)"
printf 'client %s against server %s\n' "${client_version##* }" "${server_version##* }"

# Server N listens on the Nth port from here.
base_port="${CLUSTER_PORT:-18490}"

work="$(mktemp -d)"

case "$0" in
    */*) script_dir="${0%/*}" ;;
    *) script_dir=. ;;
esac
script_dir="$(cd "$script_dir" && pwd)"

# shellcheck source=cluster-lib.sh
. "$script_dir/cluster-lib.sh"

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

make_shared
make_source
make_large_source

for scenario_file in "$script_dir"/cluster-scenarios/*.sh; do
    [ -f "$scenario_file" ] || fail "no scenario file in $script_dir/cluster-scenarios"
    # The files in the folder are found at run time, so ShellCheck cannot follow them.
    # shellcheck source=/dev/null
    . "$scenario_file"
done

run_scenarios

echo "cluster: all scenarios as expected"
