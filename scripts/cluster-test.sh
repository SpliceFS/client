#!/bin/sh
# One client against several servers, all real programs on this machine. compat-test.sh
# proves that one client works with one server; this script proves what only a cluster
# shows: the folder listing is the union of the servers, a read uses only the servers that
# hold the winning version, and a server that is in the node list but not running does not
# break either.
#
#   cluster-test.sh SPLICEFSD SPLICEFS
#
# Three servers share users, signing keys and the node list through one folder (each has
# its state folder's "shared" as a link to it); their own data folders are separate. The
# client's cluster host is server 1. Every scenario starts from stopped programs, empty data
# folders, an empty cache and an empty node list, and prints "ok: <name>" when all its
# checks passed.
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
client_pid=""
# A program that wait_until must not outlive: when it has ended, waiting longer for it only
# delays the report.
watch_pid=""
# The name of the running scenario, for the failure message.
scenario="set-up"
# With a mount path of "/" every share of the cluster is a folder below the mount point
# (client docs, "Mounts"), so the share media is this folder.
share="$work/mnt/media"
# Fixed modification times for the scenarios that need one copy to be newer than another.
time_old="2026-01-01 12:00:00"
time_mid="2026-03-01 12:00:00"
time_new="2026-06-01 12:00:00"

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
    if [ -s "$work/check.txt" ]; then
        echo "--- the last comparison of the failed check ---"
        cat "$work/check.txt"
    fi
    for log_name in client server1 server2 server3; do
        if [ -f "$work/$log_name.log" ]; then
            echo "--- last 30 lines of the $log_name log ---"
            tail -n 30 "$work/$log_name.log"
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
    for cleanup_number in 1 2 3; do
        if [ -f "$work/server$cleanup_number.pid" ]; then
            cleanup_pid="$(cat "$work/server$cleanup_number.pid")"
            stop_process "$cleanup_pid"
        fi
    done
    if mountpoint -q "$work/mnt"; then
        echo "cluster-test.sh: $work/mnt is still mounted, folder kept" >&2
    else
        rm -rf "$work"
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# A failed check names its scenario; cleanup prints the logs.
fail() {
    echo "cluster-test.sh: scenario \"$scenario\": $*" >&2
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

# The port of server N.
port_of() {
    echo $((base_port + $1 - 1))
}

# Sorted list of the files below a folder, as paths relative to it.
list_files() {
    (cd "$1" && find . -type f | LC_ALL=C sort)
}

# The SHA-256 of a file, without the file name. A file that cannot be read fails the check
# here (the exit of fail ends only the substitution this runs in, and set -e in the caller
# ends the script with the same status).
hash_of() {
    sum_line="$(sha256sum <"$1")" || fail "cannot read: $1"
    printf '%s\n' "${sum_line%% *}"
}

# The users, the signing keys and the node list that all servers share. The password hash
# comes from the server program, so it is written here once and not per server.
make_shared() {
    mkdir -p "$work/shared/users"
    secret_hash="$(printf 'secret\n' | "$server_bin" hash-password)"
    [ -n "$secret_hash" ] || fail "hash-password printed nothing"
    cat >"$work/shared/users/alice.yaml" <<EOF
password_hash: "$secret_hash"
shares: [media]
EOF
}

# The files every scenario that needs "the same five files" copies. They are made once, so
# the 12 MiB random file costs one read of /dev/urandom for the whole run.
make_source() {
    mkdir -p "$work/source"
    put_text "$work/source" hello.txt 'hello from the cluster'
    : >"$work/source/empty.txt"
    put_text "$work/source" "with space.txt" 'the name has a space'
    put_text "$work/source" one/two/deep.txt 'two folders deep'
    # More than two blocks of 5 MiB, so a read spans several requests.
    put_random "$work/source" big.bin $((12 * 1024 * 1024))
}

# The file that only scenario 1 copies. A block is 5 MiB and the client keeps 4 requests in
# flight per server, so a 12 MiB file is read from one server alone. 96 MiB is 20 blocks,
# enough for every server to be asked for some. It is made once, apart from the five files,
# so the other scenarios do not carry it.
make_large_source() {
    mkdir -p "$work/source-large"
    put_random "$work/source-large" large.bin $((96 * 1024 * 1024))
}

# put_text FOLDER NAME TEXT and put_random FOLDER NAME BYTES write one file, with its
# folders. A scenario in which two servers hold one name with different contents calls
# them once per server.
put_text() {
    put_target="$1/$2"
    mkdir -p "${put_target%/*}"
    printf '%s\n' "$3" >"$put_target"
}

put_random() {
    put_target="$1/$2"
    mkdir -p "${put_target%/*}"
    head -c "$3" /dev/urandom >"$put_target"
}

# Copies the five files into the data folder of server N. cp -p keeps size and
# modification time, so every server holds the same version of each file.
copy_source() {
    cp -pR "$work/source/." "$work/data$1/"
}

copy_large() {
    cp -p "$work/source-large/large.bin" "$work/data$1/"
}

# Sets the modification time of a file. GNU touch; the script runs on Linux only.
stamp() {
    touch -d "$3" "$1/$2"
}

# The folders and the configuration of server N: its own data folder (the share media),
# its own state folder, and "shared" in it as a link to the one shared folder. The link is
# made before the first start. Every server has its own node name, because of two servers
# with one name the node list keeps only one.
make_server() {
    make_port="$(port_of "$1")"
    mkdir -p "$work/data$1" "$work/etc$1/conf.d" "$work/etc$1/shares.d" "$work/state$1"
    ln -sfn "$work/shared" "$work/state$1/shared"
    cat >"$work/etc$1/conf.d/10-server.yaml" <<EOF
listen:
  - address: 127.0.0.1
    port: $make_port
    plain: true
cluster:
  node: n$1
  url: "http://127.0.0.1:$make_port/api"
log:
  level: info
  format: text
EOF
    printf 'path: %s\n' "$work/data$1" >"$work/etc$1/shares.d/media.yaml"
}

server_answers() {
    answer_port="$(port_of "$1")"
    curl -fsS --max-time 2 "http://127.0.0.1:$answer_port/api/v1/health" >/dev/null 2>&1
}

# Starts server N and waits for its health answer. The process number goes into a file,
# because a shell has no list of numbered variables.
start_server() {
    make_server "$1"
    "$server_bin" --config "$work/etc$1" --state "$work/state$1" >"$work/server$1.log" 2>&1 &
    started_pid=$!
    printf '%s\n' "$started_pid" >"$work/server$1.pid"
    watch_pid="$started_pid"
    wait_until 20 "server $1 answers on its health URL" server_answers "$1"
    watch_pid=""
}

stop_server() {
    [ -f "$work/server$1.pid" ] || return 0
    server_pid="$(cat "$work/server$1.pid")"
    stop_process "$server_pid"
    rm -f "$work/server$1.pid"
}

# Puts node N in the node list without running it: the list is every file in nodes/, and a
# server that is stopped stays in it as well, so a file for a port where nothing listens
# is the same thing. The name is 16 hex digits.
write_idle_node() {
    idle_port="$(port_of "$1")"
    idle_file="$(printf 'ffffffffffffff%02x' "$1")"
    mkdir -p "$work/shared/nodes"
    printf 'name: n%s\nurl: http://127.0.0.1:%s/api\n' "$1" "$idle_port" \
        >"$work/shared/nodes/$idle_file.yaml"
}

# The client's configuration. Its cluster host is server 1; the other servers it learns from
# the node list.
make_client_config() {
    first_port="$(port_of 1)"
    mkdir -p "$work/c/conf.d" "$work/c/clusters.d" "$work/c/mounts.d" "$work/mnt"
    printf 'cache:\n  dir: %s\n' "$work/cache" >"$work/c/conf.d/10-cache.yaml"
    cat >"$work/c/clusters.d/test.yaml" <<EOF
url: http://127.0.0.1:$first_port/api
user: alice
password_file: secret
EOF
    printf 'secret\n' >"$work/c/secret"
    chmod 600 "$work/c/secret"
    printf 'cluster: test\ntarget: %s\n' "$work/mnt" >"$work/c/mounts.d/m.yaml"
}

share_listable() {
    mountpoint -q "$work/mnt" && ls "$share" >/dev/null 2>&1
}

start_client() {
    make_client_config
    "$client_bin" --config "$work/c" mount >"$work/client.log" 2>&1 &
    client_pid=$!
    watch_pid="$client_pid"
    wait_until 30 "the mount at $work/mnt can be listed" share_listable
    watch_pid=""
}

not_mounted() {
    ! mountpoint -q "$work/mnt"
}

stop_client() {
    [ -n "$client_pid" ] || return 0
    kill -TERM "$client_pid" 2>/dev/null || true
    wait_until 20 "the client ends after SIGTERM" process_ended "$client_pid"
    wait "$client_pid" || true
    client_pid=""
    wait_until 20 "$work/mnt is no mount point" not_mounted
}

restart_client() {
    stop_client
    start_client
}

restart_client_empty_cache() {
    stop_client
    empty_cache
    start_client
}

# The folder goes, not only what is in it, so the client makes it as it does on a first
# start.
empty_cache() {
    rm -rf "$work/cache"
}

clear_data() {
    for clear_number in 1 2 3; do
        rm -rf "$work/data$clear_number"
        mkdir -p "$work/data$clear_number"
    done
}

# Every scenario starts here: nothing runs, no data, no cache, an empty node list, and no
# state that one server keeps for itself.
begin_scenario() {
    scenario="$1"
    stop_client
    for begin_number in 1 2 3; do
        stop_server "$begin_number"
        rm -rf "$work/state$begin_number/local"
    done
    clear_data
    empty_cache
    rm -rf "$work/shared/nodes"
    mkdir -p "$work/shared/nodes"
}

end_scenario() {
    echo "ok: $scenario"
}

# A check that follows a start may run before the client has heard from every server, so each
# check compares again once a second for at most 15 s and fails only when it still differs.
# What the last comparison found is left in check.txt for the failure report.
trees_equal() {
    list_files "$1" >"$work/files.want"
    list_files "$share" >"$work/files.got" 2>/dev/null
    diff -u "$work/files.want" "$work/files.got" >"$work/check.txt" 2>&1
}

# The files below the mount are the files below DIR (names only).
expect_same_tree() {
    wait_until 15 "the files below the mount are the files of $1" trees_equal "$1"
}

hash_is() {
    if [ ! -f "$1" ]; then
        printf 'missing: %s\n' "$1" >"$work/check.txt"
        return 1
    fi
    is_sum="$(sha256sum <"$1" 2>"$work/check.txt")" || return 1
    is_hash="${is_sum%% *}"
    if [ "$is_hash" != "$2" ]; then
        printf '%s\nhas SHA-256 %s\nwanted      %s\n' "$1" "$is_hash" "$2" >"$work/check.txt"
        return 1
    fi
    : >"$work/check.txt"
}

# The file has this SHA-256 (as hash_of prints it).
expect_hash() {
    wait_until 15 "$1 has the wanted content" hash_is "$1" "$2"
}

# The file has the SHA-256 HASH1 or HASH2, and never anything else, which would be a mix of
# the two copies. Which one it was is left in picked_hash.
hash_is_either() {
    either_hash=""
    if [ ! -f "$1" ]; then
        printf 'missing: %s\n' "$1" >"$work/check.txt"
        return 1
    fi
    either_sum="$(sha256sum <"$1" 2>"$work/check.txt")" || return 1
    either_hash="${either_sum%% *}"
    if [ "$either_hash" != "$2" ] && [ "$either_hash" != "$3" ]; then
        printf '%s\nhas SHA-256 %s\nwanted      %s or %s\n' "$1" "$either_hash" "$2" "$3" \
            >"$work/check.txt"
        return 1
    fi
    : >"$work/check.txt"
}

expect_either_copy() {
    wait_until 15 "$1 has one of the two copies" hash_is_either "$1" "$2" "$3"
    picked_hash="$either_hash"
}

# Every file below DIR is below the mount with the same content. Names with a space need
# the loop to read whole lines.
expect_hashes_from() {
    list_files "$1" >"$work/files.hash"
    while IFS= read -r hashed_file; do
        want="$(hash_of "$1/$hashed_file")"
        expect_hash "$share/$hashed_file" "$want"
    done <"$work/files.hash"
}

# The file NAME of the share was fetched from at least MIN servers. A server logs one line
# per request (text format, which make_server sets): method, path, status. Run this after
# the file was read, because a read is what makes the client fetch.
expect_served_by_servers() {
    served=0
    for served_number in 1 2 3; do
        if grep -Eq "method=GET path=/api/v1/file/media/$1 status=(200|206)( |\$)" \
            "$work/server$served_number.log"; then
            served=$((served + 1))
        fi
    done
    [ "$served" -ge "$2" ] || fail "$1 was fetched from $served servers, wanted at least $2"
}

scenario_three_servers_same_files() {
    begin_scenario "three servers, the same files"
    for n in 1 2 3; do
        copy_source "$n"
        copy_large "$n"
    done
    for n in 1 2 3; do start_server "$n"; done
    start_client
    expect_same_tree "$work/data1"
    expect_hashes_from "$work/source"
    # Reading large.bin completely is what makes the client fetch its blocks.
    expect_hashes_from "$work/source-large"
    expect_served_by_servers large.bin 2
    end_scenario
}

scenario_two_running_same_files() {
    begin_scenario "three in the list, two running, the same files"
    for n in 1 2; do copy_source "$n"; done
    write_idle_node 3
    for n in 1 2; do start_server "$n"; done
    start_client
    expect_same_tree "$work/data1"
    expect_hashes_from "$work/source"
    end_scenario
}

scenario_two_running_different_files() {
    begin_scenario "three in the list, two running, different files"
    put_text "$work/data1" a.txt 'only server 1 has this'
    put_random "$work/data1" only-one/x.bin $((1024 * 1024))
    put_text "$work/data2" b.txt 'only server 2 has this'
    put_random "$work/data2" only-two/y.bin $((1024 * 1024))
    write_idle_node 3
    for n in 1 2; do start_server "$n"; done
    start_client
    # The four names are all different, so the union is the copy of both folders.
    rm -rf "$work/union"
    mkdir -p "$work/union"
    cp -pR "$work/data1/." "$work/data2/." "$work/union/"
    expect_same_tree "$work/union"
    expect_hashes_from "$work/data1"
    expect_hashes_from "$work/data2"
    end_scenario
}

scenario_two_running_files_on_one() {
    begin_scenario "three in the list, two running, files on one only"
    copy_source 2
    write_idle_node 3
    for n in 1 2; do start_server "$n"; done
    start_client
    expect_same_tree "$work/data2"
    expect_hashes_from "$work/source"
    end_scenario
}

# doc-a.txt is newer on server 1, doc-b.txt on server 2, and the texts differ in length. The
# newer copy wins whichever server holds it.
expect_newest_copies() {
    expect_hash "$share/doc-a.txt" "$1"
    expect_hash "$share/doc-b.txt" "$2"
}

scenario_newest_wins() {
    begin_scenario "same name, different contents, the newest wins"
    put_text "$work/data1" doc-a.txt 'doc a as server 1 has it, the newer copy of this file'
    stamp "$work/data1" doc-a.txt "$time_new"
    put_text "$work/data2" doc-a.txt 'older doc a'
    stamp "$work/data2" doc-a.txt "$time_old"
    put_text "$work/data1" doc-b.txt 'older doc b'
    stamp "$work/data1" doc-b.txt "$time_old"
    put_text "$work/data2" doc-b.txt 'doc b as server 2 has it, the newer copy of this file'
    stamp "$work/data2" doc-b.txt "$time_new"
    write_idle_node 3
    for n in 1 2; do start_server "$n"; done
    start_client
    hash_a="$(hash_of "$work/data1/doc-a.txt")"
    hash_b="$(hash_of "$work/data2/doc-b.txt")"
    expect_newest_copies "$hash_a" "$hash_b"
    # Again in the same mount, with the cache kept, and with the cache emptied: it is the
    # same copy every time.
    expect_newest_copies "$hash_a" "$hash_b"
    restart_client
    expect_newest_copies "$hash_a" "$hash_b"
    restart_client_empty_cache
    expect_newest_copies "$hash_a" "$hash_b"
    end_scenario
}

scenario_same_size_and_time() {
    begin_scenario "same name, same size and time, different contents"
    put_random "$work/data1" same.bin $((1024 * 1024))
    stamp "$work/data1" same.bin "$time_mid"
    put_random "$work/data2" same.bin $((1024 * 1024))
    stamp "$work/data2" same.bin "$time_mid"
    write_idle_node 3
    for n in 1 2; do start_server "$n"; done
    start_client
    hash_one="$(hash_of "$work/data1/same.bin")"
    hash_two="$(hash_of "$work/data2/same.bin")"
    # Which copy wins is fixed by the client's order, so it is not named here; it must be one
    # of the two, and the same one every time.
    expect_either_copy "$share/same.bin" "$hash_one" "$hash_two"
    first_hash="$picked_hash"
    expect_hash "$share/same.bin" "$first_hash"
    restart_client
    expect_hash "$share/same.bin" "$first_hash"
    restart_client_empty_cache
    expect_hash "$share/same.bin" "$first_hash"
    end_scenario
}

scenario_most_servers_win() {
    begin_scenario "same name, different contents, most servers win"
    put_text "$work/data1" vote.txt 'the text that two servers hold'
    stamp "$work/data1" vote.txt "$time_old"
    cp -p "$work/data1/vote.txt" "$work/data2/vote.txt"
    put_text "$work/data3" vote.txt 'another text, held by one server only, with the newer time'
    stamp "$work/data3" vote.txt "$time_new"
    for n in 1 2 3; do start_server "$n"; done
    start_client
    hash_two_servers="$(hash_of "$work/data1/vote.txt")"
    expect_hash "$share/vote.txt" "$hash_two_servers"
    end_scenario
}

# Reads FILE once, waiting at most 20 s, and prints one line that says what happened. Nothing
# is checked: what the client should do when the server that holds the chosen copy is gone is
# not decided yet, and this line shows what it does today.
note_read() {
    read_status=0
    timeout -k 5 20 cat "$1" >"$work/read.out" 2>"$work/read.err" || read_status=$?
    if [ "$read_status" -eq 0 ]; then
        read_hash="$(hash_of "$work/read.out")"
        case "$read_hash" in
            "$2") echo "note: the read gave the copy of server 1" ;;
            "$3") echo "note: the read gave the copy of server 2" ;;
            *) echo "note: the read gave neither copy" ;;
        esac
    elif [ "$read_status" -eq 124 ]; then
        echo "note: the read did not finish in 20 s"
    else
        read_error="$(tr '\n' ' ' <"$work/read.err")"
        echo "note: the read failed with status $read_status: ${read_error% }"
    fi
}

scenario_chosen_server_stops() {
    begin_scenario "the server with the chosen copy stops"
    put_text "$work/data1" gone.txt 'the older copy on server 1'
    stamp "$work/data1" gone.txt "$time_old"
    put_text "$work/data2" gone.txt 'the newer copy on server 2, with a longer text'
    stamp "$work/data2" gone.txt "$time_new"
    write_idle_node 3
    for n in 1 2; do start_server "$n"; done
    start_client
    gone_one="$(hash_of "$work/data1/gone.txt")"
    gone_two="$(hash_of "$work/data2/gone.txt")"
    expect_hash "$share/gone.txt" "$gone_two"
    stop_client
    stop_server 2
    empty_cache
    start_client
    note_read "$share/gone.txt" "$gone_one" "$gone_two"
    end_scenario
}

make_shared
make_source
make_large_source

scenario_three_servers_same_files
scenario_two_running_same_files
scenario_two_running_different_files
scenario_two_running_files_on_one

scenario_newest_wins
scenario_same_size_and_time
scenario_most_servers_win
scenario_chosen_server_stops

# more scenarios follow here

echo "cluster: all scenarios as expected"
