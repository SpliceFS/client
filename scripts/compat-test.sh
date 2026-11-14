#!/bin/sh
# One run of one client against one server, both real programs on this machine. The owner's
# rule is that a client works with every server that has the same first version number and
# an equal or higher second number; scripts/compat.sh picks the servers, this script is
# one run.
#
#   compat-test.sh works   SPLICEFSD SPLICEFS   the client mounts the server's share and
#                                               reads every file back unchanged
#   compat-test.sh refused SPLICEFSD SPLICEFS   the client must refuse this server
#
# It needs Linux, root and /dev/fuse (a mount is made), and it exits with status 2 where
# they are missing. Everything lives in one temporary folder that is removed at the end,
# and so are the two programs and the mount, whatever happens. Status 1 means the test
# failed; the last lines of both logs are printed then.
set -eu

usage() {
    echo "usage: $0 works|refused SPLICEFSD SPLICEFS" >&2
    exit 2
}

[ "$#" -eq 3 ] || usage
mode="$1"
case "$mode" in
    works | refused) ;;
    *) usage ;;
esac

system="$(uname -s)"
user_id="$(id -u)"
if [ "$system" != Linux ] || [ "$user_id" -ne 0 ] || [ ! -c /dev/fuse ]; then
    echo "compat-test.sh needs Linux, root and /dev/fuse" >&2
    exit 2
fi

# The programs run from inside nothing but their own arguments, so a relative path is made
# absolute once here.
case "$2" in
    /*) server_bin="$2" ;;
    *) server_bin="$PWD/$2" ;;
esac
case "$3" in
    /*) client_bin="$3" ;;
    *) client_bin="$PWD/$3" ;;
esac

# The last word of "splicefs v0.4.4" is the version.
client_version="$("$client_bin" --version)"
server_version="$("$server_bin" --version)"
printf 'client %s against server %s\n' "${client_version##* }" "${server_version##* }"

port="${COMPAT_PORT:-18480}"
health_url="http://127.0.0.1:$port/api/v1/health"

work="$(mktemp -d)"
server_pid=""
client_pid=""
# A program that wait_until must not outlive: when it has ended, waiting longer for it only
# delays the report.
watch_pid=""
# With a mount path of "/" every share of the cluster is a folder below the mount point
# (client docs, "Mounts"), so the share media is this folder.
share="$work/mnt/media"

# The state letter of a process, or nothing when it is gone. A program that ended and was
# not waited for yet is a zombie (Z) and has ended too; kill -0 cannot tell the two apart.
process_state() {
    awk '/^State:/ { print $2 }' "/proc/$1/status" 2>/dev/null || true
}

process_ended() {
    ended_state="$(process_state "$1")"
    case "$ended_state" in
        "" | Z) return 0 ;;
        *) return 1 ;;
    esac
}

# Asks a program to end, kills it after 20 s and collects it.
stop_process() {
    [ -n "$1" ] || return 0
    kill "$1" 2>/dev/null || true
    stopped=0
    while :; do
        stop_state="$(process_state "$1")"
        case "$stop_state" in
            "" | Z) break ;;
            *) ;;
        esac
        if [ "$stopped" -ge 20 ]; then
            kill -9 "$1" 2>/dev/null || true
            break
        fi
        sleep 1
        stopped=$((stopped + 1))
    done
    wait "$1" 2>/dev/null || true
}

show_logs() {
    for name in server client; do
        if [ -f "$work/$name.log" ]; then
            echo "--- last 40 lines of the $name log ---"
            tail -n 40 "$work/$name.log"
        fi
    done
}

# Runs on every way out. The client is asked first so that it unmounts itself; the lazy
# unmount is for a client that could not. The folder is removed only when nothing is
# mounted in it, so that a mount that is still there can never lead into the data.
cleanup() {
    status=$?
    trap - EXIT INT TERM
    if [ "$status" -ne 0 ]; then show_logs >&2; fi
    stop_process "$client_pid"
    umount -l "$work/mnt" 2>/dev/null || true
    stop_process "$server_pid"
    if mountpoint -q "$work/mnt"; then
        echo "compat-test.sh: $work/mnt is still mounted, folder kept" >&2
    else
        rm -rf "$work"
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
    echo "compat-test.sh: $*" >&2
    exit 1
}

# wait_until SECONDS WHAT COMMAND...: runs the command once a second until it succeeds.
wait_until() {
    limit="$1"
    what="$2"
    shift 2
    waited=0
    until "$@"; do
        if [ -n "$watch_pid" ]; then
            watch_state="$(process_state "$watch_pid")"
            case "$watch_state" in
                "" | Z) fail "a program ended while waiting for: $what" ;;
                *) ;;
            esac
        fi
        waited=$((waited + 1))
        [ "$waited" -le "$limit" ] || fail "after $limit s still waiting for: $what"
        sleep 1
    done
}

server_answers() {
    curl -fsS --max-time 2 "$health_url" >/dev/null 2>&1
}

share_readable() {
    mountpoint -q "$work/mnt" && cmp -s "$work/data/hello.txt" "$share/hello.txt"
}

# Sorted list of the files below a folder, as paths relative to it.
list_files() {
    (cd "$1" && find . -type f | LC_ALL=C sort)
}

make_data() {
    mkdir -p "$work/data/one/two"
    printf 'hello from the server\n' >"$work/data/hello.txt"
    : >"$work/data/empty.txt"
    printf 'the name has a space\n' >"$work/data/with space.txt"
    printf 'two folders deep\n' >"$work/data/one/two/deep.txt"
    # More than two blocks of 5 MiB, so a read spans several requests.
    head -c $((12 * 1024 * 1024)) /dev/urandom >"$work/data/big.bin"
}

start_server() {
    mkdir -p "$work/etc/conf.d" "$work/etc/shares.d" "$work/var/shared/users"
    cat >"$work/etc/conf.d/10-listen.yaml" <<EOF
listen:
  - address: 127.0.0.1
    port: $port
    plain: true
EOF
    printf 'path: %s\n' "$work/data" >"$work/etc/shares.d/media.yaml"
    hash="$(printf 'secret\n' | "$server_bin" hash-password)"
    [ -n "$hash" ] || fail "hash-password printed nothing"
    cat >"$work/var/shared/users/alice.yaml" <<EOF
password_hash: "$hash"
shares: [media]
EOF
    "$server_bin" --config "$work/etc" --state "$work/var" >"$work/server.log" 2>&1 &
    server_pid=$!
    watch_pid="$server_pid"
    wait_until 20 "the server answers at $health_url" server_answers
    watch_pid=""
}

start_client() {
    mkdir -p "$work/c/conf.d" "$work/c/clusters.d" "$work/c/mounts.d" "$work/mnt"
    printf 'cache:\n  dir: %s\n' "$work/cache" >"$work/c/conf.d/10-cache.yaml"
    cat >"$work/c/clusters.d/test.yaml" <<EOF
url: http://127.0.0.1:$port/api
user: alice
password_file: secret
EOF
    printf 'secret\n' >"$work/c/secret"
    chmod 600 "$work/c/secret"
    printf 'cluster: test\ntarget: %s\n' "$work/mnt" >"$work/c/mounts.d/m.yaml"
    "$client_bin" --config "$work/c" mount >"$work/client.log" 2>&1 &
    client_pid=$!
}

check_works() {
    watch_pid="$client_pid"
    wait_until 30 "the mount at $work/mnt shows hello.txt" share_readable
    watch_pid=""

    list_files "$work/data" >"$work/files.want"
    list_files "$share" >"$work/files.got"
    if ! diff -u "$work/files.want" "$work/files.got"; then
        fail "the files below the mount are not the files of the share"
    fi

    # Names with a space need the loop to read whole lines.
    while IFS= read -r file; do
        want="$(sha256sum <"$work/data/$file")"
        got="$(sha256sum <"$share/$file")"
        [ "$want" = "$got" ] || fail "content differs: $file"
    done <"$work/files.want"

    # The first read filled the cache; this one is served from it.
    want="$(sha256sum <"$work/data/big.bin")"
    got="$(sha256sum <"$share/big.bin")"
    [ "$want" = "$got" ] || fail "content differs on the second read: big.bin"

    kill -TERM "$client_pid"
    wait_until 20 "the client ends after SIGTERM" process_ended "$client_pid"
    wait "$client_pid" || true
    client_pid=""
    if mountpoint -q "$work/mnt"; then
        fail "the mount is still there after the client ended"
    fi
    echo "ok: works"
}

check_refused() {
    wait_until 20 "the client ends by itself" process_ended "$client_pid"
    status=0
    wait "$client_pid" || status=$?
    client_pid=""
    [ "$status" -eq 1 ] || fail "the client ended with status $status, wanted 1"
    if mountpoint -q "$work/mnt"; then
        fail "the client mounted, but it must refuse this server"
    fi
    grep -q 'needs splicefsd' "$work/client.log" ||
        fail "the client log does not contain: needs splicefsd"
    echo "ok: refused"
}

make_data
start_server
start_client
case "$mode" in
    works) check_works ;;
    refused) check_refused ;;
    *) usage ;;
esac
